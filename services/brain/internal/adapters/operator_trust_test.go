package adapters_test

// Operator-trust gate tests (issue #63). They prove the model-facing HTTP
// /dispatch and MCP tools/call adapters refuse the mutating surface —
// hooks_local install/uninstall/run and code_apply_changeset — on verbs
// whose catalog spec carries RequiresOperatorTrust when the request does
// not carry an explicit operator opt-in. Direct CLI (which calls
// codeserve.Handle itself, bypassing this gate) is untouched and is
// covered separately by services/brain/cmd/sentra-code-memory/cli_local_test.go.
//
// The fixtures use a scratch git repo because hooks_local install/uninstall
// requires one; status does not.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/adapters"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

// operatorTrustGitRepo is the per-test git fixture: a single-commit repo
// with HOME pointed at a temp dir so a developer's global hooksPath
// configuration never leaks in (mirrors TestMain in cli_local_test.go).
func operatorTrustGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	env := []string{
		"HOME=" + dir, "LANG=C",
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(dir, "gitconfig"),
		"GIT_CONFIG_SYSTEM=/dev/null",
	}
	init := exec.Command("git", "init", "-b", "main", dir)
	init.Env = env
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	add := exec.Command("git", "-C", dir, "-c", "user.email=test@example.invalid",
		"-c", "user.name=test", "add", ".")
	add.Env = env
	_ = add.Run()
	commit := exec.Command("git", "-C", dir, "-c", "user.email=test@example.invalid",
		"-c", "user.name=test", "commit", "-m", "init")
	commit.Env = env
	_ = commit.Run()
	return dir
}

// dispatchJSON issues a /dispatch request with optional headers/query and
// returns the codeserve response plus HTTP status. It is the shared
// harness for every test in this file.
func dispatchJSON(t *testing.T, h http.Handler, body, trustHeader, trustQuery string) (codeserve.Response, int) {
	t.Helper()
	target := "/dispatch"
	if trustQuery != "" {
		target = target + "?" + trustQuery + "=1"
	}
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	if trustHeader != "" {
		req.Header.Set(trustHeader, "1")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var resp codeserve.Response
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rr.Body.String())
	}
	return resp, rr.Code
}

// findTrustRequired returns the parsed trust_required block from a codeserve
// response (or nil if absent) so each test can assert on the metadata
// without re-parsing the envelope.
func findTrustRequired(t *testing.T, resp codeserve.Response) map[string]any {
	t.Helper()
	tr, _ := resp["trust_required"].(map[string]any)
	return tr
}

// callMCPTool drives the MCP stdio loop for one tools/call and returns the
// inner codeserve response so we can assert on the same envelope the
// HTTP path returns.
func callMCPTool(t *testing.T, name string, arguments map[string]any) codeserve.Response {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": arguments})
	reqLine, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": json.RawMessage(params),
	})
	lines := serveMCP(t, string(reqLine))
	if len(lines) != 1 {
		t.Fatalf("mcp dispatch: want 1 response, got %d (%q)", len(lines), lines)
	}
	return mustToolCallInner(t, lines[0])
}

// TestHTTPHooksLocalInstallRefusedWithoutOperatorTrust proves the HTTP
// adapter fails closed for hooks_local install when no operator opt-in is
// present. The git fixture is needed because codeserve errors out on a
// missing root, but the gate must fire BEFORE codeserve sees the request:
// if the gate ever regressed, the test would observe codeserve's
// "root does not exist" error instead of the structured trust refusal.
func TestHTTPHooksLocalInstallRefusedWithoutOperatorTrust(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{})
	root := operatorTrustGitRepo(t)
	body := `{"verb":"hooks_local","action":"install","root":"` + root + `","cli_path":"/tmp/evil-binary"}`
	resp, code := dispatchJSON(t, h, body, "", "")
	if code != http.StatusForbidden {
		t.Fatalf("want 403, got %d (%+v)", code, resp)
	}
	if resp["ok"] != false {
		t.Fatalf("expected ok:false, got %+v", resp)
	}
	if got, _ := resp["error_code"].(string); got != string(codeserve.ErrOperatorTrust) {
		t.Fatalf("want error_code=%q, got %q (resp=%+v)",
			codeserve.ErrOperatorTrust, got, resp)
	}
	tr := findTrustRequired(t, resp)
	if tr == nil {
		t.Fatalf("trust_required metadata missing: %+v", resp)
	}
	if tr["verb"] != "hooks_local" || tr["action"] != "install" || tr["surface"] != "http" {
		t.Fatalf("trust_required metadata wrong: %+v", tr)
	}
	// Gate must run BEFORE codeserve, so no host state could have changed.
	if _, err := os.Stat(filepath.Join(root, ".sentra")); !os.IsNotExist(err) {
		t.Fatalf("install was attempted despite refusal: %v", err)
	}
}

