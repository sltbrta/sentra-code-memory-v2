package contentprivacy_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/contentprivacy"
)

type evaluationDetector map[string][]contentprivacy.Finding

func (d evaluationDetector) Detect(text string, _ []contentprivacy.Class) ([]contentprivacy.Finding, error) {
	return append([]contentprivacy.Finding(nil), d[text]...), nil
}

func TestEvaluateDeterministicPrivacyMetrics(t *testing.T) {
	policy := basePolicy()
	individual := policy.Scopes[contentprivacy.ScopeIndividual]
	individual.Actions[contentprivacy.ClassAPIKey] = contentprivacy.ActionRedact
	policy.Scopes[contentprivacy.ScopeIndividual] = individual
	scope := contentprivacy.Scope{Kind: contentprivacy.ScopeIndividual, ID: "alice"}

	first := "mail alice@example.com token"
	emailStart := strings.Index(first, "alice@example.com")
	tokenStart := strings.Index(first, "token")
	second := "bob@example.com"
	third := "sk_1234567890abcd"
	detector := evaluationDetector{
		first: {
			{Class: contentprivacy.ClassEmail, Start: emailStart, End: emailStart + len("alice@example.com")},
			{Class: contentprivacy.ClassAPIKey, Start: tokenStart, End: tokenStart + len("token")},
		},
		third: {{Class: contentprivacy.ClassAPIKey, Start: 0, End: len(third)}},
	}
	cases := []contentprivacy.EvaluationCase{
		{
			Name: "one", Input: contentprivacy.Input{
				TenantID: "acme", ID: "one", Scope: scope, Content: first, Blind: true,
				Citations: []contentprivacy.Citation{
					{ID: "safe", Start: 0, End: len("mail"), Quote: "mail"},
					{ID: "sensitive", Start: emailStart, End: emailStart + len("alice@example.com"), Quote: "alice@example.com"},
				},
			},
			ExpectedFindings: []contentprivacy.Finding{{Class: contentprivacy.ClassEmail, Surface: "content", Start: emailStart, End: emailStart + len("alice@example.com")}},
			EvaluateDeletion: true,
		},
		{
			Name: "two", Input: contentprivacy.Input{TenantID: "acme", ID: "two", Scope: scope, Content: second},
			ExpectedFindings: []contentprivacy.Finding{{Class: contentprivacy.ClassEmail, Surface: "content", Start: 0, End: len(second)}},
			EvaluateDeletion: true,
		},
		{
			Name: "three", Input: contentprivacy.Input{TenantID: "acme", ID: "three", Scope: scope, Content: third},
			ExpectedFindings: []contentprivacy.Finding{{Class: contentprivacy.ClassAPIKey, Surface: "content", Start: 0, End: len(third)}},
			EvaluateDeletion: true,
		},
	}
	clock := func() time.Time { return testNow }
	got, err := contentprivacy.Evaluate(policy, detector, cases, clock)
	if err != nil {
		t.Fatal(err)
	}
	wantFalseDenominator := uint64(len("alice@example.com") + len("token") + len(third))
	if got.Cases != 3 || got.Precision.Numerator != 2 || got.Precision.Denominator != 3 || got.Precision.Rate != 2.0/3.0 {
		t.Fatalf("precision metrics = %+v", got)
	}
	if got.Recall.Numerator != 2 || got.Recall.Denominator != 3 || got.Recall.Rate != 2.0/3.0 {
		t.Fatalf("recall metrics = %+v", got)
	}
	if got.FalseRedactionRate.Numerator != uint64(len("token")) || got.FalseRedactionRate.Denominator != wantFalseDenominator || got.FalseRedactionRate.Rate != float64(len("token"))/float64(wantFalseDenominator) {
		t.Fatalf("false-redaction metrics = %+v", got.FalseRedactionRate)
	}
	if got.DetectorCoverage.Numerator != 1 || got.DetectorCoverage.Denominator != 2 || got.DetectorCoverage.Rate != 0.5 {
		t.Fatalf("coverage metrics = %+v", got.DetectorCoverage)
	}
	if got.DeletionCorrectness.Numerator != 3 || got.DeletionCorrectness.Denominator != 3 || got.DeletionCorrectness.Rate != 1 {
		t.Fatalf("deletion metrics = %+v", got.DeletionCorrectness)
	}
	if got.CitationToRedactedSpanRate.Numerator != 0 || got.CitationToRedactedSpanRate.Denominator != 1 || got.CitationToRedactedSpanRate.Rate != 0 {
		t.Fatalf("citation metrics = %+v", got.CitationToRedactedSpanRate)
	}

	again, err := contentprivacy.Evaluate(policy, detector, cases, clock)
	if err != nil || !reflect.DeepEqual(got, again) {
		t.Fatalf("evaluation is not deterministic: %+v / %+v / %v", got, again, err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"alice@example.com", "bob@example.com", third, "acme"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("metrics leaked evaluation payload %q: %s", secret, encoded)
		}
	}
}

func TestEvaluateRejectsMalformedOrRuntimeEmbeddedGold(t *testing.T) {
	policy := basePolicy()
	scope := contentprivacy.Scope{Kind: contentprivacy.ScopeIndividual, ID: "alice"}
	_, err := contentprivacy.Evaluate(policy, nil, []contentprivacy.EvaluationCase{{
		Name: "bad", Input: contentprivacy.Input{TenantID: "acme", ID: "bad", Scope: scope, Content: "short"},
		ExpectedFindings: []contentprivacy.Finding{{Class: contentprivacy.ClassEmail, Surface: "content", Start: 0, End: 99}},
	}}, func() time.Time { return testNow })
	if err != contentprivacy.ErrInvalid {
		t.Fatalf("malformed gold = %v", err)
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(contentprivacy.Input{}), reflect.TypeOf(contentprivacy.Projection{}), reflect.TypeOf(contentprivacy.Receipt{})} {
		if _, exists := typ.FieldByName("ExpectedFindings"); exists {
			t.Fatalf("evaluation gold leaked into runtime type %s", typ.Name())
		}
	}
}
