package codeserve_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/testsupport"
)

// The operator-trust gate used to live only in the HTTP and MCP adapters.
// codeserve.Handle -- the single dispatch entry point -- did not inspect it,
// so the JSONL `serve` loop, which calls Handle directly, applied no gate at
// all. That is the surface the README tells coding agents to keep warm, and
// code_apply_changeset reaches /bin/sh through it.
//
// These tests pin the gate to Handle so a surface cannot forget it. Trust is
// carried on the context, never in the request map: a value the caller can put
// in its own request is not a trust boundary, it is a formality.

// TestHandleRefusesApplyChangeSetWithoutOperatorTrust is the regression test
// for the remote code execution proven during the 2026-08-21 audit.
func TestHandleRefusesApplyChangeSetWithoutOperatorTrust(t *testing.T) {
	root := testsupport.WorkTree(t, map[string]string{"main.go": "package main\n"})
	marker := filepath.Join(t.TempDir(), "executed")

	resp := codeserve.Handle(context.Background(), codeserve.Request{
		"verb": "code_apply_changeset",
		"root": root,
		"changeset": map[string]any{
			"schema": "sentra-scm.workflow/v1",
			"base":   "deadbeef",
			"edits": []any{map[string]any{
				"path":        "main.go",
				"range":       map[string]any{"start": 0, "end": 0},
				"replacement": "// x\n",
			}},
			"verification_commands": []any{"/usr/bin/touch " + marker},
		},
	})

	if resp["ok"] != false {
		t.Fatalf("ok = %v, want false: an ungated caller reached the changeset applier", resp["ok"])
	}
	if code, _ := resp["error_code"].(string); code != string(codeserve.ErrOperatorTrust) {
		t.Fatalf("error_code = %q, want %q (response=%v)", code, codeserve.ErrOperatorTrust, resp)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("verification command executed: the trust gate did not run before the applier")
	}
}

// TestHandleRefusesHooksLocalMutationsWithoutOperatorTrust covers the other
// gated verb. Installing hooks writes executables into a user's repository.
func TestHandleRefusesHooksLocalMutationsWithoutOperatorTrust(t *testing.T) {
	root := testsupport.FakeGitRepo(t, map[string]string{"a.go": "package a\n"})

	for _, action := range []string{"install", "uninstall", "run"} {
		t.Run(action, func(t *testing.T) {
			resp := codeserve.Handle(context.Background(), codeserve.Request{
				"verb":   "hooks_local",
				"action": action,
				"root":   root,
			})
			if code, _ := resp["error_code"].(string); code != string(codeserve.ErrOperatorTrust) {
				t.Fatalf("error_code = %q, want %q (response=%v)", code, codeserve.ErrOperatorTrust, resp)
			}
		})
	}
}

// TestHandleAdmitsGatedVerbWhenTheContextCarriesOperatorTrust proves the gate
// is a gate and not a wall: with trust granted out of band the request reaches
// the handler, where it fails on its own merits rather than on trust.
func TestHandleAdmitsGatedVerbWhenTheContextCarriesOperatorTrust(t *testing.T) {
	root := testsupport.FakeGitRepo(t, map[string]string{"a.go": "package a\n"})
	ctx := codeserve.WithOperatorTrust(context.Background())

	resp := codeserve.Handle(ctx, codeserve.Request{
		"verb":   "hooks_local",
		"action": "status",
		"root":   root,
	})
	if code, _ := resp["error_code"].(string); code == string(codeserve.ErrOperatorTrust) {
		t.Fatalf("trusted context still refused: %v", resp)
	}
}

// TestHandleLeavesUngatedVerbsReachableWithoutTrust keeps the gate narrow --
// read verbs are the product's core surface and must not require an opt-in.
func TestHandleLeavesUngatedVerbsReachableWithoutTrust(t *testing.T) {
	for _, verb := range []string{"ping", "catalog"} {
		t.Run(verb, func(t *testing.T) {
			resp := codeserve.Handle(context.Background(), codeserve.Request{"verb": verb})
			if resp["ok"] != true {
				t.Fatalf("%s: ok = %v, want true (response=%v)", verb, resp["ok"], resp)
			}
		})
	}
}

// TestOperatorTrustIsNotGrantableFromTheRequestMap is the property that makes
// the gate meaningful on a model-facing surface: a model that can write the
// request cannot write itself a grant.
func TestOperatorTrustIsNotGrantableFromTheRequestMap(t *testing.T) {
	root := testsupport.FakeGitRepo(t, map[string]string{"a.go": "package a\n"})

	for _, field := range []string{"_operator_trust", "operator_trust", "trusted"} {
		t.Run(field, func(t *testing.T) {
			resp := codeserve.Handle(context.Background(), codeserve.Request{
				"verb": "hooks_local", "action": "install", "root": root,
				field: true,
			})
			if code, _ := resp["error_code"].(string); code != string(codeserve.ErrOperatorTrust) {
				t.Fatalf("request field %q granted trust: %v", field, resp)
			}
		})
	}
}

// TestOperatorTrustGatesAgreeWithCatalogMetadata closes the drift between the
// two sources of truth: adapters gate on IsOperatorTrustAction while tools/list
// advertises CatalogMetadata().RequiresOperatorTrust. Nothing asserted they
// described the same set, so a verb could be advertised as gated and not be.
func TestOperatorTrustGatesAgreeWithCatalogMetadata(t *testing.T) {
	gated := map[string]bool{}
	for verb := range codeserve.OperatorTrustGates {
		gated[string(verb)] = true
	}
	for _, spec := range codeserve.CatalogMetadata() {
		_, hasGate := codeserve.OperatorTrustGates[codeserve.Verb(spec.Name)]
		if spec.RequiresOperatorTrust != hasGate {
			t.Fatalf("verb %q: catalog RequiresOperatorTrust=%v but OperatorTrustGates entry=%v",
				spec.Name, spec.RequiresOperatorTrust, hasGate)
		}
		delete(gated, spec.Name)
	}
	for verb := range gated {
		t.Fatalf("verb %q has a trust gate but no catalog entry", verb)
	}
}
