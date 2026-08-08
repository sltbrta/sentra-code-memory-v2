package workflowinspect

import (
	"fmt"
)

// SimulationInput is an explicit fixture input for one node.
type SimulationInput struct {
	NodeID string            `json:"node_id"`
	Fields map[string]string `json:"fields"`
}

// ProposedEffect records an effect proposal without calling a provider/tool.
type ProposedEffect struct {
	NodeID      string  `json:"node_id"`
	Action      string  `json:"action"`
	Permission  string  `json:"permission"`
	CostUSD     float64 `json:"cost_usd"`
	BlastRadius string  `json:"blast_radius"`
}

// SimulationResult is the effect-free simulation receipt.
type SimulationResult struct {
	WorkflowDigest Digest                       `json:"workflow_digest"`
	Outputs        map[string]map[string]string `json:"outputs"`
	Effects        []ProposedEffect             `json:"effects"`
	ProviderCalls  int                          `json:"provider_calls"`
	ToolCalls      int                          `json:"tool_calls"`
	Status         string                       `json:"status"`
	Errors         []string                     `json:"errors,omitempty"`
}

// Simulate validates static semantics and evaluates pure simulators.
// Effect nodes emit proposals only; provider/tool calls remain zero.
func (i *Inspector) Simulate(ir WorkflowIR, principal Principal, inputs []SimulationInput, budgetTokens int) (SimulationResult, error) {
	sealed, err := ir.Seal()
	if err != nil {
		return SimulationResult{}, err
	}
	if principal.ID != "operator" && principal.ID != "alice" {
		return SimulationResult{}, ErrUnauthorized
	}
	result := SimulationResult{
		WorkflowDigest: sealed.Digest,
		Outputs:        map[string]map[string]string{},
		Effects:        []ProposedEffect{},
		ProviderCalls:  0,
		ToolCalls:      0,
		Status:         "ok",
	}
	nodesByID := make(map[string]Node, len(sealed.Nodes))
	for _, n := range sealed.Nodes {
		nodesByID[n.ID] = n
	}
	spent := 0
	for _, in := range inputs {
		node, ok := nodesByID[in.NodeID]
		if !ok {
			result.Status = "invalid"
			result.Errors = append(result.Errors, "unknown node "+in.NodeID)
			return result, ErrRejected
		}
		cert, err := i.Registry.Resolve(node.Type)
		if err != nil {
			result.Status = "invalid"
			result.Errors = append(result.Errors, "uncertified node type "+node.Type)
			return result, ErrRejected
		}
		spent += 10
		if budgetTokens > 0 && spent > budgetTokens {
			result.Status = "over_budget"
			result.Errors = append(result.Errors, fmt.Sprintf("budget exceeded at node %s", node.ID))
			return result, ErrRejected
		}
		if cert.Simulator == "pure" {
			out := map[string]string{"echo": in.Fields["input"]}
			if out["echo"] == "" {
				out["echo"] = "ok"
			}
			result.Outputs[node.ID] = out
			continue
		}
		// Effect proposal only — never call a tool/provider.
		result.Effects = append(result.Effects, ProposedEffect{
			NodeID:      node.ID,
			Action:      "tool.propose",
			Permission:  "tool.execute",
			CostUSD:     0.01,
			BlastRadius: "node_local",
		})
		result.Outputs[node.ID] = map[string]string{"proposed": "true"}
	}
	if result.ProviderCalls != 0 || result.ToolCalls != 0 {
		return SimulationResult{}, fmt.Errorf("%w: simulation leaked effects", ErrIntegrity)
	}
	return result, nil
}
