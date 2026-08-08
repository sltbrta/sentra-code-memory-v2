package tracer001

import "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"

// NodeKind mirrors Stage 05 PlanNodeKind for the compiler projection.
type NodeKind string

const (
	// NodeOrchestrator is the single non-leaf plan root.
	NodeOrchestrator NodeKind = "orchestrator"
	// NodeLeaf is one terminal worker with exclusive write scope.
	NodeLeaf NodeKind = "leaf"
	// NodeReview is the optional fresh read-only reviewer node.
	NodeReview NodeKind = "review"
)

// Frozen Stage 05 / Tracer 001 identity constants.
const (
	OrchestratorNodeID = "orchestrator"
	ReviewNodeID       = "review"

	// LeafGrantAction is the sole leaf execution authority.
	LeafGrantAction = "factory.leaf.execute"

	// Forbidden leaf / grant actions — never expressible on Tracer 001 leaves.
	ActionDispatch   = "factory.dispatch"
	ActionTaskCreate = "factory.task.create"
	ActionMerge      = "github.merge"
	ActionDeploy     = "deploy.release"
	ActionRelease    = "github.release"
	ActionPromote    = "profile.promote"
	ActionForcePush  = "github.force_push"
	ActionBranchDel  = "github.branch.delete"

	// SchemaVersion pins the handoff and workflow projection schema.
	SchemaVersion = "tracer-001/v1"
	// WorkflowVersion is the human-readable WorkflowIR revision label.
	WorkflowVersion = "tracer-001/workflow/v1"
	// DigestDomain binds canonical workflow digests.
	DigestDomain = "ouroboros.stage06.tracer001.workflow.v1"
)

// RequiredGates lists the four non-removable deterministic gates.
var RequiredGates = []string{"BUILD", "TEST", "DOCS", "SECURITY"}

// ForbiddenLeafActions is the closed set of actions leaves must never carry.
var ForbiddenLeafActions = []string{
	ActionDispatch,
	ActionTaskCreate,
	ActionMerge,
	ActionDeploy,
	ActionRelease,
	ActionPromote,
	ActionForcePush,
	ActionBranchDel,
}

// LeafInput proposes one leaf of the compiled DAG.
type LeafInput struct {
	// NodeID is stable within the plan: ^[a-z][a-z0-9-]{0,63}$.
	NodeID string
	// Goal is the exact leaf goal prose (digest-bound only in the projection).
	Goal []byte
	// OwnedPaths is the leaf's exclusive write scope.
	OwnedPaths []string
	// ForbiddenPaths lists explicit protected non-goal write boundaries.
	ForbiddenPaths []string
	// HolderPrincipal names the worker principal; empty derives node-id-worker.
	HolderPrincipal string
}

// CompileRequest carries one approved-intent handoff for DAG compilation.
type CompileRequest struct {
	// Tenant, Principal, and Session name the authenticated admission scope.
	Tenant    string
	Principal string
	Session   string
	// RunID and PlanID are opaque server-authored identities.
	RunID  string
	PlanID string
	// BaseGitOID is the exact approved repository base commit (40 or 64 hex).
	BaseGitOID string
	// ApprovedScopePaths is the intent's approved path scope.
	ApprovedScopePaths []string
	// Leaves proposes the one-to-three leaf decomposition.
	Leaves []LeafInput
	// Review includes the fresh read-only review node when true.
	Review bool
	// DynamicLeafMin/Max bound N (always 1..3 for Tracer 001).
	DynamicLeafMin uint32
	DynamicLeafMax uint32
	// PolicyDigestHex pins the policy evaluated for the run.
	PolicyDigestHex string
}

// Node is one typed plan node in the compiled projection.
type Node struct {
	// NodeID is stable within the plan.
	NodeID string
	// Kind is orchestrator, leaf, or review.
	Kind NodeKind
	// OwnedPaths is non-empty only for leaves.
	OwnedPaths []string
	// ForbiddenPaths is leaf-only.
	ForbiddenPaths []string
	// GoalDigest binds goal bytes when present.
	GoalDigest contracts.Digest
	// Actions lists grant actions for leaves (never dispatch/merge/deploy).
	Actions []string
	// HolderPrincipal is the lease holder for leaves.
	HolderPrincipal string
	// CanRedispatch is always false for leaves.
	CanRedispatch bool
}

// Edge is one directed dependency. From is always the orchestrator in v1.
type Edge struct {
	// FromNodeID is the producer node identity.
	FromNodeID string
	// ToNodeID is the consumer node identity.
	ToNodeID string
}

// Gate is one required deterministic gate attached to the plan.
type Gate struct {
	// Kind is BUILD, TEST, DOCS, or SECURITY.
	Kind string
	// Required is always true for Tracer 001 gates.
	Required bool
}

// CompiledWorkflow is the typed one-layer DAG plus its binding digests.
type CompiledWorkflow struct {
	// SchemaVersion is tracer-001/v1.
	SchemaVersion string
	// WorkflowVersion is the WorkflowIR revision label.
	WorkflowVersion string
	// RunID and PlanID echo the request identities.
	RunID  string
	PlanID string
	// BaseGitOID is the exact approved base.
	BaseGitOID string
	// N is the compiled leaf count in [1,3].
	N int
	// Nodes is the complete typed node set (orchestrator + leaves [+ review]).
	Nodes []Node
	// Edges is the complete one-layer edge set.
	Edges []Edge
	// Gates is the four required gates.
	Gates []Gate
	// WorkflowDigest binds the canonical projection bytes.
	WorkflowDigest contracts.Digest
	// LeafActions is the sole leaf grant action set (no merge/deploy).
	LeafActions []string
}

// IntentHandoff is the L1 change-intent fixture shape consumed by tests and
// composition. Fields mirror tests/fixtures/stage-06/tracer/change-intent.json.
type IntentHandoff struct {
	// SchemaVersion must be tracer-001/change-intent/v1.
	SchemaVersion string `json:"schemaVersion"`
	// BaseGitOID is the exact repository base commit.
	BaseGitOID string `json:"baseGitOid"`
	// DynamicLeafMin/Max bound N.
	DynamicLeafMin int `json:"dynamicLeafMin"`
	DynamicLeafMax int `json:"dynamicLeafMax"`
	// ExpectedN is the fixture's expected leaf width for the positive path.
	ExpectedN int `json:"expectedN"`
	// ScopePaths is the approved intent scope.
	ScopePaths []string `json:"scopePaths"`
	// Leaves lists the proposed leaf scopes.
	Leaves []IntentLeaf `json:"leaves"`
	// RequiredGateKinds lists BUILD/TEST/DOCS/SECURITY.
	RequiredGateKinds []string `json:"requiredGateKinds"`
	// Summary is non-secret intent prose.
	Summary string `json:"summary"`
}

// IntentLeaf is one leaf handoff entry.
type IntentLeaf struct {
	// NodeID is the stable leaf identity.
	NodeID string `json:"nodeId"`
	// OwnedPaths is the exclusive write scope.
	OwnedPaths []string `json:"ownedPaths"`
}
