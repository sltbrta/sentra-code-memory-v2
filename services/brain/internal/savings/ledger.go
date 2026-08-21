// Package savings records deterministic, local token-savings estimates.
//
// Every token figure here is an estimate, and the wire names say so. They were
// spelled baseline_tokens / served_tokens / saved_tokens and carried no
// qualifier anywhere, while being derived from a flat bytes/4 heuristic -- so
// a reader had no way to tell a counted token from an approximated one. The
// suffix is _tokens_est, and each step and summary carries the identity of the
// estimator that produced it, so a figure produced by one estimator is never
// silently compared against a figure produced by another.
package savings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	// Filename is the ledger file stored beneath the caller-provided cache.
	Filename = "token-savings.json"
	// DefaultMaxSteps bounds active per-step history. Older steps are folded
	// into the persisted rollup while totals remain available to callers.
	DefaultMaxSteps = 256
	// version is the on-disk schema. It went to 2 when the token fields were
	// renamed to carry their _est suffix and the estimator identity was added.
	// A ledger written under an earlier version is discarded rather than
	// migrated: it is derived, rebuildable, cache-resident data whose only
	// producer was the benchmark, and silently reinterpreting figures whose
	// estimator is unknown is the confusion the rename exists to prevent.
	version = 2
)

// Category identifies the local optimization measured by a step.
type Category string

const (
	CategoryRetrieval             Category = "retrieval"
	CategoryDedup                 Category = "dedup"
	CategoryCompression           Category = "compression"
	CategorySkeletonization       Category = "skeletonization"
	CategoryProgressiveDisclosure Category = "progressive_disclosure"
	CategoryMemoryRecall          Category = "memory_recall"
	CategorySessionReuse          Category = "session_reuse"
)

// Estimator identifies how a step's token figures were produced. Two figures
// are only comparable when they carry the same estimator.
type Estimator string

const (
	// EstimatorBytesDiv4 is ceil(bytes/4): a flat, model-independent
	// approximation. It is not any model's tokenizer and does not claim to
	// be; on code it is optimistic, because identifiers and punctuation
	// tokenize denser than four bytes to the token.
	EstimatorBytesDiv4 Estimator = "bytes_div_4"
)

// BaselineModel identifies what a step's baseline counts, which decides what
// the saving is a saving against.
type BaselineModel string

const (
	// BaselineWholeTree is "an agent reads every indexed source file". No real
	// agent performs it, so a ratio against it is an upper bound rather than a
	// measurement of anything that happened.
	BaselineWholeTree BaselineModel = "whole_tree"
	// BaselineGoldFiles is the set of files a query's answer actually needed:
	// what an agent would have had to read to answer it without retrieval.
	BaselineGoldFiles BaselineModel = "gold_files"
)

// Step is one recorded unit. Token counts are supplied by the caller so the
// ledger does not silently choose a tokenizer, and the caller names the
// estimator and baseline model it used.
type Step struct {
	Name              string        `json:"name"`
	Category          Category      `json:"category"`
	Estimator         Estimator     `json:"estimator"`
	BaselineModel     BaselineModel `json:"baseline_model"`
	BaselineBytes     int64         `json:"baseline_bytes"`
	ServedBytes       int64         `json:"served_bytes"`
	BaselineTokensEst int64         `json:"baseline_tokens_est"`
	ServedTokensEst   int64         `json:"served_tokens_est"`
	DedupCount        uint64        `json:"dedup_count"`
	CompressionCount  uint64        `json:"compression_count"`
	AvoidedReads      uint64        `json:"avoided_reads"`
	ModelCalls        uint64        `json:"model_calls"`
	AvoidedModelCalls uint64        `json:"avoided_model_calls"`
}

// Totals aggregates byte, token, and event counters.
type Totals struct {
	BaselineBytes     int64  `json:"baseline_bytes"`
	ServedBytes       int64  `json:"served_bytes"`
	SavedBytes        int64  `json:"saved_bytes"`
	BaselineTokensEst int64  `json:"baseline_tokens_est"`
	ServedTokensEst   int64  `json:"served_tokens_est"`
	SavedTokensEst    int64  `json:"saved_tokens_est"`
	DedupCount        uint64 `json:"dedup_count"`
	CompressionCount  uint64 `json:"compression_count"`
	AvoidedReads      uint64 `json:"avoided_reads"`
	ModelCalls        uint64 `json:"model_calls"`
	AvoidedModelCalls uint64 `json:"avoided_model_calls"`
}

