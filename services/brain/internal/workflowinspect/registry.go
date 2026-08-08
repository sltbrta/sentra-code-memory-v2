package workflowinspect

// NodeState is the certified node registry lifecycle.
type NodeState string

const (
	NodeCertified  NodeState = "certified"
	NodeDeprecated NodeState = "deprecated"
	NodeRevoked    NodeState = "revoked"
)

// CertifiedNode is one registry descriptor for simulation.
type CertifiedNode struct {
	Type      string    `json:"node_type"`
	Version   string    `json:"version"`
	State     NodeState `json:"state"`
	Effectful bool      `json:"effectful"`
	Simulator string    `json:"simulator"` // pure | effect_proposal
	BudgetMax int       `json:"budget_max_tokens"`
}

// Registry is an immutable certified-node snapshot.
type Registry struct {
	Digest string                   `json:"registry_digest"`
	Nodes  map[string]CertifiedNode `json:"nodes"`
}

// DefaultRegistry returns the Stage 12 pure + effect fixture nodes.
func DefaultRegistry() Registry {
	return Registry{
		Digest: "registry-stage12-v1",
		Nodes: map[string]CertifiedNode{
			"pure.echo": {
				Type: "pure.echo", Version: "1", State: NodeCertified,
				Effectful: false, Simulator: "pure", BudgetMax: 1000,
			},
			"effect.propose_tool": {
				Type: "effect.propose_tool", Version: "1", State: NodeCertified,
				Effectful: true, Simulator: "effect_proposal", BudgetMax: 500,
			},
			"revoked.bad": {
				Type: "revoked.bad", Version: "1", State: NodeRevoked,
				Effectful: true, Simulator: "pure", BudgetMax: 1,
			},
		},
	}
}

// Resolve returns a certified descriptor or rejects unknown/revoked nodes.
func (r Registry) Resolve(nodeType string) (CertifiedNode, error) {
	n, ok := r.Nodes[nodeType]
	if !ok {
		return CertifiedNode{}, ErrRejected
	}
	if n.State == NodeRevoked {
		return CertifiedNode{}, ErrRejected
	}
	return n, nil
}
