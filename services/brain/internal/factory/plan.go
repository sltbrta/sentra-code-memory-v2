package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"buf.build/go/protovalidate"
	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	orchestratorNodeID = "orchestrator"
	reviewNodeID       = "review"
	// leafGrantAction is the single bounded Stage 05 leaf authority: brokered
	// execution inside the attenuated scope. Leaf grants never carry
	// factory.dispatch or factory.task.create.
	leafGrantAction = "factory.leaf.execute"
)

var nodeIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// compiledNode is one plan node joined with its vault staging facts for
// storage.
type compiledNode struct {
	node           *contractsv1.PlanNode
	goalArtifactID string
	goalDigestHex  string
	leaseHolder    string
	leaseExpiresAt int64
}

// compiledPlan is the validated DAG plus its storage rows.
type compiledPlan struct {
	plan  *contractsv1.ChangePlan
	nodes []compiledNode
}

// compilePlan builds and validates the frozen one-layer DAG from one approved
// admission: one orchestrator, one to three prefix-disjoint in-scope leaves,
// at most one review node, the four non-removable required gates, and edges
// originating only at the orchestrator and reaching every node. The compiled
// proto is re-validated through protovalidate so any divergence between the
// kernel's mirrored checks and the frozen CEL rules fails closed.
func (k *Kernel) compilePlan(
	ctx context.Context,
	authenticated Identity,
	request AdmitRequest,
	runID, planID string,
	goalArtifacts map[string]stagedPayload,
) (*compiledPlan, error) {
	if err := validateLeafSpecs(request.Leaves, request.ApprovedScopePaths); err != nil {
		return nil, err
	}
	intent := request.Intent
	plan := &contractsv1.ChangePlan{
		PlanId: &contractsv1.Identifier{Namespace: "factory-plan", Value: planID},
		RunId:  &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
		Intent: intent,
		State:  contractsv1.ChangeRunState_CHANGE_RUN_STATE_PLANNING,
	}
	orchestrator := &contractsv1.PlanNode{
		NodeId: orchestratorNodeID,
		Kind:   contractsv1.PlanNodeKind_PLAN_NODE_KIND_ORCHESTRATOR,
		GoalDigest: &contractsv1.Digest{
			Algorithm: "sha256",
			Hex:       goalArtifacts[orchestratorNodeID].digestHex,
		},
	}
	plan.Nodes = append(plan.Nodes, orchestrator)
	nodes := []compiledNode{{
		node:           orchestrator,
		goalArtifactID: goalArtifacts[orchestratorNodeID].artifactID,
		goalDigestHex:  goalArtifacts[orchestratorNodeID].digestHex,
	}}
	now := k.clock.NowUnixMilli()
	for _, leaf := range request.Leaves {
		staged := goalArtifacts[leaf.NodeID]
		holder := leaf.HolderPrincipal
		if holder == "" {
			holder = leaf.NodeID + "-worker"
		}
		route, err := k.router.Route(ctx, RouteRequest{
			RunID: runID, NodeID: leaf.NodeID, GoalDigestHex: staged.digestHex, OwnedPaths: leaf.OwnedPaths,
		})
		if err != nil {
			return nil, fmt.Errorf("factory: route leaf %s: %w", leaf.NodeID, err)
		}
		if !isHexDigest(route.ProfileDigestHex) || !modelIdentityPattern.MatchString(route.ModelIdentity) ||
			route.RationaleCode == "" || len(route.RationaleCode) > 64 ||
			!rationaleCodePattern.MatchString(route.RationaleCode) {
			return nil, fmt.Errorf("%w: router returned invalid decision", ErrInvalidInput)
		}
		fence := uint64(1)
		expiresAt := now + k.leaseTTLMillis
		leaseID := identity("ouroboros.stage05.lease.v1",
			authenticated.Tenant, authenticated.Principal, runID, leaf.NodeID, "1")
		holderRef := &contractsv1.AuthenticatedPrincipalRef{
			PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: holder},
			TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: authenticated.Tenant},
		}
		initiatorRef := &contractsv1.AuthenticatedPrincipalRef{
			PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: authenticated.Principal},
			TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: authenticated.Tenant},
			SessionId:   &contractsv1.Identifier{Namespace: "session", Value: authenticated.Session},
		}
		node := &contractsv1.PlanNode{
			NodeId:         leaf.NodeID,
			Kind:           contractsv1.PlanNodeKind_PLAN_NODE_KIND_LEAF,
			GoalDigest:     &contractsv1.Digest{Algorithm: "sha256", Hex: staged.digestHex},
			OwnedPaths:     append([]string(nil), leaf.OwnedPaths...),
			ForbiddenPaths: append([]string(nil), leaf.ForbiddenPaths...),
			Route: &contractsv1.ModelRoute{
				ProfileDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: route.ProfileDigestHex},
				ModelIdentity: route.ModelIdentity,
				RationaleCode: route.RationaleCode,
			},
			Lease: &contractsv1.Lease{
				LeaseId:   &contractsv1.Identifier{Namespace: "lease", Value: leaseID},
				Holder:    holderRef,
				Fence:     fence,
				ExpiresAt: timestamppb.New(unixMillis(expiresAt)),
			},
			CapabilityGrant: &contractsv1.CapabilityGrant{
				GrantId: &contractsv1.Identifier{
					Namespace: "factory-grant",
					Value:     identity("ouroboros.stage05.grant.v1", runID, leaf.NodeID),
				},
				Initiator:  initiatorRef,
				TaskId:     &contractsv1.Identifier{Namespace: "factory-task", Value: leaf.NodeID},
				WorkflowId: &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
				Lease: &contractsv1.Lease{
					LeaseId:   &contractsv1.Identifier{Namespace: "lease", Value: leaseID},
					Holder:    holderRef,
					Fence:     fence,
					ExpiresAt: timestamppb.New(unixMillis(expiresAt)),
				},
				Actions:          []string{leafGrantAction},
				Resources:        []*contractsv1.Identifier{{Namespace: "tenant", Value: authenticated.Tenant}},
				RepositoryGitOid: intent.GetRepositoryGitOid(),
				AllowedPaths:     append([]string(nil), leaf.OwnedPaths...),
				Nonce:            identity("ouroboros.stage05.grant-nonce.v1", runID, leaf.NodeID, "1"),
				RevocationEpoch:  k.revocationEpoch,
				ExpiresAt:        timestamppb.New(unixMillis(expiresAt)),
				PolicyDigest:     &contractsv1.Digest{Algorithm: "sha256", Hex: k.policyDigestHex},
				CommandFence:     fence,
			},
		}
		plan.Nodes = append(plan.Nodes, node)
		nodes = append(nodes, compiledNode{
			node:           node,
			goalArtifactID: staged.artifactID,
			goalDigestHex:  staged.digestHex,
			leaseHolder:    holder,
			leaseExpiresAt: expiresAt,
		})
		plan.Edges = append(plan.Edges, &contractsv1.PlanEdge{
			FromNodeId: orchestratorNodeID,
			ToNodeId:   leaf.NodeID,
		})
	}
	if request.Review {
		reviewStaged := goalArtifacts[reviewNodeID]
		reviewNode := &contractsv1.PlanNode{
			NodeId:     reviewNodeID,
			Kind:       contractsv1.PlanNodeKind_PLAN_NODE_KIND_REVIEW,
			GoalDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: reviewStaged.digestHex},
		}
		plan.Nodes = append(plan.Nodes, reviewNode)
		nodes = append(nodes, compiledNode{
			node:           reviewNode,
			goalArtifactID: reviewStaged.artifactID,
			goalDigestHex:  reviewStaged.digestHex,
		})
		plan.Edges = append(plan.Edges, &contractsv1.PlanEdge{
			FromNodeId: orchestratorNodeID,
			ToNodeId:   reviewNodeID,
		})
	}
	for _, kind := range []contractsv1.FactoryGateKind{
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_BUILD,
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_TEST,
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_DOCS,
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_SECURITY,
	} {
		kindText, err := gateKindText(kind)
		if err != nil {
			return nil, err
		}
		plan.Gates = append(plan.Gates, &contractsv1.GateSpec{
			GateId: &contractsv1.Identifier{
				Namespace: "factory-gate",
				Value:     identity("ouroboros.stage05.gate.v1", runID, kindText),
			},
			Kind:     kind,
			Required: true,
			Status:   contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PENDING,
		})
	}
	validator, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("factory: build validator: %w", err)
	}
	if err := validator.Validate(plan); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPlanInvalid, err)
	}
	return &compiledPlan{plan: plan, nodes: nodes}, nil
}

