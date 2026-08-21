// Package scmbench is the deterministic, offline benchmark scaffold for the
// local-first SCM code-memory workflow (Phase 0).
//
// It measures what an agent workflow costs through the codeserve protocol —
// response bytes, estimated tokens, tool calls, and latency per step — and
// compares it against a naive "read the whole tree" baseline. Everything is
// local: no network providers, no hosted inference, no wall-clock-dependent
// assertions. Latency is recorded, never asserted.
//
// Token accounting is a fixed heuristic (EstimateTokens): four bytes per
// token, rounded up. It is not a model tokenizer; it is a stable yardstick
// for savings ratios across runs and machines.
package scmbench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codecrawl"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/savings"
)

// placeholderRoot is the sentinel value used to mask absolute paths in
// normalized reports so the serialized output is machine-independent.
const placeholderRoot = "<root>"

// normalizePath replaces an absolute path prefix with a stable placeholder.
func normalizePath(p, root string) string {
	if root == "" || p == "" {
		return p
	}
	p = filepath.ToSlash(p)
	root = filepath.ToSlash(root)
	if p == root {
		return placeholderRoot
	}
	prefix := strings.TrimSuffix(root, "/") + "/"
	if strings.HasPrefix(p, prefix) {
		return placeholderRoot + "/" + p[len(prefix):]
	}
	return p
}

func normalizeValue(value any, roots ...string) any {
	switch v := value.(type) {
	case string:
		for _, root := range roots {
			if normalized := normalizePath(v, root); normalized != v {
				v = normalized
			}
		}
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if key == "duration_ms" {
				out[key] = float64(0)
				continue
			}
			out[key] = normalizeValue(item, roots...)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = normalizeValue(item, roots...)
		}
		return out
	default:
		return value
	}
}

// Step is one protocol call in a workflow scenario.
type Step struct {
	Name string         `json:"name"`
	Verb string         `json:"verb"`
	Args map[string]any `json:"args,omitempty"`
}

// Scenario is a deterministic workflow against one indexed root.
type Scenario struct {
	Name       string `json:"name"`
	Root       string `json:"root"`
	IndexCache string `json:"index_cache"`
	Steps      []Step `json:"steps"`
}

// StepMetric records the measured cost of one step.
type StepMetric struct {
	Name          string `json:"name"`
	Verb          string `json:"verb"`
	OK            bool   `json:"ok"`
	ResponseBytes int    `json:"response_bytes"`
	EstTokens     int    `json:"est_tokens"`
	ToolCalls     int    `json:"tool_calls"`
	DurationMS    int64  `json:"duration_ms"`
}

// Totals aggregates step metrics.
type Totals struct {
	ResponseBytes int   `json:"response_bytes"`
	EstTokens     int   `json:"est_tokens"`
	ToolCalls     int   `json:"tool_calls"`
	DurationMS    int64 `json:"duration_ms"`
}

// Report is the deterministic benchmark artifact (safe to commit/diff).
type Report struct {
	Contract             string       `json:"contract"`
	Scenario             string       `json:"scenario"`
	Steps                []StepMetric `json:"steps"`
	Totals               Totals       `json:"totals"`
	FailedSteps          int          `json:"failed_steps"`
	BaselineBytes        int64        `json:"baseline_bytes"`
	BaselineTokensEst    int          `json:"baseline_tokens_est"`
	SavedBytes           int64        `json:"saved_bytes"`
	SavedTokensEst       int          `json:"saved_tokens_est"`
	TokenSavingsRatioEst float64      `json:"token_savings_ratio_est"`
}

// EstimateTokens is the deterministic offline token yardstick:
// ceil(len/4) over bytes. Empty input estimates to zero.
func EstimateTokens(s string) int {
	return estimateBytes(int64(len(s)))
}

func estimateBytes(n int64) int {
	if n <= 0 {
		return 0
	}
	return int((n + 3) / 4)
}

// NaiveBaselineBytes sums the sizes of all source files the crawler would
// index, using the same extension and repository-ignore policy. It is the
// comparable cost floor of an agent that reads every indexed source file.
func NaiveBaselineBytes(root string) (int64, error) {
	paths, err := codecrawl.SourceFiles(root)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, path := range paths {
		st, err := os.Stat(path)
		if err != nil {
			return 0, err
		}
		if st.Mode().IsRegular() {
			total += st.Size()
		}
	}
	return total, nil
}

