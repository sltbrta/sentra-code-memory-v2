package workflowinspect_test

import (
	"errors"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/workflowinspect"
)

func sampleIR() workflowinspect.WorkflowIR {
	return workflowinspect.WorkflowIR{
		WorkflowID: "wf-demo",
		Version:    "1",
		Nodes: []workflowinspect.Node{
			{ID: "n1", Type: "pure.echo", BudgetTokens: 100, Effectful: false, Capabilities: []string{"cpu"}},
			{ID: "n2", Type: "effect.propose_tool", BudgetTokens: 50, Effectful: true, Capabilities: []string{"tool"}},
		},
		Edges: []workflowinspect.Edge{{From: "n1", To: "n2", Required: true}},
	}
}

func TestIRRoundTrip(t *testing.T) {
	out, err := workflowinspect.RoundTripJSON(sampleIR())
	if err != nil {
		t.Fatal(err)
	}
	if out.Digest == "" || out.WorkflowID != "wf-demo" {
		t.Fatalf("out = %+v", out)
	}
}

func TestInspectPermissionsAndRender(t *testing.T) {
	insp := workflowinspect.NewInspector()
	model, err := insp.Inspect(sampleIR(), workflowinspect.Principal{ID: "inspector"}, map[string]string{"n1": "running"})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.Nodes) != 2 || model.Nodes[0].State != "running" {
		t.Fatalf("nodes = %+v", model.Nodes)
	}
	for _, p := range model.Permissions {
		if p.Action == "edit" && p.Allowed {
			t.Fatal("edit must be denied")
		}
		if p.Action == "publish" && p.Allowed {
			t.Fatal("publish must be denied")
		}
	}
	if _, err := insp.Inspect(sampleIR(), workflowinspect.Principal{ID: "stranger"}, nil); !errors.Is(err, workflowinspect.ErrUnauthorized) {
		t.Fatalf("stranger = %v", err)
	}
}

func TestReplayCompleteGappedTampered(t *testing.T) {
	ir := sampleIR()
	principal := workflowinspect.Principal{ID: "operator"}
	complete := []workflowinspect.TraceEvent{
		{Sequence: 1, EventID: "e1", NodeID: "n1", Kind: "started", Digest: workflowinspect.EventDigest("e1", "n1", "started", 1)},
		{Sequence: 2, EventID: "e2", NodeID: "n1", Kind: "completed", Digest: workflowinspect.EventDigest("e2", "n1", "completed", 2)},
		{Sequence: 3, EventID: "e3", NodeID: "n2", Kind: "started", Digest: workflowinspect.EventDigest("e3", "n2", "started", 3)},
	}
	res, err := workflowinspect.Replay(ir, principal, complete)
	if err != nil || res.Status != "complete" || res.NodeStates["n1"] != "completed" {
		t.Fatalf("complete = %+v %v", res, err)
	}

	gapped := []workflowinspect.TraceEvent{
		{Sequence: 1, EventID: "e1", NodeID: "n1", Kind: "started", Digest: workflowinspect.EventDigest("e1", "n1", "started", 1)},
		{Sequence: 3, EventID: "e3", NodeID: "n1", Kind: "completed", Digest: workflowinspect.EventDigest("e3", "n1", "completed", 3)},
	}
	res, err = workflowinspect.Replay(ir, principal, gapped)
	if !errors.Is(err, workflowinspect.ErrIntegrity) || res.Status != "gapped" {
		t.Fatalf("gapped = %+v %v", res, err)
	}

	tampered := []workflowinspect.TraceEvent{
		{Sequence: 1, EventID: "e1", NodeID: "n1", Kind: "started", Digest: "deadbeef"},
	}
	res, err = workflowinspect.Replay(ir, principal, tampered)
	if !errors.Is(err, workflowinspect.ErrIntegrity) || res.Status != "tampered" {
		t.Fatalf("tampered = %+v %v", res, err)
	}

	if _, err := workflowinspect.Replay(ir, workflowinspect.Principal{ID: "inspector"}, complete); !errors.Is(err, workflowinspect.ErrUnauthorized) {
		t.Fatalf("inspector replay = %v", err)
	}
}

func TestSimulatePureEffectOverBudgetUnauthorizedZeroEffects(t *testing.T) {
	insp := workflowinspect.NewInspector()
	ir := sampleIR()
	principal := workflowinspect.Principal{ID: "alice"}
	inputs := []workflowinspect.SimulationInput{
		{NodeID: "n1", Fields: map[string]string{"input": "hello"}},
		{NodeID: "n2", Fields: map[string]string{"input": "tool"}},
	}
	res, err := insp.Simulate(ir, principal, inputs, 1000)
	if err != nil || res.Status != "ok" {
		t.Fatalf("sim = %+v %v", res, err)
	}
	if res.ProviderCalls != 0 || res.ToolCalls != 0 {
		t.Fatalf("effects leaked: %+v", res)
	}
	if res.Outputs["n1"]["echo"] != "hello" {
		t.Fatalf("pure output = %+v", res.Outputs)
	}
	if len(res.Effects) != 1 || res.Effects[0].Action != "tool.propose" {
		t.Fatalf("effect proposals = %+v", res.Effects)
	}

	// Over budget.
	res, err = insp.Simulate(ir, principal, inputs, 5)
	if !errors.Is(err, workflowinspect.ErrRejected) || res.Status != "over_budget" {
		t.Fatalf("budget = %+v %v", res, err)
	}

	// Unauthorized.
	if _, err := insp.Simulate(ir, workflowinspect.Principal{ID: "inspector"}, inputs, 1000); !errors.Is(err, workflowinspect.ErrUnauthorized) {
		t.Fatalf("inspector sim = %v", err)
	}

	// Revoked/uncertified node type.
	bad := sampleIR()
	bad.Nodes[0].Type = "revoked.bad"
	res, err = insp.Simulate(bad, principal, []workflowinspect.SimulationInput{{NodeID: "n1"}}, 1000)
	if !errors.Is(err, workflowinspect.ErrRejected) || res.Status != "invalid" {
		t.Fatalf("revoked = %+v %v", res, err)
	}
}
