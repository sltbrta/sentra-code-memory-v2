package tracer001

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// SelectN chooses the compiled leaf width. Candidates outside [min,max] with
// 1 <= min <= max <= 3 fail closed. The selected N equals the candidate count
// when it sits inside the bound — Tracer 001 does not invent or drop leaves.
func SelectN(candidates int, min, max uint32) (int, error) {
	if min < 1 || max > 3 || min > max {
		return 0, fmt.Errorf("%w: dynamic leaf bounds must satisfy 1 <= min <= max <= 3", ErrInvalidInput)
	}
	if candidates < int(min) || candidates > int(max) {
		return 0, fmt.Errorf("%w: leaf count %d outside [%d,%d]", ErrPlanInvalid, candidates, min, max)
	}
	return candidates, nil
}

// Compile builds the typed one-layer Tracer 001 workflow from an approved
// intent handoff. It validates N, prefix-disjoint in-scope leaves, one-layer
// edges (orchestrator-only origins), no leaf redispatch, required gates, and
// the sealed leaf action set. The returned WorkflowDigest is deterministic.
func Compile(request CompileRequest) (*CompiledWorkflow, error) {
	if err := validateRequest(request); err != nil {
		return nil, err
	}
	min, max := request.DynamicLeafMin, request.DynamicLeafMax
	if min == 0 && max == 0 {
		min, max = 1, 3
	}
	n, err := SelectN(len(request.Leaves), min, max)
	if err != nil {
		return nil, err
	}
	if err := validateLeaves(request.Leaves, request.ApprovedScopePaths); err != nil {
		return nil, err
	}

	nodes := make([]Node, 0, n+2)
	edges := make([]Edge, 0, n+1)
	nodes = append(nodes, Node{
		NodeID: OrchestratorNodeID,
		Kind:   NodeOrchestrator,
	})
	for _, leaf := range request.Leaves {
		holder := leaf.HolderPrincipal
		if holder == "" {
			holder = leaf.NodeID + "-worker"
		}
		goalDigest := digestBytes(leaf.Goal)
		nodes = append(nodes, Node{
			NodeID:          leaf.NodeID,
			Kind:            NodeLeaf,
			OwnedPaths:      append([]string(nil), leaf.OwnedPaths...),
			ForbiddenPaths:  append([]string(nil), leaf.ForbiddenPaths...),
			GoalDigest:      goalDigest,
			Actions:         []string{LeafGrantAction},
			HolderPrincipal: holder,
			CanRedispatch:   false,
		})
		edges = append(edges, Edge{FromNodeID: OrchestratorNodeID, ToNodeID: leaf.NodeID})
	}
	if request.Review {
		nodes = append(nodes, Node{
			NodeID: ReviewNodeID,
			Kind:   NodeReview,
		})
		edges = append(edges, Edge{FromNodeID: OrchestratorNodeID, ToNodeID: ReviewNodeID})
	}
	gates := make([]Gate, 0, len(RequiredGates))
	for _, kind := range RequiredGates {
		gates = append(gates, Gate{Kind: kind, Required: true})
	}

	workflow := &CompiledWorkflow{
		SchemaVersion:   SchemaVersion,
		WorkflowVersion: WorkflowVersion,
		RunID:           request.RunID,
		PlanID:          request.PlanID,
		BaseGitOID:      request.BaseGitOID,
		N:               n,
		Nodes:           nodes,
		Edges:           edges,
		Gates:           gates,
		LeafActions:     []string{LeafGrantAction},
	}
	if err := ValidateNoRedispatch(workflow); err != nil {
		return nil, err
	}
	if err := ValidateSealedActions(workflow); err != nil {
		return nil, err
	}
	workflow.WorkflowDigest = DigestWorkflow(workflow)
	return workflow, nil
}

// CompileFromHandoff maps an L1 IntentHandoff into a CompileRequest and compiles.
// Goals default to the handoff summary bytes per leaf when Goal is absent.
func CompileFromHandoff(handoff IntentHandoff, tenant, principal, session, runID, planID, policyDigestHex string, review bool) (*CompiledWorkflow, error) {
	if handoff.SchemaVersion != "" && handoff.SchemaVersion != "tracer-001/change-intent/v1" {
		return nil, fmt.Errorf("%w: unexpected change-intent schema %q", ErrInvalidInput, handoff.SchemaVersion)
	}
	leaves := make([]LeafInput, 0, len(handoff.Leaves))
	for _, leaf := range handoff.Leaves {
		goal := []byte(handoff.Summary)
		if len(goal) == 0 {
			goal = []byte(leaf.NodeID)
		}
		leaves = append(leaves, LeafInput{
			NodeID:     leaf.NodeID,
			Goal:       goal,
			OwnedPaths: append([]string(nil), leaf.OwnedPaths...),
		})
	}
	return Compile(CompileRequest{
		Tenant:             tenant,
		Principal:          principal,
		Session:            session,
		RunID:              runID,
		PlanID:             planID,
		BaseGitOID:         handoff.BaseGitOID,
		ApprovedScopePaths: append([]string(nil), handoff.ScopePaths...),
		Leaves:             leaves,
		Review:             review,
		DynamicLeafMin:     uint32(handoff.DynamicLeafMin),
		DynamicLeafMax:     uint32(handoff.DynamicLeafMax),
		PolicyDigestHex:    policyDigestHex,
	})
}

