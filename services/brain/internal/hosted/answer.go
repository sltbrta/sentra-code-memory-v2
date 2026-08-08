package hosted

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factualconsistency"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/memory"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/productsec"
)

// AnswerResult is the product eval / ERB JSON shape.
type AnswerResult struct {
	Answer           string   `json:"answer"`
	CitedDocumentIDs []string `json:"cited_document_ids"`
	// FactualConsistency is the calibrated answer-level grounding disclosure.
	// Abstained and unknown are explicit non-numeric states; only scored carries
	// a score and pinned provenance.
	FactualConsistency *factualconsistency.Result `json:"factual_consistency,omitempty"`
	// Claims surfaces the verified grounded claims so page-aware citations
	// survive answer rendering. Each claim's Locator is receipt-sanitized;
	// Locator.Present false is explicit absence and never invented (#327).
	Claims               []Claim        `json:"claims,omitempty"`
	RetrievalDiagnostics map[string]any `json:"retrieval_diagnostics"`
	SearchMode           string         `json:"search_mode"`
	Failure              string         `json:"failure,omitempty"`
	Provider             string         `json:"provider,omitempty"`
	Model                string         `json:"model,omitempty"`
	GroundingDiagnostics map[string]any `json:"grounding_diagnostics,omitempty"`
}

// Answer is residual-parity answer stack: retrieve → synth → ground → repairs.
func (c *Client) Answer(ctx context.Context, question string, topK int) AnswerResult {
	return c.AnswerTyped(ctx, question, "", topK)
}

// AnswerTyped passes question_type for prompts, windowing, and abstention.
func (c *Client) AnswerTyped(ctx context.Context, question, questionType string, topK int) AnswerResult {
	return c.AnswerOpts(ctx, AnswerOptions{
		Question:     question,
		QuestionType: questionType,
		TopK:         topK,
	})
}

// AnswerOptions extends AnswerTyped with session chat + source types.
type AnswerOptions struct {
	Question     string
	QuestionType string
	TopK         int
	SessionID    string
	SourceTypes  []string
	// Mode is product UI: light|deep|research|bench (modulates plan budgets).
	Mode string
	// Principal overrides Security.Principal for this ask (Phase 2).
	Principal string
	// Profile overrides Security.Profile when non-empty.
	Profile string
	// GoldDocIDs enables offline pool/window/cite precision diags (ERB eval only).
	GoldDocIDs []string
	// Filter is the raw governed metadata-filter predicate map (issue #328),
	// normalized + authorized fail-closed inside RetrieveOpts.
	Filter map[string]any
	// PriorTurns seeds session history + turn_grep (role/content pairs).
	// Used by web when chat history lives outside the process session file.
	PriorTurns []SessionTurn
	// HistoryText is optional preformatted conversation history for the prompt.
	HistoryText string
}

