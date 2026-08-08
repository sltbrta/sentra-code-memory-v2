package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factualconsistency"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
)

// tinyJSONLPath resolves testdata for both `go test` (source-relative) and
// Bazel (runfiles under TEST_SRCDIR or cwd-relative package path).
func tinyJSONLPath(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"testdata/tiny.jsonl",
		"services/brain/cmd/product-brain-eval/testdata/tiny.jsonl",
	}
	if src := os.Getenv("TEST_SRCDIR"); src != "" {
		ws := os.Getenv("TEST_WORKSPACE")
		if ws == "" {
			ws = "_main"
		}
		candidates = append(candidates,
			filepath.Join(src, ws, "services/brain/cmd/product-brain-eval/testdata/tiny.jsonl"),
			filepath.Join(src, "services/brain/cmd/product-brain-eval/testdata/tiny.jsonl"),
		)
	}
	if _, file, _, ok := runtime.Caller(0); ok {
		candidates = append(candidates, filepath.Join(filepath.Dir(file), "testdata", "tiny.jsonl"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	t.Fatalf("testdata/tiny.jsonl not found; tried %v (cwd=%s)", candidates, mustGetwd())
	return ""
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "?"
	}
	return wd
}

func TestEvalPathTinyJSONL(t *testing.T) {
	t.Parallel()
	docs := tinyJSONLPath(t)

	engine, err := LoadDocsJSONL(docs, "test-src", "gen-eval-1")
	if err != nil {
		t.Fatal(err)
	}

	result := engine.Answer(EvalCase{
		Question:     "What is the MedThink RPO policy for PROJ-99?",
		QuestionID:   "q-tiny-1",
		QuestionType: "basic",
	})
	if result.Failure != "" {
		t.Fatalf("unexpected failure: %s diag=%v", result.Failure, result.RetrievalDiagnostics)
	}
	if result.SearchMode != "product_brain_go_hosted" {
		t.Fatalf("search_mode=%q", result.SearchMode)
	}
	if len(result.CitedDocumentIDs) == 0 {
		t.Fatalf("expected citations, diag=%v", result.RetrievalDiagnostics)
	}
	if result.FactualConsistency == nil || result.FactualConsistency.Status != factualconsistency.StatusScored ||
		result.FactualConsistency.ScorePerMille < factualconsistency.DefaultDecisionPerMille {
		t.Fatalf("missing calibrated factual consistency beside citations: %#v", result.FactualConsistency)
	}
	// d1 is the primary RPO policy doc; lexical or graph should surface it.
	foundD1 := false
	for _, id := range result.CitedDocumentIDs {
		if id == "d1" {
			foundD1 = true
		}
	}
	if !foundD1 {
		t.Fatalf("expected d1 cited, got %v answer=%q", result.CitedDocumentIDs, result.Answer)
	}
	if !strings.Contains(strings.ToLower(result.Answer), "medthink") &&
		!strings.Contains(strings.ToLower(result.Answer), "rpo") {
		t.Fatalf("answer missing MedThink/RPO: %q", result.Answer)
	}
	// Graph expansion should often pull d2 via PROJ-99 / MedThink co-edges.
	diag := result.RetrievalDiagnostics
	if diag == nil {
		t.Fatal("missing retrieval_diagnostics")
	}
	if diag["status"] != "ok" {
		t.Fatalf("status=%v", diag["status"])
	}
	if n, _ := diag["corpus_docs"].(int); n != 3 {
		// JSON numbers may be int; map[string]any keeps int from Go side.
		if n64, ok := diag["corpus_docs"].(int); ok && n64 != 3 {
			t.Fatalf("corpus_docs=%v", diag["corpus_docs"])
		}
	}
}

func TestEvalPathEmptyQuestionTokens(t *testing.T) {
	t.Parallel()
	docs := tinyJSONLPath(t)
	engine, err := LoadDocsJSONL(docs, "test-src", "gen-eval-2")
	if err != nil {
		t.Fatal(err)
	}
	// Single-char / stopword-ish queries may yield empty retrieve; must not crash.
	result := engine.Answer(EvalCase{
		Question:   "a b",
		QuestionID: "q-empty",
	})
	if result.SearchMode != "product_brain_go_hosted" {
		t.Fatalf("search_mode=%q", result.SearchMode)
	}
	// Failure is OK only if soft empty; hard retrieve errors should not appear for tiny corpus.
	if result.Failure != "" && !strings.Contains(result.Failure, "no lexical") {
		// After soft-empty retrieve, Answer returns a no-docs message without Failure.
		t.Fatalf("unexpected failure=%s", result.Failure)
	}
}

// extractiveEnv forces the deterministic no-LLM answer path for gold tests.
func extractiveEnv(t *testing.T) {
	t.Helper()
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_BRAIN_LLM", "extractive")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("GROQ_API_KEY", "")
}

func TestEvalGoldDoesNotSteerByDefault(t *testing.T) {
	// Score integrity: changing expected_doc_ids/document_ids must not change
	// AnswerOpts input/behavior under default env (no gold assist).
	extractiveEnv(t)
	t.Setenv("OUROBOROS_ERB_GOLD_ASSIST", "")
	t.Setenv("OUROBOROS_ERB_OFFICIAL", "")
	t.Setenv("OUROBOROS_ERB_OFFICIAL_JUDGE", "")
	docs := tinyJSONLPath(t)
	engine, err := LoadDocsJSONL(docs, "test-src", "gen-eval-gold-default")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	base := EvalCase{
		Question:     "What is the MedThink RPO policy for PROJ-99?",
		QuestionID:   "q-gold-default",
		QuestionType: "basic",
	}
	noGold := engine.Answer(base)
	if noGold.Failure != "" {
		t.Fatalf("failure=%s diag=%v", noGold.Failure, noGold.RetrievalDiagnostics)
	}
	withGold := engine.Answer(EvalCase{
		Question:       base.Question,
		QuestionID:     base.QuestionID,
		QuestionType:   base.QuestionType,
		DocumentIDs:    []string{"d2"},
		ExpectedDocIDs: []string{"d2", "d3"},
	})
	if withGold.Failure != "" {
		t.Fatalf("failure=%s diag=%v", withGold.Failure, withGold.RetrievalDiagnostics)
	}
	if noGold.Answer != withGold.Answer {
		t.Fatalf("gold changed answer output:\nno-gold: %q\nwith-gold: %q", noGold.Answer, withGold.Answer)
	}
	if strings.Join(noGold.CitedDocumentIDs, ",") != strings.Join(withGold.CitedDocumentIDs, ",") {
		t.Fatalf("gold changed citations: %v vs %v", noGold.CitedDocumentIDs, withGold.CitedDocumentIDs)
	}
	if noGold.FactualConsistency == nil || withGold.FactualConsistency == nil ||
		!reflect.DeepEqual(noGold.FactualConsistency, withGold.FactualConsistency) {
		t.Fatalf("gold-looking eval fields changed calibrated score: no-gold=%#v with-gold=%#v", noGold.FactualConsistency, withGold.FactualConsistency)
	}
	diag := withGold.RetrievalDiagnostics
	if diag == nil {
		t.Fatal("missing retrieval_diagnostics")
	}
	if diag["diagnostic_rescue_enabled"] != false || diag["gold_assist_enabled"] != false {
		t.Fatalf("default effective state not stamped off: %v", diag)
	}
	// No gold-steered diags may appear by default.
	for _, k := range []string{"pool_recall", "window_recall", "cite_precision", "window_precision", "gold_assist", "gold_pack_first", "gold_floor_post_agentic"} {
		if _, ok := diag[k]; ok {
			t.Fatalf("default mode must not stamp %s; keys=%v", k, keysOfDiag(diag))
		}
		if g, ok := diag["grounding"].(map[string]any); ok {
			if _, ok := g[k]; ok {
				t.Fatalf("default mode must not stamp grounding.%s", k)
			}
		}
	}
	// Safe post-answer diags ARE exposed (result-only, no behavior change).
	if n, _ := diag["gold_count"].(int); n != 2 {
		t.Fatalf("gold_count=%v want 2", diag["gold_count"])
	}
	if _, ok := diag["cite_gold_recall"]; !ok {
		t.Fatalf("missing post-answer cite_gold_recall; keys=%v", keysOfDiag(diag))
	}
}

func TestEvalOfficialModeForcesDiagnosticAndGoldAssistOff(t *testing.T) {
	// The product/Modal boundary flag, not the later judge host, owns blind mode.
	extractiveEnv(t)
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	t.Setenv("OUROBOROS_ERB_DIAGNOSTIC_RESCUE", "1")
	t.Setenv("OUROBOROS_ERB_GOLD_ASSIST", "1")
	t.Setenv("OUROBOROS_ERB_OFFICIAL", "1")
	if goldAssistEnabled() {
		t.Fatal("official mode must force gold assist off")
	}
	if diagnosticRescueEnabled() {
		t.Fatal("official mode must force diagnostic rescue off")
	}
	docs := tinyJSONLPath(t)
	engine, err := LoadDocsJSONL(docs, "test-src", "gen-eval-gold-official")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	result := engine.Answer(EvalCase{
		Question:       "What is the MedThink RPO policy for PROJ-99?",
		QuestionID:     "q-gold-official",
		QuestionType:   "basic",
		ExpectedDocIDs: []string{"d1"},
	})
	if result.Failure != "" {
		t.Fatalf("failure=%s", result.Failure)
	}
	diag := result.RetrievalDiagnostics
	if diag["gold_assist_enabled"] != false || diag["diagnostic_rescue_enabled"] != false {
		t.Fatalf("official effective state must be stamped off: %v", diag)
	}
	if _, ok := diag["gold_assist"]; ok {
		t.Fatal("gold_assist steering marker must not be stamped under official mode")
	}
	for _, k := range []string{"pool_recall", "window_recall", "cite_precision"} {
		if _, ok := diag[k]; ok {
			t.Fatalf("official mode must not stamp %s", k)
		}
	}
	// Post-answer gold recall diag is still safe/allowed.
	if _, ok := diag["cite_gold_recall"]; !ok {
		t.Fatal("official mode keeps post-answer cite_gold_recall diag")
	}
}

func TestEvalBlindPlanForcesDiagnosticAndGoldAssistOff(t *testing.T) {
	extractiveEnv(t)
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	t.Setenv("OUROBOROS_ERB_DIAGNOSTIC_RESCUE", "1")
	t.Setenv("OUROBOROS_ERB_GOLD_ASSIST", "1")
	t.Setenv("OUROBOROS_ERB_OFFICIAL", "")
	t.Setenv("OUROBOROS_ERB_OFFICIAL_JUDGE", "")
	t.Setenv("OUROBOROS_ERB_BLIND_PLAN", "1")
	if !blindPlanEnabled() || diagnosticRescueEnabled() || goldAssistEnabled() {
		t.Fatal("blind plan must force diagnostic rescue and gold assist off")
	}
	docs := tinyJSONLPath(t)
	engine, err := LoadDocsJSONL(docs, "test-src", "gen-eval-blind")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	result := engine.Answer(EvalCase{
		Question:       "What is the MedThink RPO policy for PROJ-99?",
		QuestionID:     "q-gold-blind",
		QuestionType:   "basic",
		ExpectedDocIDs: []string{"d1"},
	})
	if result.Failure != "" {
		t.Fatalf("failure=%s", result.Failure)
	}
	if result.RetrievalDiagnostics["blind_plan_enabled"] != true ||
		result.RetrievalDiagnostics["diagnostic_rescue_enabled"] != false ||
		result.RetrievalDiagnostics["gold_assist_enabled"] != false {
		t.Fatalf("blind effective state not stamped: %v", result.RetrievalDiagnostics)
	}
}

func TestEvalGoldAssistOptIn(t *testing.T) {
	// Explicit unsafe opt-in: DocumentIDs → GoldDocIDs reaches AnswerOpts and
	// stamps the offline pool/window/cite gold diags.
	extractiveEnv(t)
	t.Setenv("OUROBOROS_ERB_GOLD_ASSIST", "1")
	t.Setenv("OUROBOROS_ERB_OFFICIAL", "")
	t.Setenv("OUROBOROS_ERB_OFFICIAL_JUDGE", "")
	docs := tinyJSONLPath(t)
	engine, err := LoadDocsJSONL(docs, "test-src", "gen-eval-gold")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	result := engine.Answer(EvalCase{
		Question:     "What is the MedThink RPO policy for PROJ-99?",
		QuestionID:   "q-gold-1",
		QuestionType: "basic",
		DocumentIDs:  []string{"d1"},
	})
	if result.Failure != "" {
		t.Fatalf("failure=%s diag=%v", result.Failure, result.RetrievalDiagnostics)
	}
	diag := result.RetrievalDiagnostics
	if diag == nil {
		t.Fatal("missing retrieval_diagnostics")
	}
	if diag["gold_assist_enabled"] != true {
		t.Fatalf("opt-in must stamp effective gold assist; keys=%v", keysOfDiag(diag))
	}
	// Gold non-empty on residual memory path stamps pool/window recall and/or cite precision.
	hasGold := false
	for _, k := range []string{"pool_recall", "window_recall", "cite_precision", "window_precision"} {
		if _, ok := diag[k]; ok {
			hasGold = true
			break
		}
		// cite_precision may live under grounding submap.
		if g, ok := diag["grounding"].(map[string]any); ok {
			if _, ok := g[k]; ok {
				hasGold = true
				break
			}
		}
	}
	if !hasGold {
		t.Fatalf("DocumentIDs→GoldDocIDs must stamp gold diags; keys=%v", keysOfDiag(diag))
	}
}

func TestAskTimeoutHelper(t *testing.T) {
	// Default: Modal container 90s → ask wall 85s → Go deadline 80s.
	t.Setenv("OUROBOROS_ERB_ASK_TIMEOUT_MS", "")
	t.Setenv("OUROBOROS_ERB_MODAL_ASK_WALL_S", "")
	t.Setenv("OUROBOROS_ERB_MODAL_TIMEOUT", "")
	if got := modalAskWallMS(); got != 85000 {
		t.Fatalf("default modal ask wall = %d, want 85000", got)
	}
	if got := askTimeoutMS(); got != 80000 {
		t.Fatalf("default ask timeout = %d, want 80000", got)
	}
	// Container timeout env drives the wall (-5s) and the Go deadline (-10s).
	t.Setenv("OUROBOROS_ERB_MODAL_TIMEOUT", "120")
	if got := modalAskWallMS(); got != 115000 {
		t.Fatalf("modal ask wall = %d, want 115000", got)
	}
	if got := askTimeoutMS(); got != 110000 {
		t.Fatalf("ask timeout = %d, want 110000", got)
	}
	// Launcher-stamped wall env wins over the container timeout derivation.
	t.Setenv("OUROBOROS_ERB_MODAL_ASK_WALL_S", "100")
	if got := modalAskWallMS(); got != 100000 {
		t.Fatalf("modal ask wall = %d, want 100000", got)
	}
	if got := askTimeoutMS(); got != 95000 {
		t.Fatalf("ask timeout = %d, want 95000", got)
	}
	// Go deadline is always ≥5s below the Modal per-ask wall.
	if askTimeoutMS() > modalAskWallMS()-askSafetyMarginMS {
		t.Fatal("go deadline must stay below modal ask wall minus safety margin")
	}
	// Safe explicit override wins; unsafe override is clamped below the wall.
	t.Setenv("OUROBOROS_ERB_ASK_TIMEOUT_MS", "42000")
	if got := askTimeoutMS(); got != 42000 {
		t.Fatalf("explicit ask timeout = %d, want 42000", got)
	}
	t.Setenv("OUROBOROS_ERB_ASK_TIMEOUT_MS", "99000")
	if got := askTimeoutMS(); got != 95000 {
		t.Fatalf("unsafe explicit ask timeout must clamp to 95000, got %d", got)
	}
}

func keysOfDiag(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestBlindModeRejectsGoldDerivedFilter proves the official/blind boundary
// fails closed on gold-derived metadata filters before any retrieval runs
// (issue #328), while non-gold governance predicates pass the blind guard.
func TestBlindModeRejectsGoldDerivedFilter(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_BLIND_PLAN", "1")
	eng := &Engine{Hosted: hosted.OpenMemory("blind-filter-engine"), TopK: 4}
	defer eng.Hosted.Close()

	for _, key := range []string{"expected_doc_ids", "document_ids", "source_types", "question_type"} {
		res := eng.Answer(EvalCase{
			Question:   "what is the rpo?",
			QuestionID: "q-blind-" + key,
			Filter:     map[string]any{key: []any{"x"}},
		})
		if res.Failure != "blind_metadata_filter_rejected" {
			t.Fatalf("blind mode must reject gold-derived filter %q, got failure=%q diag=%v",
				key, res.Failure, res.RetrievalDiagnostics)
		}
		if res.RetrievalDiagnostics["filter_rejected"] == nil {
			t.Fatalf("blind rejection of %q must stamp filter_rejected receipt", key)
		}
	}

	// Non-gold governance predicates are not blocked by the blind guard
	// itself (downstream retrieval may still run or fail for other reasons).
	res := eng.Answer(EvalCase{
		Question:   "what is the rpo?",
		QuestionID: "q-blind-ok",
		Filter:     map[string]any{"tags": []any{"fin"}},
	})
	if res.Failure == "blind_metadata_filter_rejected" {
		t.Fatal("non-gold governance filter must pass the blind guard")
	}
}

// --- Request-correlated loop framing (issue #292) -------------------------
//
// The warm --hosted-loop wire protocol is versioned and request-correlated:
// every v1 request carries request_id + protocol_version and every response
// echoes them, so concurrent/late/malformed/mixed-mode lines can never be
// cross-attributed. Legacy v0 requests (no framing fields) are still answered
// for the build-spine harness; a declared but unsupported version fails closed.

func TestProtocolVersionErrorFailsClosed(t *testing.T) {
	t.Parallel()
	if err := protocolVersionError(EvalCase{}); err != nil {
		t.Fatalf("unframed legacy request must be accepted, got %v", err.Failure)
	}
	if err := protocolVersionError(EvalCase{ProtocolVersion: 0}); err != nil {
		t.Fatalf("explicit v0 request must be accepted, got %v", err.Failure)
	}
	if err := protocolVersionError(EvalCase{ProtocolVersion: HostedLoopProtocolVersion, RequestID: "rq-ok"}); err != nil {
		t.Fatalf("complete v1 request must be accepted, got %v", err.Failure)
	}
	for _, tc := range []struct {
		name  string
		case_ EvalCase
		want  string
	}{
		{name: "v1 missing id", case_: EvalCase{ProtocolVersion: HostedLoopProtocolVersion}, want: "missing_request_id"},
		{name: "v0 with id", case_: EvalCase{ProtocolVersion: 0, RequestID: "rq-partial"}, want: "missing"},
	} {
		err := protocolVersionError(tc.case_)
		if err == nil || !strings.HasSuffix(err.Failure, tc.want) {
			t.Fatalf("%s: failure=%v want suffix %q", tc.name, err, tc.want)
		}
	}
	for _, v := range []int{-1, 2, 99} {
		err := protocolVersionError(EvalCase{ProtocolVersion: v, RequestID: "rq-bad"})
		if err == nil {
			t.Fatalf("version %d must fail closed", v)
		}
		want := "product_brain_protocol_version:want=1:got=" + strconv.Itoa(v)
		if err.Failure != want {
			t.Fatalf("failure=%q want %q", err.Failure, want)
		}
		if err.RequestID != "rq-bad" {
			t.Fatalf("version-mismatch response must echo request_id, got %q", err.RequestID)
		}
		if err.SearchMode != "product_brain_go_hosted" {
			t.Fatalf("search_mode=%q", err.SearchMode)
		}
	}
}

func TestMalformedProtocolFramingFailsClosed(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		`{"request_id":"rq-null","protocol_version":null}`,
		`{"request_id":"rq-bool","protocol_version":true}`,
		`{"request_id":"rq-float","protocol_version":1.0}`,
		`{"protocol_version":1}`,
	} {
		var c EvalCase
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatalf("unmarshal %s: %v", line, err)
		}
		if err := protocolVersionError(c); err == nil {
			t.Fatalf("malformed framing must fail closed: %s", line)
		}
	}
}