// ValidateNoRedispatch enforces the one-layer no-redispatch invariant: every
// edge originates at the orchestrator, no leaf has CanRedispatch, and no edge
// targets the orchestrator.
func ValidateNoRedispatch(workflow *CompiledWorkflow) error {
	if workflow == nil {
		return fmt.Errorf("%w: workflow is nil", ErrInvalidInput)
	}
	kinds := make(map[string]NodeKind, len(workflow.Nodes))
	for _, node := range workflow.Nodes {
		if node.Kind == NodeLeaf && node.CanRedispatch {
			return fmt.Errorf("%w: leaf %q sets CanRedispatch", ErrRedispatch, node.NodeID)
		}
		kinds[node.NodeID] = node.Kind
	}
	for _, edge := range workflow.Edges {
		if kinds[edge.FromNodeID] != NodeOrchestrator {
			return fmt.Errorf("%w: edge from non-orchestrator %q", ErrRedispatch, edge.FromNodeID)
		}
		if edge.ToNodeID == OrchestratorNodeID {
			return fmt.Errorf("%w: edge targets orchestrator", ErrRedispatch)
		}
		if kinds[edge.ToNodeID] == "" {
			return fmt.Errorf("%w: edge targets unknown node %q", ErrPlanInvalid, edge.ToNodeID)
		}
	}
	return nil
}

// ValidateSealedActions rejects any leaf grant that carries dispatch, merge,
// deploy, release, promote, force-push, or branch-delete authority.
func ValidateSealedActions(workflow *CompiledWorkflow) error {
	if workflow == nil {
		return fmt.Errorf("%w: workflow is nil", ErrInvalidInput)
	}
	forbidden := make(map[string]struct{}, len(ForbiddenLeafActions))
	for _, action := range ForbiddenLeafActions {
		forbidden[action] = struct{}{}
	}
	for _, node := range workflow.Nodes {
		if node.Kind != NodeLeaf {
			continue
		}
		if len(node.Actions) != 1 || node.Actions[0] != LeafGrantAction {
			return fmt.Errorf("%w: leaf %q actions must be exactly [%s]", ErrPlanInvalid, node.NodeID, LeafGrantAction)
		}
		for _, action := range node.Actions {
			if _, bad := forbidden[action]; bad {
				return fmt.Errorf("%w: leaf %q carries forbidden action %q", ErrPlanInvalid, node.NodeID, action)
			}
		}
	}
	for _, action := range workflow.LeafActions {
		if _, bad := forbidden[action]; bad {
			return fmt.Errorf("%w: leaf action set includes %q", ErrPlanInvalid, action)
		}
	}
	return nil
}

// DigestWorkflow binds the canonical workflow projection (stable field order).
func DigestWorkflow(workflow *CompiledWorkflow) contracts.Digest {
	type projection struct {
		Domain          string   `json:"domain"`
		SchemaVersion   string   `json:"schemaVersion"`
		WorkflowVersion string   `json:"workflowVersion"`
		RunID           string   `json:"runId"`
		PlanID          string   `json:"planId"`
		BaseGitOID      string   `json:"baseGitOid"`
		N               int      `json:"n"`
		Nodes           []Node   `json:"nodes"`
		Edges           []Edge   `json:"edges"`
		Gates           []Gate   `json:"gates"`
		LeafActions     []string `json:"leafActions"`
	}
	payload, err := json.Marshal(projection{
		Domain:          DigestDomain,
		SchemaVersion:   workflow.SchemaVersion,
		WorkflowVersion: workflow.WorkflowVersion,
		RunID:           workflow.RunID,
		PlanID:          workflow.PlanID,
		BaseGitOID:      workflow.BaseGitOID,
		N:               workflow.N,
		Nodes:           workflow.Nodes,
		Edges:           workflow.Edges,
		Gates:           workflow.Gates,
		LeafActions:     workflow.LeafActions,
	})
	if err != nil {
		sum := sha256.Sum256([]byte("marshal-failed"))
		return contracts.Digest{Algorithm: "sha256", Hex: hex.EncodeToString(sum[:])}
	}
	return digestBytes(payload)
}