// TestHTTPHooksLocalUninstallRefusedWithoutOperatorTrust covers the second
// mutating verb path: uninstall removes sentra-managed hook files; on a
// model-facing surface it must require the same opt-in or a malicious
// caller could rip state out.
func TestHTTPHooksLocalUninstallRefusedWithoutOperatorTrust(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{})
	root := operatorTrustGitRepo(t)
	body := `{"verb":"hooks_local","action":"uninstall","root":"` + root + `"}`
	resp, code := dispatchJSON(t, h, body, "", "")
	if code != http.StatusForbidden || resp["ok"] != false {
		t.Fatalf("want 403 ok:false, got %d %+v", code, resp)
	}
	if got, _ := resp["error_code"].(string); got != string(codeserve.ErrOperatorTrust) {
		t.Fatalf("want error_code=%q, got %q", codeserve.ErrOperatorTrust, got)
	}
}

// TestHTTPHooksLocalRunRefusedWithoutOperatorTrust locks the third
// mutating path. `run` is the entry point installed hook scripts call;
// exposing it over the model-facing HTTP surface would let an attacker
// trigger arbitrary lifecycle events from any loopback caller.
func TestHTTPHooksLocalRunRefusedWithoutOperatorTrust(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{})
	root := operatorTrustGitRepo(t)
	body := `{"verb":"hooks_local","action":"run","root":"` + root + `","event":"post-commit"}`
	resp, code := dispatchJSON(t, h, body, "", "")
	if code != http.StatusForbidden || resp["ok"] != false {
		t.Fatalf("want 403 ok:false, got %d %+v", code, resp)
	}
	if got, _ := resp["error_code"].(string); got != string(codeserve.ErrOperatorTrust) {
		t.Fatalf("want error_code=%q, got %q", codeserve.ErrOperatorTrust, got)
	}
}

// TestHTTPHooksLocalStatusAlwaysAdmitted is the positive read-only path:
// status must NOT be gated, so a model can always ask "what's installed?"
// without setting the operator opt-in flag.
func TestHTTPHooksLocalStatusAlwaysAdmitted(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{})
	root := operatorTrustGitRepo(t)
	body := `{"verb":"hooks_local","action":"status","root":"` + root + `"}`
	resp, code := dispatchJSON(t, h, body, "", "")
	if code != http.StatusOK || resp["ok"] != true {
		t.Fatalf("status must be admitted without opt-in, got %d %+v", code, resp)
	}
	if resp["verb"] != "hooks_local" {
		t.Fatalf("response verb drift: %+v", resp)
	}
}

// TestHTTPHooksLocalInstallAdmittedWithOperatorTrustHeader exercises the
// explicit opt-in path via header. With X-Sentra-Operator-Trust: 1 set,
// the gate forwards the request and the lifecycle installer actually
// runs against the temp repo. The follow-up status check proves the
// install was not silently refused; if the gate ever leaked the request
// into codeserve without actually installing, manifest existence would
// catch it.
func TestHTTPHooksLocalInstallAdmittedWithOperatorTrustHeader(t *testing.T) {
	h := adapters.NewHTTP(adapters.HTTPConfig{})
	root := operatorTrustGitRepo(t)
	body := `{"verb":"hooks_local","action":"install","root":"` + root + `","strategy":"repo-hooks"}`
	resp, code := dispatchJSON(t, h, body, "X-Sentra-Operator-Trust", "")
	if code != http.StatusOK || resp["ok"] != true {
		t.Fatalf("opt-in install must succeed, got %d %+v", code, resp)
	}
	if _, err := os.Stat(filepath.Join(root, ".sentra", "state", "sentra-manifest.json")); err != nil {
		t.Fatalf("manifest should exist after opt-in install: %v", err)
	}
}

// TestHTTPHooksLocalInstallAdmittedWithOperatorTrustQueryParam covers the
// second opt-in shape (?operator_trust=1). HTTP bridges that cannot set
// arbitrary headers rely on the query form, so both must be accepted and
// equivalent in effect.
func TestHTTPHooksLocalInstallAdmittedWithOperatorTrustQueryParam(t *testing.T) {
	h := adapters.NewHTTP(adapters.HTTPConfig{})
	root := operatorTrustGitRepo(t)
	body := `{"verb":"hooks_local","action":"install","root":"` + root + `","strategy":"repo-hooks"}`
	resp, code := dispatchJSON(t, h, body, "", "operator_trust")
	if code != http.StatusOK || resp["ok"] != true {
		t.Fatalf("query opt-in install must succeed, got %d %+v", code, resp)
	}
	if _, err := os.Stat(filepath.Join(root, ".sentra", "hooks", "post-commit")); err != nil {
		t.Fatalf("post-commit hook should exist after opt-in install: %v", err)
	}
}