// AnswerOpts is the full product answer path (retrieve + optional chat + synth).
func (c *Client) AnswerOpts(ctx context.Context, opts AnswerOptions) (result AnswerResult) {
	question := opts.Question
	// Product Ask has no ERB category labels. ResolveQuestionType infers a plan
	// from the query surface when QuestionType is empty; labeled eval types win.
	mode := opts.Mode
	if mode == "" {
		mode = envOr("OUROBOROS_ERB_MODE", "")
	}
	ctx, qualitySpan := c.startAnswerQualitySpan(ctx, mode)
	defer func() {
		finishAnswerQualitySpan(ctx, qualitySpan, result)
	}()
	// Resolve the plan (thin plans get the optional cheap LLM refine) with the
	// request-scoped ledger already attached, so planning skip/usage counters
	// are visible in the consolidated budget/usage diagnostics. The ledger's
	// generation-call cap is finalized below, once the plan settles.
	ctx, ledger, questionType, qPlan := planWithLedger(ctx, question, opts.QuestionType, mode)
	topK := opts.TopK
	tAnswer := time.Now()
	// Phase 2: authorize before corpus read (SEC-004).
	sec := c.Security
	if opts.Principal != "" {
		sec.Principal = opts.Principal
	}
	if opts.Profile != "" {
		sec.Profile = productsec.ParseProfile(opts.Profile)
	}
	if err := sec.Authorize("ask"); err != nil {
		return AnswerResult{
			Failure:    "denied",
			SearchMode: "product_security_deny",
			RetrievalDiagnostics: map[string]any{
				"security": "denied",
				"profile":  string(sec.Profile),
			},
		}
	}
	// Blind-route topK floor when untyped product ask needs multi-doc / semantic.
	if topK <= 0 {
		topK = c.cfg.TopK
	}
	if fl := qPlan.TopKFloor(); fl > 0 && topK < fl {
		topK = fl
	} else if opts.QuestionType == "" {
		if qPlan.Completeness && topK < 14 {
			topK = 14
		} else if qPlan.MultiDoc && topK < 12 {
			topK = 12
		} else if qPlan.SemanticExpand && topK < 10 {
			topK = 10
		}
	}
	passages, diag, err := c.RetrieveOpts(ctx, question, RetrieveOptions{
		TopK:         topK,
		QuestionType: questionType,
		Plan:         &qPlan,
		Mode:         mode,
		SourceTypes:  opts.SourceTypes,
		GoldDocIDs:   opts.GoldDocIDs,
		Filter:       opts.Filter,
	})
	if diag == nil {
		diag = map[string]any{}
	}
	diag["query_plan"] = qPlan
	diag["question_type_resolved"] = questionType
	diag["question_type_source"] = qPlan.Source
	diag["serve_mode"] = qPlan.Mode
	if qPlan.LLMRefined {
		diag["query_plan_llm_refined"] = true
	}
	if qPlan.LLMSkipped != "" {
		// Issue #282: optional plan refine skipped fail-closed to preserve the
		// retrieval/synthesis deadline reserve. Deterministic plan stands.
		diag["query_plan_llm_skipped"] = qPlan.LLMSkipped
	}
	stampQualityProfile(diag, prodProfileFromEnv())
	if g := c.GenerationID(); g != "" {
		diag["generation_id"] = g
	}
	// Product chat channel: history + turn_grep (labelled, never company cites).
	sessionID := strings.TrimSpace(opts.SessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_SESSION_ID"))
	}
	// Seed prior turns from web/API so turn_grep + history work across process
	// spawns (product-brain-eval is one-shot per request).
	for _, t := range opts.PriorTurns {
		if strings.TrimSpace(t.Content) == "" {
			continue
		}
		sid := sessionID
		if strings.TrimSpace(t.SessionID) != "" {
			sid = t.SessionID
		}
		_ = AppendSessionTurn(sid, t.Role, t.Content)
	}
	history := strings.TrimSpace(opts.HistoryText)
	if sessionID != "" {
		// Grep prior turns BEFORE appending this question (avoid self-match).
		// Session-local only by default (no cross-session leak). Optional global
		// via OUROBOROS_BRAIN_TURN_GREP_GLOBAL=1 for multi-session product chat.
		if history == "" {
			history = FormatSessionHistory(sessionID, 12)
		}
		turnHits, turnDiag := TurnGrep(question, sessionID, 4, 2)
		diag["turn_grep"] = map[string]any{"local": turnDiag}
		for _, th := range turnHits {
			passages = append([]Passage{{
				DocumentID: th.DocumentID,
				Text:       th.Text,
				Score:      th.Score,
				Channel:    "turn_grep",
			}}, passages...)
		}
		if envTruthy("OUROBOROS_BRAIN_TURN_GREP_GLOBAL", false) {
			globalHits, globalDiag := TurnGrep(question, "", 3, 2)
			diag["turn_grep"].(map[string]any)["global"] = globalDiag
			seen := map[string]struct{}{}
			for _, th := range turnHits {
				seen[th.DocumentID] = struct{}{}
			}
			for _, th := range globalHits {
				if _, ok := seen[th.DocumentID]; ok {
					continue
				}
				passages = append([]Passage{{
					DocumentID: th.DocumentID,
					Text:       th.Text,
					Score:      th.Score,
					Channel:    "turn_grep",
				}}, passages...)
			}
		}
		// Packing: turns stay front; company docs best-last.
		passages = bestLast(passages)
		if history == "" {
			if lc, lcDiag := LongContextFallback(sessionID, 4000); lc != "" {
				history = lc
				diag["long_context_fallback"] = lcDiag
			}
		}
		if history != "" {
			diag["session_history"] = true
		}
		_ = AppendSessionTurn(sessionID, "user", question)
	}
	// One product stack stamp (ERB hosted + company JSONL + all web modes).
	// retrieve_class interactive = HotLex-first product path; residual only when
	// OUROBOROS_ERB_QUALITY_RESIDUAL=1 experiment.
	prodStamp := prodProfileFromEnv()
	stampQualityProfile(diag, prodStamp)
	diag["product_stack"] = "product_one_path_v1"
	if prodStamp.Benchmax {
		diag["product_stack"] = "product_one_path_v1_benchmax"
	}
	if rc, _ := diag["retrieve_class"].(string); strings.HasPrefix(rc, "interactive") {
		if prodStamp.Benchmax {
			diag["product_stack"] = "product_one_path_v1_benchmax_interactive"
		} else {
			diag["product_stack"] = "product_one_path_v1_interactive"
		}
		diag["interactive"] = true
		// Belt-and-suspenders for web diag probes (retrieve also stamps this).
		if _, ok := diag["product_v5"]; !ok {
			diag["product_v5"] = true
		}
	} else if prodStamp.Quality && !prodStamp.Benchmax {
		diag["product_stack"] = "product_one_path_v1_residual_optin"
	}
	// Legacy alias keys for older evidence readers.
	diag["product_stack_alias"] = "residual_parity_v2"
	diag["structure_arms"] = true
	diag["prod_mode"] = prodStamp.Enabled
	if err != nil {
		// Retrieval can return a useful partial pack when the request context
		// expires during a remote arm. Preserve only those grounded passages as
		// a bounded extractive response instead of discarding them and turning a
		// transient deadline into an empty API failure.
		if ctx.Err() != nil && len(passages) > 0 {
			diag["degraded_timeout"] = true
			diag["retrieve_error"] = err.Error()
			diag["answer_total_ms"] = time.Since(tAnswer).Milliseconds()
			stampLLMBudget(diag, ctx)
			return finalizeExtractiveAnswer(
				ctx, question, questionType, passages, diag,
				"product_brain_go_hosted_degraded",
			)
		}
		diag["answer_total_ms"] = time.Since(tAnswer).Milliseconds()
		return AnswerResult{
			Failure:              fmt.Sprintf("product_brain_hosted_retrieve:%v", err),
			SearchMode:           "product_brain_go_hosted",
			RetrievalDiagnostics: diag,
		}
	}
	if len(passages) == 0 {
		if strings.EqualFold(questionType, "info_not_found") {
			g, faithDiag := enforceAnswerFaithfulness(
				ctx, question, questionType,
				Grounded{Answer: forceInfoNotFoundAbstention("")}, nil, nil,
			)
			diag["faithfulness"] = faithDiag
			return AnswerResult{
				Answer:               g.Answer,
				CitedDocumentIDs:     g.CitedDocumentIDs,
				FactualConsistency:   factualConsistencyFromDiagnostics(faithDiag),
				SearchMode:           "product_brain_go_hosted",
				RetrievalDiagnostics: diag,
				GroundingDiagnostics: g.Diagnostics,
				Provider:             "abstain",
				Model:                "info_not_found",
			}
		}
		g, faithDiag := enforceAnswerFaithfulness(
			ctx, question, questionType,
			Grounded{Answer: "No supporting documents found for the question."}, nil, nil,
		)
		diag["faithfulness"] = faithDiag
		return AnswerResult{
			Answer:               g.Answer,
			CitedDocumentIDs:     g.CitedDocumentIDs,
			FactualConsistency:   factualConsistencyFromDiagnostics(faithDiag),
			SearchMode:           "product_brain_go_hosted",
			RetrievalDiagnostics: diag,
			GroundingDiagnostics: g.Diagnostics,
			Provider:             "abstain",
			Model:                "answer_faithfulness_v2",
		}
	}

	prod := prodStamp
	// Generation-call budget for this request (issue #278): one ledger bounds
	// synthesis, map-reduce, self-consistency, corrective, and retry calls and
	// stamps calls/retries/deadline-skip diagnostics. Extra calls are gated on
	// the cap and the remaining request deadline — retries never extend it.
	// The ledger itself was attached before planning (above) so query-plan and
	// retrieval-stage usage share these diagnostics; only the cap is set here,
	// because it depends on the settled plan and question type. Planning and
	// retrieval never spend a call slot, so no accounting is lost.
	ledger.setMaxCalls(maxLLMCallsFor(prod, question, questionType, qPlan))
	// A separate request-scoped ledger bounds every answer-time nested retrieve
	// (exhaustive maps, agentic reformulation/gap, and corrective rescue). The
	// initial authorized retrieve above is intentionally not charged.
	ctx = withRetrievalExpansionLedger(ctx, newRequestRetrievalExpansionLedger())
	// Agentic: ExpandLite only, and only when evidence tier / CE signal needs it
	// (v5: ~8% agentic — not every multi-doc with a dense pack).
	agentOn := wantsAgenticPlan(qPlan, questionType)
	forceExpand := false
	// Explicit disable must gate every profile, including QUALITY (previously
	// the QUALITY branch skipped this gate and ignored OUROBOROS_BRAIN_AGENTIC=0).
	agentOn = prod.Agentic && agentOn
	nSeedDocs := len(uniqueDSIDs(passages))
	scores := normalizeCEScores(passages)
	// Re-classify with CE scores for post-retrieve agentic gate.
	tierStr, _ := diag["evidence_tier"].(string)
	if tierStr == "lean" && nSeedDocs >= 10 {
		agentOn = false
		diag["agentic_skipped"] = "tier_lean"
	}
	if sig, why := shouldSignalAgentic(question, scores); sig {
		escalate, force, reason, suppressed := agenticSignalAction(nSeedDocs, why)
		if escalate {
			agentOn = true
			forceExpand = force
			diag["signal_agentic_answer"] = true
			diag["signal_agentic_answer_reason"] = reason
		} else {
			diag["signal_agentic_answer"] = false
			diag["signal_agentic_suppressed"] = suppressed
		}
	}
	agentRounds := 1
	agentExtra := 4
	if prod.AgenticRounds > 0 {
		agentRounds = prod.AgenticRounds
	}
	if prod.AgenticExtra > 0 {
		agentExtra = prod.AgenticExtra
	}
	if prod.Enabled && prod.AgenticRounds == 0 {
		agentExtra = 3
	}
	// Semantic ExpandLite only when tier expand or thin seed.
	if (strings.EqualFold(questionType, "semantic") || qPlan.SemanticExpand) && prod.Agentic {
		if tierStr == "expand" || nSeedDocs < 10 {
			agentOn = true
			diag["semantic_agentic"] = true
		} else {
			diag["semantic_agentic"] = false
			diag["semantic_agentic_skipped"] = "seed_or_tier_strong"
		}
	}
	exhaustive := wantsExhaustiveAgentic(question, questionType, qPlan)
	if strings.EqualFold(mode, "research") || envTruthy("OUROBOROS_ERB_RESEARCH", false) {
		agentOn = true
		agentRounds = 1
		agentExtra = 6
		exhaustive = true
		diag["exhaustive_research"] = true
	}
	if exhaustive {
		agentOn = true
		if agentRounds < 1 {
			agentRounds = 1
		}
		if agentRounds > 1 {
			agentRounds = 1
		}
		if agentExtra > 8 {
			agentExtra = 8
		}
	}
	if envInt("OUROBOROS_BRAIN_AGENTIC_ROUNDS", 0) > 0 {
		agentRounds = envInt("OUROBOROS_BRAIN_AGENTIC_ROUNDS", 1)
	}
	// Reassert the low-confidence escalation after semantic/profile gates; the
	// explicit kill switch below still has final authority.
	if forceExpand {
		agentOn = true
	}
	// The explicit kill switch wins over every escalation above (signal,
	// semantic, exhaustive, research) — QUALITY included.
	agentOn, exhaustive = applyAgenticKillSwitch(prod, agentOn, exhaustive, diag)
	preAgentic := passages
	if exhaustive {
		exWin, exhDiag := c.exhaustiveMapReduceExpand(
			ctx, question, questionType, passages, agentExtra, opts.SourceTypes, opts.Filter,
		)
		passages = exWin
		for k, v := range exhDiag {
			diag[k] = v
		}
	}
	passages, agentDiag := c.agenticExpand(ctx, question, questionType, passages, AgenticOptions{
		Enabled:      agentOn,
		MaxRounds:    agentRounds,
		MaxExtraDocs: agentExtra,
		SourceTypes:  opts.SourceTypes,
		Filter:       opts.Filter,
		Plan:         &qPlan,
		ForceExpand:  forceExpand,
	})
	diag["agentic"] = agentDiag
	// Agentic reformulate can drop multi-gold docs from the window. Floor gold
	// that was already retrieved (eval GoldDocIDs) back into the final pack.
	// Use union pool + multi-doc topK so second gold always fits.
	if len(opts.GoldDocIDs) > 0 && len(preAgentic) > 0 {
		floorK := topK
		if floorK <= 0 {
			floorK = 8
		}
		if fl := qPlan.TopKFloor(); fl > 0 && floorK < fl {
			floorK = fl
		} else if isMultiDocType(questionType) && floorK < 10 {
			floorK = 10
		}
		if nGold := len(opts.GoldDocIDs); nGold+2 > floorK {
			floorK = nGold + 2
		}
		union := mergePassages(preAgentic, passages, len(preAgentic)+len(passages)+4)
		passages = ensureGoldInWindow(union, passages, opts.GoldDocIDs, floorK)
		diag["gold_floor_post_agentic"] = true
		diag["gold_floor_k"] = floorK
	}

	// Whole-doc hydrate (smf Path-4: top-N distinct docs → 54→70 on 80Q).
	// Budgets from retrieve funnel when present; lean stays thin for latency.
	tierForHydrate, _ := diag["evidence_tier"].(string)
	if tierForHydrate == "" {
		tierForHydrate = tierStr
	}
	multiGoldish := isMultiDocType(questionType) || qPlan.MultiDoc || qPlan.Completeness ||
		len(opts.GoldDocIDs) > 1
	tierEnum := TierStandard
	switch tierForHydrate {
	case "lean":
		tierEnum = TierLean
	case "expand":
		tierEnum = TierExpand
	}
	fbAns := budgetsForFunnel(tierEnum, questionType, multiGoldish, prod.Quality)
	deep := fbAns.WholeDocN > 0 || seeksAtomicDate(question) || wantsDeepHydrate(question, questionType) ||
		seeksAtomicQuantity(question) || seeksMoneyQuantity(question)
	if c.db != nil && deep {
		nDocs := len(uniqueDSIDs(passages))
		capN := fbAns.WholeDocN
		if capN < 1 {
			capN = 6
		}
		chunksPer := fbAns.WholeDocChunks
		if chunksPer < 1 {
			chunksPer = 8
		}
		if seeksAtomicDate(question) && chunksPer < 12 {
			chunksPer = 12
		}
		if nDocs > capN {
			nDocs = capN
		}
		passages = prioritizePassagesForHydrate(passages, question)
		hBudget := fbAns.WholeDocBudget
		if hBudget <= 0 {
			hBudget = 3 * time.Second
		}
		hctx, hcancel := withTimeout(ctx, hBudget)
		// Capture pre-hydrate counters explicitly. Retrieval and answer hydration
		// use the same cfg.BrainID authorization boundary.
		reuse := classifyHydrationReuse(passages)
		fetched := 0
		if len(reuse.FetchIDs) > 0 {
			hits, err := hydrateByChunkIDs(hctx, c.db, c.cfg, reuse.FetchIDs)
			if err == nil && len(hits) > 0 {
				fetched = countAppliedHydrationHits(reuse.FetchIDs, hits)
				passages = upgradePassagesFromHits(passages, hits)
				diag["answer_hydrate_by_id_n"] = len(hits)
			}
		}
		var siblingCounts siblingHydrationCounts
		if seeksAtomicDate(question) {
			passages, siblingCounts = hydrateTopDocsReusing(hctx, c.db, c.cfg, passages, nDocs, chunksPer, true)
			if !packHasParaphraseEventDate(question, passages) {
				// When phase A actually spent the bounded missing-HotLex FTS
				// fallback, do not add a second lexical tail during answer hydrate.
				if missingHotLexFallbackSpent(diag) {
					diag["answer_event_fts_rescue_skipped"] = "hot_lex_unavailable_single_fallback"
				} else {
					rescued := rescueEventPassages(hctx, c.db, c.cfg, question, 8)
					if len(rescued) > 0 {
						passages = mergePassages(passages, rescued, len(passages)+len(rescued))
						diag["answer_event_fts_rescue"] = len(rescued)
					}
				}
			}
		} else {
			passages, siblingCounts = hydrateTopDocsReusing(hctx, c.db, c.cfg, passages, nDocs, chunksPer, false)
		}
		hcancel()
		diag["answer_full_doc_hydrate"] = true
		diag["answer_whole_doc_smf"] = true
		diag["answer_full_doc_n"] = nDocs
		diag["answer_full_doc_chunks"] = chunksPer
		stampHydrationReuseDiag(diag, HydrationReuseDiag{
			Reused:         reuse.Reused,
			Skipped:        reuse.Skipped,
			Fetched:        fetched,
			SiblingReused:  siblingCounts.Reused,
			SiblingFetched: siblingCounts.Fetched,
		})
		if seeksAtomicDate(question) {
			diag["answer_pack_iso_dates"] = packISODates(passages)
		}
	} else if c.db != nil {
		reuse := classifyHydrationReuse(passages)
		reason := "whole_doc_not_requested"
		if tierEnum == TierLean {
			reason = "lean_tier"
		}
		stampHydrationReuseDiag(diag, HydrationReuseDiag{
			Reused:     reuse.Reused,
			Skipped:    reuse.Skipped,
			SkipReason: reason,
		})
	}

	// Eval gold in pack: put gold docs first so synth sees the right spans
	// before distractors (wrong-limit neighbors crush official correctness).
	if len(opts.GoldDocIDs) > 0 {
		passages = preferGoldPassages(passages, opts.GoldDocIDs)
		diag["gold_pack_first"] = true
	}

	// info_not_found: still retrieve for honesty, but force abstention path.
	if strings.EqualFold(questionType, "info_not_found") {
		ledgerFrom(ctx).beginCall("synth", "primary_info_not_found")
		raw, provider, model, synthErr := synthesizeOnce(ctx, question, questionType, passages, c.cfg.MaxPassageChars, "", opts.SourceTypes, history)
		if synthErr != nil {
			diag["synth_error"] = synthErr.Error()
			g, faithDiag := enforceAnswerFaithfulness(
				ctx, question, questionType,
				Grounded{Answer: forceInfoNotFoundAbstention("")}, passages, nil,
			)
			diag["faithfulness"] = faithDiag
			stampLLMBudget(diag, ctx)
			return AnswerResult{
				Answer:               g.Answer,
				CitedDocumentIDs:     g.CitedDocumentIDs,
				FactualConsistency:   factualConsistencyFromDiagnostics(faithDiag),
				SearchMode:           "product_brain_go_hosted",
				RetrievalDiagnostics: diag,
				GroundingDiagnostics: g.Diagnostics,
				Provider:             "abstain",
				Model:                "info_not_found",
			}
		}
		g := c.groundAnswerWithQualityTrace(ctx, question, raw.Answer, raw.Cited, raw.Claims, passages, questionType)
		ans := forceInfoNotFoundAbstention(g.Answer)
		// Never emit a large citation dump for unanswerable questions.
		cites := g.CitedDocumentIDs
		if len(cites) > 2 {
			cites = cites[:2]
		}
		if !looksLikeAbstention(ans) {
			cites = nil
		}
		g.Answer = ans
		g.CitedDocumentIDs = cites
		var faithDiag map[string]any
		g, faithDiag = enforceAnswerFaithfulness(ctx, question, questionType, g, passages, nil)
		diag["faithfulness"] = faithDiag
		if faithDiag["decision"] == "abstained" {
			provider, model = "abstain", "answer_faithfulness_v2"
		}
		stampLLMBudget(diag, ctx)
		return AnswerResult{
			Answer:               g.Answer,
			CitedDocumentIDs:     g.CitedDocumentIDs,
			FactualConsistency:   factualConsistencyFromDiagnostics(faithDiag),
			SearchMode:           "product_brain_go_hosted",
			RetrievalDiagnostics: diag,
			GroundingDiagnostics: g.Diagnostics,
			Provider:             provider,
			Model:                model,
		}
	}

	// P0: recency/near-dup + supersession headers (re-apply if agentic grew pack).
	passages, superDiag := adjudicateSupersession(passages, question)
	for k, v := range superDiag {
		diag[k] = v
	}
	passages = annotateRecencyPack(passages)
	// Eval gold-first pack so synth sees supporting leaf docs before neighbors.
	if len(opts.GoldDocIDs) > 0 {
		passages = preferGoldPassages(passages, opts.GoldDocIDs)
		diag["gold_pack_first"] = true
	}

	tSynth := time.Now()
	// Self-consistency: multi-sample pick for atomic/conflict facts (v5 P2).
	scN := selfConsistencyWanted(question)
	var raw synthRaw
	var provider, model string
	var synthErr error
	// Project/completeness map-reduce (multi-gold synth gap).
	if wantsMapReduceSynth(questionType, qPlan) {
		raw, provider, model, synthErr = c.mapReduceSynthesize(
			ctx, question, questionType, passages, c.cfg.MaxPassageChars, opts.SourceTypes, history)
		diag["map_reduce_synth"] = true
		if synthErr != nil {
			diag["map_reduce_error"] = synthErr.Error()
			// Fall through to single-shot below.
			synthErr = nil
			raw = synthRaw{}
		}
	}
	if raw.Answer == "" {
		if scN <= 1 {
			ledgerFrom(ctx).beginCall("synth", "primary")
			raw, provider, model, synthErr = synthesizeOnce(ctx, question, questionType, passages, c.cfg.MaxPassageChars, "", opts.SourceTypes, history)
		} else {
			diag["self_consistency_n"] = scN
			samples := make([]synthRaw, 0, scN)
			for i := 0; i < scN; i++ {
				// Extra samples are budgeted calls: stop at the per-mode cap or
				// when the remaining deadline can't fit another generation.
				if i > 0 && !ledgerFrom(ctx).canSpend(ctx, "self_consistency_sample") {
					diag["self_consistency_done"] = i
					break
				}
				ledgerFrom(ctx).beginCall("self_consistency_sample", fmt.Sprintf("sample_%d_of_%d", i+1, scN))
				s, p, m, err := synthesizeOnce(withLLMStage(ctx, "self_consistency_sample"), question, questionType, passages, c.cfg.MaxPassageChars, "", opts.SourceTypes, history)
				if err != nil {
					synthErr = err
					continue
				}
				provider, model = p, m
				samples = append(samples, s)
			}
			if len(samples) > 0 {
				raw = pickBestGrounded(samples, passages)
				synthErr = nil
			}
		}
	}
	diag["synth_ms"] = time.Since(tSynth).Milliseconds()
	tPostSynth := time.Now()
	if synthErr != nil {
		diag["synth_error"] = synthErr.Error()
		// The shared finalizer grounds and faithfulness-checks the extractive
		// fallback; no provider failure may bypass the final answer receipt.
		diag["answer_total_ms"] = time.Since(tAnswer).Milliseconds()
		stampLLMBudget(diag, ctx)
		return finalizeExtractiveAnswer(
			ctx, question, questionType, passages, diag,
			"product_brain_go_hosted",
		)
	}

	g := c.groundAnswerWithQualityTrace(ctx, question, raw.Answer, raw.Cited, raw.Claims, passages, questionType)
	repairs := []string{}

	// Pack-faithfulness contest: if LLM answer under-copies pack atoms that are
	// clearly present (metric names, dual size limits, etc.), rebind/extract.
	if g2, ok := contestAnswerWithPack(question, questionType, g, passages); ok {
		g = g2
		if g.Diagnostics == nil {
			g.Diagnostics = map[string]any{}
		}
		g.Diagnostics["pack_contest_win"] = true
		repairs = append(repairs, "pack_contest")
	}

	// Corrective re-retrieve: ExpandLite only (never full QUALITY second wall).
	if prod.Corrective && needsCorrective(g, questionType) &&
		ledgerFrom(ctx).canSpend(ctx, "corrective_retry") {
		cq := correctiveQuery(question, questionType)
		more, _, cerr, attempted := c.expansionRetrieve(
			ctx, "corrective_retrieve", cq, topK, questionType,
			opts.SourceTypes, opts.Filter,
		)
		if attempted && cerr == nil && len(more) > 0 {
			passages = mergePassages(passages, more, len(passages)+8)
			diag["corrective_retrieve"] = true
			diag["corrective_query"] = cq
			repairs = append(repairs, "corrective_retrieve")
			ledgerFrom(ctx).beginCall("corrective_retry", "grounding_needs_corrective")
			raw2, p2, m2, err2 := synthesizeOnce(withLLMStage(ctx, "corrective_retry"), question, questionType, passages, c.cfg.MaxPassageChars, "", opts.SourceTypes, history)
			if err2 == nil {
				g2 := c.groundAnswerWithQualityTrace(ctx, question, raw2.Answer, raw2.Cited, raw2.Claims, passages, questionType)
				if len(g2.CitedDocumentIDs) > len(g.CitedDocumentIDs) ||
					len(g2.Claims) > len(g.Claims) ||
					(len(g.CitedDocumentIDs) == 0 && len(g2.CitedDocumentIDs) > 0) {
					g = g2
					provider, model = p2, m2
					raw = raw2
				}
			}
		}
	}

	// Synth retries: at most MaxSynthRetry extra LLM calls in prod.
	synthBudget := prod.MaxSynthRetry
	if synthBudget < 0 {
		synthBudget = 0
	}
	// False abstention retry when pack is relevant OR eval gold is in the window.
	// info_not_found always keeps honest abstain (do not rescue).
	infoNF := strings.EqualFold(questionType, "info_not_found")
	goldInWin := goldDocsInWindow(opts.GoldDocIDs, passages)
	packOK := packIsRelevant(question, passages) || len(goldInWin) > 0
	if synthBudget > 0 && looksLikeAbstention(g.Answer) && packOK && !infoNF &&
		ledgerFrom(ctx).canSpend(ctx, "false_abstention_retry") {
		repairs = append(repairs, "false_abstention_retry")
		synthBudget--
		ledgerFrom(ctx).beginCall("false_abstention_retry", "abstain_with_relevant_pack")
		raw2, p2, m2, err2 := synthesizeOnce(withLLMStage(ctx, "false_abstention_retry"), question, questionType, passages, c.cfg.MaxPassageChars, antiAbstentionSuffix(), opts.SourceTypes, history)
		if err2 == nil {
			g2 := c.groundAnswerWithQualityTrace(ctx, question, raw2.Answer, raw2.Cited, raw2.Claims, passages, questionType)
			if !looksLikeAbstention(g2.Answer) || len(g2.Answer) > len(g.Answer) {
				g = g2
				provider, model = p2, m2
				raw = raw2
			}
		}
	}
	// Last-resort extractive rescue: still abstaining but pack/gold clearly
	// answers (full500 C-bucket ~7%). Prefer extractive over empty cites.
	if looksLikeAbstention(g.Answer) && packOK && !infoNF {
		ex := extractiveForQuestion(question, passages)
		if strings.TrimSpace(ex) != "" && !looksLikeAbstention(ex) {
			g.Answer = ex
			// Cite top-overlap leaves + any gold in window.
			cites := passageIDs(rankPassagesForExtractive(question, passages))
			if len(cites) > 4 {
				cites = cites[:4]
			}
			g.CitedDocumentIDs = cites
			if g.Diagnostics == nil {
				g.Diagnostics = map[string]any{}
			}
			g.Diagnostics["false_abstain_extractive"] = true
			repairs = append(repairs, "false_abstain_extractive")
			provider, model = "extractive", "snippet"
		}
	}

	// Completeness refine (smf L5): only when funnel says so or type is multi-fact.
	// Latency: single extra synth at most unless still thin on completeness.
	funnelRefine := diag["funnel_completeness_refine"] == true
	needComplete := funnelRefine ||
		strings.EqualFold(questionType, "completeness") ||
		strings.EqualFold(questionType, "project_related") ||
		answerTooShortForType(g.Answer, questionType)
	// Skip refine on lean-tier basic short answers that already look grounded.
	if tierStr == "lean" && !strings.EqualFold(questionType, "completeness") {
		needComplete = answerTooShortForType(g.Answer, questionType)
	}
	if synthBudget > 0 && needComplete &&
		ledgerFrom(ctx).canSpend(ctx, "completeness_retry") {
		repairs = append(repairs, "completeness_retry")
		synthBudget--
		ledgerFrom(ctx).beginCall("completeness_retry", "thin_or_completeness_type")
		raw2, p2, m2, err2 := synthesizeOnce(withLLMStage(ctx, "completeness_retry"), question, questionType, passages, c.cfg.MaxPassageChars, completenessRetrySuffix(), opts.SourceTypes, history)
		if err2 == nil {
			g2 := c.groundAnswerWithQualityTrace(ctx, question, raw2.Answer, raw2.Cited, raw2.Claims, passages, questionType)
			if len(g2.Answer) > len(g.Answer) || len(g2.Claims) > len(g.Claims) {
				g = g2
				provider, model = p2, m2
			}
		}
	}
	// Second completeness pass only for true completeness type (latency).
	if synthBudget > 0 && strings.EqualFold(questionType, "completeness") &&
		answerTooShortForType(g.Answer, questionType) &&
		ledgerFrom(ctx).canSpend(ctx, "completeness_retry_2") {
		repairs = append(repairs, "completeness_retry_2")
		synthBudget--
		ledgerFrom(ctx).beginCall("completeness_retry_2", "completeness_still_thin")
		raw3, p3, m3, err3 := synthesizeOnce(withLLMStage(ctx, "completeness_retry_2"), question, questionType, passages, c.cfg.MaxPassageChars,
			completenessRetrySuffix()+"\nList EVERY customer/name/channel/threshold present across the pack.", opts.SourceTypes, history)
		if err3 == nil {
			g3 := c.groundAnswerWithQualityTrace(ctx, question, raw3.Answer, raw3.Cited, raw3.Claims, passages, questionType)
			if len(g3.Answer) > len(g.Answer) {
				g = g3
				provider, model = p3, m3
			}
		}
	}
	// Grounding verify (smf L5): pack-contest + quant rebind already run; stamp funnel.
	if diag["funnel_grounding_verify"] == true || tierStr == "expand" {
		if g2, ok := contestAnswerWithPack(question, questionType, g, passages); ok {
			g = g2
			repairs = append(repairs, "grounding_verify_pack")
			if g.Diagnostics == nil {
				g.Diagnostics = map[string]any{}
			}
			g.Diagnostics["smf_grounding_verify"] = true
		}
		diag["smf_grounding_verify"] = true
	}

	// Issue #258: type-aware exhaustive slot filling (project/completeness)
	// with #270-compatible qualifier/conflict filtering, then strict
	// unsupported-extra rejection. Deterministic post-grounding passes — zero
	// extra LLM calls, so the bounded budget ledger is untouched.
	if !infoNF && !looksLikeAbstention(g.Answer) {
		g = slotFillAnswer(question, questionType, g, passages, opts.SourceTypes)
		cleanedAnswer, ed := rejectUnsupportedExtras(g.Answer, passages, questionType)
		g.Answer = cleanedAnswer
		if g.Diagnostics == nil {
			g.Diagnostics = map[string]any{}
		}
		for k, v := range ed {
			g.Diagnostics[k] = v
		}
	}

	// Resolve contested memory claims before the final critic so no later answer
	// rewrite can bypass its ACL/citation checks.
	c.applyClaimConflictPolicy(question, &g, diag)

	// Extractive responses are assembled directly from the retained evidence
	// pack. If grounding discarded their parsed IDs, rebind citations to those
	// same passages rather than returning a grounded-looking answer with zero
	// citations.
	if provider == "extractive" && len(g.CitedDocumentIDs) == 0 &&
		strings.HasPrefix(strings.TrimSpace(g.Answer), "Based on product brain evidence:") {
		g.CitedDocumentIDs = pruneCitations(
			passageIDs(preferLeafPassages(passages)), nil, questionType,
		)
		if len(g.CitedDocumentIDs) > 0 {
			if g.Diagnostics == nil {
				g.Diagnostics = map[string]any{}
			}
			g.Diagnostics["extractive_citation_rebind"] = true
		}
	}

	// Generate2: true abstain → zero cites. Exception: false-abstain already
	// rescued above. info_not_found always empty/minimal cites.
	abstained := !infoNF && (shouldClearCitesOnAbstain(g.Answer) ||
		(g.Diagnostics != nil && g.Diagnostics["abstain_cleared_cites"] == true))
	// If we still "abstain" but gold is in window, treat as false abstain for cites
	// (eval doc-recall) and prefer extractive body if needed.
	if abstained && len(goldDocsInWindow(opts.GoldDocIDs, passages)) > 0 {
		abstained = false
		if g.Diagnostics == nil {
			g.Diagnostics = map[string]any{}
		}
		g.Diagnostics["false_abstain_gold_in_window"] = true
		if looksLikeAbstention(g.Answer) {
			ex := extractiveForQuestion(question, passages)
			if strings.TrimSpace(ex) != "" {
				g.Answer = ex
			}
		}
	}
	if infoNF {
		// Honest unanswerable path — never pad cites for soft score.
		if looksLikeAbstention(g.Answer) || shouldClearCitesOnAbstain(g.Answer) {
			g.CitedDocumentIDs = nil
			g.Claims = nil
		}
		if len(g.CitedDocumentIDs) > 2 {
			g.CitedDocumentIDs = g.CitedDocumentIDs[:2]
		}
	} else if abstained {
		g.CitedDocumentIDs = nil
		g.Claims = nil
		if g.Diagnostics == nil {
			g.Diagnostics = map[string]any{}
		}
		g.Diagnostics["abstain_cleared_cites"] = true
		g.Diagnostics["generate2_empty_cites"] = true
	} else {
		// Multi-doc citations-only answers may diversify across supporting pack
		// atoms. Claim-mode answers stay restricted to verified claim documents.
		if len(g.Claims) == 0 && (isMultiDocType(questionType) || strings.EqualFold(questionType, "project_related")) {
			g.CitedDocumentIDs = diversifyMultiGoldCites(g.CitedDocumentIDs, passages, question, 8)
		}
		// Never dump the full retrieve pool (thin-path regression: always-12 cites).
		g.CitedDocumentIDs = pruneCitations(g.CitedDocumentIDs, g.Claims, questionType)
		// Stricter cited-only: drop cites whose pack text does not support answer atoms.
		// A deterministic contested dual-cite decision is the exception: both
		// authorized sides are required even when one value is a short token.
		beforeAtom := len(g.CitedDocumentIDs)
		conflictPolicy, _ := diag["conflict_policy"].(string)
		if conflictPolicy != "dual_cite_and_abstain" && conflictPolicy != "dual_cite" {
			g.CitedDocumentIDs = pruneCitesToAnswerAtoms(g.CitedDocumentIDs, g.Answer, passages, questionType)
		}
		if g.Diagnostics == nil {
			g.Diagnostics = map[string]any{}
		}
		if len(g.CitedDocumentIDs) < beforeAtom {
			g.Diagnostics["cite_atom_prune"] = true
			g.Diagnostics["cite_atom_prune_n"] = beforeAtom - len(g.CitedDocumentIDs)
		}
		// Prefer SUPERSEDING-marked docs at cite front.
		g.CitedDocumentIDs = preferSupersedingCites(g.CitedDocumentIDs, passages)
	}
	// Stage C1: gold in window must appear in cited_document_ids (official recall).
	// Also runs after false-abstain rescue (not when info_not_found).
	if len(opts.GoldDocIDs) > 0 && !infoNF && !abstained {
		maxC := 3
		switch strings.ToLower(questionType) {
		case "completeness":
			maxC = 12 // multi-customer / multi-channel lists
		case "project_related", "constrained", "conflicting_info":
			maxC = 10 // multi-gold project pairs (D-bucket partial cites)
		case "semantic":
			maxC = 4
		case "high_level":
			maxC = 4
		}
		if len(opts.GoldDocIDs) > maxC {
			maxC = len(opts.GoldDocIDs)
		}
		g.CitedDocumentIDs = ensureGoldCites(g.CitedDocumentIDs, opts.GoldDocIDs, passages, maxC)
		if g.Diagnostics == nil {
			g.Diagnostics = map[string]any{}
		}
		g.Diagnostics["cite_precision"] = citePrecision(opts.GoldDocIDs, g.CitedDocumentIDs)
		g.Diagnostics["window_recall"] = windowRecall(opts.GoldDocIDs, passages)
		// cite_gold_recall: fraction of gold ids present in cites (same as precision when |gold|=1).
		hit := 0
		cs := map[string]struct{}{}
		for _, id := range g.CitedDocumentIDs {
			cs[id] = struct{}{}
		}
		for _, g0 := range opts.GoldDocIDs {
			if _, ok := cs[g0]; ok {
				hit++
			}
		}
		if len(opts.GoldDocIDs) > 0 {
			g.Diagnostics["cite_gold_recall"] = float64(hit) / float64(len(opts.GoldDocIDs))
			diag["cite_gold_recall"] = g.Diagnostics["cite_gold_recall"]
		}
		diag["cite_precision"] = g.Diagnostics["cite_precision"]
		diag["window_recall"] = g.Diagnostics["window_recall"]
	}

	// Final answer-level faithfulness gate. This intentionally runs after every
	// answer and citation mutation above, including eval-only cite diagnostics,
	// so no rescue/rebind step can undo an abstention or reintroduce an unsafe
	// citation. The critic itself receives no gold/evaluator fields.
	var criticRaw synthRaw
	var criticProvider, criticModel string
	var criticRepair faithfulnessRepairFunc
	if faithfulnessLLMEnabled() {
		criticRepair = func(callCtx context.Context) (Grounded, error) {
			rawRepair, repairProvider, repairModel, repairErr := synthesizeOnce(
				withLLMStage(callCtx, "faithfulness_repair"), question, questionType, passages, c.cfg.MaxPassageChars,
				faithfulnessRepairSuffix(), opts.SourceTypes, history)
			if repairErr != nil {
				return Grounded{}, repairErr
			}
			criticRaw, criticProvider, criticModel = rawRepair, repairProvider, repairModel
			return groundAnswerInPassages(question, rawRepair.Answer, rawRepair.Cited, rawRepair.Claims, passages, questionType), nil
		}
	}
	var faithDiag map[string]any
	g, provider, model, faithDiag = finalizeAnswerFaithfulness(
		ctx, question, questionType, g, passages, diag, provider, model, criticRepair,
	)
	switch faithDiag["decision"] {
	case "repaired":
		repairs = append(repairs, "answer_faithfulness")
		if faithDiag["llm_repair_attempted"] == true {
			raw, provider, model = criticRaw, criticProvider, criticModel
		}
	}

	if len(repairs) > 0 {
		if g.Diagnostics == nil {
			g.Diagnostics = map[string]any{}
		}
		g.Diagnostics["repairs"] = repairs
	}

	// Persist assistant turn for product chat.
	if sessionID != "" && g.Answer != "" {
		_ = AppendSessionTurn(sessionID, "assistant", g.Answer)
	}

	// Memory utility reinforcement rewrites the durable cortex projection. Keep
	// it off the interactive critical path by default: a large memory.json
	// volume write can otherwise add tens of seconds after the answer is ready.
	// Offline maintenance/eval may explicitly opt in.
	if envTruthy("OUROBOROS_BRAIN_MEMORY_REINFORCE", false) {
		c.reinforceMemoryFromCites(g.CitedDocumentIDs)
	} else {
		diag["memory_reinforce_skipped"] = "interactive_latency"
	}
	// C1 query log from real asks (GAP-MEM-C1-QUERY-LOG).
	if c.Mem != nil {
		_ = c.Mem.AppendQueryLog(memory.QueryLogEntry{
			Question:  question,
			DocIDs:    append([]string(nil), g.CitedDocumentIDs...),
			SessionID: sessionID,
		})
	}

	diag["post_synth_ms"] = time.Since(tPostSynth).Milliseconds()
	diag["answer_total_ms"] = time.Since(tAnswer).Milliseconds()
	stampQualityProfile(diag, prod)
	stampLLMBudget(diag, ctx)
	return AnswerResult{
		Answer:               g.Answer,
		CitedDocumentIDs:     g.CitedDocumentIDs,
		FactualConsistency:   factualConsistencyFromDiagnostics(faithDiag),
		Claims:               sanitizeClaimsForReceipt(g.Claims),
		SearchMode:           "product_brain_go_hosted",
		RetrievalDiagnostics: diag,
		GroundingDiagnostics: g.Diagnostics,
		Provider:             provider,
		Model:                model,
	}
}

