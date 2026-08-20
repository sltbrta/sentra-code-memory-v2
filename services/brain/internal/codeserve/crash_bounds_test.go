package codeserve_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/testsupport"
)

// codeserve.Handle is documented "Never panics; always returns a map", and the
// JSONL and MCP surfaces depend on that: neither has a recover, so a panic in
// any of ~25 handlers discards every pipelined request and kills the process.
// The claim was aspirational -- there was no recover anywhere on the dispatch
// path, and `top_k` reached make() unclamped on four verbs.

// TestHandleDoesNotPanicOnHostileTopK is the regression test for the proven
// one-line process kill: {"verb":"code_defs","top_k":1e15}.
func TestHandleDoesNotPanicOnHostileTopK(t *testing.T) {
	root := testsupport.WorkTree(t, map[string]string{
		"a.go": "package a\n\nfunc Token() {}\n",
	})
	ctx := context.Background()

	// code_search clamps; these four did not.
	for _, verb := range []string{"code_exact", "code_defs", "code_refs", "code_imports"} {
		for _, topK := range []any{
			1e15,                   // the proven kill
			float64(math.MaxInt64), // the largest value JSON can carry
			math.MaxInt32,
			-1,
			0,
		} {
			t.Run(verb, func(t *testing.T) {
				resp := codeserve.Handle(ctx, codeserve.Request{
					"verb": verb, "root": root, "q": "Token", "top_k": topK,
				})
				if resp == nil {
					t.Fatalf("%s top_k=%v returned nil", verb, topK)
				}
				if _, ok := resp["ok"]; !ok {
					t.Fatalf("%s top_k=%v returned a map with no ok field: %v", verb, topK, resp)
				}
			})
		}
	}
}

// TestHandleReturnsAStructuredErrorForAnOutOfRangeTopK: not panicking is the
// floor. A caller that asks for an impossible page size should be told so.
func TestHandleReturnsAStructuredErrorForAnOutOfRangeTopK(t *testing.T) {
	root := testsupport.WorkTree(t, map[string]string{"a.go": "package a\n"})
	resp := codeserve.Handle(context.Background(), codeserve.Request{
		"verb": "code_defs", "root": root, "q": "Token", "top_k": 1e15,
	})
	if resp["ok"] != false {
		t.Fatalf("ok = %v, want false for an out-of-range top_k: %v", resp["ok"], resp)
	}
	if code, _ := resp["error_code"].(string); code != string(codeserve.ErrInvalidRequest) {
		t.Fatalf("error_code = %q, want %q", code, codeserve.ErrInvalidRequest)
	}
}

// TestHandleRecoversFromAHandlerPanic pins the boundary itself rather than one
// input that reaches it. Without a recover, any future defect in any handler
// is a process kill on the two stdio surfaces.
func TestHandleRecoversFromAHandlerPanic(t *testing.T) {
	// code_read on a root whose path resolution is degenerate previously had
	// several nil-map paths; rather than depend on one, drive the documented
	// contract: every verb, with a request shaped to be maximally hostile,
	// must return a map.
	hostile := []codeserve.Request{
		{"verb": "code_read", "root": "", "path": ""},
		{"verb": "code_read", "root": "\x00", "path": "\x00"},
		{"verb": "code_find_relevant", "root": "", "q": "", "top_k": -99},
		{"verb": "code_expand", "root": "", "seed": ""},
		{"verb": "code_impact", "root": "", "seed": "", "max_depth": -1, "max_files": -1},
		{"verb": "code_repo_map", "root": "", "q": "", "max_bytes": -1},
		{"verb": "code_structural_search", "root": "", "pattern": "", "max_matches": -1},
		{"verb": "memory_put", "dir": "", "text": ""},
		{"verb": "session_continuation", "dir": ""},
	}
	for _, req := range hostile {
		verb, _ := req["verb"].(string)
		t.Run(verb, func(t *testing.T) {
			resp := codeserve.Handle(context.Background(), req)
			if resp == nil {
				t.Fatalf("%v returned nil", req)
			}
			if _, ok := resp["ok"]; !ok {
				t.Fatalf("%v returned a map with no ok field: %v", req, resp)
			}
		})
	}
}

// TestCodeWatchRejectsAnAbsurdTimeoutInsteadOfClampingUp: the handler clamped a
// caller-supplied timeout_ms *up* to the 24h maximum, so one request could hold
// the single-threaded serve loop for a day.
func TestCodeWatchRejectsAnAbsurdTimeoutInsteadOfClampingUp(t *testing.T) {
	root := testsupport.WorkTree(t, map[string]string{"a.go": "package a\n"})
	resp := codeserve.Handle(context.Background(), codeserve.Request{
		"verb": "code_watch", "root": root, "timeout_ms": 999999999999,
	})
	if resp["ok"] != false {
		t.Fatalf("an out-of-range timeout_ms must be refused, got %v", resp)
	}
	if code, _ := resp["error_code"].(string); code != string(codeserve.ErrInvalidRequest) {
		t.Fatalf("error_code = %q, want %q (%v)", code, codeserve.ErrInvalidRequest, resp)
	}
}

// TestCodeWatchStillAcceptsAReasonableTimeout keeps the bound from becoming a
// wall.
func TestCodeWatchStillAcceptsAReasonableTimeout(t *testing.T) {
	root := testsupport.WorkTree(t, map[string]string{"a.go": "package a\n"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_watch", "root": root, "timeout_ms": 200, "max_cycles": 1,
	})
	if code, _ := resp["error_code"].(string); code == string(codeserve.ErrInvalidRequest) {
		t.Fatalf("a 200ms watch must be admitted: %v", resp)
	}
}
