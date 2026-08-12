package codeserve_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/savings"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/sessionlog"
)

// --- Typed agent-memory operators (issue #47) -----------------------------

func TestMemoryPutSearchListPromote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()

	// Put (admit) a typed entry; principal is the policy gate.
	putResp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "memory_put", "dir": dir, "principal": "alice",
		"kind": "fact", "tier": "stm", "text": "the build uses bazel",
		"tags": []any{"build", "ci"},
	})
	var put codeserve.MemoryPutResponse
	if err := codeserve.DecodeResponse(putResp, &put); err != nil {
		t.Fatal(err)
	}
	if !put.OK || put.Entry.ID == "" {
		t.Fatalf("put: %+v", put)
	}
	if put.Entry.Tier != "stm" || put.Entry.Principal != "alice" {
		t.Fatalf("entry fields wrong: %+v", put.Entry)
	}
	if len(put.Entry.Tags) != 2 {
		t.Fatalf("tags not admitted: %+v", put.Entry)
	}
	id := put.Entry.ID

	// Put a second entry in a higher tier to exercise tier ordering.
	put2 := codeserve.Handle(ctx, codeserve.Request{
		"verb": "memory_put", "dir": dir, "principal": "alice",
		"kind": "preference", "tier": "ltm", "text": "prefer tabs over spaces",
	})
	if put2["ok"] != true {
		t.Fatalf("put2: %v", put2)
	}

	// Search (typed recall) matches on substring.
	searchResp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "memory_search", "dir": dir, "principal": "alice", "q": "bazel",
	})
	var search codeserve.MemoryEntriesResponse
	if err := codeserve.DecodeResponse(searchResp, &search); err != nil {
		t.Fatal(err)
	}
	if !search.OK || len(search.Entries) != 1 {
		t.Fatalf("search hits=%d want 1: %+v", len(search.Entries), search)
	}
	if search.Entries[0].ID != id {
		t.Fatalf("search returned wrong entry: %+v", search.Entries[0])
	}

	// List returns both entries (tier-ordered).
	listResp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "memory_list", "dir": dir, "principal": "alice",
	})
	var list codeserve.MemoryEntriesResponse
	if err := codeserve.DecodeResponse(listResp, &list); err != nil {
		t.Fatal(err)
	}
	if !list.OK || list.Count != 2 {
		t.Fatalf("list count=%d want 2: %+v", list.Count, list)
	}

	// Principal isolation: bob sees nothing.
	bobResp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "memory_list", "dir": dir, "principal": "bob",
	})
	var bob codeserve.MemoryEntriesResponse
	if err := codeserve.DecodeResponse(bobResp, &bob); err != nil {
		t.Fatal(err)
	}
	if bob.Count != 0 {
		t.Fatalf("bob should see 0 entries, got %+v", bob)
	}

	// Promote stm -> ltm.
	promoteResp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "memory_promote", "dir": dir, "id": id, "tier": "ltm",
	})
	var promote codeserve.MemoryPromoteResponse
	if err := codeserve.DecodeResponse(promoteResp, &promote); err != nil {
		t.Fatal(err)
	}
	if !promote.OK || promote.Tier != "ltm" {
		t.Fatalf("promote: %+v", promote)
	}

	// Missing principal is rejected with a stable error code.
	bad := codeserve.Handle(ctx, codeserve.Request{
		"verb": "memory_put", "dir": dir, "text": "no principal",
	})
	var badErr codeserve.ErrorResponse
	if err := codeserve.DecodeResponse(bad, &badErr); err != nil {
		t.Fatal(err)
	}
	if badErr.OK || badErr.ErrorCode != codeserve.ErrInvalidRequest {
		t.Fatalf("expected invalid_request, got: %+v", badErr)
	}
}

func TestMemoryVerbsRequireDir(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, verb := range []string{"memory_put", "memory_search", "memory_list"} {
		resp := codeserve.Handle(ctx, codeserve.Request{"verb": verb, "principal": "alice"})
		if resp["ok"] != false {
			t.Fatalf("%s without dir must fail: %v", verb, resp)
		}
	}
}

// --- Session continuation composite (issue #47) ---------------------------

func seedSessionLog(t *testing.T, dir string) {
	t.Helper()
	w, err := sessionlog.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	events := []sessionlog.Event{
		{Kind: sessionlog.KindTaskStart, Session: "task-1", FreeText: "implement auth"},
		{Kind: sessionlog.KindRead, Session: "task-1", Verb: "code_read",
			Provenance: sessionlog.Provenance{Repository: "local", Tree: "abc123", Path: "a/b.go"},
			Freshness:  sessionlog.FreshnessAsOf},
		{Kind: sessionlog.KindEdit, Session: "task-1", Verb: "code_edit",
			Provenance: sessionlog.Provenance{Repository: "local", Tree: "abc123", Path: "a/b.go", Symbol: "Alpha"},
			Freshness:  sessionlog.FreshnessAsOf},
		{Kind: sessionlog.KindFailure, Session: "task-1", FreeText: "test red: missing token"},
	}
	for _, ev := range events {
		if _, err := w.Append(ev); err != nil {
			t.Fatalf("append %s: %v", ev.Kind, err)
		}
	}
}

