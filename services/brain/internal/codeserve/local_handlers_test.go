package codeserve_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

// localGitRepo spins up a small git repo so the local hooks handler can
// exercise its git-common-dir path. Caller is responsible for cleanup.
func localGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-b", "main", dir)
	cmd.Env = []string{
		"HOME=" + dir, "LANG=C", "PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(dir, "gitconfig"),
		"GIT_CONFIG_SYSTEM=/dev/null",
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0o644)
	_ = exec.Command("git", "-C", dir, "-c", "user.email=test@example.invalid",
		"-c", "user.name=test", "add", ".").Run()
	_ = exec.Command("git", "-C", dir, "-c", "user.email=test@example.invalid",
		"-c", "user.name=test", "commit", "-m", "init").Run()
	return dir
}

// TestCodeserveHooksLocalInstallStatusUninstall drives the JSONL verb for
// the local hooks lifecycle. It exercises the codeserve handler end to end
// without touching the CLI surface, so JSONL/MCP/HTTP callers can rely on
// the same evidence as the CLI.
func TestCodeserveHooksLocalInstallStatusUninstall(t *testing.T) {
	dir := localGitRepo(t)
	ctx := context.Background()

	install := codeserve.Handle(ctx, codeserve.Request{
		"verb":     "hooks_local",
		"action":   "install",
		"root":     dir,
		"strategy": "repo-hooks",
	})
	if install["ok"] != true {
		t.Fatalf("install: %+v", install)
	}
	hooksDir := filepath.Join(dir, ".sentra", "hooks")
	for _, kind := range []string{"post-commit", "post-checkout", "post-merge", "pre-push"} {
		if _, err := os.Stat(filepath.Join(hooksDir, kind)); err != nil {
			t.Fatalf("%s missing: %v", kind, err)
		}
	}
	stateDir := filepath.Join(dir, ".sentra", "state")
	if _, err := os.Stat(filepath.Join(stateDir, "sentra-manifest.json")); err != nil {
		t.Fatalf("manifest missing: %v", err)
	}

	status := codeserve.Handle(ctx, codeserve.Request{
		"verb": "hooks_local", "action": "status", "root": dir,
	})
	if status["ok"] != true {
		t.Fatalf("status: %+v", status)
	}
	installed, _ := status["installed"].([]string)
	if len(installed) != 4 {
		t.Fatalf("installed len=%d want 4: %+v", len(installed), status)
	}

	uninstall := codeserve.Handle(ctx, codeserve.Request{
		"verb": "hooks_local", "action": "uninstall", "root": dir,
	})
	if uninstall["ok"] != true {
		t.Fatalf("uninstall: %+v", uninstall)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "sentra-manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("manifest should be gone: %v", err)
	}
}

// TestCodeserveHooksLocalValidatesAction ensures missing action fails fast.
func TestCodeserveHooksLocalValidatesAction(t *testing.T) {
	got := codeserve.Handle(context.Background(), codeserve.Request{
		"verb": "hooks_local", "root": "/tmp",
	})
	if got["ok"] != false {
		t.Fatalf("expected fail: %+v", got)
	}
	if code, _ := got["error_code"].(string); code == "" {
		t.Fatalf("missing error_code: %+v", got)
	}
}

// TestCodeserveHooksLocalIdempotent reruns install twice and confirms the
// manifest stays self-consistent (issue #59 acceptance: idempotent).
func TestCodeserveHooksLocalIdempotent(t *testing.T) {
	dir := localGitRepo(t)
	ctx := context.Background()
	first := codeserve.Handle(ctx, codeserve.Request{
		"verb": "hooks_local", "action": "install", "root": dir, "strategy": "repo-hooks",
	})
	if first["ok"] != true {
		t.Fatalf("first install: %+v", first)
	}
	second := codeserve.Handle(ctx, codeserve.Request{
		"verb": "hooks_local", "action": "install", "root": dir, "strategy": "repo-hooks",
	})
	if second["ok"] != true {
		t.Fatalf("second install: %+v", second)
	}
	if first["manifest"] == nil || second["manifest"] == nil {
		t.Fatalf("expected manifest in both responses")
	}
	firstRaw, _ := json.Marshal(first["manifest"])
	secondRaw, _ := json.Marshal(second["manifest"])
	if string(firstRaw) != string(secondRaw) {
		t.Fatalf("manifest drift on idempotent install:\n first=%s\n second=%s",
			firstRaw, secondRaw)
	}
}

// TestCodeserveDenseLocalSearchValidates exercises the JSONL handler's
// required-fields contract.
func TestCodeserveDenseLocalSearchValidates(t *testing.T) {
	ctx := context.Background()
	noQ := codeserve.Handle(ctx, codeserve.Request{
		"verb": "dense_local_search", "root": "/tmp",
	})
	if noQ["ok"] != false {
		t.Fatalf("missing q: %+v", noQ)
	}
	noRoot := codeserve.Handle(ctx, codeserve.Request{
		"verb": "dense_local_search", "q": "billing",
	})
	if noRoot["ok"] != false {
		t.Fatalf("missing root: %+v", noRoot)
	}
}

// TestCodeserveDenseLocalSearchHappyPath issues a real lexical search
// over a tiny in-memory corpus and confirms the JSONL report shape.
func TestCodeserveDenseLocalSearchHappyPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "billing.go"),
		[]byte("package billing\nfunc InvoiceTotal() int { return 42 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := codeserve.Handle(context.Background(), codeserve.Request{
		"verb": "dense_local_search",
		"q":    "billing",
		"root": dir,
	})
	if got["ok"] != true {
		t.Fatalf("expected ok, got %+v", got)
	}
	if got["route"] != "lexical" {
		t.Fatalf("unexpected route: %s", got["route"])
	}
	raw, _ := json.Marshal(got["hits"])
	if !strings.Contains(string(raw), "billing.go") {
		t.Fatalf("hits missing billing.go: %s", raw)
	}
}

// TestCodeserveDenseLocalSearchRefusesUnknown is the friendly-error path.
func TestCodeserveDenseLocalSearchRefusesUnknown(t *testing.T) {
	got := codeserve.Handle(context.Background(), codeserve.Request{
		"verb": "dense_local_search", "q": "x", "root": "/nonexistent-path",
	})
	if got["ok"] != false {
		t.Fatalf("expected fail: %+v", got)
	}
	if code, _ := got["error_code"].(string); code == "" {
		t.Fatalf("missing error_code: %+v", got)
	}
}

// TestCatalogListsNewVerbs verifies the catalog remains discoverable for
// the new local-first verbs.
func TestCatalogListsNewVerbs(t *testing.T) {
	cat := codeserve.Catalog()
	found := map[string]bool{}
	for _, v := range cat {
		found[v] = true
	}
	for _, want := range []string{"hooks_local", "dense_local_search"} {
		if !found[want] {
			t.Fatalf("catalog missing %s: %v", want, cat)
		}
	}
}

// TestNewVerbsHaveSpecMetadata ensures the catalog metadata covers each new
// verb (issue #47 catalog contract).
func TestNewVerbsHaveSpecMetadata(t *testing.T) {
	for _, vs := range codeserve.CatalogMetadata() {
		if vs.Name != "hooks_local" && vs.Name != "dense_local_search" {
			continue
		}
		if vs.Status == "" || vs.Surface == "" || vs.Summary == "" {
			t.Fatalf("incomplete spec: %+v", vs)
		}
	}
}

// silenceUnused keeps strings imported during transitional edits.
var _ = strings.Contains
