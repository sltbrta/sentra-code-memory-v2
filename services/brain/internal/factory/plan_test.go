package factory

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPlanCompilerRejectsCELViolations(t *testing.T) {
	cases := []struct {
		name   string
		leaves []LeafSpec
		scope  []string
		review bool
	}{
		{
			name: "case-folded prefix overlapping scopes",
			leaves: []LeafSpec{
				leafSpec("leaf-a", "SRC/GO"),
				leafSpec("leaf-b", "src/go/modify-00.go"),
			},
			scope: []string{"src"},
		},
		{
			name: "case-folded equal scopes",
			leaves: []LeafSpec{
				leafSpec("leaf-a", "Src/Go"),
				leafSpec("leaf-b", "sRC/gO"),
			},
			scope: []string{"src"},
		},
		{
			name: "control character holder principal",
			leaves: []LeafSpec{
				{NodeID: "leaf-a", Goal: []byte("goal"), OwnedPaths: []string{"src/go/a.go"}, HolderPrincipal: "worker\x01"},
			},
			scope: []string{"src/go"},
		},
		{
			name: "overlong holder principal",
			leaves: []LeafSpec{
				{NodeID: "leaf-a", Goal: []byte("goal"), OwnedPaths: []string{"src/go/a.go"}, HolderPrincipal: strings.Repeat("w", 513)},
			},
			scope: []string{"src/go"},
		},
		{
			name:   "no leaves",
			leaves: nil,
			scope:  []string{"src/go"},
		},
		{
			name: "four leaves",
			leaves: []LeafSpec{
				leafSpec("leaf-a", "src/go/a.go"),
				leafSpec("leaf-b", "src/go/b.go"),
				leafSpec("leaf-c", "src/go/c.go"),
				leafSpec("leaf-d", "src/go/d.go"),
			},
			scope: []string{"src/go"},
		},
		{
			name: "prefix overlapping scopes",
			leaves: []LeafSpec{
				leafSpec("leaf-a", "src/go"),
				leafSpec("leaf-b", "src/go/modify-00.go"),
			},
			scope: []string{"src/go"},
		},
		{
			name: "equal scopes",
			leaves: []LeafSpec{
				leafSpec("leaf-a", "src/go/modify-00.go"),
				leafSpec("leaf-b", "src/go/modify-00.go"),
			},
			scope: []string{"src/go"},
		},
		{
			name: "scope escapes approved intent scope",
			leaves: []LeafSpec{
				leafSpec("leaf-a", "src/typescript/modify-00.ts"),
			},
			scope: []string{"src/go"},
		},
		{
			name: "duplicate node ids",
			leaves: []LeafSpec{
				leafSpec("leaf-a", "src/go/modify-00.go"),
				leafSpec("leaf-a", "src/go/modify-01.go"),
			},
			scope: []string{"src/go"},
		},
		{
			name: "reserved orchestrator node id",
			leaves: []LeafSpec{
				leafSpec("orchestrator", "src/go/modify-00.go"),
			},
			scope: []string{"src/go"},
		},
		{
			name: "reserved review node id",
			leaves: []LeafSpec{
				leafSpec("review", "src/go/modify-00.go"),
			},
			scope:  []string{"src/go"},
			review: true,
		},
		{
			name: "traversal path",
			leaves: []LeafSpec{
				leafSpec("leaf-a", "src/go/../secrets"),
			},
			scope: []string{"src"},
		},
		{
			name: "absolute path",
			leaves: []LeafSpec{
				leafSpec("leaf-a", "/src/go/modify-00.go"),
			},
			scope: []string{"src"},
		},
		{
			name: "backslash path",
			leaves: []LeafSpec{
				leafSpec("leaf-a", `src\go\modify-00.go`),
			},
			scope: []string{"src"},
		},
		{
			name: "empty segment path",
			leaves: []LeafSpec{
				leafSpec("leaf-a", "src//go"),
			},
			scope: []string{"src"},
		},
		{
			name: "dot segment path",
			leaves: []LeafSpec{
				leafSpec("leaf-a", "src/./go"),
			},
			scope: []string{"src"},
		},
		{
			name: "empty goal",
			leaves: []LeafSpec{
				{NodeID: "leaf-a", OwnedPaths: []string{"src/go/modify-00.go"}},
			},
			scope: []string{"src/go"},
		},
		{
			name: "node id pattern violation",
			leaves: []LeafSpec{
				leafSpec("Leaf-A", "src/go/modify-00.go"),
			},
			scope: []string{"src/go"},
		},
		{
			name: "empty approved scope",
			leaves: []LeafSpec{
				leafSpec("leaf-a", "src/go/modify-00.go"),
			},
			scope: nil,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newTestKernel(t)
			request := AdmitRequest{
				Authenticated:      testIdentity(),
				Caller:             testCaller(),
				Intent:             makeIntent(t, "intent-"+testCase.name, testBaseOID),
				ApprovedScopePaths: testCase.scope,
				Leaves:             testCase.leaves,
				Review:             testCase.review,
				IdempotencyKey:     "key-" + testCase.name,
			}
			if _, err := fixture.kernel.AdmitChangeIntent(context.Background(), request); !errors.Is(err, ErrPlanInvalid) {
				t.Fatalf("error = %v, want ErrPlanInvalid", err)
			}
		})
	}
}

