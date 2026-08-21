// Command bench-code is the deterministic, offline code-retrieval benchmark
// gate (issue #48). It measures hit@1/5/10, latency, context/token savings,
// and failure classification on a checked-in fixture, runs a multi-client
// CLI/HTTP/MCP smoke matrix, checks regression thresholds against a checked-in
// baseline, and emits an artifact with a stable digest. It needs no
// credentials or external services.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/scmbench"
)

// defaultFixture is the checked-in qafixture corpus relative to the repo root.
const defaultFixture = "services/brain/internal/scmbench/testdata/qafixture"

// defaultBaseline is the checked-in deterministic baseline (digest+thresholds).
const defaultBaseline = "services/brain/internal/scmbench/testdata/qafixture-baseline.json"

// defaultOut records the artifact under the gitignored local-CI log dir so a
// run leaves a reviewable report without churning the working tree.
const defaultOut = ".local-agent-ci/logs/bench-code-report.json"

// BenchReport is the full benchmark artifact.
type BenchReport struct {
	Contract string            `json:"contract"`
	Tool     string            `json:"tool"`
	Fixture  string            `json:"fixture"`
	Lane     string            `json:"lane"`
	QA       scmbench.QAReport `json:"qa"`
	Smoke    SmokeMatrix       `json:"smoke"`

	BaselinePresent bool   `json:"baseline_present"`
	BaselineDigest  string `json:"baseline_digest,omitempty"`
	BaselineMatch   bool   `json:"baseline_match"`

	Pass   bool   `json:"pass"`
	Digest string `json:"digest"`
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("bench-code", flag.ContinueOnError)
	fixture := fs.String("fixture", defaultFixture, "path to the checked-in QA fixture corpus")
	baseline := fs.String("baseline", defaultBaseline, "path to the checked-in baseline (digest+thresholds)")
	out := fs.String("out", defaultOut, "write the JSON artifact here ('-' = stdout)")
	writeBaseline := fs.String("write-baseline", "", "write the current deterministic baseline to this path and exit")
	quiet := fs.Bool("quiet", false, "only print the pass/fail line and digest")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	root, err := filepath.Abs(*fixture)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench-code: resolve fixture: %v\n", err)
		return 2
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		fmt.Fprintf(os.Stderr, "bench-code: fixture not found at %s (run from the repo root or pass --fixture)\n", root)
		return 2
	}

	cache, err := os.MkdirTemp("", "bench-code-index-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench-code: cache: %v\n", err)
		return 1
	}
	defer os.RemoveAll(cache)

	ctx := context.Background()

	// Retrieval QA suite.
	suite := scmbench.QAFixtureSuite(root, cache)
	qa, err := scmbench.RunQA(ctx, suite)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench-code: qa: %v\n", err)
		return 1
	}
	if err := qa.MeasureQABaseline(root, suite.Queries); err != nil {
		fmt.Fprintf(os.Stderr, "bench-code: baseline: %v\n", err)
		return 1
	}
	qa.CheckThresholds(scmbench.QAFixtureThresholds())

	// Regenerate the checked-in baseline on request.
	if *writeBaseline != "" {
		return writeBaselineFile(scmbench.BaselineFromReport(qa), *writeBaseline)
	}

	// Multi-client smoke matrix.
	smoke, err := runSmokeMatrix(ctx, root, cache)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench-code: smoke: %v\n", err)
		return 1
	}

	report := BenchReport{
		Contract: codeserve.ContractID,
		Tool:     "bench-code",
		Fixture:  "qafixture",
		Lane:     qa.Lane,
		QA:       qa,
		Smoke:    smoke,
		Pass:     qa.Pass && smoke.Pass,
		Digest:   qa.Digest,
	}

	// Compare against the checked-in baseline when present. A digest change is
	// surfaced (it marks a deliberate retrieval diff) but the hard gate is the
	// threshold check above; a missing baseline is reported, not fatal.
	if base, ok := loadBaseline(*baseline); ok {
		report.BaselinePresent = true
		report.BaselineDigest = base.Digest
		report.BaselineMatch = base.Digest == qa.Digest
	}

	if !*quiet {
		printSummary(report)
	} else {
		fmt.Printf("pass=%v digest=%s baseline_match=%v\n", report.Pass, report.Digest, report.BaselineMatch)
	}

	if err := writeArtifact(report, *out); err != nil {
		fmt.Fprintf(os.Stderr, "bench-code: write artifact: %v\n", err)
		return 1
	}

	if !report.Pass {
		fmt.Fprintln(os.Stderr, "bench-code: regression gate FAILED")
		return 1
	}
	return 0
}

func loadBaseline(path string) (scmbench.Baseline, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return scmbench.Baseline{}, false
	}
	var base scmbench.Baseline
	if err := json.Unmarshal(raw, &base); err != nil {
		return scmbench.Baseline{}, false
	}
	return base, true
}

func writeBaselineFile(base scmbench.Baseline, path string) int {
	raw, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench-code: encode baseline: %v\n", err)
		return 1
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "bench-code: baseline dir: %v\n", err)
		return 1
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "bench-code: write baseline: %v\n", err)
		return 1
	}
	fmt.Printf("bench-code: baseline written to %s (digest=%s)\n", path, base.Digest)
	return 0
}

func printSummary(rep BenchReport) {
	qa := rep.QA
	fmt.Println("bench-code: offline retrieval benchmark (local heuristic lane)")
	fmt.Printf("  fixture=%s lane=%s contract=%s\n", rep.Fixture, rep.Lane, rep.Contract)
	fmt.Printf("  hit@1=%.2f hit@5=%.2f hit@10=%.2f precision=%.2f queries=%d\n",
		qa.HitRateAt1, qa.HitRateAt5, qa.HitRateAt10, qa.MeanPrecision, len(qa.Queries))
	fmt.Printf("  p95_latency=%dms mean_latency=%.1fms\n", qa.P95LatencyMS, qa.MeanLatencyMS)
	// Both baselines, both named. The whole-tree ratio is a bound -- no agent
	// reads every indexed file -- and printing it alone as "token_savings"
	// invited it to be read as a measurement. The gold-file ratio is what the
	// queries actually needed.
	fmt.Printf("  token_savings_est=%.2f (estimator=%s baseline=%s) gold_baseline_savings_est=%.2f\n",
		qa.TokenSavingsRatioEst, qa.Estimator, qa.BaselineModel, qa.GoldTokenSavingsRatioEst)
	fmt.Printf("  failures=%v\n", qa.Failures)
	fmt.Printf("  smoke_matrix pass=%v (%d probes)\n", rep.Smoke.Pass, len(rep.Smoke.Results))
	if rep.BaselinePresent {
		fmt.Printf("  baseline match=%v (baseline=%s)\n", rep.BaselineMatch, rep.BaselineDigest)
		if !rep.BaselineMatch {
			fmt.Printf("  NOTE: digest differs from checked-in baseline; if intentional, run: go run ./services/brain/cmd/bench-code --write-baseline %s\n", defaultBaseline)
		}
	} else {
		fmt.Println("  baseline: not found (thresholds gate only)")
	}
	if len(qa.Reasons) > 0 {
		for _, r := range qa.Reasons {
			fmt.Printf("  threshold violation: %s\n", r)
		}
	}
	fmt.Printf("  digest=%s pass=%v\n", rep.Digest, rep.Pass)
}

func writeArtifact(rep BenchReport, out string) error {
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if out == "-" {
		_, err := os.Stdout.Write(raw)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(out, raw, 0o644)
}
