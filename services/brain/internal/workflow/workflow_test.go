package workflow_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/workflow"
)

func TestEnvelopeDeterministicJSON(t *testing.T) {
	t.Parallel()
	makeEnv := func() workflow.ActionEnvelope {
		actions := []workflow.Action{
			{Verb: "code_impact", Summary: "check blast radius", Priority: 2},
			{Verb: "code_read", Summary: "read neighborhood", Priority: 1},
			{Verb: "code_read", Summary: "read neighborhood", Priority: 1}, // dup
			{Verb: "code_find_route", Summary: "find route", Priority: 3},
		}
		handles := []workflow.ExpansionHandle{
			{Kind: "symbol", Symbol: "Alpha", Path: "a.go"},
			{Kind: "symbol", Symbol: "Alpha", Path: "a.go"}, // dup
			{Kind: "callers", Symbol: "helper"},
		}
		return workflow.BuildEnvelope("code_find_relevant", true,
			map[string]any{"hits": 3}, actions, handles,
			[]string{"unknown symbol Beta"},
			workflow.EnvelopeOptions{
				Budget:               workflow.Budget{Total: 8192},
				Confidence:           0.9,
				Freshness:            "as_of",
				VerificationCommands: []string{"go test ./...", "go vet ./..."},
			})
	}
	a := makeEnv()
	b := makeEnv()
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if !bytes.Equal(aj, bj) {
		t.Fatalf("envelope JSON not deterministic\na=%s\nb=%s", aj, bj)
	}
	// Dedup + priority ordering invariants.
	if len(a.Actions) != 3 {
		t.Fatalf("want 3 deduped actions, got %d", len(a.Actions))
	}
	if a.Actions[0].Verb != "code_read" {
		t.Fatalf("first action must be highest priority: %+v", a.Actions[0])
	}
	if len(a.ExpansionHandles) != 2 {
		t.Fatalf("want 2 deduped handles, got %d", len(a.ExpansionHandles))
	}
	if len(a.VerificationCommands) != 2 {
		t.Fatalf("want 2 verification commands, got %d", len(a.VerificationCommands))
	}
	// Remaining budget must reflect payload size.
	if a.RemainingBudget.Used == 0 || a.RemainingBudget.Remaining() <= 0 {
		t.Fatalf("remaining budget not computed: %+v", a.RemainingBudget)
	}
}

func TestEvidenceDeterministicDigest(t *testing.T) {
	t.Parallel()
	makeReport := func() workflow.Report {
		return workflow.Build(workflow.Report{
			ContextServed: workflow.ReportCounts{"bytes": 240, "tokens": 60},
			Impact: workflow.ImpactSummary{
				Severity: "medium",
				Direct:   []string{"b.go", "a.go"}, // unsorted input
				Closure:  []string{"c.go", "a.go"},
			},
			Tests: workflow.TestSummary{Run: 3, Passed: 3, Files: []string{"b_test.go", "a_test.go"}},
			Edits: []workflow.FileEdit{
				{Path: "z.go", AfterDigest: "d2", BeforeDigest: "d1"},
				{Path: "a.go", AfterDigest: "d3"},
			},
			UnknownCoverage: []string{"Beta", "Alpha"},
			Verification: []workflow.Verification{
				{Command: "go vet ./...", Passed: true},
				{Command: "go test ./...", Passed: true},
			},
		}, workflow.BuildOptions{Task: "phase4-5", Base: "abc123"})
	}
	a := makeReport()
	b := makeReport()
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if !bytes.Equal(aj, bj) {
		t.Fatalf("report JSON not deterministic\na=%s\nb=%s", aj, bj)
	}
	if a.Digest == "" {
		t.Fatal("report digest must be set")
	}
	// Digests are content-stable across rebuilds.
	if a.Digest != b.Digest {
		t.Fatalf("digest drifted: %s != %s", a.Digest, b.Digest)
	}
	// Slice fields must be sorted on output.
	if a.Edits[0].Path != "a.go" {
		t.Fatalf("edits not sorted: %+v", a.Edits)
	}
	if a.Impact.Direct[0] != "a.go" {
		t.Fatalf("impact.direct not sorted: %+v", a.Impact.Direct)
	}
	// The digest must exclude source bytes: it only carries paths/digests.
	if strings.Contains(string(aj), "source") {
		t.Fatalf("report leaked source-like content: %s", aj)
	}
}

func TestChangeSetAcceptsValid(t *testing.T) {
	t.Parallel()
	base := workflow.Digest([]byte("package a\nfunc Alpha() {}\n"))
	cs := workflow.ChangeSet{
		Base:        "abc123",
		BaseDigests: map[string]string{"a.go": base},
		Edits: []workflow.CandidateEdit{
			{Path: "a.go", Range: workflow.EditRange{Start: 1, End: 3}, Symbol: "Alpha",
				BaseDigest: base, PredictedDigest: "p1", ObservedDigest: "p1"},
			{Path: "b.go", Range: workflow.EditRange{Start: 5, End: 8},
				PredictedDigest: "p2", ObservedDigest: "p2"},
		},
		VerificationCommands: []string{"go test ./...", "go vet ./..."},
	}
	res := cs.Validate()
	if !res.Accepted {
		t.Fatalf("valid changeset rejected: %+v", res)
	}
	if res.Digest == "" {
		t.Fatal("validation receipt must carry a digest")
	}
}