func TestPlanCompilerAcceptsAdjacentNonOverlappingScopes(t *testing.T) {
	fixture := newTestKernel(t)
	// src/goo does not collide with src/go under directory-prefix semantics.
	result, err := fixture.kernel.AdmitChangeIntent(context.Background(), AdmitRequest{
		Authenticated:      testIdentity(),
		Caller:             testCaller(),
		Intent:             makeIntent(t, "intent-adjacent", testBaseOID),
		ApprovedScopePaths: []string{"src"},
		Leaves: []LeafSpec{
			leafSpec("leaf-a", "src/go"),
			leafSpec("leaf-b", "src/goo"),
			leafSpec("leaf-c", "src/go.mod"),
		},
		Review:         false,
		IdempotencyKey: "key-adjacent",
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.kernel.GetChangePlan(context.Background(), testIdentity(), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.GetNodes()) != 4 || len(plan.GetEdges()) != 3 {
		t.Fatalf("three-leaf no-review plan shape = %d nodes, %d edges", len(plan.GetNodes()), len(plan.GetEdges()))
	}
}

func TestPlanCompilerAcceptsCaseDistinctButFoldDisjointScopes(t *testing.T) {
	fixture := newTestKernel(t)
	// Under ASCII case-folding src/GOx and src/go remain disjoint, and the
	// served plan preserves the declared case.
	result, err := fixture.kernel.AdmitChangeIntent(context.Background(), AdmitRequest{
		Authenticated:      testIdentity(),
		Caller:             testCaller(),
		Intent:             makeIntent(t, "intent-fold", testBaseOID),
		ApprovedScopePaths: []string{"src"},
		Leaves: []LeafSpec{
			leafSpec("leaf-a", "src/GOx"),
			leafSpec("leaf-b", "src/go"),
		},
		Review:         false,
		IdempotencyKey: "key-fold",
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.kernel.GetChangePlan(context.Background(), testIdentity(), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]string{}
	for _, node := range plan.GetNodes() {
		if len(node.GetOwnedPaths()) == 1 {
			declared[node.GetNodeId()] = node.GetOwnedPaths()[0]
		}
	}
	if declared["leaf-a"] != "src/GOx" || declared["leaf-b"] != "src/go" {
		t.Fatalf("served scopes lost declared case: %v", declared)
	}
}

func TestMalformedHolderPrincipalDeniesBeforeStaging(t *testing.T) {
	fixture := newTestKernel(t)
	request := admitRequest(t, "key-holder", nil)
	request.Leaves = []LeafSpec{{
		NodeID: "leaf-a", Goal: []byte("goal"), OwnedPaths: []string{"src/go/a.go"},
		HolderPrincipal: "worker\n-1",
	}}
	if _, err := fixture.kernel.AdmitChangeIntent(context.Background(), request); !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("error = %v, want ErrPlanInvalid", err)
	}
	if got := fixture.payloads.putCount(); got != 0 {
		t.Fatalf("malformed holder staged %d vault objects, want 0", got)
	}
}

func TestPlanNodeOwnedPathsWithinLeafButDistinctFiles(t *testing.T) {
	fixture := newTestKernel(t)
	result, err := fixture.kernel.AdmitChangeIntent(context.Background(), admitRequest(t, "key-distinct", []LeafSpec{
		leafSpec("leaf-a", "src/go/modify-00.go"),
		leafSpec("leaf-b", "src/go/modify-01.go"),
		leafSpec("leaf-c", "src/go/modify-02.go"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.kernel.GetChangePlan(context.Background(), testIdentity(), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	leaves := 0
	for _, node := range plan.GetNodes() {
		if node.GetKind().String() == "PLAN_NODE_KIND_LEAF" {
			leaves++
			if node.GetCapabilityGrant().GetRevocationEpoch() != 7 {
				t.Fatalf("leaf grant epoch = %d, want 7", node.GetCapabilityGrant().GetRevocationEpoch())
			}
			if node.GetCapabilityGrant().GetPolicyDigest().GetHex() != testPolicyHex {
				t.Fatal("leaf grant policy digest not pinned")
			}
		}
	}
	if leaves != 3 {
		t.Fatalf("leaves = %d, want 3", leaves)
	}
}