func TestSessionContinuationBuildsPacket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	seedSessionLog(t, dir)

	// Fixed now keeps the packet deterministic across calls.
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	req := codeserve.Request{
		"verb": "session_continuation", "dir": dir,
		"repository": "local", "tree": "abc123", "now": now,
	}
	var out codeserve.SessionContinuationResponse
	if err := codeserve.DecodeResponse(codeserve.Handle(ctx, req), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.SourceEvents != 4 {
		t.Fatalf("continuation: %+v", out)
	}
	if out.Continuation.Schema == "" {
		t.Fatalf("continuation missing schema: %+v", out.Continuation)
	}
	// The seeded edit/read pointers must surface as read ranges.
	if len(out.Continuation.ReadRanges) == 0 {
		t.Fatalf("expected read ranges in continuation: %+v", out.Continuation)
	}

	// Determinism: identical inputs produce byte-identical packets.
	a := string(mustJSON(t, out.Continuation))
	var again codeserve.SessionContinuationResponse
	if err := codeserve.DecodeResponse(codeserve.Handle(ctx, req), &again); err != nil {
		t.Fatal(err)
	}
	b := string(mustJSON(t, again.Continuation))
	if a != b {
		t.Fatalf("non-deterministic continuation:\n%s\n%s", a, b)
	}
}

func TestSessionContinuationEmptyDir(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir() // no session log yet
	var out codeserve.SessionContinuationResponse
	if err := codeserve.DecodeResponse(codeserve.Handle(ctx, codeserve.Request{
		"verb": "session_continuation", "dir": dir,
	}), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.SourceEvents != 0 {
		t.Fatalf("empty continuation should succeed with 0 events: %+v", out)
	}
}

func TestSessionContinuationRejectsBadNow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	resp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "session_continuation", "dir": dir, "now": "not-a-time",
	})
	var out codeserve.ErrorResponse
	if err := codeserve.DecodeResponse(resp, &out); err != nil {
		t.Fatal(err)
	}
	if out.OK || out.ErrorCode != codeserve.ErrInvalidRequest {
		t.Fatalf("expected invalid_request for bad now: %+v", out)
	}
}

// --- Savings summary (issue #47) ------------------------------------------

func TestSavingsSummary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()

	// Empty ledger reads as a zero summary, not an error.
	var empty codeserve.SavingsSummaryResponse
	if err := codeserve.DecodeResponse(codeserve.Handle(ctx, codeserve.Request{
		"verb": "savings_summary", "dir": dir,
	}), &empty); err != nil {
		t.Fatal(err)
	}
	if !empty.OK || empty.Summary.Steps != 0 {
		t.Fatalf("empty savings: %+v", empty)
	}

	// Seed the ledger through the savings package, then read it back.
	ledger, err := savings.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Record(savings.Step{
		Name: "find-relevant", Category: savings.CategoryRetrieval,
		BaselineBytes: 1200, ServedBytes: 240, BaselineTokens: 300, ServedTokens: 60,
	}); err != nil {
		t.Fatal(err)
	}

	var out codeserve.SavingsSummaryResponse
	if err := codeserve.DecodeResponse(codeserve.Handle(ctx, codeserve.Request{
		"verb": "savings_summary", "dir": dir,
	}), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatalf("savings: %+v", out)
	}
	if out.Summary.Totals.SavedTokens != 240 {
		t.Fatalf("saved_tokens=%d want 240: %+v", out.Summary.Totals.SavedTokens, out.Summary)
	}
	if !strings.Contains(out.Text, "saved_tokens=240") {
		t.Fatalf("text summary missing tokens: %q", out.Text)
	}
}

// --- Deferred / non-goal disclosures (issue #47) ---------------------------

func TestDeferredVerbsReturnDisclosure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	deferred := []string{
		"lifecycle_install", "session_product", "code_dense_rerank",
		"hosted_tenancy", "query_advanced",
	}
	for _, verb := range deferred {
		resp := codeserve.Handle(ctx, codeserve.Request{"verb": verb})
		var out codeserve.DeferredResponse
		if err := codeserve.DecodeResponse(resp, &out); err != nil {
			t.Fatal(err)
		}
		if out.OK {
			t.Fatalf("%s must not succeed: %+v", verb, out)
		}
		if out.ErrorCode != codeserve.ErrDeferred {
			t.Fatalf("%s error_code=%v want deferred: %+v", verb, out.ErrorCode, out)
		}
		if !out.Deferred || out.Decision == "" || out.Reason == "" {
			t.Fatalf("%s missing disclosure fields: %+v", verb, out)
		}
	}

	// hosted_tenancy is an explicit non-goal, the rest are deferred.
	var ng codeserve.DeferredResponse
	if err := codeserve.DecodeResponse(codeserve.Handle(ctx, codeserve.Request{
		"verb": "hosted_tenancy",
	}), &ng); err != nil {
		t.Fatal(err)
	}
	if ng.Decision != "non_goal" {
		t.Fatalf("hosted_tenancy decision=%v want non_goal", ng.Decision)
	}
}

func TestDeferredVerbsAreCataloguedButNotMCPStable(t *testing.T) {
	t.Parallel()
	// Deferred verbs are discoverable through the catalog …
	found := map[string]bool{}
	for _, v := range codeserve.Catalog() {
		found[v] = true
	}
	for _, verb := range []string{"lifecycle_install", "session_product", "code_dense_rerank", "hosted_tenancy", "query_advanced"} {
		if !found[verb] {
			t.Fatalf("deferred verb %s missing from catalog", verb)
		}
	}
	// … but are not advertised as stable/callable in the typed metadata.
	for _, spec := range codeserve.CatalogMetadata() {
		switch spec.Name {
		case "lifecycle_install", "session_product", "code_dense_rerank", "hosted_tenancy", "query_advanced":
			if spec.Status != codeserve.StatusDeferred {
				t.Fatalf("%s status=%s want deferred", spec.Name, spec.Status)
			}
		case "memory_put", "memory_search", "memory_list", "memory_promote",
			"session_continuation", "savings_summary":
			if spec.Status != codeserve.StatusStable {
				t.Fatalf("%s status=%s want stable", spec.Name, spec.Status)
			}
		}
	}
}
