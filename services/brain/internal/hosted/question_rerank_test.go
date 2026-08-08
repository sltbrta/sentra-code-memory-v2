package hosted

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestQuestionAwareRerankConstrainedPrefersAllPassageQualifiers(t *testing.T) {
	question := "For the Atlas product in the gold tier, eu-west-1 region, production environment, what is the RPO?"
	pool := []Passage{
		{
			DocumentID: "near-miss",
			Text:       "Atlas failover has a 30 minute RPO.",
			Score:      0.99,
			SourceURI:  "confluence://runbook?product=atlas&tier=gold&region=us-east-1&environment=production",
			Channel:    "dense",
		},
		{
			DocumentID: "all-qualifiers",
			Text:       "Atlas failover has a 15 minute RPO.",
			Score:      0.20,
			SourceURI:  "confluence://runbook?product=atlas&tier=gold&region=eu-west-1&environment=production",
			Channel:    "hotlex",
		},
	}

	out, diag := questionAwareRerank(pool, question, "constrained", nil)
	if out[0].DocumentID != "all-qualifiers" {
		t.Fatalf("all-qualifier passage must outrank lexical near miss: ids=%v diag=%v", docIDs(out), diag)
	}
	if len(out) != len(pool) || !strings.Contains(out[0].Channel, "hotlex") || out[1].Channel != "dense" {
		t.Fatalf("rerank must preserve raw lexical/dense candidates and metadata: out=%+v", out)
	}
	if diag["question_aware_rerank"] != "ok" || diag["qualifier_count"] != 4 {
		t.Fatalf("missing qualifier rerank diagnostics: %v", diag)
	}
	if final := shrinkWindowKeepProtected(out, question, 1, nil); len(final) != 1 || final[0].DocumentID != "all-qualifiers" {
		t.Fatalf("qualifier promotion must survive progressive shrink: %+v", final)
	}
	for key := range diag {
		if strings.Contains(strings.ToLower(key), "gold") || strings.Contains(strings.ToLower(key), "expected") {
			t.Fatalf("blind runtime diagnostics must not depend on gold: %q", key)
		}
	}
}

func TestQuestionAwareRerankIsBoundedAndPreservesCandidateFloor(t *testing.T) {
	pool := make([]Passage, questionAwareRerankLimit+2)
	for i := range pool {
		pool[i] = Passage{
			DocumentID: "candidate-" + string(rune('A'+i)),
			Text:       "Atlas gold tier us-east-1 region production environment.",
			Channel:    "dense",
		}
	}
	pool[1].Text = "Atlas gold tier eu-west-1 region production environment."
	pool[questionAwareRerankLimit].Text = "Atlas gold tier eu-west-1 region production environment."
	tailID := pool[questionAwareRerankLimit].DocumentID

	out, diag := questionAwareRerank(
		pool,
		"For the Atlas product in the gold tier, eu-west-1 region, production environment, what is the RPO?",
		"constrained",
		nil,
	)
	if out[0].DocumentID != pool[1].DocumentID {
		t.Fatalf("matching candidate inside bound must lead: %v", docIDs(out[:3]))
	}
	if out[questionAwareRerankLimit].DocumentID != tailID {
		t.Fatalf("tail outside bound moved: got=%q want=%q", out[questionAwareRerankLimit].DocumentID, tailID)
	}
	if len(out) != len(pool) || diag["question_aware_n"] != questionAwareRerankLimit {
		t.Fatalf("candidate floor or bound changed: len=%d diag=%v", len(out), diag)
	}
}