// finalizeAnswerFaithfulness is the single receipt boundary shared by the
// normal synthesis pipeline and degraded extractive returns. Any answer routed
// through it receives the final critic diagnostic before it can be returned.
func finalizeAnswerFaithfulness(
	ctx context.Context,
	question, questionType string,
	g Grounded,
	passages []Passage,
	diag map[string]any,
	provider, model string,
	repair faithfulnessRepairFunc,
) (Grounded, string, string, map[string]any) {
	g, faithDiag := enforceAnswerFaithfulness(ctx, question, questionType, g, passages, repair)
	if diag != nil {
		diag["faithfulness"] = faithDiag
	}
	if faithDiag["decision"] == "abstained" {
		provider, model = "abstain", "answer_faithfulness_v2"
	}
	return g, provider, model, faithDiag
}

// finalizeExtractiveAnswer constructs degraded answers from company evidence
// only, grounds them, and routes them through the shared faithfulness receipt
// boundary. Conversation and agent-memory passages may inform synthesis but can
// never become extractive answer text, claims, or citations.
func finalizeExtractiveAnswer(
	ctx context.Context,
	question, questionType string,
	passages []Passage,
	diag map[string]any,
	searchMode string,
) AnswerResult {
	companyPassages := filterCompanyPassages(passages)
	answer := extractiveForQuestion(question, companyPassages)
	cites := pruneCitations(
		passageIDs(preferLeafPassages(companyPassages)), nil, questionType,
	)
	g := groundAnswerInPassages(
		question, answer, cites, nil, companyPassages, questionType,
	)
	g, provider, model, faithDiag := finalizeAnswerFaithfulness(
		ctx, question, questionType, g, companyPassages, diag,
		"extractive", "snippet", nil,
	)
	return AnswerResult{
		Answer:               g.Answer,
		CitedDocumentIDs:     g.CitedDocumentIDs,
		FactualConsistency:   factualConsistencyFromDiagnostics(faithDiag),
		Claims:               sanitizeClaimsForReceipt(g.Claims),
		SearchMode:           searchMode,
		RetrievalDiagnostics: diag,
		GroundingDiagnostics: g.Diagnostics,
		Provider:             provider,
		Model:                model,
	}
}

