package hosted

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

type qualityOverheadReceipt struct {
	SchemaVersion string `json:"schema_version"`
	Issue         int    `json:"issue"`
	Method        string `json:"method"`
	Workload      struct {
		Name                 string `json:"name"`
		Path                 string `json:"path"`
		Backend              string `json:"backend"`
		CorpusDocuments      int    `json:"corpus_documents"`
		TopK                 int    `json:"top_k"`
		Synthesis            string `json:"synthesis"`
		WarmupPerVariant     int    `json:"warmup_per_variant"`
		SamplesPerRunVariant int    `json:"samples_per_run_per_variant"`
		Runs                 int    `json:"runs"`
		Order                string `json:"order"`
		BaselineConfig       string `json:"baseline_config"`
		TracingConfig        string `json:"tracing_config"`
	} `json:"workload"`
	Measurement struct {
		Percentile     int     `json:"percentile"`
		Aggregation    string  `json:"aggregation"`
		BaselineP95NS  int64   `json:"baseline_p95_ns"`
		TracedP95NS    int64   `json:"traced_p95_ns"`
		OverheadPct    float64 `json:"overhead_pct"`
		ThresholdPct   float64 `json:"threshold_pct"`
		Passed         bool    `json:"passed"`
		IndependentRun []struct {
			BaselineP95NS int64   `json:"baseline_p95_ns"`
			TracedP95NS   int64   `json:"traced_p95_ns"`
			OverheadPct   float64 `json:"overhead_pct"`
		} `json:"independent_run_p95s"`
	} `json:"measurement"`
	Environment struct {
		GoVersion   string `json:"go_version"`
		GOOS        string `json:"goos"`
		GOARCH      string `json:"goarch"`
		CPU         string `json:"cpu"`
		LogicalCPUs int    `json:"logical_cpus"`
	} `json:"environment"`
	StructuralCaps struct {
		AttributesPerSpan     int   `json:"attributes_per_span"`
		AttributeWritesP95    int   `json:"attribute_writes_p95"`
		StringBytes           int   `json:"string_bytes"`
		Count                 int   `json:"count"`
		EstimatedCostMicroUSD int64 `json:"estimated_cost_micro_usd"`
		Events                int   `json:"events"`
		Links                 int   `json:"links"`
	} `json:"structural_caps"`
	RuntimeThresholdGate bool   `json:"runtime_threshold_gate"`
	Reproduction         string `json:"reproduction"`
	Validation           string `json:"validation"`
	Note                 string `json:"note"`
}

func TestQualityTracingOverheadReceiptSchemaAndClaim(t *testing.T) {
	var schema map[string]any
	decodeQualityEvidenceJSON(t, "docs/stages/stage-09/evidence/quality-tracing-overhead.schema.json", &schema, false)
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" || schema["additionalProperties"] != false {
		t.Fatalf("overhead schema must be draft 2020-12 and closed: %#v", schema)
	}
	required, ok := schema["required"].([]any)
	if !ok {
		t.Fatal("overhead schema missing required fields")
	}
	for _, key := range []string{"schema_version", "workload", "measurement", "environment", "runtime_threshold_gate"} {
		if !containsJSONSchemaString(required, key) {
			t.Fatalf("overhead schema does not require %q", key)
		}
	}
	var receiptValue any
	if err := json.Unmarshal(readQualityEvidenceFile(t, "docs/stages/stage-09/evidence/quality-tracing-overhead.json"), &receiptValue); err != nil {
		t.Fatal(err)
	}
	if err := validateQualityJSONSchema(schema, receiptValue, "$"); err != nil {
		t.Fatalf("quality tracing overhead receipt violates its schema: %v", err)
	}

	var receipt qualityOverheadReceipt
	decodeQualityEvidenceJSON(t, "docs/stages/stage-09/evidence/quality-tracing-overhead.json", &receipt, true)
	if receipt.SchemaVersion != "stage09.quality_tracing_overhead.v2" || receipt.Issue != 310 ||
		receipt.Method != "paired_representative_workload" {
		t.Fatalf("unexpected receipt identity: %+v", receipt)
	}
	if receipt.Workload.Path != "Client.AnswerOpts" || receipt.Workload.CorpusDocuments != 24 ||
		receipt.Workload.TopK != 16 || receipt.Workload.Runs != len(receipt.Measurement.IndependentRun) ||
		receipt.Workload.SamplesPerRunVariant < 1000 {
		t.Fatalf("receipt is not the pinned representative workload: %+v", receipt.Workload)
	}
	if receipt.Measurement.Percentile != 95 || receipt.Measurement.Aggregation != "median_of_independent_run_p95s" {
		t.Fatalf("unexpected p95 aggregation: %+v", receipt.Measurement)
	}

	baselineRuns := make([]int64, 0, len(receipt.Measurement.IndependentRun))
	tracedRuns := make([]int64, 0, len(receipt.Measurement.IndependentRun))
	for i, run := range receipt.Measurement.IndependentRun {
		if run.BaselineP95NS <= 0 || run.TracedP95NS <= 0 {
			t.Fatalf("run %d has non-positive p95: %+v", i, run)
		}
		assertOverheadCalculation(t, run.BaselineP95NS, run.TracedP95NS, run.OverheadPct)
		baselineRuns = append(baselineRuns, run.BaselineP95NS)
		tracedRuns = append(tracedRuns, run.TracedP95NS)
	}
	if got := medianInt64(baselineRuns); got != receipt.Measurement.BaselineP95NS {
		t.Fatalf("baseline p95 median=%d receipt=%d", got, receipt.Measurement.BaselineP95NS)
	}
	if got := medianInt64(tracedRuns); got != receipt.Measurement.TracedP95NS {
		t.Fatalf("traced p95 median=%d receipt=%d", got, receipt.Measurement.TracedP95NS)
	}
	assertOverheadCalculation(t, receipt.Measurement.BaselineP95NS, receipt.Measurement.TracedP95NS, receipt.Measurement.OverheadPct)
	if receipt.Measurement.ThresholdPct != 2 || receipt.Measurement.OverheadPct >= receipt.Measurement.ThresholdPct ||
		!receipt.Measurement.Passed {
		t.Fatalf("recorded tracing p95 overhead must be <2%%: %+v", receipt.Measurement)
	}
	if receipt.RuntimeThresholdGate {
		t.Fatal("wall-clock overhead must remain receipt evidence, not a flaky runtime test gate")
	}
	if receipt.StructuralCaps.AttributesPerSpan != qualityMaxAttributes ||
		receipt.StructuralCaps.AttributeWritesP95 > qualityMaxAttributes ||
		receipt.StructuralCaps.Events != 0 || receipt.StructuralCaps.Links != 0 {
		t.Fatalf("structural caps drifted: %+v", receipt.StructuralCaps)
	}
}

