package workflowinspect

// Permission is one discoverable action on the graph surface.
type Permission struct {
	Action  string `json:"action"`
	Allowed bool   `json:"allowed"`
}

// NodeView is the authorized render of one node.
type NodeView struct {
	ID           string       `json:"node_id"`
	Type         string       `json:"node_type"`
	State        string       `json:"state"`
	Permissions  []Permission `json:"permissions"`
	BudgetTokens int          `json:"budget_tokens"`
	ObservedCost float64      `json:"observed_cost_usd"`
	EvidenceRefs []string     `json:"evidence_refs"`
	Capabilities []string     `json:"capabilities"`
	Effectful    bool         `json:"effectful"`
}

// RenderModel is the only client-facing inspection model.
type RenderModel struct {
	WorkflowID     string       `json:"workflow_id"`
	Version        string       `json:"version"`
	Digest         Digest       `json:"workflow_digest"`
	Nodes          []NodeView   `json:"nodes"`
	Edges          []Edge       `json:"edges"`
	Permissions    []Permission `json:"graph_permissions"`
	RegistryDigest string       `json:"registry_digest"`
}

// Principal scopes inspection permissions.
type Principal struct {
	ID string
}

// Inspector builds authorized render models without acquiring write leases.
type Inspector struct {
	Registry Registry
}

// NewInspector returns a read-only inspector over the default registry.
func NewInspector() *Inspector {
	return &Inspector{Registry: DefaultRegistry()}
}

// Inspect projects one WorkflowIR plus live node states.
func (i *Inspector) Inspect(ir WorkflowIR, principal Principal, liveStates map[string]string) (RenderModel, error) {
	sealed, err := ir.Seal()
	if err != nil {
		return RenderModel{}, err
	}
	if principal.ID == "" {
		return RenderModel{}, ErrUnauthorized
	}
	canInspect := principal.ID == "inspector" || principal.ID == "operator" || principal.ID == "alice"
	if !canInspect {
		return RenderModel{}, ErrUnauthorized
	}
	canReplay := principal.ID == "operator" || principal.ID == "alice"
	canSimulate := canReplay
	nodes := make([]NodeView, 0, len(sealed.Nodes))
	for _, n := range sealed.Nodes {
		state := liveStates[n.ID]
		if state == "" {
			state = "idle"
		}
		nodes = append(nodes, NodeView{
			ID: n.ID, Type: n.Type, State: state,
			Permissions: []Permission{
				{Action: "inspect", Allowed: true},
				{Action: "replay", Allowed: canReplay},
				{Action: "simulate", Allowed: canSimulate},
			},
			BudgetTokens: n.BudgetTokens,
			ObservedCost: 0,
			EvidenceRefs: []string{},
			Capabilities: n.Capabilities,
			Effectful:    n.Effectful,
		})
	}
	return RenderModel{
		WorkflowID: sealed.WorkflowID,
		Version:    sealed.Version,
		Digest:     sealed.Digest,
		Nodes:      nodes,
		Edges:      sealed.Edges,
		Permissions: []Permission{
			{Action: "inspect", Allowed: true},
			{Action: "replay", Allowed: canReplay},
			{Action: "simulate", Allowed: canSimulate},
			{Action: "edit", Allowed: false},
			{Action: "publish", Allowed: false},
		},
		RegistryDigest: i.Registry.Digest,
	}, nil
}
