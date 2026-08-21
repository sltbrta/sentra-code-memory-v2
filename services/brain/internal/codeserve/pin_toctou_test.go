package codeserve

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The pin's check and the handler's use resolved the caller's path
// independently.
//
// requestWithinPin resolved a path through its symlinks and asked whether the
// result was inside the pin; the handler then took the caller's *original*
// string out of the request and resolved it again when it opened it. Two
// resolutions of one string need not agree -- a symlink component the caller
// controls can be repointed in between -- so the path that was admitted and
// the path that was opened were only assumed to be the same file.
//
// The gate now rewrites each admitted field to the path it was admitted on, so
// the handler acts on exactly what was checked. That is a narrowing, not a
// closure: a component of the resolved path can still be swapped before the
// handler opens it. Closing that needs openat-style resolution held across the
// operation, in every path-taking verb rather than here.

func TestPinRewritesAdmittedPathsToWhatWasChecked(t *testing.T) {
	pin := t.TempDir()
	real := filepath.Join(pin, "workspace")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(pin, "current")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	ctx := WithRootPin(context.Background(), pin)
	req := Request{"verb": "code_index", "root": link}
	if !requestWithinPin(ctx, req) {
		t.Fatal("a symlink inside the pin was refused")
	}

	got, _ := req["root"].(string)
	if got == link {
		t.Fatal("the request still carries the caller's original string: the " +
			"handler will resolve it a second time, and the second resolution " +
			"need not agree with the one that admitted it")
	}
	want, err := resolveRootForPin(real)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("root was rewritten to %q, want the checked path %q", got, want)
	}
}

// TestPinRewritesEveryPathBearingField covers the other three fields, since
// checking only "root" is the mistake a previous review already caught here.
func TestPinRewritesEveryPathBearingField(t *testing.T) {
	pin := t.TempDir()
	ctx := WithRootPin(context.Background(), pin)

	req := Request{"verb": "code_index"}
	want := map[string]string{}
	for _, field := range pinnedPathFields {
		real := filepath.Join(pin, field+"-target")
		if err := os.MkdirAll(real, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(pin, field+"-link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		req[field] = link
		resolved, err := resolveRootForPin(real)
		if err != nil {
			t.Fatal(err)
		}
		want[field] = resolved
	}

	if !requestWithinPin(ctx, req) {
		t.Fatal("links inside the pin were refused")
	}
	for _, field := range pinnedPathFields {
		got, _ := req[field].(string)
		if got != want[field] {
			t.Errorf("%s = %q, want the checked path %q", field, got, want[field])
		}
	}
}

// TestPinStillRefusesALinkPointingOutside keeps the rewrite from becoming a
// way in: resolving before comparing is what catches this, and the rewrite
// must not happen for a path that was refused.
func TestPinStillRefusesALinkPointingOutside(t *testing.T) {
	pin := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(pin, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	ctx := WithRootPin(context.Background(), pin)
	req := Request{"verb": "code_index", "root": link}
	if requestWithinPin(ctx, req) {
		t.Fatal("a symlink pointing outside the pin was admitted")
	}
	if got, _ := req["root"].(string); got != link {
		t.Fatalf("a refused request was rewritten to %q", got)
	}
}

// TestUnpinnedRequestsAreLeftAlone keeps the direct CLI naming its own paths,
// including relative ones, exactly as written.
func TestUnpinnedRequestsAreLeftAlone(t *testing.T) {
	req := Request{"verb": "code_index", "root": "./relative/path"}
	if !requestWithinPin(context.Background(), req) {
		t.Fatal("an unpinned request was refused")
	}
	if got, _ := req["root"].(string); got != "./relative/path" {
		t.Fatalf("an unpinned request was rewritten to %q", got)
	}
}