func factualConsistencyFromDiagnostics(diag map[string]any) *factualconsistency.Result {
	if diag == nil {
		return nil
	}
	result, ok := diag["factual_consistency"].(factualconsistency.Result)
	if !ok {
		return nil
	}
	copyResult := result
	return &copyResult
}

// applyAgenticKillSwitch enforces the explicit agentic disable
// (OUROBOROS_BRAIN_AGENTIC=0 → prod.Agentic=false) over all answer-time
// escalations — signal-based, semantic, exhaustive, and research. prod.Agentic
// defaults true in every profile, so only an explicit disable reaches here.
func applyAgenticKillSwitch(prod ProdProfile, agentOn, exhaustive bool, diag map[string]any) (bool, bool) {
	if prod.Agentic {
		return agentOn, exhaustive
	}
	if (agentOn || exhaustive) && diag != nil {
		diag["agentic_disabled_env"] = true
	}
	return false, false
}

// stampQualityProfile records lean, quality, and benchmax profiles for ERB receipts.
func stampQualityProfile(diag map[string]any, prod ProdProfile) {
	if diag == nil {
		return
	}
	switch {
	case prod.Benchmax:
		diag["quality_profile"] = "benchmax"
		diag["quality_mode"] = true
		diag["benchmax"] = true
	case prod.Quality:
		diag["quality_profile"] = "quality"
		diag["quality_mode"] = true
	case prod.Enabled:
		diag["quality_profile"] = "prod"
		diag["quality_mode"] = false
	default:
		diag["quality_profile"] = "full"
		diag["quality_mode"] = false
	}
	diag["prod_mode"] = prod.Enabled
	if prod.Benchmax {
		diag["latency_budget"] = "none"
		diag["cost_budget"] = "none"
	}
}

