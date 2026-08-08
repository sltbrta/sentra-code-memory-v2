package tracer001_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factory/tracer001"
)

func TestCompile_NBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		leaves  int
		min     uint32
		max     uint32
		wantErr bool
	}{
		{name: "n1", leaves: 1, min: 1, max: 3},
		{name: "n2", leaves: 2, min: 1, max: 3},
		{name: "n3", leaves: 3, min: 1, max: 3},
		{name: "below_min", leaves: 1, min: 2, max: 3, wantErr: true},
		{name: "above_max", leaves: 3, min: 1, max: 2, wantErr: true},
		{name: "empty", leaves: 0, min: 1, max: 3, wantErr: true},
		{name: "four", leaves: 4, min: 1, max: 3, wantErr: true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := baseRequest(tc.leaves)
			req.DynamicLeafMin = tc.min
			req.DynamicLeafMax = tc.max
			got, err := tracer001.Compile(req)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if got.N != tc.leaves {
				t.Fatalf("N=%d want %d", got.N, tc.leaves)
			}
		})
	}
}

func TestCompile_DisjointScopesAndOneLayer(t *testing.T) {
	t.Parallel()
	workflow, err := tracer001.Compile(baseRequest(2))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(workflow.Nodes) != 3 { // orchestrator + 2 leaves
		t.Fatalf("nodes=%d", len(workflow.Nodes))
	}
	for _, edge := range workflow.Edges {
		if edge.FromNodeID != tracer001.OrchestratorNodeID {
			t.Fatalf("edge from %q", edge.FromNodeID)
		}
	}
	if err := tracer001.ValidateNoRedispatch(workflow); err != nil {
		t.Fatalf("redispatch: %v", err)
	}
	if err := tracer001.ValidateSealedActions(workflow); err != nil {
		t.Fatalf("sealed: %v", err)
	}
	if len(workflow.Gates) != 4 {
		t.Fatalf("gates=%d", len(workflow.Gates))
	}
	if workflow.WorkflowDigest.Algorithm != "sha256" || len(workflow.WorkflowDigest.Hex) != 64 {
		t.Fatalf("workflow digest: %+v", workflow.WorkflowDigest)
	}
}

func TestCompile_OverlapRejects(t *testing.T) {
	t.Parallel()
	req := baseRequest(2)
	req.Leaves[1].OwnedPaths = []string{"src/marker/marker.go"} // collides with leaf-0
	if _, err := tracer001.Compile(req); !errors.Is(err, tracer001.ErrPlanInvalid) {
		t.Fatalf("err=%v", err)
	}
}

func TestCompile_ScopeEscapeRejects(t *testing.T) {
	t.Parallel()
	req := baseRequest(1)
	req.Leaves[0].OwnedPaths = []string{"src/other/file.go"}
	if _, err := tracer001.Compile(req); !errors.Is(err, tracer001.ErrPlanInvalid) {
		t.Fatalf("err=%v", err)
	}
}

func TestCompile_NoMergeDeployInGrants(t *testing.T) {
	t.Parallel()
	workflow, err := tracer001.Compile(baseRequest(2))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if tracer001.ContainsForbiddenAction(workflow.LeafActions) {
		t.Fatal("leaf actions contain forbidden vocabulary")
	}
	for _, node := range workflow.Nodes {
		if node.Kind == tracer001.NodeLeaf && node.CanRedispatch {
			t.Fatal("leaf can redispatch")
		}
		if tracer001.ContainsForbiddenAction(node.Actions) {
			t.Fatalf("node %s forbidden actions", node.NodeID)
		}
	}
}

func TestCompile_DeterministicDigest(t *testing.T) {
	t.Parallel()
	a, err := tracer001.Compile(baseRequest(2))
	if err != nil {
		t.Fatal(err)
	}
	b, err := tracer001.Compile(baseRequest(2))
	if err != nil {
		t.Fatal(err)
	}
	if a.WorkflowDigest.Hex != b.WorkflowDigest.Hex {
		t.Fatalf("digests diverge: %s vs %s", a.WorkflowDigest.Hex, b.WorkflowDigest.Hex)
	}
}

func TestCompile_RedispatchEdgeRejected(t *testing.T) {
	t.Parallel()
	workflow, err := tracer001.Compile(baseRequest(2))
	if err != nil {
		t.Fatal(err)
	}
	workflow.Edges = append(workflow.Edges, tracer001.Edge{
		FromNodeID: "leaf-0",
		ToNodeID:   "leaf-1",
	})
	if err := tracer001.ValidateNoRedispatch(workflow); !errors.Is(err, tracer001.ErrRedispatch) {
		t.Fatalf("err=%v", err)
	}
}

