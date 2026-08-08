package hosted

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factualconsistency"
)

// The answer-faithfulness critic is deliberately one pass. It uses only the
// authorized packed passages and grounder's claim/quote diagnostics; it never
// receives expected document IDs, gold answers, or other evaluator labels.
const maxFaithfulnessRepairPasses = 1

type faithfulnessRepairFunc func(context.Context) (Grounded, error)

type faithfulnessAssessment struct {
	evidenceSufficient bool
	totalClaims        int
	safeClaims         []Claim
	contradictions     int
	illegalCitations   int
	supported          bool
	qualityOutcome     string
}

var (
	defaultFactualScorerOnce sync.Once
	defaultFactualScorer     factualconsistency.Scorer
)

var faithfulnessNumberRE = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)

// enforceAnswerFaithfulness accepts, deterministically repairs, optionally
// requests one ledger-bound LLM repair, or abstains. The repair callback must
// use the same request context; this function invokes it at most once.
func enforceAnswerFaithfulness(
	ctx context.Context,
	question, questionType string,
	g Grounded,
	passages []Passage,
	repair faithfulnessRepairFunc,
) (Grounded, map[string]any) {
	if !faithfulnessEnabled() {
		// Rollback means exactly that: do not assess, sanitize, repair, or mutate
		// the pre-critic answer. The retrieval diagnostic is the only additive
		// signal, so operators can prove which path served the request.
		return g, map[string]any{
			"schema_version": "answer_faithfulness_v2",
			"enabled":        false,
			"decision":       "disabled",
			"reason":         "kill_switch",
		}
	}
	assessment := assessAnswerFaithfulness(question, g, passages)
	diag := faithfulnessDiagnostics(questionType, assessment)
	diag["enabled"] = true
	diag["critic_passes"] = maxFaithfulnessRepairPasses
	diag["llm_repair_enabled"] = faithfulnessLLMEnabled()
	diag["calibrated_threshold_per_mille"] = factualconsistency.DefaultDecisionPerMille

	finish := func(out Grounded, decision, reason string, consistency factualconsistency.Result) (Grounded, map[string]any) {
		diag["factual_consistency"] = consistency
		diag["decision"] = decision
		diag["reason"] = reason
		if out.Diagnostics == nil {
			out.Diagnostics = map[string]any{}
		}
		out.Diagnostics["faithfulness_critic"] = diag
		return out, diag
	}

	// Abstention is a safety decision, not a failed assertion to turn back into
	// one. info_not_found is always caveated even if the model attached a
	// superficially supported related claim. Neither path is eligible for the
	// deterministic or optional LLM repair below.
	if strings.EqualFold(questionType, "info_not_found") {
		g.Answer = forceInfoNotFoundAbstention(g.Answer)
		g.CitedDocumentIDs = nil
		g.Claims = nil
		diag["quality_outcome"] = "unsupported"
		return finish(g, "abstained", "abstention_preserved", factualconsistency.Abstained())
	}
	if looksLikeAbstention(g.Answer) || shouldClearCitesOnAbstain(g.Answer) {
		g.CitedDocumentIDs = nil
		g.Claims = nil
		diag["quality_outcome"] = "unsupported"
		return finish(g, "abstained", "abstention_preserved", factualconsistency.Abstained())
	}

	initialConsistency := scoreFactualConsistency(ctx, g, passages)
	diag["pre_decision_factual_consistency"] = initialConsistency
	if ctx != nil && ctx.Err() != nil {
		return finish(faithfulnessAbstention(g, "request_cancelled"), "abstained", "request_cancelled", factualconsistency.Abstained())
	}
	if assessment.supported {
		if initialConsistency.Status != factualconsistency.StatusScored {
			// A scorer outage, invalid projection, or bounded evidence overflow is
			// not evidence that an already quote/ACL-verified answer is false.
			// Preserve the existing strict floors and disclose the unavailable
			// score rather than converting uncertainty into a false abstention.
			return finish(sanitizeFaithfulCitations(g, passages, questionType), "accepted", "supported_factual_consistency_unavailable", initialConsistency)
		}
		if factualconsistency.MeetsDefaultThreshold(initialConsistency) {
			return finish(sanitizeFaithfulCitations(g, passages, questionType), "accepted", "supported", initialConsistency)
		}
		diag["low_confidence"] = true
	}

	// When some claims survived quote and semantic checks, remove every other
	// sentence and cite only those claims. This is deterministic and needs no
	// model call.
	if len(assessment.safeClaims) > 0 {
		out := repairFromSupportedClaims(g, assessment.safeClaims, passages, questionType)
		diag["deterministic_repair"] = true
		diag["post_repair_supported_claim_rate"] = float64(1)
		postConsistency := scoreFactualConsistency(ctx, out, passages)
		diag["post_repair_factual_consistency"] = postConsistency
		if ctx != nil && ctx.Err() != nil {
			return finish(faithfulnessAbstention(out, "request_cancelled"), "abstained", "request_cancelled", factualconsistency.Abstained())
		}
		if postConsistency.Status != factualconsistency.StatusScored {
			return finish(out, "repaired", "factual_consistency_unavailable", postConsistency)
		}
		if factualconsistency.MeetsDefaultThreshold(postConsistency) {
			return finish(out, "repaired", "generator_mishandled_sufficient_evidence", postConsistency)
		}
		diag["post_repair_rejected"] = "low_confidence"
		return finish(faithfulnessAbstention(out, "low_confidence"), "abstained", "low_confidence", factualconsistency.Abstained())
	}

	// Ambiguous/no-supported-claim cases may use one optional re-synthesis. It
	// shares issue #278's request ledger and deadline; there is no critique loop.
	if assessment.evidenceSufficient && faithfulnessLLMEnabled() && repair != nil {
		ledger := ledgerFrom(ctx)
		if ledger.canSpend(ctx, "faithfulness_repair") {
			ledger.beginCall("faithfulness_repair", "generator_mishandled_sufficient_evidence")
			diag["llm_repair_attempted"] = true
			candidate, err := repair(ctx)
			if err == nil {
				post := assessAnswerFaithfulness(question, candidate, passages)
				diag["post_repair_supported_claims"] = len(post.safeClaims)
				if post.totalClaims > 0 {
					diag["post_repair_supported_claim_rate"] = float64(len(post.safeClaims)) / float64(post.totalClaims)
				}
				if post.supported {
					postConsistency := scoreFactualConsistency(ctx, candidate, passages)
					diag["post_repair_factual_consistency"] = postConsistency
					if ctx != nil && ctx.Err() != nil {
						return finish(faithfulnessAbstention(candidate, "request_cancelled"), "abstained", "request_cancelled", factualconsistency.Abstained())
					}
					if postConsistency.Status != factualconsistency.StatusScored {
						return finish(sanitizeFaithfulCitations(candidate, passages, questionType), "repaired", "factual_consistency_unavailable", postConsistency)
					}
					if factualconsistency.MeetsDefaultThreshold(postConsistency) {
						return finish(sanitizeFaithfulCitations(candidate, passages, questionType), "repaired", "generator_mishandled_sufficient_evidence", postConsistency)
					}
					diag["post_repair_rejected"] = "low_confidence"
				}
				candidateAbstained := looksLikeAbstention(candidate.Answer) || shouldClearCitesOnAbstain(candidate.Answer)
				if candidateAbstained {
					diag["llm_repair_rejected"] = "abstention"
				}
				if len(post.safeClaims) > 0 && !candidateAbstained {
					out := repairFromSupportedClaims(candidate, post.safeClaims, passages, questionType)
					diag["deterministic_repair"] = true
					reducedConsistency := scoreFactualConsistency(ctx, out, passages)
					diag["post_repair_factual_consistency"] = reducedConsistency
					if ctx != nil && ctx.Err() != nil {
						return finish(faithfulnessAbstention(out, "request_cancelled"), "abstained", "request_cancelled", factualconsistency.Abstained())
					}
					if reducedConsistency.Status != factualconsistency.StatusScored {
						return finish(out, "repaired", "factual_consistency_unavailable", reducedConsistency)
					}
					if factualconsistency.MeetsDefaultThreshold(reducedConsistency) {
						return finish(out, "repaired", "generator_mishandled_sufficient_evidence", reducedConsistency)
					}
					diag["post_repair_rejected"] = "low_confidence"
				}
			} else {
				diag["llm_repair_error"] = "provider_error"
			}
		} else {
			diag["llm_repair_skip"] = "call_or_deadline_budget"
		}
	} else if assessment.evidenceSufficient && faithfulnessLLMEnabled() {
		diag["llm_repair_skip"] = "not_configured"
	}

	reason := "generator_mishandled_sufficient_evidence"
	if !assessment.evidenceSufficient {
		reason = "insufficient_evidence"
	}
	return finish(faithfulnessAbstention(g, reason), "abstained", reason, factualconsistency.Abstained())
}