func validateRequest(request CompileRequest) error {
	if request.Tenant == "" || request.Principal == "" || request.Session == "" {
		return fmt.Errorf("%w: authenticated scope incomplete", ErrInvalidInput)
	}
	if request.RunID == "" || request.PlanID == "" {
		return fmt.Errorf("%w: run or plan identity missing", ErrInvalidInput)
	}
	if !validGitOID(request.BaseGitOID) {
		return fmt.Errorf("%w: base git oid malformed", ErrInvalidInput)
	}
	if len(request.ApprovedScopePaths) == 0 || len(request.ApprovedScopePaths) > 64 {
		return fmt.Errorf("%w: approved scope empty or unbounded", ErrPlanInvalid)
	}
	for _, root := range request.ApprovedScopePaths {
		if !validRepositoryPath(root) {
			return fmt.Errorf("%w: approved scope path %q not normalized", ErrPlanInvalid, root)
		}
	}
	if request.PolicyDigestHex != "" && !isHexDigest(request.PolicyDigestHex) {
		return fmt.Errorf("%w: policy digest malformed", ErrInvalidInput)
	}
	return nil
}

func validateLeaves(leaves []LeafInput, approvedScope []string) error {
	if len(leaves) == 0 || len(leaves) > 3 {
		return fmt.Errorf("%w: leaf count %d outside 1..3", ErrPlanInvalid, len(leaves))
	}
	seen := map[string]struct{}{
		OrchestratorNodeID: {},
		ReviewNodeID:       {},
	}
	for index, leaf := range leaves {
		if !nodeIDPattern.MatchString(leaf.NodeID) {
			return fmt.Errorf("%w: node id %q malformed", ErrPlanInvalid, leaf.NodeID)
		}
		if _, duplicate := seen[leaf.NodeID]; duplicate {
			return fmt.Errorf("%w: node id %q not unique", ErrPlanInvalid, leaf.NodeID)
		}
		seen[leaf.NodeID] = struct{}{}
		if leaf.HolderPrincipal != "" && !validPrincipalID(leaf.HolderPrincipal) {
			return fmt.Errorf("%w: leaf %s holder principal invalid", ErrPlanInvalid, leaf.NodeID)
		}
		if len(leaf.Goal) == 0 {
			return fmt.Errorf("%w: leaf %s goal empty", ErrPlanInvalid, leaf.NodeID)
		}
		if len(leaf.OwnedPaths) == 0 || len(leaf.OwnedPaths) > 64 || len(leaf.ForbiddenPaths) > 64 {
			return fmt.Errorf("%w: leaf %s scope cardinality invalid", ErrPlanInvalid, leaf.NodeID)
		}
		for _, path := range leaf.OwnedPaths {
			if !validRepositoryPath(path) {
				return fmt.Errorf("%w: leaf %s owned path %q not normalized", ErrPlanInvalid, leaf.NodeID, path)
			}
			if !pathWithinScope(path, approvedScope) {
				return fmt.Errorf("%w: leaf %s owned path %q escapes approved scope", ErrPlanInvalid, leaf.NodeID, path)
			}
		}
		for _, path := range leaf.ForbiddenPaths {
			if !validRepositoryPath(path) {
				return fmt.Errorf("%w: leaf %s forbidden path %q not normalized", ErrPlanInvalid, leaf.NodeID, path)
			}
		}
		for other := index + 1; other < len(leaves); other++ {
			for _, first := range leaf.OwnedPaths {
				for _, second := range leaves[other].OwnedPaths {
					if pathsCollide(first, second) {
						return fmt.Errorf("%w: leaf scopes %q and %q overlap", ErrPlanInvalid, first, second)
					}
				}
			}
		}
	}
	return nil
}

func digestBytes(content []byte) contracts.Digest {
	sum := sha256.Sum256(content)
	return contracts.Digest{Algorithm: "sha256", Hex: hex.EncodeToString(sum[:])}
}

// ContainsForbiddenAction reports whether any action is in the sealed deny set.
func ContainsForbiddenAction(actions []string) bool {
	for _, action := range actions {
		for _, forbidden := range ForbiddenLeafActions {
			if action == forbidden {
				return true
			}
		}
		// Also reject bare merge/deploy vocabulary fragments.
		lower := strings.ToLower(action)
		if strings.Contains(lower, "merge") || strings.Contains(lower, "deploy") ||
			strings.Contains(lower, "force_push") || strings.Contains(lower, "promote") {
			return true
		}
	}
	return false
}
