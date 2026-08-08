package contentprivacy

import (
	"errors"
	"sort"
	"time"
)

// EvaluationCase is offline labeled input for deterministic component
// evaluation. ExpectedFindings are evaluation gold and intentionally live
// outside Input and every production projection/receipt type.
type EvaluationCase struct {
	Name             string    `json:"name"`
	Input            Input     `json:"input"`
	ExpectedFindings []Finding `json:"expected_findings,omitempty"`
	EvaluateDeletion bool      `json:"evaluate_deletion,omitempty"`
}

// Ratio retains exact counts alongside a deterministic finite rate. A metric
// with no denominator reports rate zero and remains distinguishable as 0/0.
type Ratio struct {
	Numerator   uint64  `json:"numerator"`
	Denominator uint64  `json:"denominator"`
	Rate        float64 `json:"rate"`
}

// EvaluationMetrics contains no source text, matched values, or identifiers.
// Precision and recall use exact class+surface+byte-range matches.
// FalseRedactionRate is byte-level over published masked bytes.
// DetectorCoverage requires every expected span of a class to be found.
type EvaluationMetrics struct {
	Cases                      uint64 `json:"cases"`
	Precision                  Ratio  `json:"precision"`
	Recall                     Ratio  `json:"recall"`
	FalseRedactionRate         Ratio  `json:"false_redaction_rate"`
	DetectorCoverage           Ratio  `json:"detector_coverage"`
	DeletionCorrectness        Ratio  `json:"deletion_correctness"`
	CitationToRedactedSpanRate Ratio  `json:"citation_to_redacted_span_rate"`
}

type findingKey struct {
	class   Class
	surface string
	start   int
	end     int
}

// Evaluate deterministically executes Guard admission against offline labels.
// It is a component evaluator, not runtime telemetry and not a production
// quality claim. Cases must have unique scoped content IDs.
func Evaluate(policy Policy, detector Detector, cases []EvaluationCase, clock func() time.Time) (EvaluationMetrics, error) {
	if len(cases) == 0 {
		return EvaluationMetrics{}, ErrInvalid
	}
	guard, err := New(policy, detector, nil, clock)
	if err != nil {
		return EvaluationMetrics{}, err
	}

	var metrics EvaluationMetrics
	classTotals := make(map[Class]uint64)
	classMatches := make(map[Class]uint64)
	seenNames := make(map[string]struct{}, len(cases))
	for _, sample := range cases {
		if !validID(sample.Name) {
			return EvaluationMetrics{}, ErrInvalid
		}
		if _, duplicate := seenNames[sample.Name]; duplicate {
			return EvaluationMetrics{}, ErrInvalid
		}
		seenNames[sample.Name] = struct{}{}
		if err := validateExpectedFindings(sample); err != nil {
			return EvaluationMetrics{}, err
		}

		decision, err := guard.Admit(sample.Input)
		if err != nil {
			return EvaluationMetrics{}, err
		}
		metrics.Cases++
		expected := findingSet(sample.ExpectedFindings)
		actual := findingSet(decision.Findings)
		for key := range actual {
			metrics.Precision.Denominator++
			if _, ok := expected[key]; ok {
				metrics.Precision.Numerator++
			}
		}
		for key := range expected {
			metrics.Recall.Denominator++
			classTotals[key.class]++
			if _, ok := actual[key]; ok {
				metrics.Recall.Numerator++
				classMatches[key.class]++
			}
		}

		if decision.Projection != nil {
			falseBytes, redactedBytes := falseRedactionBytes(decision.Findings, sample.ExpectedFindings)
			metrics.FalseRedactionRate.Numerator += falseBytes
			metrics.FalseRedactionRate.Denominator += redactedBytes
			contentFindings := findingsForSurface(decision.Findings, "content")
			for _, citation := range decision.Projection.Citations {
				metrics.CitationToRedactedSpanRate.Denominator++
				if overlapsFinding(citation.Start, citation.End, contentFindings) {
					metrics.CitationToRedactedSpanRate.Numerator++
				}
			}
		}

		if sample.EvaluateDeletion {
			metrics.DeletionCorrectness.Denominator++
			if deletionIsCorrect(guard, sample.Input) {
				metrics.DeletionCorrectness.Numerator++
			}
		}
	}

	classes := make([]Class, 0, len(classTotals))
	for class := range classTotals {
		classes = append(classes, class)
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i] < classes[j] })
	for _, class := range classes {
		metrics.DetectorCoverage.Denominator++
		if classMatches[class] == classTotals[class] {
			metrics.DetectorCoverage.Numerator++
		}
	}
	metrics.Precision = withRate(metrics.Precision)
	metrics.Recall = withRate(metrics.Recall)
	metrics.FalseRedactionRate = withRate(metrics.FalseRedactionRate)
	metrics.DetectorCoverage = withRate(metrics.DetectorCoverage)
	metrics.DeletionCorrectness = withRate(metrics.DeletionCorrectness)
	metrics.CitationToRedactedSpanRate = withRate(metrics.CitationToRedactedSpanRate)
	return metrics, nil
}