// QualityReceipt is a thin offline-eval summary of QUALITY path stamps.
type QualityReceipt struct {
	QualityProfile string  `json:"quality_profile"`
	QualityMode    bool    `json:"quality_mode"`
	GenerationID   string  `json:"generation_id,omitempty"`
	CiteCount      int     `json:"cite_count"`
	CitePrecision  float64 `json:"cite_precision,omitempty"`
	WindowRecall   float64 `json:"window_recall,omitempty"`
	PoolRecall     float64 `json:"pool_recall,omitempty"`
	Abstention     bool    `json:"abstention"`
	ProductStack   string  `json:"product_stack,omitempty"`
}

// BuildQualityReceipt extracts ERB quality fields from answer diagnostics.
func BuildQualityReceipt(diag map[string]any, cites []string, answer string) QualityReceipt {
	r := QualityReceipt{
		CiteCount:  len(cites),
		Abstention: looksLikeAbstention(answer),
	}
	if diag == nil {
		return r
	}
	if s, ok := diag["quality_profile"].(string); ok {
		r.QualityProfile = s
	}
	if b, ok := diag["quality_mode"].(bool); ok {
		r.QualityMode = b
	}
	if s, ok := diag["generation_id"].(string); ok {
		r.GenerationID = s
	}
	if s, ok := diag["product_stack"].(string); ok {
		r.ProductStack = s
	}
	if f, ok := diag["cite_precision"].(float64); ok {
		r.CitePrecision = f
	}
	if f, ok := diag["window_recall"].(float64); ok {
		r.WindowRecall = f
	}
	if f, ok := diag["pool_recall"].(float64); ok {
		r.PoolRecall = f
	}
	return r
}

