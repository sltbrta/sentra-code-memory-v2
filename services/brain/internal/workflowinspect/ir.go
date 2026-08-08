package workflowinspect

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// ErrRejected is a typed validation failure.
var ErrRejected = errors.New("workflowinspect: rejected")

// ErrUnauthorized is a non-disclosing authorization failure.
var ErrUnauthorized = errors.New("workflowinspect: not_found_or_denied")

// ErrIntegrity reports gapped or tampered trace handling.
var ErrIntegrity = errors.New("workflowinspect: integrity_failure")

// Digest is a hex-encoded SHA-256 content binding.
type Digest string

// Node is one typed Workflow IR node.
type Node struct {
	ID             string   `json:"node_id"`
	Type           string   `json:"node_type"`
	InputContract  Digest   `json:"input_contract_digest"`
	OutputContract Digest   `json:"output_contract_digest"`
	Capabilities   []string `json:"capability_requirements"`
	BudgetTokens   int      `json:"budget_tokens"`
	BudgetCostUSD  float64  `json:"budget_cost_usd"`
	RequiresHuman  bool     `json:"requires_human_gate"`
	Effectful      bool     `json:"effectful"`
}

// Edge is one typed dependency.
type Edge struct {
	From     string `json:"from_node_id"`
	To       string `json:"to_node_id"`
	Required bool   `json:"required"`
}

// WorkflowIR is the immutable typed graph (definition authority).
type WorkflowIR struct {
	WorkflowID string `json:"workflow_id"`
	Version    string `json:"version"`
	Nodes      []Node `json:"nodes"`
	Edges      []Edge `json:"edges"`
	Digest     Digest `json:"workflow_digest"`
}

// CanonicalDigest computes the content digest with sorted keys.
func (w WorkflowIR) CanonicalDigest() (Digest, error) {
	clone := w
	clone.Digest = ""
	// Stable node/edge order for digest.
	sort.Slice(clone.Nodes, func(i, j int) bool { return clone.Nodes[i].ID < clone.Nodes[j].ID })
	sort.Slice(clone.Edges, func(i, j int) bool {
		if clone.Edges[i].From == clone.Edges[j].From {
			return clone.Edges[i].To < clone.Edges[j].To
		}
		return clone.Edges[i].From < clone.Edges[j].From
	})
	raw, err := json.Marshal(clone)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return Digest(hex.EncodeToString(sum[:])), nil
}

// Seal fills workflow_digest and validates structure.
func (w WorkflowIR) Seal() (WorkflowIR, error) {
	if w.WorkflowID == "" || w.Version == "" || len(w.Nodes) == 0 {
		return WorkflowIR{}, ErrRejected
	}
	ids := make(map[string]struct{}, len(w.Nodes))
	for _, n := range w.Nodes {
		if n.ID == "" || n.Type == "" {
			return WorkflowIR{}, ErrRejected
		}
		if _, dup := ids[n.ID]; dup {
			return WorkflowIR{}, ErrRejected
		}
		ids[n.ID] = struct{}{}
	}
	for _, e := range w.Edges {
		if _, ok := ids[e.From]; !ok {
			return WorkflowIR{}, fmt.Errorf("%w: missing from node", ErrRejected)
		}
		if _, ok := ids[e.To]; !ok {
			return WorkflowIR{}, fmt.Errorf("%w: missing to node", ErrRejected)
		}
	}
	d, err := w.CanonicalDigest()
	if err != nil {
		return WorkflowIR{}, err
	}
	w.Digest = d
	return w, nil
}

// RoundTripJSON encodes and decodes with digest preservation.
func RoundTripJSON(w WorkflowIR) (WorkflowIR, error) {
	sealed, err := w.Seal()
	if err != nil {
		return WorkflowIR{}, err
	}
	raw, err := json.Marshal(sealed)
	if err != nil {
		return WorkflowIR{}, err
	}
	var out WorkflowIR
	if err := json.Unmarshal(raw, &out); err != nil {
		return WorkflowIR{}, err
	}
	again, err := out.CanonicalDigest()
	if err != nil {
		return WorkflowIR{}, err
	}
	if again != sealed.Digest {
		return WorkflowIR{}, fmt.Errorf("%w: digest drift", ErrIntegrity)
	}
	return out, nil
}
