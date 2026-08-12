package codeserve_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codecrawl"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

// readReq issues a code_read and returns the response.
func readReq(root, path string, extra map[string]any) codeserve.Response {
	req := codeserve.Request{"verb": "code_read", "root": root, "path": path}
	for k, v := range extra {
		req[k] = v
	}
	return codeserve.Handle(context.Background(), req)
}

func mustReadOK(t *testing.T, resp codeserve.Response) {
	t.Helper()
	if resp["ok"] != true {
		t.Fatalf("read denied unexpectedly: %+v", resp)
	}
}

func mustReadDenied(t *testing.T, resp codeserve.Response, code codeserve.ErrorCode) {
	t.Helper()
	if resp["ok"] != false || resp["error_code"] != string(code) {
		t.Fatalf("want %s denial, got %+v", code, resp)
	}
}

// indexRoot builds the durable index for root at its default location.
func indexRoot(t *testing.T, root string) string {
	t.Helper()
	gobPath := codecrawl.DefaultIndexPath(root)
	if _, _, _, _, err := codecrawl.OpenOrRefresh(root, gobPath, 2, false); err != nil {
		t.Fatalf("index root: %v", err)
	}
	return gobPath
}

func TestCodeReadDeniedForIgnoredFilesByDefault(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("secret.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret.md"), []byte("hidden\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Default-deny under the built-in secret exclusions and the repo's own
	// .gitignore alike, with the machine-readable path_denied code.
	mustReadDenied(t, readReq(root, ".env", nil), codeserve.ErrPathDenied)
	mustReadDenied(t, readReq(root, "secret.md", nil), codeserve.ErrPathDenied)

	// Explicit typed opt-in reads the ignored file.
	mustReadOK(t, readReq(root, "secret.md", map[string]any{"allow_ignored": true}))
}

func TestCodeReadGatedByIndexMembership(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\nfunc GateAlpha() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// notes.txt is a regular file inside the root but not in the indexed
	// extension corpus, so it is never an index member.
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	indexRoot(t, root)

	// Index member reads normally.
	mustReadOK(t, readReq(root, "a.go", nil))

	// Non-member is denied by default once an index exists…
	mustReadDenied(t, readReq(root, "notes.txt", nil), codeserve.ErrPathDenied)
	// …and the explicit opt-in restores the legacy read.
	mustReadOK(t, readReq(root, "notes.txt", map[string]any{"allow_unindexed": true}))

	// Opt-ins never bypass the path/symlink safety checks.
	mustReadDenied(t, readReq(root, "../escape.go",
		map[string]any{"allow_ignored": true, "allow_unindexed": true}),
		codeserve.ErrInvalidRequest)
}

func TestCodeReadMembershipUsesExplicitIndexCache(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cache := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "b.go"), []byte("package b\nfunc GateBeta() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := codecrawl.OpenOrRefresh(root, filepath.Join(cache, "code-index.gob"), 2, false); err != nil {
		t.Fatal(err)
	}
	req := codeserve.Request{
		"verb": "code_read", "root": root, "path": "b.go", "index_cache": cache,
	}
	mustReadOK(t, codeserve.Handle(context.Background(), req))
}

func TestCodeReadFailsClosedOnCorruptIndex(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "c.go"), []byte("package c\nfunc GateGamma() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gobPath := indexRoot(t, root)
	if err := os.WriteFile(gobPath, []byte("garbage-not-gob"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Membership cannot be verified → fail closed as index_unavailable, and
	// the explicit opt-in still works for callers that accept the risk.
	mustReadDenied(t, readReq(root, "c.go", nil), codeserve.ErrIndexUnavailable)
	mustReadOK(t, readReq(root, "c.go", map[string]any{"allow_unindexed": true}))
}

func TestNoRefreshFailsOnRootMismatch(t *testing.T) {
	t.Parallel()
	rootA := t.TempDir()
	rootB := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootA, "a.go"), []byte("package a\nfunc Alpha() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootB, "b.go"), []byte("package b\nfunc Beta() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	if _, _, _, _, err := codecrawl.OpenOrRefresh(rootA, filepath.Join(cache, "code-index.gob"), 2, false); err != nil {
		t.Fatal(err)
	}

	// no_refresh with a different root must fail clearly, not serve rootA's
	// index under rootB's name.
	resp := codeserve.Handle(context.Background(), codeserve.Request{
		"verb": "code_search", "root": rootB, "index_cache": cache,
		"q": "Alpha", "no_refresh": true,
	})
	if resp["ok"] != false || resp["error_code"] != string(codeserve.ErrIndexUnavailable) {
		t.Fatalf("root mismatch must fail as index_unavailable: %+v", resp)
	}
	msg, _ := resp["error"].(string)
	if !strings.Contains(msg, "root mismatch") {
		t.Fatalf("mismatch error must name the cause: %+v", resp)
	}

	// Control: the bound root still loads warm.
	ok := codeserve.Handle(context.Background(), codeserve.Request{
		"verb": "code_search", "root": rootA, "index_cache": cache,
		"q": "Alpha", "no_refresh": true,
	})
	if ok["ok"] != true {
		t.Fatalf("bound root no_refresh must succeed: %+v", ok)
	}
}

func TestFreshnessFailsOnRootMismatch(t *testing.T) {
	t.Parallel()
	rootA := t.TempDir()
	rootB := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootA, "a.go"), []byte("package a\nfunc Alpha() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	if _, _, _, _, err := codecrawl.OpenOrRefresh(rootA, filepath.Join(cache, "code-index.gob"), 2, false); err != nil {
		t.Fatal(err)
	}
	resp := codeserve.Handle(context.Background(), codeserve.Request{
		"verb": "code_freshness", "root": rootB, "index_cache": cache,
	})
	if resp["ok"] != false || resp["error_code"] != string(codeserve.ErrIndexUnavailable) {
		t.Fatalf("freshness root mismatch must fail as index_unavailable: %+v", resp)
	}
	if msg, _ := resp["error"].(string); !strings.Contains(msg, "root mismatch") {
		t.Fatalf("freshness mismatch error must name the cause: %+v", resp)
	}

	ok := codeserve.Handle(context.Background(), codeserve.Request{
		"verb": "code_freshness", "root": rootA, "index_cache": cache,
	})
	if ok["ok"] != true {
		t.Fatalf("freshness on bound root must succeed: %+v", ok)
	}
}