func TestQuestionAwareRerankConflictingKeepsSupersedingAndSupersededSides(t *testing.T) {
	shared := "Redwood retention policy customer records archive deletion schedule compliance handbook shared policy wording and controls"
	pool := []Passage{
		{DocumentID: "old", Text: "Earlier note effective 2026-04-01 retained records for 30 days. " + shared, Score: 0.98, Channel: "slack"},
		{DocumentID: "noise", Text: "Unrelated dated launch plan 2026-08-01.", Score: 0.90, Channel: "dense"},
		{DocumentID: "current", Text: "Updated policy supersedes the earlier note. Effective 2026-06-15 records are retained for 90 days. " + shared, Score: 0.15, Channel: "confluence"},
	}

	out, diag := questionAwareRerank(pool, "What is the correct retention period?", "conflicting_info", nil)
	if got := docIDs(out[:2]); got[0] != "current" || got[1] != "old" {
		t.Fatalf("want superseding and superseded evidence first, got %v diag=%v", got, diag)
	}
	if diag["conflict_dual_side_groups"] != 1 {
		t.Fatalf("want explicit dual-side diagnostic, got %v", diag)
	}
}

func TestQuestionAwareRerankConflictingHonorsValidityWindow(t *testing.T) {
	shared := "Atlas access retention policy enterprise tenant records compliance handbook controls archive deletion shared wording"
	pool := []Passage{
		{DocumentID: "future", Text: "Updated policy supersedes the old rule. Effective 2026-07-01 records remain 90 days. " + shared, Channel: "confluence"},
		{DocumentID: "then-current", Text: "Effective 2026-01-01 through 2026-06-30 records remain 30 days. " + shared, Channel: "confluence"},
	}

	out, diag := questionAwareRerank(pool, "As of 2026-05-15, what was the correct retention period?", "conflicting_info", nil)
	if out[0].DocumentID != "then-current" {
		t.Fatalf("policy valid at question time must outrank future supersession: %v diag=%v", docIDs(out), diag)
	}
	if diag["validity_anchor"] != "2026-05-15" || diag["validity_top"] != 1 {
		t.Fatalf("missing validity-window diagnostics: %v", diag)
	}
	out, _ = applyAuthorityRecencyQ(out, "As of 2026-05-15, what was the correct retention period?", "conflicting_info", nil)
	out, _ = adjudicateSupersession(out, "As of 2026-05-15, what was the correct retention period?")
	if out[0].DocumentID != "then-current" || !strings.Contains(out[0].Text, "[APPLICABLE") {
		t.Fatalf("authority/supersession helpers must preserve query-time validity: %+v", out)
	}
}

func TestQuestionAwareRerankIntraDocumentUsesTemporalRelation(t *testing.T) {
	pool := []Passage{
		{DocumentID: "inc-42", ChunkID: "before", Text: "INC-42 on 2026-05-20: operators enabled retries.", Score: 0.95, Channel: "hotlex"},
		{DocumentID: "other", ChunkID: "other", Text: "INC-99 on 2026-06-03: operators rolled back.", Score: 0.90, Channel: "dense"},
		{DocumentID: "inc-42", ChunkID: "after", Text: "INC-42 on 2026-06-02: operators rolled back retries.", Score: 0.30, Channel: "temporal_relation"},
	}

	out, diag := questionAwareRerank(pool, "Within INC-42, what happened after 2026-06-01?", "intra_document_reasoning", nil)
	if out[0].ChunkID != "after" {
		t.Fatalf("matching within-document event after anchor must lead: out=%+v diag=%v", out, diag)
	}
	if diag["temporal_relation"] != "after" || diag["temporal_anchor"] != "2026-06-01" {
		t.Fatalf("missing temporal diagnostics: %v", diag)
	}
}

func TestQuestionAwareRerankIntraDocumentKeepsSupportingChunksTogether(t *testing.T) {
	pool := []Passage{
		{DocumentID: "runbook-a", ChunkID: "primary", Text: "The failover checklist starts by draining traffic.", Score: 0.9},
		{DocumentID: "runbook-b", ChunkID: "noise", Text: "A different checklist verifies backups.", Score: 0.8},
		{DocumentID: "runbook-a", ChunkID: "sibling", Text: "The same failover checklist then promotes the replica.", Score: 0.3},
	}

	out, diag := questionAwareRerank(pool, "How does the failover checklist reach replica promotion?", "intra_document_reasoning", nil)
	if out[0].DocumentID != "runbook-a" || out[1].DocumentID != "runbook-a" {
		t.Fatalf("intra-document evidence must remain together: %v diag=%v", docIDs(out), diag)
	}
	if diag["temporal_relation"] != "document_sequence" {
		t.Fatalf("want document-sequence diagnostic: %v", diag)
	}
}

