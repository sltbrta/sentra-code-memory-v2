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
		{"traversal", "heads/../../etc", "heads/../../etc"},
		{"space", "heads/a b", "heads/a%20b"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := refPathEscape(test.ref)
			// A ref's slashes must survive -- the API path is
			// /git/ref/heads/main, not /git/ref/heads%2Fmain.
			if strings.Count(got, "/") != strings.Count(test.ref, "/") {
				t.Fatalf("refPathEscape(%q) = %q: slash count changed", test.ref, got)
			}
			if got != test.want {
				t.Fatalf("refPathEscape(%q) = %q, want %q", test.ref, got, test.want)
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