func scoreFactualConsistency(ctx context.Context, g Grounded, passages []Passage) factualconsistency.Result {
	if ctx == nil {
		ctx = context.Background()
	}
	if looksLikeAbstention(g.Answer) || shouldClearCitesOnAbstain(g.Answer) {
		return factualconsistency.Abstained()
	}
	request := factualConsistencyRequest(g, passages)
	if len(request.Claims) == 0 {
		return factualconsistency.Unknown(factualconsistency.ReasonInvalidResult, len(g.Claims))
	}
	defaultFactualScorerOnce.Do(func() {
		scorer, err := factualconsistency.NewDefaultScorer()
		if err == nil {
			defaultFactualScorer = scorer
		}
	})
	result, err := factualconsistency.Evaluate(ctx, defaultFactualScorer, request, factualconsistency.DefaultLimits())
	if err != nil {
		return factualconsistency.Unknown(factualconsistency.ReasonScorerFailed, len(request.Claims))
	}
	return result
}

// factualConsistencyRequest receives only the final claim text and exact
// authorized support text. It deliberately has no route to AnswerOptions,
// GoldDocIDs, diagnostics, principals, tenant IDs, or source identities.
func factualConsistencyRequest(g Grounded, passages []Passage) factualconsistency.Request {
	request := factualconsistency.Request{}
	for _, claim := range g.Claims {
		if strings.TrimSpace(claim.Text) == "" || strings.TrimSpace(claim.Quote) == "" {
			continue
		}
		request.Claims = append(request.Claims, factualconsistency.Claim{
			Statement: claim.Text,
			Supports:  []string{claim.Quote},
		})
	}
	if len(request.Claims) > 0 {
		return request
	}

	// Strictly verified extractive answers have no model claims. Represent each
	// cited company passage as an exact-support claim; authorization and exact
	// extractive matching already ran before this scoring-only projection.
	evidence := evidenceByDocument(passages)
	allowed := allowedSet(passages)
	ids, ok := authorizedExtractiveCitations(g.Answer, evidence, allowed)
	if !ok {
		return request
	}
	limits := factualconsistency.DefaultLimits()
	statement := extractiveAnswerStatement(g.Answer)
	remaining := limits.MaxTotalBytes - len(statement)
	var supports []string
	for _, id := range ids {
		for _, support := range prioritizedEvidenceSupports(evidence[id], statement, limits.MaxSupportBytes) {
			if len(supports) >= limits.MaxSupportsPerClaim || remaining <= 0 {
				break
			}
			if len(support) > remaining {
				support = support[:remaining]
			}
			if support == "" {
				break
			}
			supports = append(supports, support)
			remaining -= len(support)
		}
		if len(supports) >= limits.MaxSupportsPerClaim || remaining <= 0 {
			break
		}
	}
	if statement != "" && len(supports) > 0 {
		// Score the answer text against bounded slices of the authorized source,
		// never against a statement copied from that same source. Large evidence
		// packs are chunked instead of silently becoming UNKNOWN.
		request.Claims = append(request.Claims, factualconsistency.Claim{
			Statement: statement,
			Supports:  supports,
		})
	}
	return request
}