func TestRemoteRerankBackendsPreserveUnscoredCandidateFloor(t *testing.T) {
	pool := []Passage{
		{DocumentID: "lexical-head", Text: "alpha", Channel: "hotlex"},
		{DocumentID: "dense-winner", Text: "beta", Channel: "dense"},
		{DocumentID: "raw-tail", Text: "gamma", Channel: "hotlex"},
	}
	for _, backend := range []string{"cohere", "zeroentropy"} {
		t.Run(backend, func(t *testing.T) {
			out, err := assembleRemoteRerank(pool, []remoteRerankResult{{Index: 1, RelevanceScore: 0.99}}, backend)
			if err != nil {
				t.Fatal(err)
			}
			if got := docIDs(out); len(got) != 3 || got[0] != "dense-winner" || got[1] != "lexical-head" || got[2] != "raw-tail" {
				t.Fatalf("provider %s dropped or changed raw floor: %v", backend, got)
			}
		})
	}
}

func TestMLXRerankEmptyPassagesReturnsEmpty(t *testing.T) {
	passages := []Passage{}
	out, err := mlxRerank(context.Background(), "question", passages, 10)
	if err != nil {
		t.Fatalf("empty MLX rerank returned error: %v", err)
	}
	if !reflect.DeepEqual(out, passages) {
		t.Fatalf("empty MLX rerank output=%#v want %#v", out, passages)
	}
}

func TestRemoteRerankMalformedResponseFallsBackWithoutPrivilegeBroadening(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
			{"index": 0, "relevance_score": 0.9},
			{"index": 0, "relevance_score": 0.8},
			{"index": 99, "relevance_score": 1.0},
		}})
	}))
	defer server.Close()

	t.Setenv("OUROBOROS_ERB_RERANK", "1")
	t.Setenv("OUROBOROS_BRAIN_RANKER", "hosted")
	t.Setenv("COHERE_API_KEY", "")
	t.Setenv("CO_API_KEY", "")
	t.Setenv("SENTRA_COHERE_API_KEY", "")
	t.Setenv("ZEROENTROPY_API_KEY", "test")
	t.Setenv("SENTRA_ZEROENTROPY_API_KEY", "")
	t.Setenv("ZE_API_KEY", "")
	t.Setenv("OUROBOROS_ERB_ZE_BASE", server.URL)

	allowed := []Passage{
		{DocumentID: "allowed-a", Text: "RPO policy alpha", Channel: "hotlex"},
		{DocumentID: "allowed-b", Text: "RPO policy beta", Channel: "dense"},
	}
	out, diag := crossEncodeRerank(context.Background(), "RPO policy", allowed, 2)
	if diag["rerank_backend"] != "lexical" || diag["rerank_fallback"] != true {
		t.Fatalf("malformed remote response must fail closed to lexical with diagnostics: %v", diag)
	}
	if got := diag["rerank_fallback_reasons"]; !reflect.DeepEqual(got, []string{"invalid_response"}) {
		t.Fatalf("want sanitized provider failure diagnostic: %v", diag)
	}
	if latency, ok := diag["rerank_fallback_latency_ms"].(int64); !ok || latency < 0 || latency > maxRerankLatencyDiag.Milliseconds() {
		t.Fatalf("want bounded fallback latency diagnostic: %v", diag)
	}
	if _, leaked := diag["rerank_ze_error"]; leaked {
		t.Fatalf("raw provider error must not enter diagnostics: %v", diag)
	}
	if len(out) != len(allowed) {
		t.Fatalf("fallback changed authorized candidate floor: got=%d want=%d", len(out), len(allowed))
	}
	got := map[string]int{}
	for _, p := range out {
		got[p.DocumentID]++
	}
	if got["allowed-a"] != 1 || got["allowed-b"] != 1 || len(got) != 2 {
		t.Fatalf("reranker broadened or duplicated authorized candidates: %v", got)
	}
}