// Run executes the scenario through codeserve.Handle and measures each step.
// Root and IndexCache are injected into every request unless the step args
// already set them.
func Run(ctx context.Context, sc Scenario) (Report, error) {
	rep := Report{Contract: codeserve.ContractID, Scenario: sc.Name}
	for _, step := range sc.Steps {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		req := codeserve.Request{"verb": step.Verb}
		if sc.Root != "" {
			req["root"] = sc.Root
		}
		if sc.IndexCache != "" {
			req["index_cache"] = sc.IndexCache
		}
		for k, v := range step.Args {
			req[k] = v
		}
		t0 := time.Now()
		resp := codeserve.Handle(ctx, req)
		dur := time.Since(t0)
		// Normalize path-bearing response fields before accounting so checkout
		// location does not masquerade as token savings.
		wire, err := json.Marshal(resp)
		if err != nil {
			return rep, err
		}
		var decoded any
		if err := json.Unmarshal(wire, &decoded); err != nil {
			return rep, err
		}
		raw, err := json.Marshal(normalizeValue(decoded, sc.Root, sc.IndexCache))
		if err != nil {
			return rep, err
		}
		ok, _ := resp["ok"].(bool)
		m := StepMetric{
			Name:          step.Name,
			Verb:          step.Verb,
			OK:            ok,
			ResponseBytes: len(raw),
			EstTokens:     EstimateTokens(string(raw)),
			ToolCalls:     1,
			DurationMS:    dur.Milliseconds(),
		}
		rep.Steps = append(rep.Steps, m)
		if !m.OK {
			rep.FailedSteps++
		}
		rep.Totals.ResponseBytes += m.ResponseBytes
		rep.Totals.EstTokens += m.EstTokens
		rep.Totals.ToolCalls += m.ToolCalls
		rep.Totals.DurationMS += m.DurationMS
	}
	return rep, nil
}

// Normalize returns a copy of the report suitable for deterministic
// serialization: absolute paths are replaced with stable placeholders
// and timing fields are zeroed. Response-byte savings are unaffected
// because duration_ms is excluded from the normalized step payloads.
func (rep Report) Normalize(root, cache string) Report {
	n := rep
	n.Totals.DurationMS = 0
	steps := make([]StepMetric, len(rep.Steps))
	for i, s := range rep.Steps {
		s.DurationMS = 0
		steps[i] = s
	}
	n.Steps = steps
	// Normalize paths that embed machine-local roots in the scenario label too.
	n.Scenario = normalizePath(normalizePath(n.Scenario, root), cache)
	return n
}

// RecordSavings appends the measured scenario to the optional local savings
// ledger beneath cacheDir. A benchmark baseline covers the whole scenario, so
// the report is recorded as one ledger step rather than double-counting it
// across protocol calls. Calling this method is opt-in and does not change the
// codeserve wire response or the Report JSON shape.
func (rep Report) RecordSavings(cacheDir string) error {
	if rep.FailedSteps > 0 {
		return fmt.Errorf("benchmark has %d failed step(s)", rep.FailedSteps)
	}
	if rep.SavedBytes != rep.BaselineBytes-int64(rep.Totals.ResponseBytes) ||
		rep.SavedTokensEst != rep.BaselineTokensEst-rep.Totals.EstTokens {
		return fmt.Errorf("benchmark baseline has not been measured or is stale")
	}
	ledger, err := savings.Open(cacheDir)
	if err != nil {
		return err
	}
	return ledger.Record(savings.Step{
		Name:      rep.Scenario,
		Category:  savings.CategoryRetrieval,
		Estimator: savings.EstimatorBytesDiv4,
		// The scenario lane's baseline is the whole indexed tree, which is a
		// bound rather than a measurement. Naming it here means a figure
		// produced against it is never silently compared with one produced
		// against the gold-file baseline the QA lane uses.
		BaselineModel:     savings.BaselineWholeTree,
		BaselineBytes:     rep.BaselineBytes,
		ServedBytes:       int64(rep.Totals.ResponseBytes),
		BaselineTokensEst: int64(rep.BaselineTokensEst),
		ServedTokensEst:   int64(rep.Totals.EstTokens),
	})
}

// MeasureBaseline fills the baseline and savings fields against root.
func (rep *Report) MeasureBaseline(root string) error {
	if rep.FailedSteps > 0 {
		return fmt.Errorf("benchmark has %d failed step(s)", rep.FailedSteps)
	}
	b, err := NaiveBaselineBytes(root)
	if err != nil {
		return err
	}
	rep.BaselineBytes = b
	rep.BaselineTokensEst = estimateBytes(b)
	rep.SavedBytes = b - int64(rep.Totals.ResponseBytes)
	rep.SavedTokensEst = rep.BaselineTokensEst - rep.Totals.EstTokens
	if rep.BaselineTokensEst > 0 {
		rep.TokenSavingsRatioEst = float64(rep.SavedTokensEst) / float64(rep.BaselineTokensEst)
	}
	return nil
}