func extractiveAnswerStatement(answer string) string {
	lines := strings.Split(strings.TrimSpace(answer), "\n")
	var statements []string
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- [") {
			if end := strings.Index(trimmed, "]"); end >= 3 {
				trimmed = strings.TrimSpace(trimmed[end+1:])
			}
		}
		if trimmed != "" {
			statements = append(statements, trimmed)
		}
	}
	return strings.Join(statements, " ")
}

func boundedEvidenceSupports(text string, maxBytes int) []string {
	text = strings.TrimSpace(text)
	if text == "" || maxBytes <= 0 {
		return nil
	}
	if len(text) <= maxBytes {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		end := maxBytes
		if end > len(text) {
			end = len(text)
		}
		for end > 0 && end < len(text) && text[end]&0xc0 == 0x80 {
			end--
		}
		if end == 0 {
			end = len(text)
		}
		chunks = append(chunks, text[:end])
		text = text[end:]
	}
	return chunks
}

// prioritizedEvidenceSupports keeps the scorer's bounded request focused on
// the source chunks that actually contain the verified extractive answer. A
// first-prefix-only slice could otherwise turn a valid late-in-document quote
// into a low score and an incorrect abstention.
func prioritizedEvidenceSupports(text, statement string, maxBytes int) []string {
	chunks := boundedEvidenceSupports(text, maxBytes)
	if len(chunks) < 2 {
		return chunks
	}
	tokens := uniqueStrings(contentTokens(statement))
	normalizedStatement := normText(statement)
	sort.SliceStable(chunks, func(i, j int) bool {
		return evidenceChunkScore(chunks[i], normalizedStatement, tokens) > evidenceChunkScore(chunks[j], normalizedStatement, tokens)
	})
	return chunks
}

