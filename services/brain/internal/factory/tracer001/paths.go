package tracer001

import (
	"regexp"
	"strings"
)

var nodeIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// validRepositoryPath mirrors Stage 05 factory.paths: normalized relative path,
// no empty/dot/dot-dot segments, no leading/trailing slash, no controls.
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

func foldPath(path string) string {
	return strings.ToLower(path)
}

// pathsCollide reports equal or directory-prefix overlap under ASCII case-fold.
func pathsCollide(first, second string) bool {
	first = foldPath(first)
	second = foldPath(second)
	return first == second ||
		strings.HasPrefix(second, first+"/") ||
		strings.HasPrefix(first, second+"/")
}

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

func validGitOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return false
	}
	return true
}

func isHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return false
	}
	return true
}

func validPrincipalID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}