func decodeQualityEvidenceJSON(t *testing.T, relative string, dst any, strict bool) {
	t.Helper()
	raw := readQualityEvidenceFile(t, relative)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(dst); err != nil {
		t.Fatalf("decode %s: %v", relative, err)
	}
}

func readQualityEvidenceFile(t *testing.T, relative string) []byte {
	t.Helper()
	paths := []string{relative}
	if testRoot := os.Getenv("TEST_SRCDIR"); testRoot != "" {
		paths = append([]string{filepath.Join(testRoot, os.Getenv("TEST_WORKSPACE"), relative)}, paths...)
	}
	if wd, err := os.Getwd(); err == nil {
		dir := wd
		for i := 0; i < 8; i++ {
			paths = append(paths, filepath.Join(dir, relative))
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	for _, path := range paths {
		if raw, err := os.ReadFile(path); err == nil {
			return raw
		}
	}
	t.Fatalf("read %s (tried %v)", relative, paths)
	return nil
}

func containsJSONSchemaString(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertOverheadCalculation(t *testing.T, baseline, traced int64, got float64) {
	t.Helper()
	want := 100 * float64(traced-baseline) / float64(baseline)
	if math.Abs(want-got) > 0.0001 {
		t.Fatalf("overhead_pct=%0.4f want %0.4f from baseline=%d traced=%d", got, want, baseline, traced)
	}
}

func medianInt64(values []int64) int64 {
	ordered := append([]int64(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	return ordered[len(ordered)/2]
}

// validateQualityJSONSchema implements the closed subset used by the checked-in
// receipt schema. Keeping this validator local avoids adding a runtime or test
// dependency merely to validate one evidence artifact.
func validateQualityJSONSchema(schema map[string]any, value any, path string) error {
	if expected, ok := schema["const"]; ok && !reflect.DeepEqual(expected, value) {
		return fmt.Errorf("%s=%v, want const %v", path, value, expected)
	}
	typeName, _ := schema["type"].(string)
	switch typeName {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be an object", path)
		}
		if required, ok := schema["required"].([]any); ok {
			for _, rawKey := range required {
				key, _ := rawKey.(string)
				if _, exists := object[key]; !exists {
					return fmt.Errorf("%s missing required property %q", path, key)
				}
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		allowAdditional := true
		if configured, ok := schema["additionalProperties"].(bool); ok {
			allowAdditional = configured
		}
		for key, item := range object {
			rawChild, exists := properties[key]
			if !exists {
				if allowAdditional {
					continue
				}
				return fmt.Errorf("%s has unknown property %q", path, key)
			}
			child, ok := rawChild.(map[string]any)
			if !ok {
				return fmt.Errorf("schema for %s.%s is invalid", path, key)
			}
			if err := validateQualityJSONSchema(child, item, path+"."+key); err != nil {
				return err
			}
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s must be an array", path)
		}
		if minimum, ok := schema["minItems"].(float64); ok && len(items) < int(minimum) {
			return fmt.Errorf("%s has %d items, want at least %d", path, len(items), int(minimum))
		}
		itemSchema, _ := schema["items"].(map[string]any)
		for i, item := range items {
			if err := validateQualityJSONSchema(itemSchema, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s must be a string", path)
		}
		if minimum, ok := schema["minLength"].(float64); ok && len(text) < int(minimum) {
			return fmt.Errorf("%s is shorter than %d", path, int(minimum))
		}
	case "integer":
		number, ok := value.(float64)
		if !ok || math.Trunc(number) != number {
			return fmt.Errorf("%s must be an integer", path)
		}
		if err := validateQualityJSONNumber(schema, number, path); err != nil {
			return err
		}
	case "number":
		number, ok := value.(float64)
		if !ok {
			return fmt.Errorf("%s must be a number", path)
		}
		if err := validateQualityJSONNumber(schema, number, path); err != nil {
			return err
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be a boolean", path)
		}
	}
	return nil
}

func validateQualityJSONNumber(schema map[string]any, number float64, path string) error {
	if minimum, ok := schema["minimum"].(float64); ok && number < minimum {
		return fmt.Errorf("%s=%v is below minimum %v", path, number, minimum)
	}
	if minimum, ok := schema["exclusiveMinimum"].(float64); ok && number <= minimum {
		return fmt.Errorf("%s=%v is not above %v", path, number, minimum)
	}
	return nil
}