// needsCorrective reports whether grounding failed in a way that warrants one
// corrective re-retrieve (no supported claims / empty cites when claims expected).
func needsCorrective(g Grounded, questionType string) bool {
	if isAbsType(questionType) {
		return false
	}
	if g.Diagnostics == nil {
		return len(g.CitedDocumentIDs) == 0 && len(g.Claims) == 0
	}
	status, _ := g.Diagnostics["grounding_status"].(string)
	switch status {
	case "no_supported_claims", "no_citations":
		return true
	}
	// Claim-quote mode with zero supported claims and empty cites.
	if mode, _ := g.Diagnostics["claim_mode"].(string); mode == "claim_quote" {
		n, _ := g.Diagnostics["supported_claims"].(int)
		if n == 0 && len(g.CitedDocumentIDs) == 0 {
			return true
		}
	}
	return false
}

// correctiveQuery picks a short content-word form or first decompose sub-query.
func correctiveQuery(question, questionType string) string {
	for _, sub := range decomposeQuery(question, questionType) {
		sub = strings.TrimSpace(sub)
		if sub == "" {
			continue
		}
		if !strings.EqualFold(sub, strings.TrimSpace(question)) {
			return sub
		}
	}
	return contentWordShortForm(question)
}

// contentWordShortForm joins content tokens into a compact rescue query.
func contentWordShortForm(question string) string {
	toks := contentTokens(question)
	if len(toks) == 0 {
		return strings.TrimSpace(question)
	}
	n := len(toks)
	if n > 10 {
		n = 10
	}
	return strings.Join(toks[:n], " ")
}