func evidenceChunkScore(chunk, normalizedStatement string, tokens []string) int {
	normalizedChunk := normText(chunk)
	score := 0
	if normalizedStatement != "" && strings.Contains(normalizedChunk, normalizedStatement) {
		score += len(tokens) + 1
	}
	for _, token := range tokens {
		if strings.Contains(normalizedChunk, token) {
			score++
		}
	}
	return score
}

// faithfulnessEnabled is the default-on rollout switch for the entire final
// answer gate. Setting OUROBOROS_ERB_FAITHFULNESS=0 restores the pre-critic
// answer path without assessment, citation rewrites, or repair calls.
func faithfulnessEnabled() bool {
	return envTruthy("OUROBOROS_ERB_FAITHFULNESS", true)
}

func faithfulnessLLMEnabled() bool {
	return faithfulnessEnabled() && envTruthy("OUROBOROS_ERB_FAITHFULNESS_LLM", false)
}

func faithfulnessRepairSuffix() string {
	return "\n\nFAITHFULNESS REPAIR (single pass): Regenerate the answer using only the supplied evidence pack. Every claim must include a verbatim support quote and an authorized document_id from that pack. Remove unsupported or contradictory claims. If the pack is insufficient, return an explicit abstention with answer text only and cited_document_ids/claims as empty arrays."
}

func assessAnswerFaithfulness(question string, g Grounded, passages []Passage) faithfulnessAssessment {
	authorized := filterCompanyPassages(passages)
	a := faithfulnessAssessment{
		evidenceSufficient: packIsRelevant(question, authorized),
		qualityOutcome:     "unsupported",
	}

	if g.Diagnostics != nil {
		if dropped, ok := g.Diagnostics["dropped_claims"].([]string); ok {
			a.totalClaims += len(dropped)
		}
		if illegal, ok := g.Diagnostics["illegal_citations"].([]string); ok {
			a.illegalCitations = len(illegal)
		}
	}
	a.totalClaims += len(g.Claims)

	evidence := make(map[string]string, len(authorized))
	allowed := make(map[string]struct{}, len(authorized))
	for _, p := range authorized {
		if strings.TrimSpace(p.DocumentID) == "" {
			continue
		}
		evidence[p.DocumentID] += "\n" + p.Text
		allowed[p.DocumentID] = struct{}{}
	}
	for _, claim := range g.Claims {
		ok, contradictory := claimFaithfulToAuthorizedEvidence(claim, evidence)
		if contradictory {
			a.contradictions++
		}
		if ok {
			a.safeClaims = append(a.safeClaims, claim)
		}
	}

	if a.contradictions > 0 {
		a.qualityOutcome = "contradictory"
	} else if !a.evidenceSufficient {
		a.qualityOutcome = "insufficient_context"
	} else if a.totalClaims > 0 && len(a.safeClaims) == a.totalClaims && a.illegalCitations == 0 {
		a.qualityOutcome = "supported"
		a.supported = true
	} else if a.totalClaims == 0 && citationOnlyAnswerSupported(g, evidence, allowed) {
		a.qualityOutcome = "supported"
		a.supported = true
	}

	// An explicit abstention is an output decision, not a supported answer.
	if looksLikeAbstention(g.Answer) || shouldClearCitesOnAbstain(g.Answer) {
		a.supported = false
		if !a.evidenceSufficient {
			a.qualityOutcome = "insufficient_context"
		}
	}
	return a
}

func faithfulnessDiagnostics(questionType string, a faithfulnessAssessment) map[string]any {
	rate := float64(0)
	if a.totalClaims > 0 {
		rate = float64(len(a.safeClaims)) / float64(a.totalClaims)
	}
	return map[string]any{
		"schema_version":        "answer_faithfulness_v2",
		"question_type":         strings.ToLower(strings.TrimSpace(questionType)),
		"evidence_sufficient":   a.evidenceSufficient,
		"generated_claims":      a.totalClaims,
		"supported_claims":      len(a.safeClaims),
		"supported_claim_rate":  rate,
		"contradictory_claims":  a.contradictions,
		"illegal_citations":     a.illegalCitations,
		"quality_outcome":       a.qualityOutcome,
		"bounded_repair_passes": maxFaithfulnessRepairPasses,
	}
}

