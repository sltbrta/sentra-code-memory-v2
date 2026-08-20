package github

import (
	"strings"
	"testing"
)

// Every REST URL interpolated owner, repo and ref straight into a format
// string while the fine-grained PAT was attached to the request. validateTuple
// only checked those fields for emptiness, so `..` in owner or repo escaped the
// /repos/ path prefix and an `&` or `#` in a ref rewrote the query.

func TestRefPathEscapeKeepsSlashesAndEscapesEverythingElse(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want string
	}{
		{"ordinary ref", "heads/main", "heads/main"},
		{"ampersand", "heads/a&b", "heads/a&b"},
		{"query start", "heads/a?b", "heads/a%3Fb"},
		{"fragment", "heads/a#b", "heads/a%23b"},
		// The first version of this test asserted the traversal was returned
		// verbatim, i.e. it enshrined the gap it was written to close. A dot
		// segment is now rejected outright: no legitimate ref contains one, and
		// PathEscape leaves "." alone because it is unreserved, so escaping
		// could never have neutralised it.
		{"traversal", "heads/../../etc", ""},
		{"single dot segment", "heads/./main", ""},
		{"space", "heads/a b", "heads/a%20b"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := refPathEscape(test.ref)
			if got != test.want {
				t.Fatalf("refPathEscape(%q) = %q, want %q", test.ref, got, test.want)
			}
			// A ref's slashes must survive when it is accepted at all -- the
			// API path is /git/ref/heads/main, not /git/ref/heads%2Fmain.
			if got != "" && strings.Count(got, "/") != strings.Count(test.ref, "/") {
				t.Fatalf("refPathEscape(%q) = %q: slash count changed", test.ref, got)
			}
		})
	}
}

// TestRefPathEscapeNeutralisesQueryInjection is the property that matters: a
// ref cannot introduce new query parameters into the request it appears in.
func TestRefPathEscapeNeutralisesQueryInjection(t *testing.T) {
	hostile := "heads/main?per_page=1&access_token=leak"
	got := refPathEscape(hostile)
	if strings.Contains(got, "?") {
		t.Fatalf("refPathEscape(%q) = %q: a ref can still start a query string", hostile, got)
	}
}

// TestRepoSegmentEscapeRejectsAnythingThatIsNotAName closes the other half:
// owner and repo went through PathEscape, which preserves "..", so `..` in a
// repository name escaped the /repos/ prefix with the PAT attached.
func TestRepoSegmentEscapeRejectsAnythingThatIsNotAName(t *testing.T) {
	for _, bad := range []string{"..", ".", "a/b", "a\\b", "../../etc"} {
		if got := repoSegmentEscape(bad); got != "" {
			t.Fatalf("repoSegmentEscape(%q) = %q, want a rejection", bad, got)
		}
	}
	for _, good := range []string{"ouroboros", "sentra-code-memory-v2", "a.b", "x_1"} {
		if got := repoSegmentEscape(good); got == "" {
			t.Fatalf("repoSegmentEscape(%q) rejected an ordinary name", good)
		}
	}
}
