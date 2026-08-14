package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCLI is the shared CLI-level harness: it executes one command line and
// decodes the single JSON response.
func runCLI(t *testing.T, args ...string) map[string]any {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := execute(args, bytes.NewReader(nil), &stdout, &stderr); code != 0 {
		t.Fatalf("%v exit=%d stderr=%s", args, code, stderr.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil {
		t.Fatalf("%v did not emit JSON: %v (%s)", args, err, stdout.String())
	}
	return resp
}

// TestHooksCLILifecycle exercises install → status → uninstall through the
// direct CLI against a temp root with the default repo-hooks strategy.
func TestHooksCLILifecycle(t *testing.T) {
	root := t.TempDir()

	install := runCLI(t, "hooks", "install", "--root", root)
	if install["ok"] != true {
		t.Fatalf("install: %+v", install)
	}
	if _, err := os.Stat(filepath.Join(root, ".sentra", "hooks")); err != nil {
		t.Fatalf("hooks dir missing after install: %v", err)
	}

	// Flags after the action must also parse (extractAction contract).
	status := runCLI(t, "hooks", "--root", root, "status")
	if status["ok"] != true {
		t.Fatalf("status: %+v", status)
	}
	installed, _ := status["installed"].([]any)
	if len(installed) == 0 {
		t.Fatalf("status reports no installed hooks: %+v", status)
	}

	uninstall := runCLI(t, "hooks", "uninstall", "--root", root)
	if uninstall["ok"] != true {
		t.Fatalf("uninstall: %+v", uninstall)
	}
	statusAfter := runCLI(t, "hooks", "status", "--root", root)
	installedAfter, _ := statusAfter["installed"].([]any)
	if len(installedAfter) != 0 {
		t.Fatalf("hooks still installed after uninstall: %+v", statusAfter)
	}
}

// TestHooksCLIRequiresAction rejects a bare hooks invocation.
func TestHooksCLIRequiresAction(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := execute([]string{"hooks"}, bytes.NewReader(nil), &stdout, &stderr); code != 2 {
		t.Fatalf("expected exit 2, got %d (%s)", code, stdout.String())
	}
}

// TestDenseLocalCLISearch runs the lexical arm through the CLI over a tiny
// temp corpus.
func TestDenseLocalCLISearch(t *testing.T) {
	root := t.TempDir()
	src := "package billing\nfunc InvoiceTotal() int { return 42 }\n"
	if err := os.WriteFile(filepath.Join(root, "billing.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runCLI(t, "dense-local", "--root", root, "--q", "billing invoice")
	if got["ok"] != true {
		t.Fatalf("dense-local: %+v", got)
	}
	if got["route"] != "lexical" {
		t.Fatalf("route = %v, want lexical", got["route"])
	}
	raw, _ := json.Marshal(got["hits"])
	if !strings.Contains(string(raw), "billing.go") {
		t.Fatalf("hits missing billing.go: %s", raw)
	}
}

// TestDenseLocalCLIHardBounds proves request-controlled ceilings cannot
// exceed the hard defaults: top-k 999 clamps to 50 and max-corpus 999999
// clamps to 8192, and the receipt reports the enforced envelope.
func TestDenseLocalCLIHardBounds(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\nfunc Alpha() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runCLI(t, "dense-local", "--root", root, "--q", "alpha",
		"--top-k", "999", "--max-corpus", "999999")
	if got["ok"] != true {
		t.Fatalf("dense-local: %+v", got)
	}
	bounded, _ := got["bounded_by"].(map[string]any)
	if bounded["max_top_k"] != float64(50) {
		t.Fatalf("max_top_k not clamped to hard default: %+v", bounded)
	}
	if bounded["max_corpus"] != float64(8192) {
		t.Fatalf("max_corpus not clamped to hard default: %+v", bounded)
	}
	hits, _ := got["hits"].([]any)
	if len(hits) > 50 {
		t.Fatalf("more hits than the hard top-k ceiling: %d", len(hits))
	}
}

// TestDenseLocalCLIRejectsBadBounds refuses non-positive bound overrides.
func TestDenseLocalCLIRejectsBadBounds(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runCLI(t, "dense-local", "--root", root, "--q", "a", "--max-corpus", "-3")
	if got["ok"] != false {
		t.Fatalf("expected bounds refusal, got %+v", got)
	}
}

// TestDenseLocalCLIRequiresQueryAndRoot fails fast on missing required flags.
func TestDenseLocalCLIRequiresQueryAndRoot(t *testing.T) {
	noQ := runCLI(t, "dense-local", "--root", t.TempDir())
	if noQ["ok"] != false {
		t.Fatalf("missing q should fail: %+v", noQ)
	}
	noRoot := runCLI(t, "dense-local", "--q", "billing")
	if noRoot["ok"] != false {
		t.Fatalf("missing root should fail: %+v", noRoot)
	}
}