// TestHTTPHooksLocalInstallRejectsWrongOptInValue proves the gate is not
// a pass-through on any non-empty value: an attacker must spell the
// opt-in value exactly ("1") or the gate stays closed. Header "yes" and
// query "true" must both be refused.
func TestHTTPHooksLocalInstallRejectsWrongOptInValue(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{})
	root := operatorTrustGitRepo(t)
	body := `{"verb":"hooks_local","action":"install","root":"` + root + `"}`

	req := httptest.NewRequest(http.MethodPost, "/dispatch", strings.NewReader(body))
	req.Header.Set("X-Sentra-Operator-Trust", "yes")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("opt-in value 'yes' must be refused, got %d (%s)", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/dispatch?operator_trust=true", strings.NewReader(body))
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("opt-in value 'true' must be refused, got %d (%s)", rr.Code, rr.Body.String())
	}
}

// TestHTTPNonGatedVerbIgnoresOperatorTrustOptIn ensures the opt-in
// mechanism is verb-scoped: a value on the wire for a verb that does not
// require it must be ignored by the gate (codeserve strips it via the
// adapter layer's argument map). This protects future catalog changes
// from accidentally inheriting the gate when the verb is mutated.
func TestHTTPNonGatedVerbIgnoresOperatorTrustOptIn(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{})
	resp, code := dispatchJSON(t, h,
		`{"verb":"ping"}`, "X-Sentra-Operator-Trust", "")
	if code != http.StatusOK || resp["ok"] != true {
		t.Fatalf("ping must always succeed (no gate), got %d %+v", code, resp)
	}
}

// TestMCPToolsListAdvertisesHooksLocalWithTrustNote is the catalog-side
// evidence that the gate is also documented in tools/list: the
// hooks_local tool carries the explicit "_operator_trust" boolean
// property and a description that names the gate, so an MCP client can
// learn the rule by introspection rather than trial and error. Advertise
// + refuse is intentional: status is available and a model can still see
// "the verb exists but mutating calls require opt-in."
func TestMCPToolsListAdvertisesHooksLocalWithTrustNote(t *testing.T) {
	t.Parallel()
	for _, tool := range adapters.MCPTools() {
		if tool.Name != "hooks_local" {
			continue
		}
		if !strings.Contains(tool.Description, "_operator_trust") {
			t.Fatalf("hooks_local description must mention the gate: %q", tool.Description)
		}
		props, _ := tool.InputSchema["properties"].(map[string]any)
		field, _ := props["_operator_trust"].(map[string]any)
		if field == nil {
			t.Fatalf("hooks_local schema missing _operator_trust: %+v", props)
		}
		if field["type"] != "boolean" {
			t.Fatalf("_operator_trust schema must be boolean, got %v", field["type"])
		}
		return
	}
	t.Fatalf("hooks_local missing from MCPTools()")
}

// TestMCPToolsListDensityAndReadVerbsAreUnchanged locks the negative
// invariant: verbs without RequiresOperatorTrust never advertise
// _operator_trust, so the field's presence is a precise signal. A
// regression that added the property to unrelated verbs would be caught
// here without drowning the test in unrelated assertions.
func TestMCPToolsListDensityAndReadVerbsAreUnchanged(t *testing.T) {
	t.Parallel()
	for _, tool := range adapters.MCPTools() {
		if tool.Name == "hooks_local" || tool.Name == "code_apply_changeset" {
			continue
		}
		props, _ := tool.InputSchema["properties"].(map[string]any)
		if _, ok := props["_operator_trust"]; ok {
			t.Fatalf("%s must not advertise _operator_trust (no gate needed): %+v",
				tool.Name, tool.InputSchema)
		}
	}
}

// TestMCPHooksLocalInstallRefusedWithoutOperatorTrust is the parallel of
// TestHTTPHooksLocalInstallRefusedWithoutOperatorTrust over the MCP
// stdio transport. The gate must produce the same structured envelope
// (codeserve.OperatorTrustError) so an MCP client can branch on
// error_code="operator_trust_required" regardless of transport. The
// returned isError flag mirrors how a model-controlled caller would
// observe the refusal.
func TestMCPHooksLocalInstallRefusedWithoutOperatorTrust(t *testing.T) {
	t.Parallel()
	root := operatorTrustGitRepo(t)
	inner := callMCPTool(t, "hooks_local", map[string]any{
		"verb": "hooks_local", "action": "install",
		"root": root, "cli_path": "/tmp/evil-binary",
	})
	if inner["ok"] != false {
		t.Fatalf("want ok:false, got %+v", inner)
	}
	if got, _ := inner["error_code"].(string); got != string(codeserve.ErrOperatorTrust) {
		t.Fatalf("want error_code=%q, got %q (resp=%+v)",
			codeserve.ErrOperatorTrust, got, inner)
	}
	tr, _ := inner["trust_required"].(map[string]any)
	if tr == nil || tr["verb"] != "hooks_local" ||
		tr["action"] != "install" || tr["surface"] != "mcp" {
		t.Fatalf("trust_required metadata drift: %+v", inner)
	}
	if _, err := os.Stat(filepath.Join(root, ".sentra")); !os.IsNotExist(err) {
		t.Fatalf("install was attempted despite MCP refusal: %v", err)
	}
}

