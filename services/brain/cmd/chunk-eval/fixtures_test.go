package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/chunking"
)

const minGoldenQueries = 50

func TestGoldenFixtureShape(t *testing.T) {
	docs, queries := GenerateFixtures()

	if len(queries) < minGoldenQueries {
		t.Fatalf("need at least %d golden fixtures, got %d", minGoldenQueries, len(queries))
	}

	// Every needle must be corpus-unique and live in exactly its gold doc.
	needleDocs := map[string][]string{}
	for _, d := range docs {
		for _, q := range queries {
			for _, g := range q.Gold {
				if g.DocumentID == d.ID && strings.Contains(d.Body, g.Needle) {
					needleDocs[g.Needle] = appendUnique(needleDocs[g.Needle], d.ID)
				}
			}
		}
	}
	for _, q := range queries {
		for _, g := range q.Gold {
			owners := needleDocs[g.Needle]
			if len(owners) != 1 || owners[0] != g.DocumentID {
				t.Fatalf("needle %s should appear only in %s, found %v", g.Needle, g.DocumentID, owners)
			}
		}
		if !strings.Contains(q.Question, q.Gold[0].Needle) {
			t.Errorf("query %s question must contain its needle", q.QueryID)
		}
		if q.Kind == "" {
			t.Errorf("query %s missing kind", q.QueryID)
		}
	}

	// Documents must be long enough that the 500-token baseline produces
	// more than one chunk (otherwise the benchmark degenerates).
	for _, d := range docs {
		if n := chunking.CountTokens(d.Title + " " + d.Body); n < 700 {
			t.Errorf("document %s too small to exercise chunking: %d tokens", d.ID, n)
		}
	}

	// Every fixture kind is represented.
	seen := map[string]bool{}
	for _, d := range docs {
		seen[d.Kind] = true
	}
	for _, k := range chunking.Kinds {
		if !seen[string(k)] {
			t.Errorf("golden corpus misses kind %s", k)
		}
	}
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

func TestGeneratorIsDeterministic(t *testing.T) {
	docs1, queries1 := GenerateFixtures()
	docs2, queries2 := GenerateFixtures()
	a, err := marshalDocuments(docs1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := marshalDocuments(docs2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("document generation is not deterministic")
	}
	qa, err := marshalQueries(queries1)
	if err != nil {
		t.Fatal(err)
	}
	qb, err := marshalQueries(queries2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(qa, qb) {
		t.Fatal("query generation is not deterministic")
	}
}

// TestGoldenFixturesAreCurrent pins the committed JSONL files to the
// generator (fixture rebuild identity). Regenerate with
// OUROBOROS_CHUNK_EVAL_REGEN=1 go test ./... -run TestGoldenFixturesAreCurrent.
func TestGoldenFixturesAreCurrent(t *testing.T) {
	docs, queries := GenerateFixtures()
	wantDocs, err := marshalDocuments(docs)
	if err != nil {
		t.Fatal(err)
	}
	wantQueries, err := marshalQueries(queries)
	if err != nil {
		t.Fatal(err)
	}
	docPath := filepath.Join("testdata", "golden", "documents.jsonl")
	queryPath := filepath.Join("testdata", "golden", "queries.jsonl")

	if os.Getenv("OUROBOROS_CHUNK_EVAL_REGEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(docPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(docPath, wantDocs, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(queryPath, wantQueries, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skip("regenerated golden fixtures")
	}

	gotDocs, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("committed fixtures missing (%v); rerun with OUROBOROS_CHUNK_EVAL_REGEN=1", err)
	}
	gotQueries, err := os.ReadFile(queryPath)
	if err != nil {
		t.Fatalf("committed fixtures missing (%v); rerun with OUROBOROS_CHUNK_EVAL_REGEN=1", err)
	}
	if !bytes.Equal(gotDocs, wantDocs) {
		t.Fatal("documents.jsonl is stale; rerun with OUROBOROS_CHUNK_EVAL_REGEN=1")
	}
	if !bytes.Equal(gotQueries, wantQueries) {
		t.Fatal("queries.jsonl is stale; rerun with OUROBOROS_CHUNK_EVAL_REGEN=1")
	}
}

func TestLoadFixturesRoundTrip(t *testing.T) {
	docs, queries, err := loadFixtures(filepath.Join("testdata", "golden"))
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) == 0 || len(queries) < minGoldenQueries {
		t.Fatalf("loaded %d docs / %d queries; want >= 1 doc / %d queries",
			len(docs), len(queries), minGoldenQueries)
	}
	srcs := ToSourceDocuments(docs)
	if len(srcs) != len(docs) {
		t.Fatalf("ToSourceDocuments dropped documents: %d -> %d", len(docs), len(srcs))
	}
}
