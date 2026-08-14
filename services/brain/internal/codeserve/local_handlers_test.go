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

// TestCodeserveHooksLocalSequentialSubsets drives two subset installs in a
// row through the JSONL verb and asserts the manifest accumulates both
// hooks instead of orphaning the first one.
func TestCodeserveHooksLocalSequentialSubsets(t *testing.T) {
	dir := localGitRepo(t)
	ctx := context.Background()

	first := codeserve.Handle(ctx, codeserve.Request{
		"verb": "hooks_local", "action": "install", "root": dir,
		"strategy": "repo-hooks", "kinds": "post-commit",
	})
	if first["ok"] != true {
		t.Fatalf("first install: %+v", first)
	}
	second := codeserve.Handle(ctx, codeserve.Request{
		"verb": "hooks_local", "action": "install", "root": dir,
		"strategy": "repo-hooks", "kinds": "post-merge",
	})
	if second["ok"] != true {
		t.Fatalf("second install: %+v", second)
	}
	raw, _ := json.Marshal(second["manifest"])
	var m struct {
		Installed []struct {
			Kind string `json:"kind"`
		} `json:"installed"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, e := range m.Installed {
		kinds[e.Kind] = true
	}
	if !kinds["post-commit"] || !kinds["post-merge"] {
		t.Fatalf("sequential subset install lost entries: %s", raw)
	}

	status := codeserve.Handle(ctx, codeserve.Request{
		"verb": "hooks_local", "action": "status", "root": dir,
	})
	installed, _ := status["installed"].([]string)
	if len(installed) != 2 {
		t.Fatalf("status installed=%v want both subset hooks", installed)
	}

	uninstall := codeserve.Handle(ctx, codeserve.Request{
		"verb": "hooks_local", "action": "uninstall", "root": dir,
	})
	if uninstall["ok"] != true {
		t.Fatalf("uninstall: %+v", uninstall)
	}
	for _, kind := range []string{"post-commit", "post-merge"} {
		if _, err := os.Stat(filepath.Join(dir, ".sentra", "hooks", kind)); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed: %v", kind, err)
		}
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

// TestHooksLocalRequiresOperatorTrust locks the issue #63 trust contract:
// install/uninstall/run on `hooks_local` carry the operator-trust marker
// because they leave host state behind (filesystem hook scripts under
// <root>/.sentra/hooks or <git-common>/hooks). Status is intentionally
// NOT in the gated set so a model-facing surface can still answer
// "what's installed?" without an explicit opt-in. Catalog clients use
// the RequiresOperatorTrust flag to render the gate in their own UIs.
func TestHooksLocalRequiresOperatorTrust(t *testing.T) {
	hooks := mustSpec(t, codeserve.CatalogMetadata(), "hooks_local")
	if !hooks.RequiresOperatorTrust {
		t.Fatalf("hooks_local must carry RequiresOperatorTrust, got %+v", hooks)
	}
	for _, action := range []string{"install", "uninstall", "run"} {
		if !codeserve.IsOperatorTrustAction("hooks_local", action) {
			t.Fatalf("IsOperatorTrustAction(hooks_local, %q) = false, want true", action)
		}
	}
	if codeserve.IsOperatorTrustAction("hooks_local", "status") {
		t.Fatalf("status must NOT be gated; status is read-only")
	}
	if codeserve.IsOperatorTrustAction("hooks_local", "") {
		t.Fatalf("missing action must NOT be gated; codeserve rejects empty action before any side effect")
	}
}

// TestApplyChangeSetRequiresOperatorTrust locks the trust contract for
// code_apply_changeset: the verb takes no `action` parameter and every
// dispatch promotes a ChangeSet onto the filesystem under root, so the
// whole verb is gated via the empty-action key. Direct CLI / JSONL
// operator pipelines are unaffected because codeserve.Handle never
// inspects the gate.
func TestApplyChangeSetRequiresOperatorTrust(t *testing.T) {
	apply := mustSpec(t, codeserve.CatalogMetadata(), "code_apply_changeset")
	if !apply.RequiresOperatorTrust {
		t.Fatalf("code_apply_changeset must carry RequiresOperatorTrust, got %+v", apply)
	}
	if !codeserve.IsOperatorTrustAction("code_apply_changeset", "") {
		t.Fatalf("code_apply_changeset (no action param) must be gated via the empty-action key")
	}
}

// TestDenseLocalDoesNotRequireOperatorTrust is the negative-control lock:
// dense_local_search is read-only and remains directly callable from any
// surface, including model-facing adapters, with no opt-in required.
func TestDenseLocalDoesNotRequireOperatorTrust(t *testing.T) {
	dense := mustSpec(t, codeserve.CatalogMetadata(), "dense_local_search")
	if dense.RequiresOperatorTrust {
		t.Fatalf("dense_local_search must not carry RequiresOperatorTrust, got %+v", dense)
	}
	if codeserve.IsOperatorTrustAction("dense_local_search", "search") {
		t.Fatalf("dense_local_search is read-only; it must not be gated")
	}
}

// TestReadVerbsStayOutsideOperatorTrustGate covers the catalog invariants:
// read verbs and the derived-index mutation verbs stay un-gated so the
// paths an agent depends on (ping, code_search, code_read,
// session_recall, implicit index refresh, etc.) remain reachable from
// model-facing surfaces exactly as before. Index-mutation verbs
// (code_index, code_ingest_paths, code_ingest_scip, code_watch) write
// only bounded, regenerable derived-index artifacts inside the repo's
// own index cache — the same writes the read paths perform implicitly —
// so gating them would add no trust boundary; they stay un-gated by
// deliberate decision, locked here.
func TestReadVerbsStayOutsideOperatorTrustGate(t *testing.T) {
	gated := map[string]bool{}
	for _, vs := range codeserve.CatalogMetadata() {
		if vs.RequiresOperatorTrust {
			gated[vs.Name] = true
		}
	}
	if len(gated) != 2 || !gated["hooks_local"] || !gated["code_apply_changeset"] {
		t.Fatalf("only hooks_local and code_apply_changeset should carry RequiresOperatorTrust, got %v", gated)
	}
	for _, verb := range []string{"ping", "code_search", "code_read",
		"code_index", "code_ingest_paths", "code_ingest_scip", "code_watch",
		"session_recall", "memory_search", "dense_local_search"} {
		if codeserve.IsOperatorTrustAction(verb, "anything") {
			t.Fatalf("%q must remain outside the operator-trust gate", verb)
		}
	}
}

// TestOperatorTrustErrorEnvelopeShape guarantees adapter callers see the
// same machine-readable envelope codeserve.Handle would emit: a
// non-OK response with `error_code == "operator_trust_required"`, a
// human-readable `error` line, and a structured `trust_required` block
// that names the verb/action/surface so a UI can render an actionable
// message. The codeserve producer is canonical so HTTP and MCP cannot
// drift apart on the surface they refuse with.
func TestOperatorTrustErrorEnvelopeShape(t *testing.T) {
	got := codeserve.OperatorTrustError("hooks_local", "install", "http")
	if got["ok"] != false {
		t.Fatalf("operator-trust error must be ok:false, got %+v", got)
	}
	if code, _ := got["error_code"].(string); code != string(codeserve.ErrOperatorTrust) {
		t.Fatalf("error_code = %q, want %q", code, codeserve.ErrOperatorTrust)
	}
	if err, _ := got["error"].(string); !strings.Contains(err, "operator trust") {
		t.Fatalf("error message must explain the gate, got %q", err)
	}
	tr, ok := got["trust_required"].(map[string]any)
	if !ok {
		t.Fatalf("trust_required block missing: %+v", got)
	}
	if tr["verb"] != "hooks_local" || tr["action"] != "install" ||
		tr["surface"] != "http" {
		t.Fatalf("trust_required metadata drift: %+v", tr)
	}
	if codeserve.ErrOperatorTrust == codeserve.ErrUnauthorized {
		t.Fatal("ErrOperatorTrust must be distinct from ErrUnauthorized so callers can branch on the failure class")
	}
}

// mustSpec returns the catalog spec for name or fails the test so the
// trust helpers below can assume the verb is present.
func mustSpec(t *testing.T, specs []codeserve.VerbSpec, name string) codeserve.VerbSpec {
	t.Helper()
	for _, s := range specs {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("catalog missing %q", name)
	return codeserve.VerbSpec{}
}

// silenceUnused keeps strings imported during transitional edits.
var _ = strings.Contains
