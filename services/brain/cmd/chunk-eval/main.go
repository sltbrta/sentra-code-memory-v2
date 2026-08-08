package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/chunking"
)

func main() {
	fixturesDir := flag.String("fixtures", "", "golden fixtures dir (documents.jsonl + queries.jsonl); "+
		"defaults to $OUROBOROS_CHUNK_EVAL_FIXTURES or the in-repo testdata/golden")
	reportPath := flag.String("report", "", "write JSON report to this path (default stdout)")
	topK := flag.Int("top-k", 8, "retrieval depth k")
	strategies := flag.String("strategies", "", "comma-separated subset of strategies to evaluate (default all)")
	blind := flag.Bool("blind", false, "ERB-blind comparison mode: label arms arm_a.. and omit policy fingerprints")
	blindKeyPath := flag.String("blind-key", "", "write the blind label -> strategy key to this path (with --blind)")
	flag.Parse()

	dir, err := resolveFixturesDir(*fixturesDir)
	if err != nil {
		fail(err)
	}
	docs, queries, err := loadFixtures(dir)
	if err != nil {
		fail(err)
	}

	policies := chunking.EvalStrategies()
	if *strategies != "" {
		policies, err = filterStrategies(strings.Split(*strategies, ","))
		if err != nil {
			fail(err)
		}
	}
	if *blind && *blindKeyPath == "" {
		fail(fmt.Errorf("--blind requires --blind-key to persist the label key"))
	}

	report, err := Run(ToSourceDocuments(docs), queries, policies, *topK, *blind)
	if err != nil {
		fail(err)
	}

	if *blind {
		if err := writeBlindKey(*blindKeyPath, report); err != nil {
			fail(err)
		}
	}

	out := os.Stdout
	if *reportPath != "" {
		f, err := os.Create(*reportPath)
		if err != nil {
			fail(err)
		}
		defer f.Close()
		out = f
	}
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "chunk-eval: %v\n", err)
	os.Exit(2)
}

// resolveFixturesDir finds the golden set: explicit flag, then env, then the
// in-repo default relative to the working directory.
func resolveFixturesDir(flagValue string) (string, error) {
	candidates := []string{}
	if flagValue != "" {
		candidates = append(candidates, flagValue)
	}
	if env := strings.TrimSpace(os.Getenv("OUROBOROS_CHUNK_EVAL_FIXTURES")); env != "" {
		candidates = append(candidates, env)
	}
	candidates = append(candidates,
		filepath.Join("services", "brain", "cmd", "chunk-eval", "testdata", "golden"),
		filepath.Join("testdata", "golden"),
	)
	for _, c := range candidates {
		if fileExists(filepath.Join(c, "documents.jsonl")) && fileExists(filepath.Join(c, "queries.jsonl")) {
			return c, nil
		}
	}
	return "", fmt.Errorf("no golden fixtures found; pass --fixtures or set OUROBOROS_CHUNK_EVAL_FIXTURES")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// loadFixtures reads the committed golden JSONL files.
func loadFixtures(dir string) ([]FixtureDocument, []FixtureQuery, error) {
	var docs []FixtureDocument
	if err := readJSONL(filepath.Join(dir, "documents.jsonl"), func(d FixtureDocument) {
		docs = append(docs, d)
	}); err != nil {
		return nil, nil, fmt.Errorf("documents.jsonl: %w", err)
	}
	var queries []FixtureQuery
	if err := readJSONL(filepath.Join(dir, "queries.jsonl"), func(q FixtureQuery) {
		queries = append(queries, q)
	}); err != nil {
		return nil, nil, fmt.Errorf("queries.jsonl: %w", err)
	}
	if len(docs) == 0 || len(queries) == 0 {
		return nil, nil, fmt.Errorf("empty golden fixture set in %s", dir)
	}
	return docs, queries, nil
}

func readJSONL[T any](path string, add func(T)) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	for dec.More() {
		var v T
		if err := dec.Decode(&v); err != nil {
			return err
		}
		add(v)
	}
	return nil
}

// filterStrategies maps names to the canonical eval policies.
func filterStrategies(names []string) ([]chunking.Policy, error) {
	known := map[string]chunking.Policy{}
	for _, p := range chunking.EvalStrategies() {
		known[string(p.Strategy)] = p
	}
	var out []chunking.Policy
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		p, ok := known[name]
		if !ok {
			return nil, fmt.Errorf("unknown strategy %q (known: whole_doc, fixed, structure, parent_child)", name)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty strategy filter")
	}
	return out, nil
}

// writeBlindKey persists the label -> strategy mapping so a blind report can
// be opened later; the report itself never carries the mapping.
func writeBlindKey(path string, report *Report) error {
	key := map[string]any{
		"note":        "chunk-eval blind key: maps report arm labels to strategies and policy fingerprints",
		"top_k":       report.TopK,
		"score_class": report.ScoreClass,
		"arms":        map[string]any{},
	}
	arms := key["arms"].(map[string]any)
	for _, s := range report.Strategies {
		if s.BlindLabel == "" {
			return fmt.Errorf("blind report missing label for %s", s.Strategy)
		}
		arms[s.BlindLabel] = map[string]any{"strategy": s.Strategy}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(key)
}