// TestMCPHooksLocalUninstallRefusedWithoutOperatorTrust and the run/status
// counterparts ensure every mutating action on hooks_local is gated over
// MCP, while status is admitted unconditionally.
func TestMCPHooksLocalUninstallRefusedWithoutOperatorTrust(t *testing.T) {
	t.Parallel()
	root := operatorTrustGitRepo(t)
	inner := callMCPTool(t, "hooks_local", map[string]any{
		"verb": "hooks_local", "action": "uninstall", "root": root,
	})
	if inner["ok"] != false {
		t.Fatalf("want ok:false, got %+v", inner)
	}
	if got, _ := inner["error_code"].(string); got != string(codeserve.ErrOperatorTrust) {
		t.Fatalf("want error_code=%q, got %q", codeserve.ErrOperatorTrust, got)
	}
}

func TestMCPHooksLocalRunRefusedWithoutOperatorTrust(t *testing.T) {
	t.Parallel()
	root := operatorTrustGitRepo(t)
	inner := callMCPTool(t, "hooks_local", map[string]any{
		"verb": "hooks_local", "action": "run",
		"root": root, "event": "post-commit",
	})
	if inner["ok"] != false {
		t.Fatalf("want ok:false, got %+v", inner)
	}
	if got, _ := inner["error_code"].(string); got != string(codeserve.ErrOperatorTrust) {
		t.Fatalf("want error_code=%q, got %q", codeserve.ErrOperatorTrust, got)
	}
}

func TestMCPHooksLocalStatusAlwaysAdmitted(t *testing.T) {
	t.Parallel()
	root := operatorTrustGitRepo(t)
	inner := callMCPTool(t, "hooks_local", map[string]any{
		"verb": "hooks_local", "action": "status", "root": root,
	})
	if inner["ok"] != true {
		t.Fatalf("status must be admitted over MCP without opt-in, got %+v", inner)
	}
	if inner["verb"] != "hooks_local" {
		t.Fatalf("response verb drift: %+v", inner)
	}
}

// TestMCPHooksLocalInstallAdmittedWithOperatorTrustOptIn proves the
// explicit MCP opt-in path: passing arguments._operator_trust=true
// admits the call and lifecycle.Install actually runs, mirroring the
// HTTP header/query opt-in symmetrically. After install we assert via
// status (also MCP) that the manifest is on disk: that catches any
// regression where the gate was bypassed but the underlying dispatch
// was somehow short-circuited.
func TestMCPHooksLocalInstallAdmittedWithOperatorTrustOptIn(t *testing.T) {
	root := operatorTrustGitRepo(t)
	install := callMCPTool(t, "hooks_local", map[string]any{
		"verb": "hooks_local", "action": "install",
		"root": root, "strategy": "repo-hooks",
		"_operator_trust": true,
	})
	if install["ok"] != true {
		t.Fatalf("opt-in install must succeed, got %+v", install)
	}
	status := callMCPTool(t, "hooks_local", map[string]any{
		"verb": "hooks_local", "action": "status", "root": root,
	})
	installed, _ := status["installed"].([]any)
	if len(installed) == 0 {
		t.Fatalf("MCP opt-in install did not register any hooks: %+v", status)
	}
}

// TestMCPHooksLocalInstallRejectsTruthyStringOptIn is the MCP analogue
// of the HTTP strict-value check: the gate accepts the strict bool
// form (true) only; a string "true" or 1 must be refused so a caller
// cannot smuggle in an opt-in via JSON coercion.
func TestMCPHooksLocalInstallRejectsTruthyStringOptIn(t *testing.T) {
	t.Parallel()
	root := operatorTrustGitRepo(t)
	for _, fake := range []any{"true", "1", 1, "yes"} {
		inner := callMCPTool(t, "hooks_local", map[string]any{
			"verb": "hooks_local", "action": "install",
			"root": root, "_operator_trust": fake,
		})
		if inner["ok"] != false {
			t.Fatalf("opt-in value %v (%T) must be refused, got %+v", fake, fake, inner)
		}
		if got, _ := inner["error_code"].(string); got != string(codeserve.ErrOperatorTrust) {
			t.Fatalf("want error_code=%q, got %q", codeserve.ErrOperatorTrust, got)
		}
	}
}

