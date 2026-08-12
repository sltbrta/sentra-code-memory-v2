package contextpack

import (
	"fmt"
	"strings"
)

// RenderMode selects how source content is compressed before packing.
type RenderMode string

const (
	// RenderFull emits source verbatim.
	RenderFull RenderMode = "full"
	// RenderSignatures keeps declaration lines only (func/type/class/...).
	RenderSignatures RenderMode = "signatures"
	// RenderSkeleton keeps declaration lines plus bare structural
	// closers so nesting shape survives.
	RenderSkeleton RenderMode = "skeleton"
	// RenderCompact keeps all lines but strips comments, blank lines, and
	// trailing whitespace.
	RenderCompact RenderMode = "compact"
)

// ParseRenderMode validates a wire value. Empty selects RenderFull.
func ParseRenderMode(s string) (RenderMode, error) {
	switch RenderMode(strings.TrimSpace(s)) {
	case "", RenderFull:
		return RenderFull, nil
	case RenderSignatures:
		return RenderSignatures, nil
	case RenderSkeleton:
		return RenderSkeleton, nil
	case RenderCompact:
		return RenderCompact, nil
	default:
		return "", fmt.Errorf("invalid render mode %q (want full|signatures|skeleton|compact)", s)
	}
}

// Render applies mode to content. Output is deterministic.
func Render(content string, mode RenderMode) string {
	switch mode {
	case RenderSignatures:
		return joinLines(filterLines(content, isDeclLine))
	case RenderSkeleton:
		return joinLines(filterLines(content, func(l string) bool {
			return isDeclLine(l) || isCloserLine(l)
		}))
	case RenderCompact:
		return joinLines(filterLines(content, func(l string) bool {
			return !isBlank(l) && !isCommentLine(l)
		}))
	default: // RenderFull
		return content
	}
}

// declKeywords are line-leading keywords treated as declarations across the
// languages the index covers (Go-first, plus common siblings).
var declKeywords = []string{
	"package", "import", "func", "type", "const", "var",
	"class", "struct", "interface", "enum", "trait", "impl",
	"def", "fn", "module", "namespace", "export", "public", "private", "protected",
}

func isDeclLine(line string) bool {
	t := strings.TrimSpace(line)
	for _, kw := range declKeywords {
		if t == kw {
			return true
		}
		if strings.HasPrefix(t, kw) {
			rest := t[len(kw)]
			switch rest {
			case ' ', '\t', '(', '{', ':':
				return true
			}
		}
	}
	return false
}

func isCloserLine(line string) bool {
	switch strings.TrimSpace(line) {
	case "}", "});", ")", "]", "end":
		return true
	}
	return false
}

func isBlank(line string) bool {
	return strings.TrimSpace(line) == ""
}

func isCommentLine(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#") ||
		strings.HasPrefix(t, "/*") || strings.HasPrefix(t, "*") ||
		strings.HasPrefix(t, "--")
}

func filterLines(content string, keep func(string) bool) []string {
	var out []string
	for _, l := range strings.Split(content, "\n") {
		l = strings.TrimRight(l, " \t")
		if keep(l) {
			out = append(out, l)
		}
	}
	return out
}

func joinLines(lines []string) string {
	return strings.Join(lines, "\n")
}