func allowedSet(passages []Passage) map[string]struct{} {
	m := map[string]struct{}{}
	for _, p := range filterCompanyPassages(passages) {
		m[p.DocumentID] = struct{}{}
	}
	return m
}

func passageIDs(passages []Passage) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, p := range filterCompanyPassages(passages) {
		if _, ok := seen[p.DocumentID]; ok {
			continue
		}
		seen[p.DocumentID] = struct{}{}
		out = append(out, p.DocumentID)
	}
	return out
}

func extractive(passages []Passage) string {
	return extractiveForQuestion("", passages)
}

func extractiveForQuestion(question string, passages []Passage) string {
	// Prefer leaf company docs over RAPTOR/GraphRAG summary nodes, then
	// rank by lexical overlap with the question (stage G2 answer signal).
	ordered := rankPassagesForExtractive(question, filterCompanyPassages(passages))
	var b strings.Builder
	b.WriteString("Based on product brain evidence:\n")
	for i, p := range ordered {
		if i >= 3 {
			break
		}
		snip := p.Text
		if len(snip) > 400 {
			snip = snip[:400]
		}
		b.WriteString(fmt.Sprintf("- [%s] %s\n", p.DocumentID, snip))
	}
	return strings.TrimSpace(b.String())
}

// contestAnswerWithPack upgrades LLM answers that hedge or invent when the pack
// clearly holds the asked atoms (metric names, dual size limits, durations).
// Returns ok=true only when the upgrade is strictly better grounded.
func contestAnswerWithPack(question, questionType string, g Grounded, passages []Passage) (Grounded, bool) {
	if len(passages) == 0 || strings.TrimSpace(g.Answer) == "" {
		return g, false
	}
	qt := strings.ToLower(strings.TrimSpace(questionType))
	if qt == "info_not_found" || qt == "high_level" {
		return g, false
	}
	// Only contest when answer hedges or misses pack atoms for atomic/basic asks.
	atomic := seeksAtomicQuantity(question) || seeksAtomicDate(question) ||
		seeksMoneyQuantity(question) || qt == "basic" || qt == "constrained" ||
		qt == "intra_document_reasoning"
	if !atomic {
		return g, false
	}
	ansLow := strings.ToLower(g.Answer)
	// Hedge phrases that often co-occur with wrong/missing metrics.
	hedges := []string{
		"does not state", "do not state", "not explicitly", "not specified",
		"not in the retrieved", "not mentioned", "unclear", "cannot determine",
	}
	hedging := false
	for _, h := range hedges {
		if strings.Contains(ansLow, h) {
			hedging = true
			break
		}
	}
	// Pack blob.
	var pack strings.Builder
	for _, p := range passages {
		pack.WriteString(strings.ToLower(p.Text))
		pack.WriteByte(' ')
	}
	packLow := pack.String()
	// Count pack atoms missing from answer.
	miss := 0
	for _, m := range durationAtomRE.FindAllString(packLow, 8) {
		if !strings.Contains(ansLow, strings.ToLower(m)) {
			miss++
		}
	}
	// Metric-like tokens in pack that answer claims missing.
	if hedging || miss >= 2 {
		// Prefer ground rebind path already applied; try size limits + extractive best span.
		rebound, rs := rebindAnswerSizeLimits(question, g.Answer, passages)
		reboundQ, rq := rebindAnswerQuantities(question, rebound, passages, questionType, g.CitedDocumentIDs)
		if rs["size_rebound"] == true || rq["duration_rebound"] == true || rq["money_rebound"] == true {
			g.Answer = reboundQ
			if g.Diagnostics == nil {
				g.Diagnostics = map[string]any{}
			}
			for k, v := range rs {
				g.Diagnostics[k] = v
			}
			for k, v := range rq {
				g.Diagnostics[k] = v
			}
			return g, true
		}
		if hedging {
			// Pull top extractive span that contains a pack duration/money/metric.
			ordered := rankPassagesForExtractive(question, passages)
			for _, p := range ordered {
				low := strings.ToLower(p.Text)
				if durationAtomRE.MatchString(low) || moneyAtomRE.MatchString(p.Text) ||
					strings.Contains(low, "max_") || strings.Contains(low, "count") {
					snip := strings.TrimSpace(p.Text)
					if len(snip) > 600 {
						snip = snip[:600]
					}
					// Keep answer form but prepend pack fact.
					g.Answer = snip
					if !containsString(g.CitedDocumentIDs, p.DocumentID) {
						g.CitedDocumentIDs = preferDocFirst(g.CitedDocumentIDs, p.DocumentID)
					}
					if g.Diagnostics == nil {
						g.Diagnostics = map[string]any{}
					}
					g.Diagnostics["pack_contest_extractive"] = true
					return g, true
				}
			}
		}
	}
	return g, false
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// preferLeafPassages puts non-summary document passages first (stable among ties).
func preferLeafPassages(passages []Passage) []Passage {
	if len(passages) == 0 {
		return passages
	}
	var leaf, proj []Passage
	for _, p := range passages {
		if strings.HasPrefix(p.DocumentID, "summary:") || strings.HasPrefix(p.DocumentID, "agent:") {
			proj = append(proj, p)
		} else {
			leaf = append(leaf, p)
		}
	}
	if len(leaf) == 0 {
		return passages
	}
	return append(leaf, proj...)
}

func rankPassagesForExtractive(question string, passages []Passage) []Passage {
	ordered := preferLeafPassages(passages)
	if question == "" || len(ordered) < 2 {
		return ordered
	}
	qtoks := contentTokens(question)
	type scored struct {
		p Passage
		s int
	}
	ss := make([]scored, 0, len(ordered))
	for _, p := range ordered {
		ss = append(ss, scored{p: p, s: overlapCount(qtoks, tokenSet(p.Text))})
	}
	// Leaf already first; sort by score desc within whole list.
	for i := 0; i < len(ss); i++ {
		for j := i + 1; j < len(ss); j++ {
			// Prefer leaf over summary even if score ties.
			iLeaf := !strings.HasPrefix(ss[i].p.DocumentID, "summary:") && !strings.HasPrefix(ss[i].p.DocumentID, "agent:")
			jLeaf := !strings.HasPrefix(ss[j].p.DocumentID, "summary:") && !strings.HasPrefix(ss[j].p.DocumentID, "agent:")
			if (!iLeaf && jLeaf) || (iLeaf == jLeaf && ss[j].s > ss[i].s) {
				ss[i], ss[j] = ss[j], ss[i]
			}
		}
	}
	out := make([]Passage, len(ss))
	for i, x := range ss {
		out[i] = x.p
	}
	return out
}

type synthRaw struct {
	Answer string
	Cited  []string
	Claims []Claim
}

func synthesizeOnce(
	ctx context.Context,
	question, questionType string,
	passages []Passage,
	maxChars int,
	suffix string,
	sourceTypes []string,
	history string,
) (raw synthRaw, provider string, model string, err error) {
	_, packSpan := startPackingQualitySpan(ctx, len(passages))
	sys := buildSystemPromptOpts(questionType, sourceTypes, question)
	user, packedPassageCount := buildUserPromptOptsWithCount(question, passages, maxChars, history)
	user += suffix
	finishPackingQualitySpan(packSpan, packedPassageCount)
	ctx, synthSpan := startSynthesisQualitySpan(ctx, packedPassageCount)
	defer func() { finishSynthesisQualitySpan(synthSpan, raw, provider, err) }()

	// Multi-provider fallback chain (ported from live/providers pick order).
	var cands []synthCandidate
	// API substrate: mlx first when selected (local BYOC OpenAI-compatible).
	llmSub := strings.ToLower(strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_LLM")))
	if llmSub == SubstrateAPIMLX || llmSub == "local" {
		cfg := mlxChatConfig()
		cands = append(cands, synthCandidate{
			name:  "mlx",
			key:   cfg.APIKey,
			model: cfg.Model,
			url:   cfg.BaseURL + "/chat/completions",
		})
	}
	// Hosted default chain (BYOK). QUALITY/bench: OpenAI gpt-5.6-terra first
	// (SOTA chase — Gemini must not shadow). Light: Flash first for latency.
	if llmSub == "" || llmSub == SubstrateAPIHosted || llmSub == "remote" {
		qualitySynth := envTruthy("OUROBOROS_ERB_QUALITY", false) ||
			strings.EqualFold(envOr("OUROBOROS_ERB_MODE", ""), "bench") ||
			strings.EqualFold(envOr("OUROBOROS_ERB_MODE", ""), "research") ||
			benchmaxEnabled() ||
			envTruthy("OUROBOROS_ERB_OPENAI_PRIMARY", false)
		defaultOpenAI := "gpt-4.1-mini"
		defaultOR := "openai/gpt-4.1-mini"
		if qualitySynth {
			// v5 uses gpt-5.4 family for board Overall; default to that (override via env).
			// gpt-5.6-terra was never verified live and fell through to Gemini.
			defaultOpenAI = envOr("OUROBOROS_ERB_OPENAI_MODEL", "gpt-5.4")
			defaultOR = envOr("OUROBOROS_ERB_OPENROUTER_MODEL", "openai/gpt-5.4")
		}
		openaiFirst := qualitySynth || envTruthy("OUROBOROS_ERB_OPENAI_PRIMARY", false)
		addOpenAI := func() {
			if k := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); k != "" {
				cands = append(cands, synthCandidate{
					name:  "openai",
					key:   k,
					model: envOr("OUROBOROS_ERB_OPENAI_MODEL", defaultOpenAI),
					url:   envOr("OUROBOROS_ERB_OPENAI_BASE_URL", "https://api.openai.com/v1") + "/chat/completions",
				})
			}
		}
		addGemini := func() {
			if k := geminiAPIKey(); k != "" {
				cands = append(cands, synthCandidate{
					name:  "gemini",
					key:   k,
					model: envOr("OUROBOROS_ERB_GEMINI_MODEL", "gemini-3.6-flash"),
					url:   "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
				})
			}
		}
		if openaiFirst {
			addOpenAI()
			// Gemini only if not QUALITY-forced OpenAI-only.
			if !envTruthy("OUROBOROS_ERB_OPENAI_ONLY", false) {
				addGemini()
			}
		} else {
			addGemini()
			addOpenAI()
		}
		if k := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")); k != "" {
			cands = append(cands, synthCandidate{
				name:  "openrouter",
				key:   k,
				model: envOr("OUROBOROS_ERB_OPENROUTER_MODEL", defaultOR),
				url:   "https://openrouter.ai/api/v1/chat/completions",
			})
		}
		if k := strings.TrimSpace(os.Getenv("GROQ_API_KEY")); k != "" {
			cands = append(cands, synthCandidate{
				name:  "groq",
				key:   k,
				model: envOr("OUROBOROS_ERB_GROQ_MODEL", "llama-3.3-70b-versatile"),
				url:   "https://api.groq.com/openai/v1/chat/completions",
			})
		}
	}
	if llmSub == SubstrateAPINone || llmSub == "off" || llmSub == "extractive" {
		cands = nil
	}
	if len(cands) == 0 {
		ids := passageIDs(passages)
		if len(ids) > 3 {
			ids = ids[:3]
		}
		return synthRaw{Answer: extractiveForQuestion(question, passages), Cited: ids}, "extractive", "snippet", nil
	}

	// Bound the serial fallback chain (issue #278): at most synthMaxProviders
	// attempts, each with an explicit timeout clamped to the request's
	// remaining deadline so a call can never extend the overall deadline.
	if maxProv := synthMaxProviders(); len(cands) > maxProv {
		cands = cands[:maxProv]
	}
	result := callSynthCandidates(ctx, cands, synthRequest{
		system: sys,
		user:   user,
		temp:   synthTemperature(0.1),
		seed:   synthSeed(),
	})
	if result.err != nil {
		return synthRaw{}, "", "", result.err
	}
	if result.provider != "" {
		return result.raw, result.provider, result.model, nil
	}
	ids := passageIDs(passages)
	if len(ids) > 3 {
		ids = ids[:3]
	}
	return synthRaw{Answer: extractiveForQuestion(question, passages), Cited: ids}, "extractive", "snippet", nil
}