// TestMCPOptInFieldNeverReachesCodeserve is a transport-truthfulness
// check: the adapter strips "_operator_trust" from the request map
// before codeserve sees it. If codeserve were ever to start honoring
// the field, the install path would silently admit a request that
// codeserve should never have authority over, so the adapter removes
// the field unconditionally. We prove it via status: codeserve reports
// only the verb's canonical keys in the response envelope and the
// raw map after install contains no "_operator_trust" key when read
// back through codeserve.
func TestMCPOptInFieldNeverReachesCodeserve(t *testing.T) {
	root := operatorTrustGitRepo(t)
	install := callMCPTool(t, "hooks_local", map[string]any{
		"verb": "hooks_local", "action": "install",
		"root": root, "strategy": "repo-hooks",
		"_operator_trust":                         true,
		"surprise_key_attempting_to_inject_state": "noop",
	})
	if install["ok"] != true {
		t.Fatalf("opt-in install must succeed with oddball keys: %+v", install)
	}
	// The status response should not carry the opt-in field; codeserve does
	// not return arbitrary echoed arguments on status.
	status := callMCPTool(t, "hooks_local", map[string]any{
		"verb": "hooks_local", "action": "status", "root": root,
	})
	if v, ok := status["_operator_trust"]; ok {
		t.Fatalf("_operator_trust leaked through to codeserve response: %v", v)
	}
}

// TestCLIRemainsUnaffected is the parity check that the direct CLI path
// bypasses the operator-trust gate entirely: codeserve.Handle is invoked
// from runHooks without going through adapters, so an explicit CLI
// invocation continues to install/verify/uninstall across the same
// fixture. We run this test only when git is available; on bare
// environments it is skipped.
func TestCLIRemainsUnaffected(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := t.TempDir()
	env := []string{
		"HOME=" + root, "LANG=C",
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(root, "gitconfig"),
		"GIT_CONFIG_SYSTEM=/dev/null",
	}
	init := exec.Command("git", "init", "-b", "main", root)
	init.Env = env
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	_ = os.WriteFile(filepath.Join(root, "README.md"), []byte("hi"), 0o644)
	add := exec.Command("git", "-C", root, "-c", "user.email=test@example.invalid",
		"-c", "user.name=test", "add", ".")
	add.Env = env
	_ = add.Run()
	commit := exec.Command("git", "-C", root, "-c", "user.email=test@example.invalid",
		"-c", "user.name=test", "commit", "-m", "init")
	commit.Env = env
	_ = commit.Run()

	if resp := dispatch(t, context.Background(),
		codeserve.Request{"verb": "hooks_local", "action": "install",
			"root": root, "strategy": "repo-hooks"}); resp["ok"] != true {
		t.Fatalf("direct codeserve install must succeed (no gate at codeserve level): %+v", resp)
	}
	if resp := dispatch(t, context.Background(),
		codeserve.Request{"verb": "hooks_local", "action": "status", "root": root}); resp["ok"] != true {
		t.Fatalf("direct codeserve status must succeed: %+v", resp)
	}
	if resp := dispatch(t, context.Background(),
		codeserve.Request{"verb": "hooks_local", "action": "uninstall", "root": root}); resp["ok"] != true {
		t.Fatalf("direct codeserve uninstall must succeed (no gate): %+v", resp)
	}
}

// changeSetRefusalBody is a well-formed but fail-closed ChangeSet request
// (empty edits are rejected by validation). It lets the operator-trust
// tests distinguish "the gate refused" (operator_trust_required) from
// "the gate leaked the request into codeserve" (changeset_rejected) —
// codeserve's own validation never touches the filesystem for this body.
func changeSetRefusalBody(root string) string {
	return `{"verb":"code_apply_changeset","root":"` + root +
		`","changeset":{"base":"deadbeef","edits":[]}}`
}

// TestHTTPApplyChangeSetRefusedWithoutOperatorTrust locks the F3 trust
// gap: code_apply_changeset promotes a ChangeSet onto the filesystem
// under root, so over the model-facing /dispatch surface it must require
// the same explicit operator opt-in as hooks_local install. The verb
// takes no `action` parameter; the whole verb is gated.
func TestHTTPApplyChangeSetRefusedWithoutOperatorTrust(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{})
	root := t.TempDir()
	resp, code := dispatchJSON(t, h, changeSetRefusalBody(root), "", "")
	if code != http.StatusForbidden {
		t.Fatalf("want 403, got %d (%+v)", code, resp)
	}
	if resp["ok"] != false {
		t.Fatalf("expected ok:false, got %+v", resp)
	}
	if got, _ := resp["error_code"].(string); got != string(codeserve.ErrOperatorTrust) {
		t.Fatalf("want error_code=%q, got %q (resp=%+v)",
			codeserve.ErrOperatorTrust, got, resp)
	}
	tr := findTrustRequired(t, resp)
	if tr == nil {
		t.Fatalf("trust_required metadata missing: %+v", resp)
	}
	if tr["verb"] != "code_apply_changeset" || tr["surface"] != "http" {
		t.Fatalf("trust_required metadata wrong: %+v", tr)
	}
}

