package factory

import "strings"

// validRepositoryPath mirrors the frozen plan_node.safe_paths and
// preview_edit.safe_paths CEL rules: a normalized repository-relative path of
// at most 4096 bytes with no leading or trailing separator, no backslash, no
// control characters, and no empty, dot, or dot-dot segments. The contract
// deliberately permits uppercase bytes, so paths are stored and served
// case-exactly as declared.
func validRepositoryPath(path string) bool {
	if path == "" || len(path) > 4096 || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return false
	}
	for _, character := range path {
		if character == '\\' || character < 0x20 || character == 0x7f {
			return false
		}
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// foldPath ASCII-folds case for filesystem-semantics comparisons. The shipped
// platform's default filesystem (APFS on macOS) is case-insensitive, and the
// isolated candidate is materialized on disk, so two paths that differ only
// in ASCII case address the same file even though they are distinct Git tree
// entries. Scope disjointness and attenuation use this fold; the served plan
// keeps the declared case.
func foldPath(path string) string {
	return strings.ToLower(path)
}

// pathsCollide mirrors the frozen change_plan.disjoint_write_scopes CEL rule:
// two scopes overlap when they are equal or either is a proper directory
// prefix of the other, so src/go and src/go/modify-00.go collide while
// src/goo does not. Comparison folds ASCII case (see foldPath), so SRC/GO and
// src/go/modify-00.go also collide.
func pathsCollide(first, second string) bool {
	first = foldPath(first)
	second = foldPath(second)
	return first == second ||
		strings.HasPrefix(second, first+"/") ||
		strings.HasPrefix(first, second+"/")
}

// pathWithinScope reports whether path is equal to or beneath at least one
// approved scope root, using the same directory-prefix and case-fold
// semantics as pathsCollide.
func pathWithinScope(path string, scope []string) bool {
	folded := foldPath(path)
	for _, root := range scope {
		root = foldPath(root)
		if folded == root || strings.HasPrefix(folded, root+"/") {
			return true
		}
	}
	return false
}