// claimFaithfulToAuthorizedEvidence validates the claim's quote and concrete
// atoms against its authorized document. A conflicting concrete value is a
// deterministic contradiction; textual entailment remains with the existing
// quote grounder rather than guessing with a lexical heuristic.
func claimFaithfulToAuthorizedEvidence(claim Claim, evidence map[string]string) (bool, bool) {
	doc := strings.TrimSpace(claim.DocumentID)
	body, ok := evidence[doc]
	if !ok || strings.TrimSpace(claim.Text) == "" || strings.TrimSpace(claim.Quote) == "" {
		return false, false
	}
	bodyLow := normText(body)
	quoteLow := normText(claim.Quote)
	// The grounder permits a soft quote match for recall, then tries to recover
	// a verbatim span. The final critic is the acceptance boundary: only the
	// resulting exact normalized span of the grounder's minimum length may
	// authorize a claim.
	if len(quoteLow) < minimumNormalizedQuoteLength(claim.Quote) || !strings.Contains(bodyLow, quoteLow) {
		return false, false
	}
	claimLow := normText(claim.Text)

	for _, family := range []struct {
		claimAtoms []string
		quoteAtoms []string
	}{
		{durationAtomRE.FindAllString(claimLow, -1), durationAtomRE.FindAllString(quoteLow, -1)},
		{moneyAtomRE.FindAllString(claimLow, -1), moneyAtomRE.FindAllString(quoteLow, -1)},
		{isoDateRE.FindAllString(claimLow, -1), isoDateRE.FindAllString(quoteLow, -1)},
	} {
		for _, atom := range family.claimAtoms {
			if !strings.Contains(quoteLow, normText(atom)) {
				return false, len(family.quoteAtoms) > 0
			}
		}
	}

	// Concrete atoms must be supported by this claim's quoted span, not merely
	// occur somewhere else in the same authorized document. Otherwise a model
	// could cite a 90-day retention sentence while borrowing an unrelated
	// 30-day value from a later paragraph and still pass the critic.
	quoteNumbers := uniqueStrings(faithfulnessNumberRE.FindAllString(quoteLow, -1))
	for _, atom := range faithfulnessNumberRE.FindAllString(claimLow, -1) {
		if !stringInSlice(atom, quoteNumbers) {
			return false, len(quoteNumbers) > 0
		}
	}
	// High-confidence finite/infinite conflict. This catches a fluent claim that
	// cites a real finite-retention quote while changing its meaning to forever.
	if strings.Contains(claimLow, "forever") && len(durationAtomRE.FindAllString(quoteLow, -1)) > 0 &&
		!strings.Contains(quoteLow, "forever") {
		return false, true
	}
	// Recovered quotes can be lexically real but unrelated to the claim tail.
	// Require a modest content-token floor; semantic paraphrases still pass,
	// while a claim that merely shares the subject but invents the predicate does not.
	tokens := uniqueStrings(contentTokens(claim.Text))
	if len(tokens) > 0 {
		hits := 0
		for _, token := range tokens {
			if strings.Contains(bodyLow, token) {
				hits++
			}
		}
		if hits < 2 || float64(hits)/float64(len(tokens)) < 0.6 {
			return false, false
		}
	}
	return true, false
}