// TestHTTPApplyChangeSetAdmittedWithOperatorTrust proves the opt-in
// forwards the request to codeserve: with X-Sentra-Operator-Trust: 1 the
// gate steps aside and codeserve's own fail-closed validation answers
// (changeset_rejected for the empty-edits fixture). Any
// operator_trust_required response here means the gate ignored the
// opt-in.
func TestHTTPApplyChangeSetAdmittedWithOperatorTrust(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{})
	root := t.TempDir()
	resp, _ := dispatchJSON(t, h, changeSetRefusalBody(root), "X-Sentra-Operator-Trust", "")
	if got, _ := resp["error_code"].(string); got == string(codeserve.ErrOperatorTrust) {
		t.Fatalf("opt-in request must not be trust-refused: %+v", resp)
	}
	if got, _ := resp["error_code"].(string); got != string(codeserve.ErrChangeSetRejected) {
		t.Fatalf("want codeserve's own changeset_rejected after the gate, got %q (%+v)", got, resp)
	}
}

// TestMCPApplyChangeSetRefusedWithoutOperatorTrust is the MCP parallel:
// tools/call on code_apply_changeset without "_operator_trust": true
// must be refused with the same structured envelope as HTTP.
func TestMCPApplyChangeSetRefusedWithoutOperatorTrust(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inner := callMCPTool(t, "code_apply_changeset", map[string]any{
		"root":      root,
		"changeset": map[string]any{"base": "deadbeef", "edits": []any{}},
	})
	if inner["ok"] != false {
		t.Fatalf("want ok:false, got %+v", inner)
	}
	if got, _ := inner["error_code"].(string); got != string(codeserve.ErrOperatorTrust) {
		t.Fatalf("want error_code=%q, got %q (resp=%+v)",
			codeserve.ErrOperatorTrust, got, inner)
	}
	tr, _ := inner["trust_required"].(map[string]any)
	if tr == nil || tr["verb"] != "code_apply_changeset" || tr["surface"] != "mcp" {
		t.Fatalf("trust_required metadata wrong: %+v", inner)
	}
}

// TestMCPApplyChangeSetAdmittedWithOperatorTrustOptIn proves the MCP
// opt-in forwards to codeserve (its fail-closed validation answers with
// changeset_rejected, never operator_trust_required).
func TestMCPApplyChangeSetAdmittedWithOperatorTrustOptIn(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inner := callMCPTool(t, "code_apply_changeset", map[string]any{
		"root":            root,
		"changeset":       map[string]any{"base": "deadbeef", "edits": []any{}},
		"_operator_trust": true,
	})
	if got, _ := inner["error_code"].(string); got == string(codeserve.ErrOperatorTrust) {
		t.Fatalf("opt-in request must not be trust-refused: %+v", inner)
	}
	if got, _ := inner["error_code"].(string); got != string(codeserve.ErrChangeSetRejected) {
		t.Fatalf("want codeserve's own changeset_rejected after the gate, got %q (%+v)", got, inner)
	}
}

// TestMCPToolsListAdvertisesApplyChangeSetWithTrustNote mirrors the
// hooks_local catalog test: the gated verb must advertise the
// _operator_trust property so MCP clients learn the gate by
// introspection.
func TestMCPToolsListAdvertisesApplyChangeSetWithTrustNote(t *testing.T) {
	t.Parallel()
	for _, tool := range adapters.MCPTools() {
		if tool.Name != "code_apply_changeset" {
			continue
		}
		if !strings.Contains(tool.Description, "_operator_trust") {
			t.Fatalf("code_apply_changeset description must mention the gate: %q", tool.Description)
		}
		props, _ := tool.InputSchema["properties"].(map[string]any)
		field, _ := props["_operator_trust"].(map[string]any)
		if field == nil || field["type"] != "boolean" {
			t.Fatalf("code_apply_changeset schema missing boolean _operator_trust: %+v", props)
		}
		return
	}
	t.Fatalf("code_apply_changeset missing from MCPTools()")
}

// TestHTTPApplyChangeSetRefusedWithIgnoredActionField locks the F3
// bypass regression: code_apply_changeset is a whole-verb gate, so
// including an `action` field in the dispatch body must not bypass the
// trust gate. The verb takes no action parameter; any action value the
// caller attaches is irrelevant to the verb's behavior and irrelevant
// to the gate's decision. Without this guard, an attacker could slip
// past the gate by appending `,"action":"ignored"` to a request body
// and reaching codeserve, which would then execute the ChangeSet.
func TestHTTPApplyChangeSetRefusedWithIgnoredActionField(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{})
	root := t.TempDir()
	// action="ignored" was the documented bypass. We also sweep a few
	// plausible hostile values so the regression test would catch any
	// future refactor that accidentally re-keys the gate on the action
	// string.
	for _, action := range []string{
		`"ignored"`,
		`"run"`,
		`"install"`,
		`"status"`,
		`"apply"`,
	} {
		action := action
		t.Run("action="+action, func(t *testing.T) {
			t.Parallel()
			body := `{"verb":"code_apply_changeset","action":` + action +
				`,"root":"` + root +
				`","changeset":{"base":"deadbeef","edits":[]}}`
			resp, code := dispatchJSON(t, h, body, "", "")
			if code != http.StatusForbidden {
				t.Fatalf("want 403 for action=%s, got %d (%+v)", action, code, resp)
			}
			if got, _ := resp["error_code"].(string); got != string(codeserve.ErrOperatorTrust) {
				t.Fatalf("want error_code=%q, got %q (resp=%+v)",
					codeserve.ErrOperatorTrust, got, resp)
			}
			// The trust_required envelope must echo back the action the
			// caller tried, so a UI can render an actionable diagnostic.
			tr := findTrustRequired(t, resp)
			if tr == nil || tr["verb"] != "code_apply_changeset" || tr["surface"] != "http" {
				t.Fatalf("trust_required metadata wrong: %+v", resp)
			}
			// Critical: the gate must fire BEFORE codeserve, so no host
			// state mutation could have happened.
			if got, _ := resp["error_code"].(string); got == string(codeserve.ErrChangeSetRejected) {
				t.Fatalf("request leaked into codeserve despite refusal (resp=%+v)", resp)
			}
		})
	}
}

