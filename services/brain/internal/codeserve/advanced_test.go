package codeserve_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/workflow"
)

func TestRepoMapStructuralAndDiagnosticsSurfaces(t *testing.T) {
	root := t.TempDir()
	body := "package demo\n\nfunc Alpha() {}\nfunc Beta() { Alpha() }\n"
	if err := os.WriteFile(filepath.Join(root, "demo.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(t.TempDir(), "cache")
	ctx := context.Background()
	if got := codeserve.Handle(ctx, codeserve.Request{"verb": "code_index", "root": root, "index_cache": cache}); got["ok"] != true {
		t.Fatal(got)
	}

	m := codeserve.Handle(ctx, codeserve.Request{"verb": "code_repo_map", "root": root, "index_cache": cache, "no_refresh": true, "q": "Alpha", "mode": "quality", "max_bytes": 256})
	if m["ok"] != true || m["authority"] != "heuristic" {
		t.Fatalf("repo map: %+v", m)
	}
	if text, _ := m["map"].(string); !strings.Contains(text, "demo.go") || len(text) > 256 {
		t.Fatalf("unbounded/bad map %q", text)
	}

	s := codeserve.Handle(ctx, codeserve.Request{"verb": "code_structural_search", "root": root, "pattern": "func $NAME()", "max_matches": 4, "max_bytes": 256})
	if s["ok"] != true || s["authority"] != "heuristic" {
		t.Fatalf("structural: %+v", s)
	}
	if matches, _ := s["matches"].([]codeserve.StructuralMatch); len(matches) == 0 || len(matches) > 4 {
		t.Fatalf("matches: %#v", s["matches"])
	}

	d := codeserve.Handle(ctx, codeserve.Request{"verb": "code_diagnostics", "root": root, "index_cache": cache, "no_refresh": true, "mode": "fast"})
	if d["ok"] != true || d["authority"] != "heuristic" {
		t.Fatalf("diagnostics: %+v", d)
	}
}

func TestApplyChangeSetSurface(t *testing.T) {
	root := t.TempDir()
	before := []byte("package demo\nfunc Alpha() {}\n")
	if err := os.WriteFile(filepath.Join(root, "demo.go"), before, 0o644); err != nil {
		t.Fatal(err)
	}
	base := workflow.Digest(before)
	after := []byte("package demo\nfunc Beta() {}\n")
	cs := workflow.ChangeSet{Base: "tree", BaseDigests: map[string]string{"demo.go": base}, Edits: []workflow.CandidateEdit{{Path: "demo.go", Range: workflow.EditRange{Start: 18, End: 23}, Replacement: "Beta", BaseDigest: base, PredictedDigest: workflow.Digest(after)}}}
	// code_apply_changeset is operator-trust gated at the dispatch point, so the
	// surface test grants trust the way the direct CLI does.
	got := codeserve.Handle(codeserve.WithOperatorTrust(context.Background()), codeserve.Request{"verb": "code_apply_changeset", "root": root, "changeset": cs})
	if got["ok"] != true || got["reindexed"] != true || got["index_matches"] != true {
		t.Fatalf("apply surface: %+v", got)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "demo.go"))
	if string(raw) != string(after) {
		t.Fatalf("not promoted: %q", raw)
	}
	if strings.Contains(fmt.Sprint(got["receipt"]), "Beta") {
		t.Fatalf("receipt leaked replacement: %+v", got["receipt"])
	}
}

func TestAdvancedModesFailClosed(t *testing.T) {
	for _, verb := range []string{"code_repo_map", "code_structural_search", "code_diagnostics"} {
		req := codeserve.Request{"verb": verb, "root": t.TempDir(), "mode": "escape"}
		if verb == "code_repo_map" {
			req["q"] = "x"
		}
		if verb == "code_structural_search" {
			req["pattern"] = "x"
		}
		if got := codeserve.Handle(context.Background(), req); got["ok"] != false {
			t.Fatalf("%s accepted mode: %+v", verb, got)
		}
	}
}
