package codeserve_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codecrawl"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeindex"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/sessionlog"
)

func scipDocument() map[string]any {
	return map[string]any{
		"toolName": "scip-test",
		"occurrences": []any{map[string]any{
			"range":  []any{float64(1), float64(5), float64(1), float64(11)},
			"symbol": "scheme pkg m Anchor.", "symbolRoles": float64(1),
		}},
	}
}

func TestSCIPAndRecallCatalogMetadata(t *testing.T) {
	t.Parallel()
	byName := map[string]codeserve.VerbSpec{}
	for _, spec := range codeserve.CatalogMetadata() {
		byName[spec.Name] = spec
	}
	for verb, alias := range map[string]string{
		"code_ingest_scip": "ingest-scip",
		"session_recall":   "session-recall",
	} {
		spec := byName[verb]
		if spec.Status != codeserve.StatusStable || !containsString(spec.Aliases, alias) {
			t.Fatalf("catalog metadata for %s: %+v", verb, spec)
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestCodeIngestSCIPIsSafeBoundedAndIdempotent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.go"), []byte("package source\nfunc Anchor() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(root, ".cache")
	req := codeserve.Request{
		"verb": "code_ingest_scip", "root": root, "index_cache": cache,
		"path": "source.go", "language": "go", "document": scipDocument(),
	}

	var first, second codeserve.SCIPIngestResponse
	if err := codeserve.DecodeResponse(codeserve.Handle(context.Background(), req), &first); err != nil {
		t.Fatal(err)
	}
	if err := codeserve.DecodeResponse(codeserve.Handle(context.Background(), req), &second); err != nil {
		t.Fatal(err)
	}
	if !first.OK || first.Authority != "scip" || first.Stats.Edges != 1 {
		t.Fatalf("first ingest: %+v", first)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeat ingest drifted:\nfirst=%+v\nsecond=%+v", first, second)
	}

	idx, _, err := codecrawl.Load(filepath.Join(cache, "code-index.gob"))
	if err != nil {
		t.Fatal(err)
	}
	edges := idx.Graph().SortedEdges("source.go")
	scipEdges := 0
	for _, edge := range edges {
		if edge.Authority == codecrawl.AuthoritySCIP {
			scipEdges++
		}
	}
	if scipEdges != 1 {
		t.Fatalf("persisted SCIP edges=%d want 1: %+v", scipEdges, edges)
	}

	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte("package outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.go")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	for name, bad := range map[string]codeserve.Request{
		"missing document":   {"verb": "code_ingest_scip", "root": root, "path": "source.go", "language": "go"},
		"malformed document": {"verb": "code_ingest_scip", "root": root, "path": "source.go", "language": "go", "document": "{bad"},
		"oversized document": {"verb": "code_ingest_scip", "root": root, "path": "source.go", "language": "go", "document": strings.Repeat("x", codeindex.MaxSCIPDocumentBytes+1)},
		"escaping path":      {"verb": "code_ingest_scip", "root": root, "path": "../outside.go", "language": "go", "document": scipDocument()},
		"symlink escape":     {"verb": "code_ingest_scip", "root": root, "path": "escape.go", "language": "go", "document": scipDocument()},
	} {
		t.Run(name, func(t *testing.T) {
			resp := codeserve.Handle(context.Background(), bad)
			if resp["ok"] != false {
				t.Fatalf("accepted malformed request: %+v", resp)
			}
			if _, claimed := resp["authority"]; claimed {
				t.Fatalf("failed ingest claimed SCIP authority: %+v", resp)
			}
		})
	}
}

func TestSessionRecallIsRepoLocalProvenanceFirstAndAbstaining(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, ".sentra")
	w, err := sessionlog.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []sessionlog.Event{
		{Kind: sessionlog.KindRead, FreeText: "Alpha without a pointer", Provenance: sessionlog.Provenance{Confidence: 0.99}},
		{Kind: sessionlog.KindRead, FreeText: "Alpha implementation", Freshness: sessionlog.FreshnessAsOf,
			Provenance: sessionlog.Provenance{Repository: "local", Tree: "tree-1", Path: "source.go", Symbol: "Alpha", Confidence: 0.9}},
	} {
		if _, err := w.Append(event); err != nil {
			t.Fatal(err)
		}
	}

	req := codeserve.Request{
		"verb": "session_recall", "root": root, "dir": ".sentra", "q": "Alpha", "top_k": 10_000,
	}
	var out codeserve.SessionRecallResponse
	if err := codeserve.DecodeResponse(codeserve.Handle(context.Background(), req), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Recall.Abstained || len(out.Recall.Memories) != 1 {
		t.Fatalf("recall: %+v", out)
	}
	if out.Recall.Memories[0].Provenance.Path != "source.go" || out.Limit != sessionlog.MaxRecallResults {
		t.Fatalf("recall did not enforce provenance/bound: %+v", out)
	}

	var abstained codeserve.SessionRecallResponse
	if err := codeserve.DecodeResponse(codeserve.Handle(context.Background(), codeserve.Request{
		"verb": "session_recall", "root": root, "dir": ".sentra", "q": "unrelated",
	}), &abstained); err != nil {
		t.Fatal(err)
	}
	if !abstained.OK || !abstained.Recall.Abstained || len(abstained.Recall.Memories) != 0 {
		t.Fatalf("weak recall must abstain: %+v", abstained)
	}

	outside := t.TempDir()
	malformedDir := filepath.Join(root, "malformed")
	if err := os.Mkdir(malformedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(malformedDir, sessionlog.Filename), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]codeserve.Request{
		"outside repo":  {"verb": "session_recall", "root": root, "dir": outside, "q": "Alpha"},
		"bad threshold": {"verb": "session_recall", "root": root, "dir": ".sentra", "q": "Alpha", "min_confidence": 2.0},
		"malformed log": {"verb": "session_recall", "root": root, "dir": "malformed", "q": "Alpha"},
	} {
		t.Run(name, func(t *testing.T) {
			resp := codeserve.Handle(context.Background(), bad)
			if resp["ok"] != false {
				encoded, _ := json.Marshal(resp)
				t.Fatalf("accepted malformed recall: %s", encoded)
			}
		})
	}
}