// CategorySummary is one deterministic category subtotal.
type CategorySummary struct {
	Category Category `json:"category"`
	Steps    int      `json:"steps"`
	Totals   Totals   `json:"totals"`
}

// Summary is a stable, order-independent view of a ledger.
type Summary struct {
	Version    int               `json:"version"`
	Steps      int               `json:"steps"`
	Totals     Totals            `json:"totals"`
	Categories []CategorySummary `json:"categories,omitempty"`
}

// String renders a stable one-line summary suitable for CLI output.
func (s Summary) String() string {
	categories := make([]string, 0, len(s.Categories))
	for _, category := range s.Categories {
		categories = append(categories, fmt.Sprintf("%s:%d", category.Category, category.Steps))
	}
	return fmt.Sprintf("steps=%d baseline_bytes=%d served_bytes=%d saved_bytes=%d baseline_tokens_est=%d served_tokens_est=%d saved_tokens_est=%d dedup=%d compression=%d avoided_reads=%d model_calls=%d avoided_model_calls=%d categories=%s",
		s.Steps,
		s.Totals.BaselineBytes, s.Totals.ServedBytes, s.Totals.SavedBytes,
		s.Totals.BaselineTokensEst, s.Totals.ServedTokensEst, s.Totals.SavedTokensEst,
		s.Totals.DedupCount, s.Totals.CompressionCount,
		s.Totals.AvoidedReads, s.Totals.ModelCalls, s.Totals.AvoidedModelCalls,
		strings.Join(categories, ","),
	)
}

type diskLedger struct {
	Version int           `json:"version"`
	Steps   []Step        `json:"steps"`
	Rollup  *ledgerRollup `json:"rollup,omitempty"`
}

type ledgerRollup struct {
	Steps      int               `json:"steps"`
	Totals     Totals            `json:"totals"`
	Categories []CategorySummary `json:"categories,omitempty"`
}

// Ledger appends measurements to one project-cache file. A Ledger is safe for
// concurrent use within a process; callers should use one Ledger per file.
type Ledger struct {
	mu               sync.Mutex
	path             string
	steps            []Step
	rolledSteps      int
	rolledTotals     Totals
	rolledCategories []CategorySummary
}