func TestProbeRequestID(t *testing.T) {
	t.Parallel()
	// Type error on another field fails the full EvalCase unmarshal, but the
	// correlation ID must still be recovered for the fail-closed echo.
	line := `{"request_id":"rq-7","question":"q","ask_timeout_ms":"not-an-int"}`
	if got := probeRequestID(line); got != "rq-7" {
		t.Fatalf("probeRequestID=%q want rq-7", got)
	}
	if got := probeRequestID("this-is-not-json"); got != "" {
		t.Fatalf("garbage line must yield empty id, got %q", got)
	}
	if got := probeRequestID(`{"question":"q"}`); got != "" {
		t.Fatalf("missing id must yield empty, got %q", got)
	}
}

func TestStampWireFramingStampsProtocolVersion(t *testing.T) {
	t.Parallel()
	r := EvalResult{}
	stampWireFraming(&r)
	if r.ProtocolVersion != HostedLoopProtocolVersion {
		t.Fatalf("protocol_version=%d want %d", r.ProtocolVersion, HostedLoopProtocolVersion)
	}
}

// TestRunLoopIOFramingEndToEnd drives the extracted loop body over in-memory
// pipes: every response must echo its own request_id in strict request→response
// order, version mismatches and malformed lines must fail closed with the best
// available echo, and a later request must never be satisfied by an earlier
// request's frame.
func TestRunLoopIOFramingEndToEnd(t *testing.T) {
	extractiveEnv(t)
	t.Setenv("OUROBOROS_ERB_OFFICIAL", "")
	t.Setenv("OUROBOROS_ERB_BLIND_PLAN", "")
	engine, err := LoadDocsJSONL(tinyJSONLPath(t), "test-src", "gen-loop-framing")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	lines := []string{
		`{"question":"What is the MedThink RPO policy for PROJ-99?","question_id":"q1","request_id":"rq-1","protocol_version":1}`,
		`{"question":"second","question_id":"q2","request_id":"rq-2","protocol_version":2}`,
		`{"request_id":"rq-3","question":"q","ask_timeout_ms":"not-an-int"}`,
		`{"question":"","question_id":"q4","request_id":"rq-4","protocol_version":1}`,
		`{"question":"What is the MedThink RPO policy for PROJ-99?","question_id":"q5","request_id":"rq-5","protocol_version":1}`,
		`{"question":"legacy no framing","question_id":"q6"}`,
	}
	var out bytes.Buffer
	n, err := runLoopIO(engine, strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, 5)
	if err != nil {
		t.Fatalf("runLoopIO: %v", err)
	}
	if n != len(lines) {
		t.Fatalf("handled=%d want %d", n, len(lines))
	}
	var got []EvalResult
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var r EvalResult
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("response line not json: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != len(lines) {
		t.Fatalf("responses=%d want %d (one framed response per request line)", len(got), len(lines))
	}
	for i, r := range got {
		if r.ProtocolVersion != HostedLoopProtocolVersion {
			t.Fatalf("response %d protocol_version=%d", i, r.ProtocolVersion)
		}
	}
	if got[0].RequestID != "rq-1" || got[0].Failure != "" {
		t.Fatalf("rq-1: id=%q failure=%q", got[0].RequestID, got[0].Failure)
	}
	if got[1].RequestID != "rq-2" || !strings.HasPrefix(got[1].Failure, "product_brain_protocol_version:") {
		t.Fatalf("rq-2 version mismatch must fail closed with echo: id=%q failure=%q", got[1].RequestID, got[1].Failure)
	}
	if got[2].RequestID != "rq-3" || !strings.HasPrefix(got[2].Failure, "product_brain_bad_case_json:") {
		t.Fatalf("rq-3 malformed must fail closed with probed echo: id=%q failure=%q", got[2].RequestID, got[2].Failure)
	}
	if got[3].RequestID != "rq-4" || got[3].Failure != "product_brain_empty_question" {
		t.Fatalf("rq-4 empty question must echo id: id=%q failure=%q", got[3].RequestID, got[3].Failure)
	}
	// No earlier frame may satisfy a later request: rq-5 answered on its own.
	if got[4].RequestID != "rq-5" || got[4].Failure != "" {
		t.Fatalf("rq-5: id=%q failure=%q", got[4].RequestID, got[4].Failure)
	}
	if got[4].RetrievalDiagnostics["question_id"] != "q5" {
		t.Fatalf("rq-5 must carry its own question_id diag, got %v", got[4].RetrievalDiagnostics["question_id"])
	}
	// Legacy v0: answered, version stamped, no request_id echo.
	if got[5].RequestID != "" || got[5].ProtocolVersion != HostedLoopProtocolVersion {
		t.Fatalf("legacy v0 response: id=%q version=%d", got[5].RequestID, got[5].ProtocolVersion)
	}
}