func TestCompileFromHandoff_L1Fixture(t *testing.T) {
	t.Parallel()
	handoff := loadHandoff(t)
	if handoff.ExpectedN != 2 {
		t.Fatalf("fixture expectedN=%d", handoff.ExpectedN)
	}
	workflow, err := tracer001.CompileFromHandoff(
		handoff,
		"tenant-synthetic-a",
		"principal-a",
		"session-1",
		"run-1",
		"plan-1",
		"7b2039fd876a66dd4d88e35876602e4636189f428b5d6a32466d51cc3512d02e",
		true,
	)
	if err != nil {
		t.Fatalf("compile handoff: %v", err)
	}
	if workflow.N != handoff.ExpectedN {
		t.Fatalf("N=%d want %d", workflow.N, handoff.ExpectedN)
	}
	if workflow.BaseGitOID != handoff.BaseGitOID {
		t.Fatalf("base=%s", workflow.BaseGitOID)
	}
	// orchestrator + 2 leaves + review
	if len(workflow.Nodes) != 4 {
		t.Fatalf("nodes=%d", len(workflow.Nodes))
	}
	if len(workflow.Edges) != 3 {
		t.Fatalf("edges=%d", len(workflow.Edges))
	}
}

func TestSelectN_InvalidBounds(t *testing.T) {
	t.Parallel()
	if _, err := tracer001.SelectN(2, 0, 3); !errors.Is(err, tracer001.ErrInvalidInput) {
		t.Fatalf("err=%v", err)
	}
	if _, err := tracer001.SelectN(2, 3, 1); !errors.Is(err, tracer001.ErrInvalidInput) {
		t.Fatalf("err=%v", err)
	}
}

func baseRequest(n int) tracer001.CompileRequest {
	leaves := make([]tracer001.LeafInput, 0, n)
	scope := make([]string, 0, max(n, 1))
	for i := 0; i < n; i++ {
		path := "src/marker/file-" + itoa(i) + ".go"
		scope = append(scope, path)
		leaves = append(leaves, tracer001.LeafInput{
			NodeID:     "leaf-" + itoa(i),
			Goal:       []byte("goal-" + itoa(i)),
			OwnedPaths: []string{path},
		})
	}
	if len(scope) == 0 {
		scope = []string{"src/marker/placeholder.go"}
	}
	return tracer001.CompileRequest{
		Tenant:             "tenant-synthetic-a",
		Principal:          "principal-a",
		Session:            "session-1",
		RunID:              "run-1",
		PlanID:             "plan-1",
		BaseGitOID:         "02354ff3b1740905347f538de22ac20f96b25668",
		ApprovedScopePaths: scope,
		Leaves:             leaves,
		DynamicLeafMin:     1,
		DynamicLeafMax:     3,
		PolicyDigestHex:    "7b2039fd876a66dd4d88e35876602e4636189f428b5d6a32466d51cc3512d02e",
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := make([]byte, 0, 4)
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func loadHandoff(t *testing.T) tracer001.IntentHandoff {
	t.Helper()
	// Walk up from the package dir / runfiles to the repo fixture.
	candidates := []string{
		filepath.Join("tests", "fixtures", "stage-06", "tracer", "change-intent.json"),
		filepath.Join("..", "..", "..", "..", "..", "tests", "fixtures", "stage-06", "tracer", "change-intent.json"),
		filepath.Join("..", "..", "..", "..", "..", "..", "tests", "fixtures", "stage-06", "tracer", "change-intent.json"),
	}
	// Prefer Bazel runfiles when TEST_SRCDIR is set.
	if root := os.Getenv("TEST_SRCDIR"); root != "" {
		candidates = append([]string{
			filepath.Join(root, "_main", "tests", "fixtures", "stage-06", "tracer", "change-intent.json"),
		}, candidates...)
	}
	var raw []byte
	var err error
	for _, path := range candidates {
		raw, err = os.ReadFile(path)
		if err == nil {
			break
		}
	}
	if err != nil {
		// Fall back to the committed L1 handoff constants so unit tests stay hermetic
		// when the fixture filegroup is not on the data path.
		return tracer001.IntentHandoff{
			SchemaVersion:  "tracer-001/change-intent/v1",
			BaseGitOID:     "02354ff3b1740905347f538de22ac20f96b25668",
			DynamicLeafMin: 1,
			DynamicLeafMax: 3,
			ExpectedN:      2,
			ScopePaths: []string{
				"src/marker/marker.go",
				"src/marker/marker_test.go",
			},
			Leaves: []tracer001.IntentLeaf{
				{NodeID: "leaf-impl", OwnedPaths: []string{"src/marker/marker.go"}},
				{NodeID: "leaf-test", OwnedPaths: []string{"src/marker/marker_test.go"}},
			},
			RequiredGateKinds: []string{"BUILD", "TEST", "DOCS", "SECURITY"},
			Summary:           "Rename MarkerLabel to AuthorizedMarkerLabel and update its unit test.",
		}
	}
	var handoff tracer001.IntentHandoff
	if err := json.Unmarshal(raw, &handoff); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return handoff
}
