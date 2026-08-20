package codeserve_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/testsupport"
)

// Every path-taking verb confines its work inside the `root` it is given, but
// nothing constrained which root a caller could name. On the model-facing
// surfaces the caller is the model, so `{"root":"/","path":"etc/hosts"}` was a
// legitimate request -- and it returned the file. The audit confirmed it
// against a real binary.
//
// A root pin is the missing half: the surface declares, out of band, which
// subtree it serves, and Handle refuses anything outside it.

func TestHandleRefusesARootOutsideThePin(t *testing.T) {
	served := testsupport.WorkTree(t, map[string]string{"in.go": "package a\n"})
	outside := testsupport.WorkTree(t, map[string]string{"secret.go": "package secret\n"})
	ctx := codeserve.WithRootPin(context.Background(), served)

	resp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_index", "root": outside,
	})
	if resp["ok"] != false {
		t.Fatalf("ok = %v, want false: a root outside the pin was served", resp["ok"])
	}
	if code, _ := resp["error_code"].(string); code != string(codeserve.ErrRootNotPermitted) {
		t.Fatalf("error_code = %q, want %q (response=%v)", code, codeserve.ErrRootNotPermitted, resp)
	}
	if msg, _ := resp["error"].(string); strings.Contains(msg, outside) {
		t.Fatalf("refusal echoed the rejected path back to the caller: %q", msg)
	}
}

func TestHandleAdmitsThePinnedRootAndItsDescendants(t *testing.T) {
	served := testsupport.WorkTree(t, map[string]string{
		"a.go":        "package a\n",
		"pkg/ب.go":    "package pkg\n",
		"pkg/deep.go": "package pkg\n",
	})
	ctx := codeserve.WithRootPin(context.Background(), served)

	for _, root := range []string{served, filepath.Join(served, "pkg")} {
		resp := codeserve.Handle(ctx, codeserve.Request{"verb": "code_index", "root": root})
		if resp["ok"] != true {
			t.Fatalf("root %q refused inside the pin: %v", root, resp)
		}
	}
}

// TestHandleRefusesTraversalOutOfThePin covers the obvious bypass.
func TestHandleRefusesTraversalOutOfThePin(t *testing.T) {
	parent := t.TempDir()
	served := filepath.Join(parent, "served")
	testsupport.WriteFiles(t, parent, map[string]string{
		"served/a.go":  "package a\n",
		"sibling/b.go": "package b\n",
	})
	ctx := codeserve.WithRootPin(context.Background(), served)

	resp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_index", "root": filepath.Join(served, "..", "sibling"),
	})
	if code, _ := resp["error_code"].(string); code != string(codeserve.ErrRootNotPermitted) {
		t.Fatalf("traversal admitted: %v", resp)
	}
}

// TestHandleWithoutAPinIsUnconstrained keeps the direct CLI usable: a person
// running `sentra-code-memory search --root /any/path` is naming their own
// path, and nothing about that needs constraining.
func TestHandleWithoutAPinIsUnconstrained(t *testing.T) {
	root := testsupport.WorkTree(t, map[string]string{"a.go": "package a\n"})
	resp := codeserve.Handle(context.Background(), codeserve.Request{"verb": "code_index", "root": root})
	if resp["ok"] != true {
		t.Fatalf("unpinned dispatch must serve any root: %v", resp)
	}
}

// TestRootPinIsNotGrantableFromTheRequestMap mirrors the operator-trust
// property: the constraint cannot be widened by the party it constrains.
func TestRootPinIsNotGrantableFromTheRequestMap(t *testing.T) {
	served := testsupport.WorkTree(t, map[string]string{"a.go": "package a\n"})
	outside := testsupport.WorkTree(t, map[string]string{"secret.go": "package secret\n"})
	ctx := codeserve.WithRootPin(context.Background(), served)

	for _, field := range []string{"root_pin", "_root_pin", "allow_root"} {
		resp := codeserve.Handle(ctx, codeserve.Request{
			"verb": "code_index", "root": outside, field: outside,
		})
		if code, _ := resp["error_code"].(string); code != string(codeserve.ErrRootNotPermitted) {
			t.Fatalf("request field %q widened the pin: %v", field, resp)
		}
	}
}