func stringInSlice(needle string, values []string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func citationOnlyAnswerSupported(g Grounded, evidence map[string]string, allowed map[string]struct{}) bool {
	if strings.TrimSpace(g.Answer) == "" || len(g.CitedDocumentIDs) == 0 {
		return false
	}
	var cited strings.Builder
	for _, id := range g.CitedDocumentIDs {
		if _, ok := allowed[id]; !ok {
			return false
		}
		cited.WriteString(evidence[id])
		cited.WriteByte(' ')
	}
	blob := normText(cited.String())
	if blob == "" {
		return false
	}
	// Claim-less prose has no quote-local support or calibrated score request.
	// Accept only the exact extractive format or the intentional contested
	// dual-citation shape; pack-wide token overlap can mix values from unrelated
	// passages (for example RPO=15 with RTO=30) and bypass the score floor.
	return authorizedExtractiveAnswer(g.Answer, evidence, allowed) || authorizedConflictAbstention(g.Answer, blob)
}

func authorizedExtractiveAnswer(answer string, evidence map[string]string, allowed map[string]struct{}) bool {
	_, ok := authorizedExtractiveCitations(answer, evidence, allowed)
	return ok
}

func authorizedExtractiveCitations(answer string, evidence map[string]string, allowed map[string]struct{}) ([]string, bool) {
	lines := strings.Split(strings.TrimSpace(answer), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "Based on product brain evidence:" {
		return nil, false
	}
	type bullet struct {
		id   string
		text []string
	}
	var bullets []bullet
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- [") {
			end := strings.Index(trimmed, "]")
			if end < 3 {
				return nil, false
			}
			bullets = append(bullets, bullet{
				id:   strings.TrimSpace(trimmed[3:end]),
				text: []string{strings.TrimSpace(trimmed[end+1:])},
			})
			continue
		}
		if len(bullets) == 0 {
			if trimmed == "" {
				continue
			}
			return nil, false
		}
		bullets[len(bullets)-1].text = append(bullets[len(bullets)-1].text, line)
	}
	var ids []string
	for _, bullet := range bullets {
		if _, ok := allowed[bullet.id]; !ok {
			return nil, false
		}
		snippet := normText(strings.Join(bullet.text, "\n"))
		if len(snippet) < 8 || !strings.Contains(normText(evidence[bullet.id]), snippet) {
			return nil, false
		}
		ids = append(ids, bullet.id)
	}
	ids = uniqueStrings(ids)
	return ids, len(ids) > 0
}

func authorizedConflictAbstention(answer, evidence string) bool {
	answer = strings.TrimSpace(answer)
	if !strings.HasPrefix(answer, "Contested: evidence conflicts on this fact (") {
		return false
	}
	start := strings.Index(answer, "(")
	end := strings.Index(answer, ").")
	if start < 0 || end <= start+1 {
		return false
	}
	values := strings.Split(answer[start+1:end], " vs ")
	if len(values) < 2 {
		return false
	}
	for _, value := range values {
		value = normText(value)
		if len(value) < 3 || !strings.Contains(evidence, value) {
			return false
		}
	}
	return true
}

func repairFromSupportedClaims(g Grounded, claims []Claim, passages []Passage, questionType string) Grounded {
	var sentences []string
	for _, claim := range claims {
		text := strings.TrimSpace(claim.Text)
		if text == "" {
			continue
		}
		if !strings.HasSuffix(text, ".") && !strings.HasSuffix(text, "!") && !strings.HasSuffix(text, "?") {
			text += "."
		}
		sentences = append(sentences, text)
	}
	g.Answer = strings.Join(sentences, " ")
	g.Claims = append([]Claim(nil), claims...)
	g.CitedDocumentIDs = pruneCitations(filterAllowed(claimDocs(claims), allowedSet(passages)), claims, questionType)
	return g
}

func sanitizeFaithfulCitations(g Grounded, passages []Passage, questionType string) Grounded {
	allowed := allowedSet(passages)
	if ids, ok := authorizedExtractiveCitations(g.Answer, evidenceByDocument(passages), allowed); ok {
		g.CitedDocumentIDs = ids
	} else {
		g.CitedDocumentIDs = filterAllowed(g.CitedDocumentIDs, allowed)
	}
	if len(g.Claims) > 0 {
		g.CitedDocumentIDs = filterAllowed(claimDocs(g.Claims), allowed)
	}
	// Acceptance is still subject to the product citation contract: type-aware
	// maxCites and leaf-document preference apply after ACL sanitization.
	g.CitedDocumentIDs = pruneCitations(g.CitedDocumentIDs, g.Claims, questionType)
	return g
}

func evidenceByDocument(passages []Passage) map[string]string {
	evidence := map[string]string{}
	for _, passage := range filterCompanyPassages(passages) {
		if strings.TrimSpace(passage.DocumentID) != "" {
			evidence[passage.DocumentID] += "\n" + passage.Text
		}
	}
	return evidence
}

func faithfulnessAbstention(g Grounded, reason string) Grounded {
	if reason == "insufficient_evidence" {
		g.Answer = "There is insufficient evidence in the supplied documents to answer this question."
	} else if reason == "low_confidence" {
		g.Answer = "I am unable to answer reliably because grounding confidence is below the calibrated safety threshold."
	} else {
		g.Answer = "I am unable to answer reliably because the generated claims could not be verified against the supplied documents."
	}
	g.CitedDocumentIDs = nil
	g.Claims = nil
	return g
}
