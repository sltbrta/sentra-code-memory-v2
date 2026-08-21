package hosted

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/rerank"
)

// The reranker saw the first 1,500 bytes of every document, which on code is
// the licence header and the imports.
//
// The open ledger entry said this could not be measured because the reranker
// is a credentialed remote service. That was wrong: LexicalReranker is this
// product's own fallback reranker, needs no network, and its scoring is fully
// inspectable. The window policy can therefore be measured offline, which is
// what these do.
//
// What is measured is the *policy*, not the remote model. Nothing here claims
// to predict what zerank-2 would score.

// sourceFile builds a file shaped like real code: a licence header and imports
// filling the head window, then the answer-bearing definition well past it.
func sourceFile(symbol string, headBytes int) string {
	var b strings.Builder
	b.WriteString("// Copyright 2026 the authors. All rights reserved.\n")
	b.WriteString("// Use of this source code is governed by a licence that\n")
	b.WriteString("// can be found in the LICENSE file.\n\npackage service\n\nimport (\n")
	for i := 0; b.Len() < headBytes; i++ {
		fmt.Fprintf(&b, "\t\"github.com/example/project/internal/pkg%03d\"\n", i)
	}
	b.WriteString(")\n\n")
	fmt.Fprintf(&b, "// %s validates a bearer token against the session store.\n", symbol)
	fmt.Fprintf(&b, "func %s(token string) (bool, error) {\n", symbol)
	fmt.Fprintf(&b, "\t// %s checks expiry, audience and the revocation epoch.\n", symbol)
	b.WriteString("\treturn true, nil\n}\n")
	return b.String()
}

// rerankAt runs the offline reranker over documents clipped by the given
// policy and reports whether the answer-bearing document ranked first.
func rerankAt(t *testing.T, window func(text, query string, limit int) string,
	query string, docs []string, want int, limit int,
) bool {
	t.Helper()
	clipped := make([]string, len(docs))
	for i, doc := range docs {
		clipped[i] = window(doc, query, limit)
	}
	ranked, err := rerank.NewLexicalReranker().Rerank(context.Background(), query, clipped, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranked) == 0 {
		return false
	}
	return ranked[0].Index == want
}

// headWindow is the previous policy: the first `limit` bytes, whatever they
// contain.
func headWindow(text, _ string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}

// TestTheHeadWindowLosesTheAnswerOnSourceFiles is the finding, measured.
func TestTheHeadWindowLosesTheAnswerOnSourceFiles(t *testing.T) {
	const limit = 1500
	const target = 3
	docs := []string{
		sourceFile("RefreshSession", 1600),
		sourceFile("EncodeCursor", 1600),
		sourceFile("ParseManifest", 1600),
		sourceFile("ValidateToken", 1600), // the answer
		sourceFile("ComputeDigest", 1600),
	}
	const query = "ValidateToken bearer token revocation epoch"

	headHit := rerankAt(t, headWindow, query, docs, target, limit)
	windowHit := rerankAt(t, rerankWindow, query, docs, target, limit)

	t.Logf("head window   hit@1=%v", headHit)
	t.Logf("query window  hit@1=%v", windowHit)

	if headHit {
		t.Fatal("the head window found the answer, so this corpus does not " +
			"exhibit the problem and the comparison below means nothing")
	}
	if !windowHit {
		t.Fatal("the query-centred window did not find the answer either: the " +
			"window policy is not what loses it, and the ledger entry should " +
			"say so instead of claiming this fixes it")
	}
}

// TestTheHeadWindowIsKeptWhenItAnswers pins that the change is not "always
// move the window". A document whose answer is in the head must be clipped
// exactly as before, because every existing receipt digest was computed over
// that text.
func TestTheHeadWindowIsKeptWhenItAnswers(t *testing.T) {
	const limit = 1500
	text := "package service\n\nfunc ValidateToken() {}\n" + strings.Repeat("// filler\n", 400)
	got := rerankWindow(text, "ValidateToken", limit)
	if want := headWindow(text, "", limit); got != want {
		t.Fatalf("a document answered in its head was re-windowed:\n got %d bytes\nwant %d bytes",
			len(got), len(want))
	}
}

// TestWindowNeverExceedsTheBudget: the policy changes where the bytes come
// from, never how many. A wider window would cost the provider more and would
// be a different decision.
func TestWindowNeverExceedsTheBudget(t *testing.T) {
	for _, limit := range []int{100, 1500, 2000} {
		for name, text := range map[string]string{
			"deep match":  sourceFile("ValidateToken", 4000),
			"no match":    sourceFile("Unrelated", 4000),
			"short":       "package a\n",
			"multibyte":   strings.Repeat("界 ValidateToken 界\n", 500),
			"single line": strings.Repeat("x", 5000) + "ValidateToken",
		} {
			got := rerankWindow(text, "ValidateToken bearer", limit)
			if len(got) > limit {
				t.Errorf("%s at limit %d: returned %d bytes", name, limit, len(got))
			}
			if !utf8.ValidString(got) {
				t.Errorf("%s at limit %d: invalid UTF-8", name, limit)
			}
		}
	}
}

// TestWindowIsDeterministic keeps the policy from becoming a source of
// nondeterministic ranking, which is a class this branch has already fixed
// eight times.
func TestWindowIsDeterministic(t *testing.T) {
	text := sourceFile("ValidateToken", 4000)
	first := rerankWindow(text, "ValidateToken bearer", 1500)
	for i := 0; i < 20; i++ {
		if got := rerankWindow(text, "ValidateToken bearer", 1500); got != first {
			t.Fatal("the window is not a function of its inputs")
		}
	}
}
