package codeserve

import (
	"context"
	"path/filepath"
	"strings"
)

// A root pin declares, out of band, which subtree a surface serves.
//
// Every path-taking verb already confines its work inside the `root` it is
// handed. What was missing is a constraint on which root a caller may name. On
// the MCP and HTTP surfaces the caller is a model, so `{"root":"/"}` was an
// ordinary request, and `code_read` answered it. The pin closes that by giving
// the surface -- not the request -- authority over the subtree.
//
// Like operator trust, the pin travels on the context. A constraint the
// constrained party can edit is not a constraint.

type rootPinKey struct{}

// WithRootPin confines every rooted request on ctx to root and its
// descendants. An empty root leaves ctx unpinned.
func WithRootPin(ctx context.Context, root string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return ctx
	}
	// Resolve once here so every later comparison is against a canonical path
	// and a symlinked pin cannot be escaped by naming the link's target.
	if resolved, err := resolveRootForPin(root); err == nil {
		root = resolved
	}
	return context.WithValue(ctx, rootPinKey{}, root)
}

// RootPin returns the pinned root on ctx, or "" when unpinned.
func RootPin(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	pinned, _ := ctx.Value(rootPinKey{}).(string)
	return pinned
}

// rootWithinPin reports whether candidate is the pinned root or below it.
// An unpinned context admits everything: the direct CLI names its own paths.
func rootWithinPin(ctx context.Context, candidate string) bool {
	pin := RootPin(ctx)
	if pin == "" {
		return true
	}
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		// A rooted verb with no root fails its own validation later with a
		// better message than the pin could give.
		return true
	}
	resolved, err := resolveRootForPin(candidate)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(pin, resolved)
	if err != nil {
		return false
	}
	// filepath.Rel is used rather than a string prefix so that a sibling
	// directory sharing a name prefix ("/srv/repo-backup" against a
	// "/srv/repo" pin) is not mistaken for a descendant.
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// rootPinError is the refusal envelope. It deliberately does not echo the
// rejected path: on a model-facing surface the response is the one channel
// back to the caller, and repeating the path would confirm what does or does
// not exist outside the served subtree.
func rootPinError(verb string) Response {
	return Response{
		"ok": false, "verb": verb,
		"error": "root is outside the subtree this surface serves; " +
			"the serving process was started with a root pin",
		"error_code":    string(ErrRootNotPermitted),
		"product_owned": true,
	}
}

// resolveRootForPin canonicalises a path for comparison. It resolves symlinks
// where it can, and falls back to the longest existing ancestor so that a root
// which does not exist yet still compares correctly rather than failing open.
func resolveRootForPin(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	remainder := ""
	current := filepath.Clean(abs)
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			if remainder == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, remainder), nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Reached the filesystem root without resolving anything.
			return filepath.Clean(abs), nil
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}