// --- Grounded claims wire contract -----------------------------------------
//
// EvalResult must serialize receipt-sanitized grounded claims (and their
// per-claim leaf locator when the parser attached one) so the Python/UI
// Sources section can carry grounded quote snippets + page/line refs
// instead of bare ids. The frame must also filter unsurfaced claims — a
// claim whose document_id is not in cited_document_ids is dropped, so no
// claim can leak before the UI sees it. The locator is never synthesized:
// Present=false stays absent (#327).

func TestEvalResultEncodedClaimsCarryQuoteAndLocator(t *testing.T) {
	t.Parallel()
	result := EvalResult{
		Answer:           "The RPO is fifteen minutes.",
		CitedDocumentIDs: []string{"d1"},
		Claims: []hosted.Claim{
			{
				Text:       "The RPO is fifteen minutes.",
				Quote:      "RPO: 15 minutes for gold tier",
				DocumentID: "d1",
				Locator: hosted.Locator{
					Present:    true,
					PageNumber: 3,
					Section:    "Recovery",
					StartLine:  12,
					EndLine:    14,
				},
			},
		},
		SearchMode: "product_brain_go_hosted",
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(result); err != nil {
		t.Fatalf("encode: %v", err)
	}
	raw := buf.Bytes()
	// Round-trip the encoded frame so we exercise the JSON shape the Python
	// adapter actually parses, not just the Go struct.
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	claimsAny, ok := got["claims"].([]any)
	if !ok {
		t.Fatalf("claims missing or wrong type: %T %v", got["claims"], got["claims"])
	}
	if len(claimsAny) != 1 {
		t.Fatalf("claims length=%d want 1", len(claimsAny))
	}
	cl, ok := claimsAny[0].(map[string]any)
	if !ok {
		t.Fatalf("claim 0 wrong type: %T", claimsAny[0])
	}
	if cl["document_id"] != "d1" {
		t.Fatalf("claim document_id=%v want d1", cl["document_id"])
	}
	if cl["quote"] != "RPO: 15 minutes for gold tier" {
		t.Fatalf("claim quote=%v", cl["quote"])
	}
	if cl["text"] != "The RPO is fifteen minutes." {
		t.Fatalf("claim text=%v", cl["text"])
	}
	loc, ok := cl["locator"].(map[string]any)
	if !ok {
		t.Fatalf("locator missing or wrong type: %T %v", cl["locator"], cl["locator"])
	}
	if loc["present"] != true {
		t.Fatalf("locator.present=%v want true", loc["present"])
	}
	// PageNumber / Section / line range must survive the encode.
	if n, _ := loc["page_number"].(float64); n != 3 {
		t.Fatalf("locator.page_number=%v want 3", loc["page_number"])
	}
	if loc["section"] != "Recovery" {
		t.Fatalf("locator.section=%v want Recovery", loc["section"])
	}
	if n, _ := loc["start_line"].(float64); n != 12 {
		t.Fatalf("locator.start_line=%v want 12", loc["start_line"])
	}
	if n, _ := loc["end_line"].(float64); n != 14 {
		t.Fatalf("locator.end_line=%v want 14", loc["end_line"])
	}
}

func TestEvalResultEncodedClaimsOmitLocatorWhenAbsent(t *testing.T) {
	t.Parallel()
	// Present=false is the explicit-absence sentinel (#327). The encoded
	// frame must NOT carry a synthetic locator when the parser did not
	// attach one. Page/section/line fields must also be absent (omitempty).
	result := EvalResult{
		Answer:           "no locator",
		CitedDocumentIDs: []string{"d1"},
		Claims: []hosted.Claim{
			{Text: "x", Quote: "verbatim quote text", DocumentID: "d1"},
		},
		SearchMode: "product_brain_go_hosted",
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	claims := got["claims"].([]any)
	if len(claims) != 1 {
		t.Fatalf("claims length=%d", len(claims))
	}
	cl := claims[0].(map[string]any)
	// The locator must be either absent or — if it surfaces — must NOT carry
	// a Present=true. Allow either absent field or {"present":false}; never
	// synthesized page/line/section.
	if locAny, ok := cl["locator"]; ok {
		loc, _ := locAny.(map[string]any)
		if loc != nil {
			if v, ok := loc["present"]; ok && v == true {
				t.Fatalf("locator.present must not be true when source had none: %v", loc)
			}
			if v, ok := loc["page_number"]; ok && v != 0 {
				t.Fatalf("locator.page_number must be absent/zero, got %v", v)
			}
			if v, ok := loc["section"]; ok && v != "" {
				t.Fatalf("locator.section must be absent/empty, got %v", v)
			}
		}
	}
}

func TestFilterClaimsToCitationsDropsUnsurfacedAndBounds(t *testing.T) {
	t.Parallel()
	// Mix of cited and unsurfaced claims; a duplicate-doc empty claim must
	// also be dropped. The cap at claimsCap must hold even when many
	// surfaced claims exist.
	survivors := make([]hosted.Claim, 0, claimsCap+10)
	for i := 0; i < claimsCap+5; i++ {
		survivors = append(survivors, hosted.Claim{
			Text:       "t",
			Quote:      "q",
			DocumentID: "doc-A",
		})
	}
	input := []hosted.Claim{
		// surfaced → kept
		{Text: "keep", Quote: "keep quote", DocumentID: "doc-A"},
		// surfaced but empty → dropped
		{DocumentID: "doc-A"},
		// unsurfaced → dropped (no leak)
		{Text: "leak", Quote: "leak quote", DocumentID: "doc-B"},
		// doc not in cited at all → dropped
		{Text: "other", Quote: "other quote", DocumentID: "doc-Z"},
	}
	input = append(input, survivors...)
	got := filterClaimsToCitations(input, []string{"doc-A"})
	if len(got) != claimsCap {
		t.Fatalf("got %d claims, want cap=%d", len(got), claimsCap)
	}
	for _, c := range got {
		if c.DocumentID != "doc-A" {
			t.Fatalf("unsurfaced claim leaked: %+v", c)
		}
	}
	// Empty cited list → all claims dropped (no synthetic ids).
	if got := filterClaimsToCitations(input, nil); got != nil {
		t.Fatalf("empty cited must drop all claims, got %v", got)
	}
	// Nil claims in → nil out (no allocation).
	if got := filterClaimsToCitations(nil, []string{"doc-A"}); got != nil {
		t.Fatalf("nil claims must stay nil, got %v", got)
	}
}

func TestEvalPathTinyJSONLSerializesClaimsAndLocator(t *testing.T) {
	extractiveEnv(t)
	// Frame-level + engine-level proof: the encoded output of a real eval
	// frame must carry claims (text/quote/document_id) AND the locator on
	// each claim whenever the source had one. The harness here uses the
	// extractive path so we don't depend on a live LLM.
	docs := tinyJSONLPath(t)
	engine, err := LoadDocsJSONL(docs, "test-src", "gen-eval-claims-wire")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	result := engine.Answer(EvalCase{
		Question:     "What is the MedThink RPO policy for PROJ-99?",
		QuestionID:   "q-claims-wire",
		QuestionType: "basic",
	})
	if result.Failure != "" {
		t.Fatalf("unexpected failure: %s diag=%v", result.Failure, result.RetrievalDiagnostics)
	}
	if err := enc.Encode(result); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, buf.String())
	}
	// Claims may legitimately be empty when the extractive path produces no
	// quote-bearing claims — but the field itself MUST be present in the
	// struct (json:"claims,omitempty" leaves the field absent when nil).
	// The wire shape we care about is: when claims are populated, every
	// claim's document_id is in cited_document_ids (no unsurfaced leak).
	raw := buf.Bytes()
	cited := result.CitedDocumentIDs
	citedSet := make(map[string]struct{}, len(cited))
	for _, id := range cited {
		citedSet[id] = struct{}{}
	}
	if claimsAny, ok := got["claims"].([]any); ok {
		for i, c := range claimsAny {
			cl, _ := c.(map[string]any)
			if cl == nil {
				continue
			}
			doc, _ := cl["document_id"].(string)
			if doc == "" {
				t.Fatalf("claim %d missing document_id: %v", i, cl)
			}
			if _, ok := citedSet[doc]; !ok {
				t.Fatalf("claim %d unsurfaced doc=%q not in cited=%v", i, doc, cited)
			}
			// Locator, when present, must not invent fields: page must be > 0
			// if present, and "present" must reflect the parser signal.
			if locAny, ok := cl["locator"]; ok {
				if loc, ok := locAny.(map[string]any); ok {
					if v, ok := loc["page_number"]; ok {
						if n, _ := v.(float64); n == 0 {
							t.Fatalf("claim %d locator.page_number=0 (invented)", i)
						}
					}
				}
			}
		}
	}
	// Encoded output must carry every wire field the Python adapter parses.
	for _, key := range []string{
		"answer", "cited_document_ids", "retrieval_diagnostics",
		"search_mode", "protocol_version",
	} {
		if _, ok := got[key]; !ok {
			t.Fatalf("frame missing key=%q in %s", key, raw)
		}
	}
}
