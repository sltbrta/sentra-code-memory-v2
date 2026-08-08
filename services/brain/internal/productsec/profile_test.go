package productsec_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/productsec"
)

func TestSingleUserAlwaysAllows(t *testing.T) {
	t.Parallel()
	c := productsec.Context{Profile: productsec.ProfileSingleUser, Principal: "anyone"}
	if err := c.Authorize("ask"); err != nil {
		t.Fatal(err)
	}
}

func TestMultiPrincipalDenyNonOwner(t *testing.T) {
	t.Parallel()
	c := productsec.Context{
		Profile: productsec.ProfileMultiPrincipal,
		Owner:   "alice", Principal: "bob",
	}
	if err := c.Authorize("ask"); err != productsec.ErrDenied {
		t.Fatalf("got %v", err)
	}
}

func TestMultiPrincipalGrantAndOwner(t *testing.T) {
	t.Parallel()
	c := productsec.Context{
		Profile: productsec.ProfileMultiPrincipal,
		Owner:   "alice", Principal: "alice",
	}
	if err := c.Authorize("ask"); err != nil {
		t.Fatal(err)
	}
	c.Principal = "bob"
	c.Grants = map[string]bool{"bob": true}
	if err := c.Authorize("ask"); err != nil {
		t.Fatal(err)
	}
}

func TestSealSessionRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := productsec.SealSession(dir, "owner", "s1", "user", "hello world"); err != nil {
		t.Fatal(err)
	}
	turns, err := productsec.OpenSealedSession(dir, "owner", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0] != "user\nhello world" {
		t.Fatalf("%v", turns)
	}
}

func TestSealSessionRejectsPathSessionID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bad := []string{"", ".", "..", "../x", "a/b", `a\b`, "foo/../bar"}
	for _, id := range bad {
		if err := productsec.SealSession(dir, "owner", id, "user", "x"); err == nil {
			t.Fatalf("expected reject for session id %q", id)
		}
		if _, err := productsec.OpenSealedSession(dir, "owner", id); err == nil {
			t.Fatalf("expected open reject for session id %q", id)
		}
	}
	// Must not create a sealed file outside sessions/ via traversal.
	if err := productsec.SealSession(dir, "owner", "../escape", "user", "x"); err == nil {
		t.Fatal("expected ../escape reject")
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.sealed")); err == nil {
		t.Fatal("traversal must not create escape.sealed at brain root")
	}
}

func TestEvidenceDigestStable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	chunks := filepath.Join(dir, "chunks.jsonl")
	if err := os.WriteFile(chunks, []byte(`{"id":"1"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d1, err := productsec.UpdateEvidenceDigest(dir)
	if err != nil || d1 == "" {
		t.Fatalf("d1=%q err=%v", d1, err)
	}
	d2, err := productsec.DigestFile(chunks)
	if err != nil || d2 != d1 {
		t.Fatalf("d1=%s d2=%s err=%v", d1, d2, err)
	}
	// Unrelated projection change must not alter digest of chunks.
	if err := os.WriteFile(filepath.Join(dir, "sidecars.jsonl"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d3, err := productsec.DigestFile(chunks)
	if err != nil || d3 != d1 {
		t.Fatalf("digest moved: %s vs %s", d1, d3)
	}
}