// TestHTTPApplyChangeSetGateAppliesToEmptyAndMissingAction locks the
// positive path for the whole-verb gate: the documented empty action
// (verb has no action parameter) and the no-action-field case are both
// refused by the gate, matching the gate's contract. This is the
// regression test for the previous empty-action lookup, which already
// refused these — the new OperatorTrustGate.AllActions flag must keep
// that behavior while extending refusal to any non-empty action.
func TestHTTPApplyChangeSetGateAppliesToEmptyAndMissingAction(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{})
	root := t.TempDir()
	// Empty action value.
	body := `{"verb":"code_apply_changeset","action":"","root":"` + root +
		`","changeset":{"base":"deadbeef","edits":[]}}`
	resp, code := dispatchJSON(t, h, body, "", "")
	if code != http.StatusForbidden || resp["ok"] != false {
		t.Fatalf("empty action must be gated, got %d %+v", code, resp)
	}
	if got, _ := resp["error_code"].(string); got != string(codeserve.ErrOperatorTrust) {
		t.Fatalf("want error_code=%q, got %q", codeserve.ErrOperatorTrust, got)
	}
	// No action field at all.
	body = `{"verb":"code_apply_changeset","root":"` + root +
		`","changeset":{"base":"deadbeef","edits":[]}}`
	resp, code = dispatchJSON(t, h, body, "", "")
	if code != http.StatusForbidden || resp["ok"] != false {
		t.Fatalf("missing action must be gated, got %d %+v", code, resp)
	}
	if got, _ := resp["error_code"].(string); got != string(codeserve.ErrOperatorTrust) {
		t.Fatalf("want error_code=%q, got %q", codeserve.ErrOperatorTrust, got)
	}
}

// TestMCPApplyChangeSetRefusedWithIgnoredActionField is the MCP parallel
// of TestHTTPApplyChangeSetRefusedWithIgnoredActionField. The action
// argument is irrelevant to the verb but must not bypass the gate.
func TestMCPApplyChangeSetRefusedWithIgnoredActionField(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, action := range []any{"ignored", "run", "install", "status", "apply", ""} {
		action := action
		t.Run(fmt.Sprintf("action=%v", action), func(t *testing.T) {
			t.Parallel()
			inner := callMCPTool(t, "code_apply_changeset", map[string]any{
				"root":      root,
				"action":    action,
				"changeset": map[string]any{"base": "deadbeef", "edits": []any{}},
			})
			if inner["ok"] != false {
				t.Fatalf("want ok:false, got %+v", inner)
			}
			if got, _ := inner["error_code"].(string); got != string(codeserve.ErrOperatorTrust) {
				t.Fatalf("want error_code=%q, got %q (resp=%+v)",
					codeserve.ErrOperatorTrust, got, inner)
			}
			tr, _ := inner["trust_required"].(map[string]any)
			if tr == nil || tr["verb"] != "code_apply_changeset" || tr["surface"] != "mcp" {
				t.Fatalf("trust_required metadata wrong: %+v", inner)
			}
			// Gate must fire BEFORE codeserve — no changeset_rejected
			// envelope means we slipped through.
			if got, _ := inner["error_code"].(string); got == string(codeserve.ErrChangeSetRejected) {
				t.Fatalf("request leaked into codeserve despite refusal (resp=%+v)", inner)
			}
		})
	}
}

// TestMCPApplyChangeSetGateAppliesWhenActionFieldAbsent locks the MCP
// variant of the positive path: the gate must apply when the action
// argument is omitted entirely from the JSON arguments map, not just
// when it carries an empty string.
func TestMCPApplyChangeSetGateAppliesWhenActionFieldAbsent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inner := callMCPTool(t, "code_apply_changeset", map[string]any{
		"root":      root,
		"changeset": map[string]any{"base": "deadbeef", "edits": []any{}},
	})
	if got, _ := inner["error_code"].(string); got != string(codeserve.ErrOperatorTrust) {
		t.Fatalf("want error_code=%q, got %q (resp=%+v)",
			codeserve.ErrOperatorTrust, got, inner)
	}
}