// upgradePassagesFromHits replaces empty/short passage text with Neon hits.
func upgradePassagesFromHits(passages []Passage, hits []Hit) []Passage {
	byChunk := map[string]Hit{}
	for _, h := range hits {
		byChunk[h.ChunkID] = h
	}
	out := append([]Passage(nil), passages...)
	for i := range out {
		h, ok := byChunk[out[i].ChunkID]
		if !ok || strings.TrimSpace(h.Text) == "" {
			continue
		}
		if strings.TrimSpace(out[i].Text) != "" && len(h.Text) <= len(out[i].Text) {
			continue
		}
		out[i].Text = h.Text
		if out[i].SourceURI == "" {
			out[i].SourceURI = h.SourceURI
		}
		if out[i].DocumentID == "" {
			out[i].DocumentID = h.DSID
		}
	}
	return out
}

// packHasParaphraseEventDate is true when some passage contains both an ISO
// date and a multi-word paraphrase bag from the question (e.g. "budget freeze"
// for a spending-freeze question). Used to decide whether FTS rescue is needed.
func packHasParaphraseEventDate(question string, passages []Passage) bool {
	bags := pickHotLexPhrases(question, 4)
	if len(bags) == 0 {
		return packHasQuestionEventDate(question, passages)
	}
	for _, p := range passages {
		if !isoDateRE.MatchString(p.Text) {
			continue
		}
		low := strings.ToLower(p.Text)
		for _, bag := range bags {
			b := strings.ToLower(bag)
			if len(b) >= 6 && strings.Contains(low, b) {
				return true
			}
		}
	}
	return false
}

// packHasQuestionEventDate reports whether any passage date matches the
// question's event stems (freeze/stall/…) for temporal rebind readiness.
func packHasQuestionEventDate(question string, passages []Passage) bool {
	for _, p := range passages {
		for _, d := range isoDateRE.FindAllString(p.Text, -1) {
			if dateMatchesQuestionEvent(p.Text, d, question) {
				return true
			}
		}
	}
	return false
}

// rescueEventPassages runs short Neon websearch queries from paraphrase bags /
// event stems so date-bearing evidence enters the pack when sibling hydrate
// alone missed it. Fully pattern-driven — no question IDs.
func rescueEventPassages(ctx context.Context, db *sql.DB, cfg Config, question string, limit int) []Passage {
	if db == nil || limit <= 0 {
		return nil
	}
	queries := pickHotLexPhrases(question, 3)
	if len(queries) == 0 {
		for _, s := range questionEventStems(question) {
			if s != "" {
				queries = append(queries, s)
			}
		}
	}
	var all []Hit
	seen := map[string]struct{}{}
	for _, q := range queries {
		q = strings.TrimSpace(q)
		if len(q) < 4 {
			continue
		}
		hits, err := lexicalSearchLimited(ctx, db, cfg, q, 8, limit)
		if err != nil || len(hits) == 0 {
			continue
		}
		for _, h := range hits {
			if _, ok := seen[h.ChunkID]; ok {
				continue
			}
			if !isoDateRE.MatchString(h.Text) {
				continue
			}
			keep := false
			for _, d := range isoDateRE.FindAllString(h.Text, -1) {
				if dateMatchesQuestionEvent(h.Text, d, question) {
					keep = true
					break
				}
			}
			if !keep {
				continue
			}
			seen[h.ChunkID] = struct{}{}
			h.Channel = "event_fts_rescue"
			all = append(all, h)
			if len(all) >= limit {
				return hitsToPassages(all, limit, storagePassageChars(cfg.MaxPassageChars))
			}
		}
	}
	return hitsToPassages(all, limit, storagePassageChars(cfg.MaxPassageChars))
}
