package codeserve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/pathguard"
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

// pinnedPathFields are the request fields that name a filesystem location.
//
// Checking only "root" was not enough, and a fresh-eyes review proved it: three
// other fields reach the filesystem and none of them was inspected.
//
//   - index_cache is both a write (code_index puts the gob there) and a read:
//     resolvePaths explicitly permits an absent root when index_cache is set,
//     so `{"index_cache": "/outside/cache", "no_refresh": true}` with no root
//     at all returned verbatim source from a repository outside the pin.
//   - dir reaches os.MkdirAll and a durable write through memory_put,
//     memory_promote and session_continuation.
//   - scip names a document read from disk.
//
// The list is explicit rather than "any field ending in _path" so adding a
// path-bearing field to a verb is a decision someone has to make here.
var pinnedPathFields = []string{"root", "dir", "index_cache", "scip"}

// requestWithinPin reports whether every path-bearing field of req stays inside
// the pinned subtree.
func requestWithinPin(ctx context.Context, req Request) bool {
	if RootPin(ctx) == "" {
		return true
	}
	named := 0
	for _, field := range pinnedPathFields {
		value := str(req, field)
		if value == "" {
			continue
		}
		named++
		if !rootWithinPin(ctx, value) {
			return false
		}
	}
	// A pinned surface refuses a request that names no location at all only if
	// the verb needs one; verbs like ping and catalog legitimately name none,
	// and they touch no filesystem.
	_ = named
	return true
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

// resolveRootForPin delegates to the shared guard.
//
// This was a line-for-line fork of pathguard.Resolve with the fail-closed
// branch removed: it treated every EvalSymlinks failure as "walk to the parent",
// so a permission error on an ancestor silently weakened the comparison. A
// review caught it as a twelfth copy of the containment logic in a package
// whose whole point was that there should be one.
func resolveRootForPin(path string) (string, error) {
	return pathguard.Resolve(path)
}

// ErrRootFlagInvalid is returned by ResolveRootFlag for a value that names
// something other than an existing directory.
var ErrRootFlagInvalid = errors.New("codeserve: --root must be an existing directory")

// ResolveRootFlag turns a surface's --root flag into the subtree it will
// serve.
//
// The default is the working directory rather than "unconstrained": these
// surfaces take their requests from a model, and an absent flag must not mean
// "the whole filesystem". Serving everything stays available -- pass the path
// separator alone -- but has to be asked for, and returns "" so the caller
// leaves the context unpinned.
//
// This lives here rather than in a command's main package because there are
// two JSONL surfaces over the same Handle. The first copy of it pinned one of
// them; the second surface took no flag at all and was reached by nothing.
func ResolveRootFlag(flagValue string) (string, error) {
	value := strings.TrimSpace(flagValue)
	if value == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("codeserve: resolve default root: %w", err)
		}
		return wd, nil
	}
	if value == string(os.PathSeparator) {
		return "", nil
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("codeserve: resolve --root: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", ErrRootFlagInvalid
	}
	return abs, nil
}
