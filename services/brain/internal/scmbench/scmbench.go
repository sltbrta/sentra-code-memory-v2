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
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

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
	Contract          string       `json:"contract"`
	Scenario          string       `json:"scenario"`
	Steps             []StepMetric `json:"steps"`
	Totals            Totals       `json:"totals"`
	BaselineBytes     int64        `json:"baseline_bytes"`
	BaselineTokens    int          `json:"baseline_tokens"`
	SavedBytes        int64        `json:"saved_bytes"`
	SavedTokens       int          `json:"saved_tokens"`
	TokenSavingsRatio float64      `json:"token_savings_ratio"`
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

// NaiveBaselineBytes sums the sizes of all regular files under root,
// skipping dot-directories (.git, .sentra, .ouroboros, …). It is the cost
// floor of an agent that reads the whole tree instead of using the index.
func NaiveBaselineBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// Run executes the scenario through codeserve.Handle and measures each step.
// Root and IndexCache are injected into every request unless the step args
// already set them.
func Run(ctx context.Context, sc Scenario) (Report, error) {
	rep := Report{Contract: codeserve.ContractID, Scenario: sc.Name}
	for _, step := range sc.Steps {
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
		raw, err := json.Marshal(resp)
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
		rep.Totals.ResponseBytes += m.ResponseBytes
		rep.Totals.EstTokens += m.EstTokens
		rep.Totals.ToolCalls += m.ToolCalls
		rep.Totals.DurationMS += m.DurationMS
	}
	return rep, nil
}

// MeasureBaseline fills the baseline and savings fields against root.
func (rep *Report) MeasureBaseline(root string) error {
	b, err := NaiveBaselineBytes(root)
	if err != nil {
		return err
	}
	rep.BaselineBytes = b
	rep.BaselineTokens = estimateBytes(b)
	rep.SavedBytes = b - int64(rep.Totals.ResponseBytes)
	rep.SavedTokens = rep.BaselineTokens - rep.Totals.EstTokens
	if rep.BaselineTokens > 0 {
		rep.TokenSavingsRatio = float64(rep.SavedTokens) / float64(rep.BaselineTokens)
	}
	return nil
}