// Open loads the ledger beneath cacheDir. Missing ledgers start empty;
// malformed committed files are rejected and never overwritten.
func Open(cacheDir string) (*Ledger, error) {
	if strings.TrimSpace(cacheDir) == "" {
		return nil, errors.New("savings: cache directory required")
	}
	path := filepath.Join(cacheDir, Filename)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Ledger{path: path}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("savings: read ledger: %w", err)
	}
	var disk diskLedger
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&disk); err != nil {
		// A ledger written under an earlier schema no longer decodes, because
		// the token fields were renamed to carry their _est suffix. It is
		// derived, rebuildable, cache-resident data, so it is discarded rather
		// than migrated: reinterpreting figures whose estimator was never
		// recorded is exactly the confusion the rename removes.
		var probe struct {
			Version int `json:"version"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.Version != version {
			return &Ledger{path: path}, nil
		}
		return nil, fmt.Errorf("savings: decode ledger: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("savings: decode ledger: %w", err)
	}
	if disk.Version != version {
		return nil, fmt.Errorf("savings: unsupported ledger version %d", disk.Version)
	}
	for i, step := range disk.Steps {
		if err := validateStep(step); err != nil {
			return nil, fmt.Errorf("savings: invalid persisted step %d: %w", i, err)
		}
	}
	ledger := &Ledger{path: path, steps: append([]Step(nil), disk.Steps...)}
	if disk.Rollup != nil {
		ledger.rolledSteps = disk.Rollup.Steps
		ledger.rolledTotals = disk.Rollup.Totals
		ledger.rolledCategories = append([]CategorySummary(nil), disk.Rollup.Categories...)
	}
	return ledger, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

// Record atomically persists step before publishing it in memory.
func (l *Ledger) Record(step Step) error {
	if l == nil || l.path == "" {
		return errors.New("savings: nil ledger")
	}
	if err := validateStep(step); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	next := append(append([]Step(nil), l.steps...), step)
	rolledSteps := l.rolledSteps
	rolledTotals := l.rolledTotals
	rolledCategories := append([]CategorySummary(nil), l.rolledCategories...)
	if len(next) > DefaultMaxSteps {
		overflow := len(next) - DefaultMaxSteps
		for _, old := range next[:overflow] {
			addStep(&rolledTotals, old)
			addRollupCategory(&rolledCategories, old)
			rolledSteps++
		}
		next = next[overflow:]
	}
	sort.Slice(rolledCategories, func(i, j int) bool {
		return rolledCategories[i].Category < rolledCategories[j].Category
	})
	var rollup *ledgerRollup
	if rolledSteps > 0 {
		rollup = &ledgerRollup{
			Steps: rolledSteps, Totals: rolledTotals,
			Categories: rolledCategories,
		}
	}
	if err := writeAtomic(l.path, diskLedger{Version: version, Steps: next, Rollup: rollup}); err != nil {
		return err
	}
	l.steps = next
	l.rolledSteps = rolledSteps
	l.rolledTotals = rolledTotals
	l.rolledCategories = rolledCategories
	return nil
}

// Steps returns a copy of all recorded steps in append order.
func (l *Ledger) Steps() []Step {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Step(nil), l.steps...)
}

// Summary returns deterministic totals and category subtotals. Category order
// is lexical and therefore independent of record order.
//
// The live steps and the rollup are snapshotted under a single lock so a
// concurrent Record that folds steps into the rollup cannot double-count them.
func (l *Ledger) Summary() Summary {
	l.mu.Lock()
	steps := append([]Step(nil), l.steps...)
	rolledSteps := l.rolledSteps
	rolledTotals := l.rolledTotals
	rolledCategories := append([]CategorySummary(nil), l.rolledCategories...)
	l.mu.Unlock()
	summary := Summary{Version: version, Steps: rolledSteps + len(steps), Totals: rolledTotals}
	byCategory := make(map[Category]*CategorySummary)
	for _, category := range rolledCategories {
		copy := category
		byCategory[category.Category] = &copy
	}
	for _, step := range steps {
		addStep(&summary.Totals, step)
		category := byCategory[step.Category]
		if category == nil {
			category = &CategorySummary{Category: step.Category}
			byCategory[step.Category] = category
		}
		category.Steps++
		addStep(&category.Totals, step)
	}
	categories := make([]string, 0, len(byCategory))
	for category := range byCategory {
		categories = append(categories, string(category))
	}
	sort.Strings(categories)
	for _, category := range categories {
		summary.Categories = append(summary.Categories, *byCategory[Category(category)])
	}
	return summary
}

func addRollupCategory(categories *[]CategorySummary, step Step) {
	for i := range *categories {
		if (*categories)[i].Category == step.Category {
			(*categories)[i].Steps++
			addStep(&(*categories)[i].Totals, step)
			return
		}
	}
	category := CategorySummary{Category: step.Category, Steps: 1}
	addStep(&category.Totals, step)
	*categories = append(*categories, category)
}

func addStep(total *Totals, step Step) {
	total.BaselineBytes += step.BaselineBytes
	total.ServedBytes += step.ServedBytes
	total.SavedBytes = total.BaselineBytes - total.ServedBytes
	total.BaselineTokensEst += step.BaselineTokensEst
	total.ServedTokensEst += step.ServedTokensEst
	total.SavedTokensEst = total.BaselineTokensEst - total.ServedTokensEst
	total.DedupCount += step.DedupCount
	total.CompressionCount += step.CompressionCount
	total.AvoidedReads += step.AvoidedReads
	total.ModelCalls += step.ModelCalls
	total.AvoidedModelCalls += step.AvoidedModelCalls
}

func validateStep(step Step) error {
	if strings.TrimSpace(step.Name) == "" {
		return errors.New("savings: step name required")
	}
	if strings.TrimSpace(string(step.Category)) == "" {
		return errors.New("savings: step category required")
	}
	if step.BaselineBytes < 0 || step.ServedBytes < 0 || step.BaselineTokensEst < 0 || step.ServedTokensEst < 0 {
		return errors.New("savings: byte and token counts must be non-negative")
	}
	return nil
}

func writeAtomic(path string, disk diskLedger) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("savings: create cache: %w", err)
	}
	raw, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return fmt.Errorf("savings: encode ledger: %w", err)
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(dir, ".token-savings-*.tmp")
	if err != nil {
		return fmt.Errorf("savings: create temporary ledger: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("savings: secure temporary ledger: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		return fmt.Errorf("savings: write temporary ledger: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("savings: sync temporary ledger: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("savings: close temporary ledger: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("savings: commit ledger: %w", err)
	}
	committed = true
	return nil
}
