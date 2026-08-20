package codeserve_test

import (
	"context"
	"os"
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

// The tests below close blockers found by a fresh-eyes review of the first
// version of the root pin. The pin inspected only the "root" field, so three
// other path-bearing fields walked straight past it -- including one that
// returned verbatim source from outside the pinned subtree.

func TestHandleRefusesAnIndexCacheOutsideThePin(t *testing.T) {
	served := testsupport.WorkTree(t, map[string]string{"in.go": "package a\n"})
	outside := t.TempDir()
	ctx := codeserve.WithRootPin(context.Background(), served)

	resp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_index", "root": served,
		"index_cache": filepath.Join(outside, "cache"),
	})
	if code, _ := resp["error_code"].(string); code != string(codeserve.ErrRootNotPermitted) {
		t.Fatalf("index_cache outside the pin was admitted: %v", resp)
	}
}

// TestHandleRefusesARootlessRequestThatNamesAnOutsideCache is the reviewer's
// worst finding: resolvePaths permits an absent root when index_cache is set,
// so a request naming no root at all skipped the pin and read source from
// another repository entirely.
func TestHandleRefusesARootlessRequestThatNamesAnOutsideCache(t *testing.T) {
	served := testsupport.WorkTree(t, map[string]string{"in.go": "package a\n"})
	outside := t.TempDir()
	ctx := codeserve.WithRootPin(context.Background(), served)

	resp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_find_relevant", "q": "secret",
		"index_cache": filepath.Join(outside, "cache"),
		"no_refresh":  true, "preview": true,
	})
	if code, _ := resp["error_code"].(string); code != string(codeserve.ErrRootNotPermitted) {
		t.Fatalf("a rootless request reached outside the pin: %v", resp)
	}
}

func TestHandleRefusesADirOutsideThePin(t *testing.T) {
	served := testsupport.WorkTree(t, map[string]string{"in.go": "package a\n"})
	outside := t.TempDir()
	ctx := codeserve.WithRootPin(context.Background(), served)

	for _, verb := range []string{"memory_put", "memory_list", "session_continuation"} {
		t.Run(verb, func(t *testing.T) {
			target := filepath.Join(outside, "mem-"+verb)
			resp := codeserve.Handle(ctx, codeserve.Request{
				"verb": verb, "dir": target,
				"principal": "p", "text": "t", "kind": "note",
			})
			if code, _ := resp["error_code"].(string); code != string(codeserve.ErrRootNotPermitted) {
				t.Fatalf("%s wrote outside the pin: %v", verb, resp)
			}
			if _, err := os.Stat(target); err == nil {
				t.Fatalf("%s created a directory outside the pin", verb)
			}
		})
	}
}

func TestHandleAdmitsADirInsideThePin(t *testing.T) {
	served := testsupport.WorkTree(t, map[string]string{"in.go": "package a\n"})
	ctx := codeserve.WithRootPin(context.Background(), served)
	resp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "memory_put", "dir": filepath.Join(served, "mem"),
		"principal": "p", "kind": "note", "text": "t",
	})
	if code, _ := resp["error_code"].(string); code == string(codeserve.ErrRootNotPermitted) {
		t.Fatalf("a dir inside the pin was refused: %v", resp)
	}
}