// TestHTTPApplyChangeSetIgnoredActionStillAdmitsWithOptIn is the
// regression test for the opt-in path: when the caller attaches an
// irrelevant action field AND presents the explicit operator opt-in,
// the gate forwards to codeserve and codeserve's own validation
// rejects the empty-edits fixture with changeset_rejected (never
// operator_trust_required). This proves the gate is action-blind for
// whole-verb gates only when no opt-in is present; with the opt-in
// present, the gate forwards regardless of the action field.
func TestHTTPApplyChangeSetIgnoredActionStillAdmitsWithOptIn(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{})
	root := t.TempDir()
	body := `{"verb":"code_apply_changeset","action":"ignored","root":"` + root +
		`","changeset":{"base":"deadbeef","edits":[]}}`
	resp, _ := dispatchJSON(t, h, body, "X-Sentra-Operator-Trust", "")
	if got, _ := resp["error_code"].(string); got == string(codeserve.ErrOperatorTrust) {
		t.Fatalf("opt-in request must not be trust-refused: %+v", resp)
	}
	if got, _ := resp["error_code"].(string); got != string(codeserve.ErrChangeSetRejected) {
		t.Fatalf("want codeserve's own changeset_rejected after the gate, got %q (%+v)", got, resp)
	}
}

// TestMCPApplyChangeSetIgnoredActionStillAdmitsWithOptIn is the MCP
// parallel of the opt-in path.
func TestMCPApplyChangeSetIgnoredActionStillAdmitsWithOptIn(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inner := callMCPTool(t, "code_apply_changeset", map[string]any{
		"root":            root,
		"action":          "ignored",
		"changeset":       map[string]any{"base": "deadbeef", "edits": []any{}},
		"_operator_trust": true,
	})
	if got, _ := inner["error_code"].(string); got == string(codeserve.ErrOperatorTrust) {
		t.Fatalf("opt-in request must not be trust-refused: %+v", inner)
	}
	if got, _ := inner["error_code"].(string); got != string(codeserve.ErrChangeSetRejected) {
		t.Fatalf("want codeserve's own changeset_rejected after the gate, got %q (%+v)", got, inner)
	}
}

// TestHTTPHooksLocalUnknownActionReachesHandlerNotGate locks the
// per-action half of the F3 fix on the HTTP path: a per-action gated
// verb (hooks_local) must NOT refuse an unknown action like
// `install_typo` at the trust gate. The gate's responsibility is to
// refuse mutating actions; the handler's responsibility is to validate
// the action. Mixing the two would either gate read-only actions or
// pretend an unknown action is trusted; the new per-action gate keeps
// them separate.
func TestHTTPHooksLocalUnknownActionReachesHandlerNotGate(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{})
	root := operatorTrustGitRepo(t)
	body := `{"verb":"hooks_local","action":"install_typo","root":"` + root + `"}`
	resp, code := dispatchJSON(t, h, body, "", "")
	if code != http.StatusOK {
		t.Fatalf("unknown action must reach the handler, got %d (%+v)", code, resp)
	}
	if resp["ok"] != false {
		t.Fatalf("handler must reject unknown action, got %+v", resp)
	}
	if got, _ := resp["error_code"].(string); got == string(codeserve.ErrOperatorTrust) {
		t.Fatalf("gate must not refuse unknown action on a per-action verb (resp=%+v)", resp)
	}
	if errStr, _ := resp["error"].(string); !strings.Contains(errStr, "install_typo") {
		t.Fatalf("handler should name the unknown action in the error: %q", errStr)
	}
}

// TestMCPHooksLocalUnknownActionReachesHandlerNotGate is the MCP
// parallel of TestHTTPHooksLocalUnknownActionReachesHandlerNotGate.
func TestMCPHooksLocalUnknownActionReachesHandlerNotGate(t *testing.T) {
	t.Parallel()
	root := operatorTrustGitRepo(t)
	inner := callMCPTool(t, "hooks_local", map[string]any{
		"verb": "hooks_local", "action": "install_typo", "root": root,
	})
	if got, _ := inner["error_code"].(string); got == string(codeserve.ErrOperatorTrust) {
		t.Fatalf("gate must not refuse unknown action on a per-action verb (resp=%+v)", inner)
	}
	if inner["ok"] != false {
		t.Fatalf("handler must reject unknown action, got %+v", inner)
	}
	if errStr, _ := inner["error"].(string); !strings.Contains(errStr, "install_typo") {
		t.Fatalf("handler should name the unknown action in the error: %q", errStr)
	}
}

// dispatch wraps codeserve.Handle for the CLI-equivalence test so the
// assertion reads like a one-liner and the import is referenced.
func dispatch(t *testing.T, ctx context.Context, req codeserve.Request) codeserve.Response {
	t.Helper()
	return codeserve.Handle(ctx, req)
}

// silenceUnused keeps imports alive during transitional edits to keep
// buildable drafts.
var (
	_ = bytes.NewReader
	_ = bytes.Buffer{}
)
