package outcomes_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/outcomes"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

func TestAdmit_MachineObservation(t *testing.T) {
	t.Parallel()
	store := outcomes.New()
	req := baseRequest()
	fact, err := store.Admit(req)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if fact.AuthorityClass != outcomes.AuthorityMachineObservation {
		t.Fatalf("class=%s", fact.AuthorityClass)
	}
	if !fact.RawTraceSeparated {
		t.Fatal("raw trace not separated")
	}
	if fact.OutcomeBundleDigest.Hex == "" || fact.Receipt.Status != "completed" {
		t.Fatalf("fact: %+v", fact)
	}
	got, ok := store.Get(req.Tenant, req.FactID)
	if !ok || got.FactID != fact.FactID {
		t.Fatal("get missing")
	}
}

func TestAdmit_ExactReplay(t *testing.T) {
	t.Parallel()
	store := outcomes.New()
	req := baseRequest()
	first, err := store.Admit(req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Admit(req)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed {
		t.Fatal("expected replay")
	}
	if second.OutcomeBundleDigest.Hex != first.OutcomeBundleDigest.Hex {
		t.Fatal("digest diverge")
	}
}

func TestAdmit_RejectsModelProposal(t *testing.T) {
	t.Parallel()
	store := outcomes.New()
	req := baseRequest()
	req.AuthorityClass = outcomes.AuthorityModelProposal
	_, err := store.Admit(req)
	if !errors.Is(err, outcomes.ErrSanitizationFailed) {
		t.Fatalf("err=%v", err)
	}
}

func TestAdmit_RejectsRawTraceNotSeparated(t *testing.T) {
	t.Parallel()
	store := outcomes.New()
	req := baseRequest()
	req.RawTraceSeparated = false
	_, err := store.Admit(req)
	if !errors.Is(err, outcomes.ErrSanitizationFailed) {
		t.Fatalf("err=%v", err)
	}
}

func TestAdmit_RejectsPromptInBundle(t *testing.T) {
	t.Parallel()
	store := outcomes.New()
	req := baseRequest()
	req.OutcomeBundle = []byte(`{"prompt":"leak me","result":"ok"}`)
	_, err := store.Admit(req)
	if !errors.Is(err, outcomes.ErrSanitizationFailed) {
		t.Fatalf("err=%v", err)
	}
	if _, ok := store.Get(req.Tenant, req.FactID); ok {
		t.Fatal("must not admit")
	}
}

func TestAdmit_RejectsElevatedEvidenceClass(t *testing.T) {
	t.Parallel()
	store := outcomes.New()
	req := baseRequest()
	req.Evidence = []outcomes.EvidenceRef{{
		EvidenceID:     "ev-1",
		Digest:         digest("cc"),
		AuthorityClass: outcomes.AuthorityModelProposal,
	}}
	_, err := store.Admit(req)
	if !errors.Is(err, outcomes.ErrSanitizationFailed) {
		t.Fatalf("err=%v", err)
	}
}

func TestAdmit_IdempotencyConflict(t *testing.T) {
	t.Parallel()
	store := outcomes.New()
	req := baseRequest()
	if _, err := store.Admit(req); err != nil {
		t.Fatal(err)
	}
	req.OutcomeBundle = []byte(`{"observation":"different","authority_class":"machine_observation"}`)
	_, err := store.Admit(req)
	if !errors.Is(err, outcomes.ErrConflict) {
		t.Fatalf("err=%v", err)
	}
}

func TestSanitizeCheck_ModelProposalMarker(t *testing.T) {
	t.Parallel()
	err := outcomes.SanitizeCheck([]byte(`{"authority_class":"model_proposal","note":"x"}`))
	if !errors.Is(err, outcomes.ErrSanitizationFailed) {
		t.Fatalf("err=%v", err)
	}
}

func TestRetainRawTrace_RequiresSeparation(t *testing.T) {
	t.Parallel()
	err := outcomes.RetainRawTrace(outcomes.RawTraceRecord{
		TraceID:              "trace-1",
		Scope:                "restricted/traces",
		Digest:               digest("dd"),
		SeparatedFromOutcome: false,
	})
	if !errors.Is(err, outcomes.ErrSanitizationFailed) {
		t.Fatalf("err=%v", err)
	}
	if err := outcomes.RetainRawTrace(outcomes.RawTraceRecord{
		TraceID:              "trace-1",
		Scope:                "restricted/traces",
		Digest:               digest("dd"),
		SeparatedFromOutcome: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAdmit_SanitizedJSONOK(t *testing.T) {
	t.Parallel()
	store := outcomes.New()
	payload, _ := json.Marshal(map[string]any{
		"schema":          "tracer-001/outcome/v1",
		"authority_class": "machine_observation",
		"draft_pr_id":     "PR_kw_1",
		"gates_passed":    true,
		"observation":     "draft PR created; gates green",
	})
	req := baseRequest()
	req.OutcomeBundle = payload
	if _, err := store.Admit(req); err != nil {
		t.Fatal(err)
	}
}

func baseRequest() outcomes.AdmitRequest {
	return outcomes.AdmitRequest{
		Tenant:         "tenant-synthetic-a",
		Principal:      "principal-a",
		FactID:         "outcome-1",
		AuthorityClass: outcomes.AuthorityMachineObservation,
		OutcomeBundle: []byte(
			`{"schema":"tracer-001/outcome/v1","authority_class":"machine_observation","result":"draft_pr_created"}`,
		),
		DraftPrReceiptDigest: digest("bb"),
		Evidence: []outcomes.EvidenceRef{{
			EvidenceID:     "ev-gate",
			Digest:         digest("aa"),
			AuthorityClass: outcomes.AuthorityMachineObservation,
		}},
		RawTraceSeparated: true,
		IdempotencyKey:    "outcome-idem-1",
	}
}

func digest(seed string) contracts.Digest {
	// Expand seed to 64 hex chars deterministically for tests.
	hex := ""
	for len(hex) < 64 {
		hex += seed
	}
	return contracts.Digest{Algorithm: "sha256", Hex: hex[:64]}
}