func TestChangeSetRejectsStaleBase(t *testing.T) {
	t.Parallel()
	frozen := workflow.Digest([]byte("v1"))
	planned := workflow.Digest([]byte("v2"))
	cs := workflow.ChangeSet{
		Base:        "abc123",
		BaseDigests: map[string]string{"a.go": frozen},
		Edits: []workflow.CandidateEdit{
			{Path: "a.go", Range: workflow.EditRange{Start: 1, End: 2},
				BaseDigest: planned, PredictedDigest: "p", ObservedDigest: "p"},
		},
	}
	res := cs.Validate()
	if res.Accepted {
		t.Fatal("stale base must fail closed")
	}
	if !containsReason(res.Rejected, workflow.RejectStaleBase) {
		t.Fatalf("want stale_base rejection, got %+v", res.Rejected)
	}
}

func TestChangeSetRejectsPathEscape(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"../escape", "/abs", "a\\b.go", "a/../../c"} {
		cs := workflow.ChangeSet{
			Edits: []workflow.CandidateEdit{
				{Path: bad, Range: workflow.EditRange{Start: 1, End: 2},
					PredictedDigest: "p", ObservedDigest: "p"},
			},
		}
		res := cs.Validate()
		if res.Accepted {
			t.Fatalf("path escape %q must fail closed", bad)
		}
		if !containsReason(res.Rejected, workflow.RejectPathEscape) {
			t.Fatalf("path %q want path_escape rejection, got %+v", bad, res.Rejected)
		}
	}
}

func TestChangeSetRejectsOverlaps(t *testing.T) {
	t.Parallel()
	cs := workflow.ChangeSet{
		Edits: []workflow.CandidateEdit{
			{Path: "a.go", Range: workflow.EditRange{Start: 1, End: 10},
				PredictedDigest: "p", ObservedDigest: "p"},
			{Path: "a.go", Range: workflow.EditRange{Start: 5, End: 8},
				PredictedDigest: "p", ObservedDigest: "p"},
		},
	}
	res := cs.Validate()
	if res.Accepted {
		t.Fatal("overlapping edits must fail closed")
	}
	if !containsReason(res.Rejected, workflow.RejectOverlap) {
		t.Fatalf("want overlapping_edits rejection, got %+v", res.Rejected)
	}
}

func TestChangeSetRejectsPartialFailure(t *testing.T) {
	t.Parallel()
	cs := workflow.ChangeSet{
		Edits: []workflow.CandidateEdit{
			{Path: "a.go", Range: workflow.EditRange{Start: 1, End: 2},
				PredictedDigest: "predicted", ObservedDigest: "different"},
		},
	}
	res := cs.Validate()
	if res.Accepted {
		t.Fatal("partial failure must fail closed")
	}
	if !containsReason(res.Rejected, workflow.RejectPartialFailure) {
		t.Fatalf("want partial_failure rejection, got %+v", res.Rejected)
	}
	if len(res.GraphDelta) != 1 || !res.GraphDelta[0].Diverged {
		t.Fatalf("graph delta must flag divergence: %+v", res.GraphDelta)
	}
}

func TestChangeSetRejectsEmptyAndBadRange(t *testing.T) {
	t.Parallel()
	if res := (workflow.ChangeSet{}).Validate(); res.Accepted {
		t.Fatal("empty changeset must fail closed")
	} else if !containsReason(res.Rejected, workflow.RejectEmpty) {
		t.Fatalf("want empty rejection, got %+v", res.Rejected)
	}
	cs := workflow.ChangeSet{Edits: []workflow.CandidateEdit{
		{Path: "a.go", Range: workflow.EditRange{Start: 10, End: 2}},
	}}
	if res := cs.Validate(); res.Accepted {
		t.Fatal("bad range must fail closed")
	} else if !containsReason(res.Rejected, workflow.RejectBadRange) {
		t.Fatalf("want bad_range rejection, got %+v", res.Rejected)
	}
}

func TestChangeSetValidationDeterministic(t *testing.T) {
	t.Parallel()
	mk := func() workflow.ChangeSet {
		base := workflow.Digest([]byte("base"))
		return workflow.ChangeSet{
			Base:        "h",
			BaseDigests: map[string]string{"a.go": base},
			Edits: []workflow.CandidateEdit{
				{Path: "a.go", Range: workflow.EditRange{Start: 1, End: 2},
					BaseDigest: base, PredictedDigest: "p", ObservedDigest: "p"},
			},
		}
	}
	a, _ := json.Marshal(mk().Validate())
	b, _ := json.Marshal(mk().Validate())
	if !bytes.Equal(a, b) {
		t.Fatalf("validation receipt not deterministic\na=%s\nb=%s", a, b)
	}
}

func containsReason(got []workflow.RejectReason, want workflow.RejectReason) bool {
	for _, r := range got {
		if r == want {
			return true
		}
	}
	return false
}
