// Package main implements product-brain-eval: one product brain for ERB/eval.
//
// Hosted mode (default when NEON+Qdrant env present, or --hosted): path2 Neon
// lexical + Qdrant dense ANN. JSONL fixture mode loads docs into hosted.OpenMemory
// (same Client.AnswerTyped path — no parallel LiveCorpus answer engine).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factualconsistency"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
)

// HostedLoopProtocolVersion is the request-correlated framing version of the
// warm --hosted-loop wire protocol (issue #292). Every v1 request carries
// request_id + protocol_version and every response echoes them, so concurrent,
// late, malformed, or mixed-mode lines can never be cross-attributed. Requests
// without framing fields are legacy v0 (correlated by the question_id diag) and
// are still answered; a declared but unsupported version fails closed.
const HostedLoopProtocolVersion = 1

// EvalCase is one stdin JSON object from the eval harness.
type EvalCase struct {
	Question     string   `json:"question"`
	QuestionID   string   `json:"question_id"`
	QuestionType string   `json:"question_type"`
	DocumentIDs  []string `json:"document_ids"`
	// ExpectedDocIDs is the EnterpriseRAG-Bench gold field (questions.jsonl).
	// Without this alias GoldDocIDs never populated → no pool/window gold diags.
	ExpectedDocIDs []string `json:"expected_doc_ids"`
	SessionID      string   `json:"session_id,omitempty"`
	SourceTypes    []string `json:"source_types,omitempty"`
	// MemoryFacts are LongMem-style recall strings ingested into OpenMemory
	// before Answer (product memory path).
	MemoryFacts []string `json:"memory_facts,omitempty"`
	// PriorTurns seeds product chat history for turn_grep + prompt (web).
	PriorTurns []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"prior_turns,omitempty"`
	// History is optional preformatted conversation history for the prompt.
	History string `json:"history,omitempty"`
	// AskTimeoutMS lets the caller reserve cold-start/queue time before the
	// context-aware hosted answer begins. Zero keeps the process default.
	AskTimeoutMS int `json:"ask_timeout_ms,omitempty"`
	// Filter is the raw governed metadata-filter predicate map (issue #328).
	// Official/blind runs reject gold-derived predicates fail-closed.
	Filter map[string]any `json:"filter,omitempty"`
	// RequestID correlates one hosted-loop response to its request (issue
	// #292); echoed verbatim on the response. Empty means legacy v0 framing.
	RequestID string `json:"request_id,omitempty"`
	// ProtocolVersion declares the loop framing version. Zero is legacy v0;
	// any non-zero value other than HostedLoopProtocolVersion fails closed.
	ProtocolVersion int `json:"protocol_version,omitempty"`

	protocolVersionPresent bool
	protocolVersionValid   bool
}

// UnmarshalJSON preserves the distinction between an absent legacy framing
// field and a malformed explicit value such as null, true, or 1.0. The warm
// loop must reject malformed framing rather than silently treating it as v0.
func (ec *EvalCase) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	versionRaw, present := fields["protocol_version"]
	delete(fields, "protocol_version")
	normalized, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	type evalCase EvalCase
	var decoded evalCase
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		return err
	}
	*ec = EvalCase(decoded)
	ec.protocolVersionPresent = present
	ec.protocolVersionValid = !present
	if !present {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(versionRaw), []byte("null")) {
		ec.protocolVersionValid = false
		return nil
	}
	var version int
	if err := json.Unmarshal(versionRaw, &version); err != nil {
		ec.protocolVersionValid = false
		return nil
	}
	ec.ProtocolVersion = version
	ec.protocolVersionValid = true
	return nil
}

// goldDocIDs returns eval gold for offline diags.
// Prefer expected_doc_ids (ERB fixture gold); document_ids is only a fallback alias.
func (ec EvalCase) goldDocIDs() []string {
	if len(ec.ExpectedDocIDs) > 0 {
		return ec.ExpectedDocIDs
	}
	return ec.DocumentIDs
}

// goldAssistEnabled reports whether fixture gold may steer AnswerOpts
// (gold pack-first / cite floor inside the answer path). UNSAFE for official
// scoring: it leaks expected_doc_ids into product behavior, so it is an
// explicit offline-diagnostics opt-in only. Official mode always forces it
// off. Default: off; gold only feeds post-answer diagnostics.
func goldAssistEnabled() bool {
	return !blindPlanEnabled() && envOn("OUROBOROS_ERB_GOLD_ASSIST")
}

func diagnosticRescueEnabled() bool {
	return !blindPlanEnabled() && !envOnDefault("OUROBOROS_ERB_PROD", true) && envOn("OUROBOROS_ERB_DIAGNOSTIC_RESCUE")
}

func officialEvalEnabled() bool {
	// OFFICIAL is set at the product/Modal boundary. OFFICIAL_JUDGE remains a
	// backward-compatible defense for older launchers.
	return envOn("OUROBOROS_ERB_OFFICIAL") || envOn("OUROBOROS_ERB_OFFICIAL_JUDGE")
}

func blindPlanEnabled() bool {
	// Official evaluation always implies blind planning. The explicit flag
	// also supports non-official blind holdouts without benchmark steering.
	return officialEvalEnabled() || envOn("OUROBOROS_ERB_BLIND_PLAN")
}

func envOn(k string) bool {
	return envOnDefault(k, false)
}

func envOnDefault(k string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return fallback
	}
	if v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes") || strings.EqualFold(v, "on") {
		return true
	}
	if v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "no") || strings.EqualFold(v, "off") {
		return false
	}
	return fallback
}

// EvalResult is the JSON response shape expected by product_adapter.
type EvalResult struct {
	Answer           string   `json:"answer"`
	CitedDocumentIDs []string `json:"cited_document_ids"`
	// Claims are receipt-sanitized grounded claims with verified verbatim
	// quotes and (when the source parser attached one) a leaf locator. The
	// locator is never synthesized — Present=false is explicit absence
	// (#327). Exposed claims are filtered to cited document IDs so a claim
	// whose document_id is not surfaced cannot leak into the UI.
	Claims                  []hosted.Claim             `json:"claims,omitempty"`
	FactualConsistency      *factualconsistency.Result `json:"factual_consistency,omitempty"`
	RetrievalDiagnostics    map[string]any             `json:"retrieval_diagnostics"`
	SearchMode              string                     `json:"search_mode"`
	Failure                 string                     `json:"failure,omitempty"`
	Provider                string                     `json:"provider,omitempty"`
	Model                   string                     `json:"model,omitempty"`
	Temperature             *float64                   `json:"temperature,omitempty"`
	Seed                    *int                       `json:"seed,omitempty"`
	SeedSupported           *bool                      `json:"seed_supported,omitempty"`
	DiagnosticRescueEnabled bool                       `json:"diagnostic_rescue_enabled"`
	GoldAssistEnabled       bool                       `json:"gold_assist_enabled"`
	OfficialEvalEnabled     bool                       `json:"official_eval_enabled"`
	BlindPlanEnabled        bool                       `json:"blind_plan_enabled"`
	// RequestID echoes the request correlation ID (issue #292). Empty for
	// legacy v0 requests or unrecoverable framing.
	RequestID string `json:"request_id,omitempty"`
	// ProtocolVersion is always stamped with HostedLoopProtocolVersion so a
	// reader can reject mixed-mode peers fail-closed.
	ProtocolVersion int `json:"protocol_version"`
}

// claimsCap bounds the per-frame claim list to a small fixed ceiling so a
// malformed or oversized receipt cannot bloat the JSON wire payload. The Go
// ground pass already caps verified claims per question; this is a wire
// safety net, not a reimplementation of grounding.
const claimsCap = 40

// filterClaimsToCitations returns the claims whose document_id appears in the
// cited list, in stable input order, capped at claimsCap. A claim is never
// re-synthesized: if its document_id is not in cited (e.g. the ground pass
// pruned it back out), the claim is dropped — no unsurfaced claim can leak
// into the wire or UI. The locator on each surviving claim is preserved
// verbatim from sanitizeClaimsForReceipt; Present=false stays absent and is
// never promoted (#327).
func filterClaimsToCitations(claims []hosted.Claim, cited []string) []hosted.Claim {
	if len(claims) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(cited))
	for _, id := range cited {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		allowed[id] = struct{}{}
	}
	if len(allowed) == 0 {
		return nil
	}
	out := make([]hosted.Claim, 0, min(len(claims), claimsCap))
	for _, c := range claims {
		if len(out) >= claimsCap {
			break
		}
		doc := strings.TrimSpace(c.DocumentID)
		if doc == "" {
			continue
		}
		if _, ok := allowed[doc]; !ok {
			continue
		}
		// Keep only grounded claim fields. Empty text+quote pairs are dropped
		// by the ground pass; this is a defense-in-depth receipt guard.
		if strings.TrimSpace(c.Text) == "" && strings.TrimSpace(c.Quote) == "" {
			continue
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Engine is always hosted.Client (path2 or memory fixture).
type Engine struct {
	Hosted *hosted.Client
	TopK   int
}

// LoadHosted opens the product path2 Neon+Qdrant brain.
func LoadHosted(topK int) (*Engine, error) {
	client, err := hosted.OpenFromEnv()
	if err != nil {
		return nil, err
	}
	if topK <= 0 {
		topK = 8
	}
	return &Engine{Hosted: client, TopK: topK}, nil
}

// LoadDocsJSONL loads company docs into OpenMemory (fixture only; same Answer path).
func LoadDocsJSONL(path, sourceID, generationID string) (*Engine, error) {
	_ = sourceID
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open docs-jsonl: %w", err)
	}
	brainID := strings.TrimSpace(generationID)
	if brainID == "" {
		brainID = "fixture"
	}
	c := hosted.OpenMemory(brainID)
	ctx := context.Background()
	if err := c.EnsureSchema(ctx); err != nil {
		return nil, err
	}
	var chunks []hosted.ChunkWrite
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		id, _ := row["document_id"].(string)
		if id == "" {
			id, _ = row["id"].(string)
		}
		title, _ := row["title"].(string)
		text, _ := row["text"].(string)
		if id == "" || text == "" {
			continue
		}
		if title != "" {
			text = title + "\n\n" + text
		}
		chunks = append(chunks, hosted.ChunkWrite{
			DocumentID: id,
			ChunkID:    id + "#0",
			Text:       text,
		})
	}
	if len(chunks) == 0 {
		return nil, fmt.Errorf("empty docs jsonl")
	}
	if _, err := c.BurstUpsert(ctx, brainID, chunks, 2); err != nil {
		return nil, err
	}
	return &Engine{Hosted: c, TopK: 8}, nil
}

// Close releases hosted resources.
func (e *Engine) Close() {
	if e != nil && e.Hosted != nil {
		_ = e.Hosted.Close()
	}
}

// LoadMemoryFacts builds an OpenMemory brain from LongMem-style fact strings.
func LoadMemoryFacts(facts []string, brainID string, topK int) (*Engine, error) {
	if brainID == "" {
		brainID = "longmem"
	}
	c := hosted.OpenMemory(brainID)
	ctx := context.Background()
	if err := c.EnsureSchema(ctx); err != nil {
		return nil, err
	}
	var chunks []hosted.ChunkWrite
	for i, fact := range facts {
		fact = strings.TrimSpace(fact)
		if fact == "" {
			continue
		}
		id := fmt.Sprintf("mem-%d", i)
		chunks = append(chunks, hosted.ChunkWrite{
			DocumentID: id,
			ChunkID:    id + "#0",
			Text:       fact,
		})
	}
	if len(chunks) == 0 {
		return nil, fmt.Errorf("empty memory_facts")
	}
	if _, err := c.BurstUpsert(ctx, brainID, chunks, 2); err != nil {
		return nil, err
	}
	// Gardener warm for memory facts.
	docs := map[string]string{}
	for _, ch := range chunks {
		docs[ch.DocumentID] = ch.Text
	}
	_, _ = c.EnrichAfterIngest(ctx, brainID, "mem-gen", docs)
	if topK <= 0 {
		topK = 8
	}
	return &Engine{Hosted: c, TopK: topK}, nil
}

// askSafetyMarginMS keeps the Go per-ask deadline strictly below the Modal
// per-ask wall so a timed-out ask never writes a late line that desyncs the
// next ask on the warm hosted loop.
const askSafetyMarginMS = 5000

// modalAskWallMS reports the Modal per-ask wall in milliseconds. Source of
// truth is tools/build-spine/modal_erb_hosted.py: python ask_timeout =
// max(25, OUROBOROS_ERB_MODAL_TIMEOUT - 5) seconds (container default 90s).
// The launcher stamps OUROBOROS_ERB_MODAL_ASK_WALL_S into container env;
// fall back to OUROBOROS_ERB_MODAL_TIMEOUT - 5s when unset.
func modalAskWallMS() int {
	if v := strings.TrimSpace(os.Getenv("OUROBOROS_ERB_MODAL_ASK_WALL_S")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n * 1000
		}
	}
	timeoutS := 90
	if v := strings.TrimSpace(os.Getenv("OUROBOROS_ERB_MODAL_TIMEOUT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			timeoutS = n
		}
	}
	wall := timeoutS - 5
	if wall < 25 {
		wall = 25
	}
	return wall * 1000
}

// askTimeoutMS is the single Go per-ask deadline. Explicit
// OUROBOROS_ERB_ASK_TIMEOUT_MS wins; otherwise derive from the Modal per-ask
// wall minus askSafetyMarginMS (Modal wall 85s → Go 80s by default).
func askTimeoutMS() int {
	ms := modalAskWallMS() - askSafetyMarginMS
	if v := strings.TrimSpace(os.Getenv("OUROBOROS_ERB_ASK_TIMEOUT_MS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 1000 {
			if n < ms {
				return n
			}
			return ms
		}
	}
	if ms < 10000 {
		ms = 10000
	}
	return ms
}

// Answer runs product retrieval + synthesis via the single Client.
func (e *Engine) Answer(ec EvalCase) EvalResult {
	return e.AnswerContext(context.Background(), ec)
}

// AnswerContext is the hosted-loop-safe variant of Answer. The caller owns the
// parent context so a warm-loop request can actually cancel retrieval/synthesis
// instead of waiting for the background Engine.Answer path to finish.
func (e *Engine) AnswerContext(parent context.Context, ec EvalCase) EvalResult {
	if e == nil || e.Hosted == nil {
		return EvalResult{
			Failure:    "product_brain_nil_engine",
			SearchMode: "product_brain_go_hosted",
		}
	}
	// Per-case memory facts: build ephemeral memory brain (LongMem product path).
	engine := e
	if len(ec.MemoryFacts) > 0 {
		mem, err := LoadMemoryFacts(ec.MemoryFacts, "longmem-"+ec.QuestionID, e.TopK)
		if err != nil {
			return EvalResult{
				Failure:    fmt.Sprintf("product_brain_memory_facts:%v", err),
				SearchMode: "product_brain_go_memory",
			}
		}
		defer mem.Close()
		engine = mem
	}
	topK := engine.TopK
	if topK <= 0 {
		topK = 8
	}

	gold := ec.goldDocIDs()
	// Score integrity: gold must NOT steer retrieval/synth by default. Only the
	// explicit offline-diagnostics opt-in passes it into AnswerOpts; post-answer
	// cite_gold_* diags below are computed from the result and never modify it.
	assistGold := gold
	if !goldAssistEnabled() {
		assistGold = nil
	}
	// Eval mode: prefer env (QUALITY/bench); empty → plan uses ApplyServeMode default.
	serveMode := os.Getenv("OUROBOROS_ERB_MODE")
	if serveMode == "" && os.Getenv("OUROBOROS_ERB_QUALITY") == "1" {
		serveMode = "bench"
	}
	// Blind product measure drops labeled type; official mode implies blind.
	qType := ec.QuestionType
	sourceTypes := ec.SourceTypes
	if blindPlanEnabled() {
		qType = ""
		// source_types is evaluator metadata, not a legitimate official input;
		// scrub it alongside question_type at the Go boundary.
		sourceTypes = nil
	}
	// Official blind mode must never receive gold-derived filters (issue
	// #328): reject before any retrieval so a poisoned case fails closed
	// instead of steering under a sanitized shape. Non-blind postures pass
	// the raw map through; RetrieveOpts performs the full authorized
	// normalization and fails closed on malformed/unauthorized predicates.
	if blindPlanEnabled() && len(ec.Filter) > 0 {
		if _, err := hosted.NormalizeMetadataFilter(ec.Filter, hosted.FilterAuthority{Blind: true}); err != nil {
			return EvalResult{
				Failure:    "blind_metadata_filter_rejected",
				SearchMode: "product_brain_go_hosted",
				RetrievalDiagnostics: map[string]any{
					"filter_rejected": err.Error(),
					"question_id":     ec.QuestionID,
					"status":          "failure",
				},
			}
		}
	}
	var prior []hosted.SessionTurn
	for _, t := range ec.PriorTurns {
		prior = append(prior, hosted.SessionTurn{
			SessionID: ec.SessionID,
			Role:      t.Role,
			Content:   t.Content,
		})
	}
	// Per-ask deadline: derived from the Modal per-ask wall (single helper so
	// Go and Python cannot drift); Modal outer kill is only the backstop. The
	// deadline lets retrieve/synth arms observe cancel and return sooner instead
	// of writing a late desync line. Override via OUROBOROS_ERB_ASK_TIMEOUT_MS.
	askMS := askTimeoutMS()
	if ec.AskTimeoutMS > 1000 && ec.AskTimeoutMS < askMS {
		askMS = ec.AskTimeoutMS
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(askMS)*time.Millisecond)
	defer cancel()
	r := engine.Hosted.AnswerOpts(ctx, hosted.AnswerOptions{
		Question:     ec.Question,
		QuestionType: qType,
		TopK:         topK,
		Mode:         serveMode,
		SessionID:    ec.SessionID,
		SourceTypes:  sourceTypes,
		GoldDocIDs:   assistGold,
		Filter:       ec.Filter,
		PriorTurns:   prior,
		HistoryText:  ec.History,
	})
	if r.RetrievalDiagnostics == nil {
		r.RetrievalDiagnostics = map[string]any{}
	}
	r.RetrievalDiagnostics["ask_timeout_ms"] = askMS
	r.RetrievalDiagnostics["modal_ask_wall_ms"] = modalAskWallMS()
	if goldAssistEnabled() && len(gold) > 0 {
		r.RetrievalDiagnostics["gold_assist"] = true
	}
	if ctx.Err() != nil {
		r.RetrievalDiagnostics["ask_deadline_exceeded"] = true
	}
	r.RetrievalDiagnostics["question_id"] = ec.QuestionID
	r.RetrievalDiagnostics["question_type"] = ec.QuestionType
	r.RetrievalDiagnostics["status"] = "ok"
	r.RetrievalDiagnostics["store"] = engine.Hosted.StoreKind()
	if len(gold) > 0 {
		r.RetrievalDiagnostics["gold_count"] = len(gold)
		// Cite-stage gold recall (official-style doc recall on cited ids).
		hit := 0
		citeSet := map[string]struct{}{}
		for _, id := range r.CitedDocumentIDs {
			citeSet[id] = struct{}{}
		}
		for _, g := range gold {
			if _, ok := citeSet[g]; ok {
				hit++
			}
		}
		r.RetrievalDiagnostics["cite_gold_recall"] = float64(hit) / float64(len(gold))
		r.RetrievalDiagnostics["cite_gold_hits"] = hit
	}
	if len(ec.MemoryFacts) > 0 {
		r.RetrievalDiagnostics["memory_facts"] = len(ec.MemoryFacts)
		r.RetrievalDiagnostics["product_memory"] = true
	}
	if r.Failure != "" {
		r.RetrievalDiagnostics["status"] = "failure"
	}
	if r.GroundingDiagnostics != nil {
		r.RetrievalDiagnostics["grounding"] = r.GroundingDiagnostics
	}
	mode := r.SearchMode
	if len(ec.MemoryFacts) > 0 && mode == "product_brain_go_hosted" {
		mode = "product_brain_go_memory"
	}
	result := EvalResult{
		Answer:           r.Answer,
		CitedDocumentIDs: r.CitedDocumentIDs,
		// filterClaimsToCitations drops any claim whose document_id is not in
		// CitedDocumentIDs and bounds the list at claimsCap. The locator on
		// each surviving claim was already receipt-sanitized by the ground
		// pass; we never re-derive or synthesize it (#327).
		Claims:               filterClaimsToCitations(r.Claims, r.CitedDocumentIDs),
		FactualConsistency:   r.FactualConsistency,
		SearchMode:           mode,
		Failure:              r.Failure,
		RetrievalDiagnostics: r.RetrievalDiagnostics,
		Provider:             r.Provider,
		Model:                r.Model,
	}
	stampEvalState(&result)
	return result
}

func stampEvalState(result *EvalResult) {
	if result == nil {
		return
	}
	result.DiagnosticRescueEnabled = diagnosticRescueEnabled()
	result.GoldAssistEnabled = goldAssistEnabled()
	result.OfficialEvalEnabled = officialEvalEnabled()
	result.BlindPlanEnabled = blindPlanEnabled()
	if result.RetrievalDiagnostics == nil {
		result.RetrievalDiagnostics = map[string]any{}
	}
	result.RetrievalDiagnostics["diagnostic_rescue_enabled"] = result.DiagnosticRescueEnabled
	result.RetrievalDiagnostics["gold_assist_enabled"] = result.GoldAssistEnabled
	result.RetrievalDiagnostics["official_eval_enabled"] = result.OfficialEvalEnabled
	result.RetrievalDiagnostics["blind_plan_enabled"] = result.BlindPlanEnabled
	// Stamp synthesis temperature and seed (issue #291).
	if result.Temperature == nil {
		t := hosted.SynthTemperature(0)
		result.Temperature = &t
	}
	if result.Seed == nil {
		result.Seed = hosted.SynthSeed()
	}
	if result.SeedSupported == nil && result.Seed != nil {
		supported := hosted.ProviderSupportsSeed(result.Provider)
		result.SeedSupported = &supported
	}
}