// validateLeafSpecs enforces the frozen DAG shape before any canonical fact
// exists: one to three leaves with unique well-formed identifiers, normalized
// repository-relative paths, pairwise prefix-disjoint owned scopes, and scopes
// attenuating the intent's approved scope.
func validateLeafSpecs(leaves []LeafSpec, approvedScope []string) error {
	if len(leaves) == 0 || len(leaves) > 3 {
		return fmt.Errorf("%w: leaf count %d outside 1..3", ErrPlanInvalid, len(leaves))
	}
	if len(approvedScope) == 0 || len(approvedScope) > 64 {
		return fmt.Errorf("%w: approved scope is empty or unbounded", ErrPlanInvalid)
	}
	for _, root := range approvedScope {
		if !validRepositoryPath(root) {
			return fmt.Errorf("%w: approved scope path %q is not normalized", ErrPlanInvalid, root)
		}
	}
	seen := make(map[string]struct{}, len(leaves)+2)
	seen[orchestratorNodeID] = struct{}{}
	seen[reviewNodeID] = struct{}{}
	for index, leaf := range leaves {
		if !nodeIDPattern.MatchString(leaf.NodeID) {
			return fmt.Errorf("%w: node id %q malformed", ErrPlanInvalid, leaf.NodeID)
		}
		if _, duplicate := seen[leaf.NodeID]; duplicate {
			return fmt.Errorf("%w: node id %q is not unique", ErrPlanInvalid, leaf.NodeID)
		}
		seen[leaf.NodeID] = struct{}{}
		if leaf.HolderPrincipal != "" && !validPrincipalID(leaf.HolderPrincipal) {
			return fmt.Errorf("%w: leaf %s holder principal is not a printable bounded identity", ErrPlanInvalid, leaf.NodeID)
		}
		if len(leaf.Goal) == 0 {
			return fmt.Errorf("%w: leaf %s goal is empty", ErrPlanInvalid, leaf.NodeID)
		}
		if len(leaf.OwnedPaths) == 0 || len(leaf.OwnedPaths) > 64 || len(leaf.ForbiddenPaths) > 64 {
			return fmt.Errorf("%w: leaf %s scope cardinality invalid", ErrPlanInvalid, leaf.NodeID)
		}
		for _, path := range leaf.OwnedPaths {
			if !validRepositoryPath(path) {
				return fmt.Errorf("%w: leaf %s owned path %q is not normalized", ErrPlanInvalid, leaf.NodeID, path)
			}
			if !pathWithinScope(path, approvedScope) {
				return fmt.Errorf("%w: leaf %s owned path %q escapes approved scope", ErrPlanInvalid, leaf.NodeID, path)
			}
		}
		for _, path := range leaf.ForbiddenPaths {
			if !validRepositoryPath(path) {
				return fmt.Errorf("%w: leaf %s forbidden path %q is not normalized", ErrPlanInvalid, leaf.NodeID, path)
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

// jsonPaths encodes a bounded path list for the schema's JSON shape columns.
func jsonPaths(paths []string) string {
	if len(paths) == 0 {
		return "[]"
	}
	encoded, err := json.Marshal(paths)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}
