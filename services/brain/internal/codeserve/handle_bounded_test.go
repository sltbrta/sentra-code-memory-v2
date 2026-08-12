package codeserve_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

// setupBoundedFixture indexes a small two-file tree and returns root/cache.
func setupBoundedFixture(t *testing.T, ctx context.Context) (string, string) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "package pkg\n\n// Alpha is the seed symbol.\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc helperA() int {\n\treturn Alpha()\n}\n"
	if err := os.WriteFile(filepath.Join(src, "a.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "b.go"), []byte(strings.ReplaceAll(body, "Alpha", "AlphaRef")), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "idx")
	resp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_index", "root": src, "index_cache": cache, "workers": 2,
	})
	if resp["ok"] != true {
		t.Fatalf("index: %v", resp)
	}
	return src, cache
}

// TestFindRelevantDefaultUnchanged: without bounded fields the response shape
// is exactly the legacy payload (no context key).
func TestFindRelevantDefaultUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src, cache := setupBoundedFixture(t, ctx)
	resp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_find_relevant", "root": src, "index_cache": cache,
		"q": "Alpha", "no_refresh": true,
	})
	if resp["ok"] != true {
		t.Fatalf("%v", resp)
	}
	if _, ok := resp["context"]; ok {
		t.Fatalf("context must be opt-in: %v", resp)
	}
	if resp["payload"] == nil {
		t.Fatalf("legacy payload missing: %v", resp)
	}
}

// TestFindRelevantBoundedBudget: max_bytes binds and metadata accounts for it.
func TestFindRelevantBoundedBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src, cache := setupBoundedFixture(t, ctx)
	req := codeserve.Request{
		"verb": "code_find_relevant", "root": src, "index_cache": cache,
		"q": "Alpha", "no_refresh": true, "max_bytes": 120, "render": "full",
	}
	resp := codeserve.Handle(ctx, req)
	if resp["ok"] != true {
		t.Fatalf("%v", resp)
	}
	if resp["payload"] == nil {
		t.Fatalf("legacy payload must still be present: %v", resp)
	}
	var out codeserve.FindRelevantResponse
	if err := codeserve.DecodeResponse(resp, &out); err != nil {
		t.Fatal(err)
	}
	if out.Context == nil {
		t.Fatalf("bounded request must return packed context: %v", resp)
	}
	meta := out.Context.Meta
	if meta.BudgetBytes != 120 {
		t.Fatalf("budget wrong: %+v", meta)
	}
	if meta.UsedBytes > 120 {
		t.Fatalf("hard budget exceeded: %+v", meta)
	}
	if meta.Governor == nil || meta.Governor.Limits.MaxOutputBytes != 120 {
		t.Fatalf("governor limits must be visible: %+v", meta.Governor)
	}
	for _, it := range out.Context.Items {
		if it.Handle == "" {
			t.Fatalf("item missing expansion handle: %+v", it)
		}
	}
	for _, om := range out.Context.Omitted {
		if om.Handle == "" || om.Reason == "" {
			t.Fatalf("silent omission: %+v", om)
		}
	}

	// Determinism: identical bounded requests without a session registry
	// produce byte-identical context blocks.
	again := codeserve.Handle(ctx, req)
	a, _ := json.Marshal(resp["context"])
	b, _ := json.Marshal(again["context"])
	if string(a) != string(b) {
		t.Fatalf("non-deterministic context:\n%s\n%s", a, b)
	}
}

// TestFindRelevantRenderModes: render=signatures strips bodies from the
// packed context while leaving the legacy payload untouched.
func TestFindRelevantRenderModes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src, cache := setupBoundedFixture(t, ctx)
	resp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_find_relevant", "root": src, "index_cache": cache,
		"q": "Alpha", "no_refresh": true, "render": "signatures",
	})
	var out codeserve.FindRelevantResponse
	if err := codeserve.DecodeResponse(resp, &out); err != nil {
		t.Fatal(err)
	}
	if out.Context == nil || out.Context.Meta.Render != "signatures" {
		t.Fatalf("render mode not applied: %+v", out.Context)
	}
	for _, it := range out.Context.Items {
		if strings.Contains(it.Content, "return 1") {
			t.Fatalf("signatures leaked body: %+v", it)
		}
		if it.Render != "signatures" {
			t.Fatalf("item render label wrong: %+v", it)
		}
	}
	// Invalid render fails with a stable error code.
	bad := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_find_relevant", "root": src, "index_cache": cache,
		"q": "Alpha", "no_refresh": true, "render": "bogus",
	})
	var errOut codeserve.ErrorResponse
	if err := codeserve.DecodeResponse(bad, &errOut); err != nil {
		t.Fatal(err)
	}
	if errOut.OK || errOut.ErrorCode != codeserve.ErrInvalidRequest {
		t.Fatalf("invalid render must fail invalid_request: %+v", errOut)
	}
}

// TestFindRelevantSessionDedup: a repeated bounded call in the same session
// emits back-pointers instead of duplicate source bytes.
func TestFindRelevantSessionDedup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src, cache := setupBoundedFixture(t, ctx)
	call := func() codeserve.FindRelevantResponse {
		resp := codeserve.Handle(ctx, codeserve.Request{
			"verb": "code_find_relevant", "root": src, "index_cache": cache,
			"q": "Alpha", "no_refresh": true, "render": "full", "session": "s1",
		})
		var out codeserve.FindRelevantResponse
		if err := codeserve.DecodeResponse(resp, &out); err != nil {
			t.Fatal(err)
		}
		if out.Context == nil {
			t.Fatalf("missing context: %v", resp)
		}
		return out
	}
	first := call()
	second := call()
	if first.Context.Meta.Deduped != 0 {
		t.Fatalf("first call must not dedup: %+v", first.Context.Meta)
	}
	if second.Context.Meta.Deduped == 0 {
		t.Fatalf("second call in session must dedup: %+v", second.Context.Meta)
	}
	for _, it := range second.Context.Items {
		if !it.Dedup {
			continue
		}
		if it.Content != "" || it.DedupRef == "" || it.Bytes != 0 {
			t.Fatalf("deduped item must be a pure back-pointer: %+v", it)
		}
	}
	// A different session emits full bytes again.
	resp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_find_relevant", "root": src, "index_cache": cache,
		"q": "Alpha", "no_refresh": true, "render": "full", "session": "s2",
	})
	var other codeserve.FindRelevantResponse
	if err := codeserve.DecodeResponse(resp, &other); err != nil {
		t.Fatal(err)
	}
	if other.Context.Meta.Deduped != 0 {
		t.Fatalf("fresh session must not dedup: %+v", other.Context.Meta)
	}
}