func withRate(ratio Ratio) Ratio {
	if ratio.Denominator != 0 {
		ratio.Rate = float64(ratio.Numerator) / float64(ratio.Denominator)
	}
	return ratio
}

func findingSet(findings []Finding) map[findingKey]struct{} {
	out := make(map[findingKey]struct{}, len(findings))
	for _, finding := range findings {
		out[findingKey{class: finding.Class, surface: finding.Surface, start: finding.Start, end: finding.End}] = struct{}{}
	}
	return out
}

func validateExpectedFindings(sample EvaluationCase) error {
	surfaces := map[string]string{"content": sample.Input.Content}
	for _, claim := range sample.Input.Claims {
		surfaces["claim:"+claim.ID] = claim.Text
	}
	seen := make(map[findingKey]struct{}, len(sample.ExpectedFindings))
	for _, finding := range sample.ExpectedFindings {
		text, ok := surfaces[finding.Surface]
		key := findingKey{class: finding.Class, surface: finding.Surface, start: finding.Start, end: finding.End}
		if !ok || finding.Start < 0 || finding.End <= finding.Start || finding.End > len(text) {
			return ErrInvalid
		}
		if _, supported := localPatterns[finding.Class]; !supported {
			return ErrInvalid
		}
		if _, duplicate := seen[key]; duplicate {
			return ErrInvalid
		}
		seen[key] = struct{}{}
	}
	return nil
}

func falseRedactionBytes(actual, expected []Finding) (uint64, uint64) {
	predicted := make(map[string]map[int]struct{})
	gold := make(map[string]map[int]struct{})
	mark := func(target map[string]map[int]struct{}, finding Finding) {
		if target[finding.Surface] == nil {
			target[finding.Surface] = make(map[int]struct{})
		}
		for offset := finding.Start; offset < finding.End; offset++ {
			target[finding.Surface][offset] = struct{}{}
		}
	}
	for _, finding := range actual {
		mark(predicted, finding)
	}
	for _, finding := range expected {
		mark(gold, finding)
	}
	var falseBytes, total uint64
	for surface, offsets := range predicted {
		for offset := range offsets {
			total++
			if _, sensitive := gold[surface][offset]; !sensitive {
				falseBytes++
			}
		}
	}
	return falseBytes, total
}

func findingsForSurface(findings []Finding, surface string) []Finding {
	out := make([]Finding, 0, len(findings))
	for _, finding := range findings {
		if finding.Surface == surface {
			out = append(out, finding)
		}
	}
	return out
}

func deletionIsCorrect(guard *Guard, input Input) bool {
	if _, err := guard.Tombstone(input.TenantID, input.Scope, input.ID, "evaluation"); err != nil {
		return false
	}
	key := contentKey(input.TenantID, input.Scope, input.ID)
	guard.mu.Lock()
	_, retained := guard.records[key]
	_, tombstoned := guard.tombstones[key]
	guard.mu.Unlock()
	if retained || !tombstoned {
		return false
	}
	if _, err := guard.Projection(input.TenantID, input.Scope, input.ID); !errors.Is(err, ErrDenied) {
		return false
	}
	if _, err := guard.Admit(input); !errors.Is(err, ErrDenied) {
		return false
	}
	for _, stone := range guard.Tombstones() {
		if stone.TenantID == input.TenantID && stone.ContentID == input.ID && stone.ScopeKey == input.Scope.Key() {
			return true
		}
	}
	return false
}
