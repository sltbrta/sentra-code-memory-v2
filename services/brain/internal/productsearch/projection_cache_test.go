package productsearch

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeindex"
)

// code_exact read and re-parsed the whole repository on every query: measured
// at 400ms per call on this repository against 76ms for a code_search, with
// nothing reused between two identical queries a second apart. Memoising the
// projection brings it to 146ms.
//
// Because the receipt digest covers every file's projection digest, a cache
// that returned anything but the exact parse would change the receipt. That is
// what these check: the memoised path must be indistinguishable from the parse
// it replaces, including after an edit.

func exactRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range map[string]string{
		"alpha.go": "package alpha\n\nfunc Marker() int { return 1 }\n",
		"beta.go":  "package alpha\n\nfunc Other() int { return Marker() }\n",
		"gamma.go": "package alpha\n\nfunc Third() int { return 3 }\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func exactQuery(t *testing.T, root, q string) Result {
	t.Helper()
	return Search(context.Background(), Request{
		Profile: ProfileCodeExact, CodeRoot: root, Question: q, TopK: 20, ExactKind: "any",
	})
}

func receipt(t *testing.T, res Result) string {
	t.Helper()
	value, ok := res.RetrievalDiagnostics["receipt_digest"].(string)
	if !ok || value == "" {
		t.Fatalf("no receipt digest: %+v", res.RetrievalDiagnostics)
	}
	return value
}

func TestRepeatedExactQueriesProduceAnIdenticalReceipt(t *testing.T) {
	root := exactRepo(t)
	first := exactQuery(t, root, "Marker")
	second := exactQuery(t, root, "Marker")

	if receipt(t, first) != receipt(t, second) {
		t.Fatalf("the memoised parse changed the receipt:\n%s\n%s",
			receipt(t, first), receipt(t, second))
	}
	if len(first.Hits) == 0 {
		t.Fatal("no hits, so this guard checked nothing")
	}
	if len(first.Hits) != len(second.Hits) {
		t.Fatalf("hit counts differ: %d vs %d", len(first.Hits), len(second.Hits))
	}
	for i := range first.Hits {
		if first.Hits[i] != second.Hits[i] {
			t.Fatalf("hit %d differs:\n%+v\n%+v", i, first.Hits[i], second.Hits[i])
		}
	}
}

// TestAnEditChangesTheExactResult is the property that makes memoising safe.
// The cache is keyed on content, so edited content is a different key -- but
// only a test that edits proves it.
func TestAnEditChangesTheExactResult(t *testing.T) {
	root := exactRepo(t)
	before := exactQuery(t, root, "Marker")

	if err := os.WriteFile(filepath.Join(root, "alpha.go"),
		[]byte("package alpha\n\nfunc Marker() int { return 2 }\n\nfunc MarkerTwo() int { return 3 }\n"),
		0o600); err != nil {
		t.Fatal(err)
	}

	after := exactQuery(t, root, "Marker")
	if receipt(t, before) == receipt(t, after) {
		t.Fatal("an edited file produced the same receipt: the cache is serving " +
			"a projection of content that is no longer on disk")
	}
	if len(after.Hits) <= len(before.Hits) {
		t.Fatalf("the added definition was not found: %d hits before, %d after",
			len(before.Hits), len(after.Hits))
	}
}

// TestProjectionCacheMatchesADirectParse compares the cached value against
// codeindex.Project itself, which is the function it is standing in for.
func TestProjectionCacheMatchesADirectParse(t *testing.T) {
	source := codeindex.SourceFile{
		Path:     "alpha.go",
		Language: codeindex.LanguageGo,
		Content:  []byte("package alpha\n\nfunc Marker() int { return 1 }\n"),
	}
	limits := codeindex.DefaultLimits()
	ctx := context.Background()

	direct, err := codeindex.Project(ctx, source, limits)
	if err != nil {
		t.Fatal(err)
	}
	// Twice: the first populates, the second reads back.
	for round := 0; round < 2; round++ {
		cached, err := projectCached(ctx, source, limits)
		if err != nil {
			t.Fatal(err)
		}
		if cached.ReceiptDigest != direct.ReceiptDigest {
			t.Fatalf("round %d: receipt %q, direct parse gave %q",
				round, cached.ReceiptDigest, direct.ReceiptDigest)
		}
		if len(cached.Occurrences) != len(direct.Occurrences) {
			t.Fatalf("round %d: %d occurrences, direct parse gave %d",
				round, len(cached.Occurrences), len(direct.Occurrences))
		}
	}
}

// TestDifferentLimitsAreDifferentProjections keeps the cache from serving a
// projection produced under limits the caller did not ask for.
func TestDifferentLimitsAreDifferentProjections(t *testing.T) {
	source := codeindex.SourceFile{
		Path:     "alpha.go",
		Language: codeindex.LanguageGo,
		Content:  []byte("package alpha\n\nfunc Marker() int { return 1 }\n"),
	}
	narrow := codeindex.DefaultLimits()
	wide := codeindex.DefaultLimits()
	wide.MaxResults = narrow.MaxResults * 2

	if projectionKey(source, narrow) == projectionKey(source, wide) {
		t.Fatal("two different limit sets share a cache key")
	}
}

func TestConcurrentExactQueriesDoNotRace(t *testing.T) {
	root := exactRepo(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if res := exactQuery(t, root, "Marker"); res.Failure != "" {
					t.Error(res.Failure)
					return
				}
			}
		}()
	}
	wg.Wait()
}
