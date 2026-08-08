package factory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factory/roster"
)

// AdmitChangeIntent admits one approved ChangeIntent under the authenticated
// session and opens its run in PLANNING. Admission is authorization-first:
// caller cross-check, current policy, intent approval, exact Git base, and
// supporting evidence all revalidate before any canonical fact commits, and
// every denial shares the static ErrNotFoundOrDenied. An exact authenticated
// idempotent replay returns the original run without re-executing; a reused
// key carrying different facts denies without mutation.
func (k *Kernel) AdmitChangeIntent(ctx context.Context, request AdmitRequest) (AdmitResult, error) {
	if k == nil || ctx == nil || !validIdentity(request.Authenticated) ||
		!validIdempotencyKey(request.IdempotencyKey) || request.Intent == nil {
		return AdmitResult{}, ErrInvalidInput
	}
	if err := crossCheck(request.Authenticated, request.Caller); err != nil {
		return AdmitResult{}, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return AdmitResult{}, ErrInvalidInput
	}
	if err := k.authorize(ctx, request.Authenticated, "factory.admit"); err != nil {
		return AdmitResult{}, err
	}
	if err := k.revalidateIntent(ctx, request); err != nil {
		return AdmitResult{}, err
	}
	if err := validateLeafSpecs(request.Leaves, request.ApprovedScopePaths); err != nil {
		return AdmitResult{}, err
	}
	intentBytes, err := marshalDeterministic(request.Intent)
	if err != nil {
		return AdmitResult{}, err
	}
	intentDigest := digestBytes(intentBytes)
	requestDigest := admissionRequestDigest(intentDigest, request)
	existing, found, err := lookupIdempotency(ctx, k.db, request.Authenticated, "admit", request.IdempotencyKey)
	if err != nil {
		return AdmitResult{}, err
	}
	if found {
		if existing.requestDigest != requestDigest {
			return AdmitResult{}, ErrNotFoundOrDenied
		}
		return AdmitResult{RunID: existing.runID, Replayed: true}, nil
	}
	base, err := k.bases.CurrentBase(ctx, request.Authenticated)
	if err != nil || base != request.Intent.GetRepositoryGitOid() {
		return AdmitResult{}, ErrNotFoundOrDenied
	}
	// The bounded slice turns exactly one approved intent into one run; a
	// second admission of the same intent under a different key denies
	// statically rather than opening a duplicate DAG. This read-only check
	// runs before any payload staging; the transaction re-checks atomically.
	var duplicateRun string
	err = k.db.QueryRowContext(ctx, `SELECT run_id FROM factory_runs
		WHERE tenant_id=? AND principal_id=? AND intent_id=?`,
		request.Authenticated.Tenant, request.Authenticated.Principal, request.Intent.GetIntentId().GetValue()).
		Scan(&duplicateRun)
	if err == nil {
		return AdmitResult{}, ErrNotFoundOrDenied
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return AdmitResult{}, fmt.Errorf("factory: read admitted intent: %w", err)
	}
	runID := identity("ouroboros.stage05.run.v1",
		request.Authenticated.Tenant, request.Authenticated.Principal, intentDigest, request.IdempotencyKey)
	planID := identity("ouroboros.stage05.plan.v1", runID)
	goalArtifacts, err := k.stageGoals(ctx, request, runID)
	if err != nil {
		return AdmitResult{}, err
	}
	intentArtifact, err := k.stagePayload(ctx, request.Authenticated.Tenant, intentBytes)
	if err != nil {
		return AdmitResult{}, err
	}
	compiled, err := k.compilePlan(ctx, request.Authenticated, request, runID, planID, goalArtifacts)
	if err != nil {
		return AdmitResult{}, err
	}
	tx, err := k.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return AdmitResult{}, fmt.Errorf("factory: begin admission: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The bounded slice turns exactly one approved intent into one run; a
	// second admission of the same intent under a different key denies
	// statically rather than opening a duplicate DAG. The transaction
	// re-checks the read-only pre-admission denial atomically.
	var duplicateRunInTx string
	err = tx.QueryRowContext(ctx, `SELECT run_id FROM factory_runs
		WHERE tenant_id=? AND principal_id=? AND intent_id=?`,
		request.Authenticated.Tenant, request.Authenticated.Principal, request.Intent.GetIntentId().GetValue()).
		Scan(&duplicateRunInTx)
	if err == nil {
		return AdmitResult{}, ErrNotFoundOrDenied
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return AdmitResult{}, fmt.Errorf("factory: re-read admitted intent: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO factory_runs
		(tenant_id,principal_id,run_id,session_id,intent_id,intent_digest,intent_artifact_id,
		 repository_git_oid,plan_id,admitted_at_ms)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		request.Authenticated.Tenant, request.Authenticated.Principal, runID, request.Authenticated.Session,
		request.Intent.GetIntentId().GetValue(), intentDigest, intentArtifact.artifactID,
		request.Intent.GetRepositoryGitOid(), planID, k.clock.NowUnixMilli()); err != nil {
		return AdmitResult{}, fmt.Errorf("factory: commit run: %w", err)
	}
	if err := appendRunState(ctx, tx, request.Authenticated, runID,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_PLANNING, k.clock.NowUnixMilli()); err != nil {
		return AdmitResult{}, err
	}
	for _, node := range compiled.nodes {
		if err := insertPlanNode(ctx, tx, request.Authenticated, runID, node); err != nil {
			return AdmitResult{}, err
		}
	}
	for _, gate := range compiled.plan.GetGates() {
		if err := insertGate(ctx, tx, request.Authenticated, runID, gate, k.clock.NowUnixMilli()); err != nil {
			return AdmitResult{}, err
		}
	}
	for _, node := range compiled.nodes {
		if node.node.GetKind() != contractsv1.PlanNodeKind_PLAN_NODE_KIND_LEAF {
			continue
		}
		if _, err := k.roster.Issue(ctx, tx, k.leaseFor(node, request.Authenticated, runID)); err != nil {
			return AdmitResult{}, err
		}
	}
	if err := insertIdempotency(ctx, tx, request.Authenticated, "admit",
		request.IdempotencyKey, requestDigest, runID, k.clock.NowUnixMilli()); err != nil {
		return AdmitResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return AdmitResult{}, fmt.Errorf("factory: commit admission: %w", err)
	}
	return AdmitResult{RunID: runID}, nil
}

// CancelChangeRun revokes one admitted run at a safe point: the run reaches
// the terminal CANCELLED state, every later fence authorization and pending
// effect denies against current state, and an exact idempotent replay returns
// the original outcome. Cancelling an unknown, cross-principal, or already
// terminal run shares the static denial.
func (k *Kernel) CancelChangeRun(ctx context.Context, request CancelRequest) (CancelResult, error) {
	if k == nil || ctx == nil || !validIdentity(request.Authenticated) ||
		!validIdempotencyKey(request.IdempotencyKey) || request.RunID == "" {
		return CancelResult{}, ErrInvalidInput
	}
	if err := crossCheck(request.Authenticated, request.Caller); err != nil {
		return CancelResult{}, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return CancelResult{}, ErrInvalidInput
	}
	if err := k.authorize(ctx, request.Authenticated, "factory.cancel"); err != nil {
		return CancelResult{}, err
	}
	requestDigest := identity("ouroboros.stage05.cancel-request.v1", request.RunID)
	existing, found, err := lookupIdempotency(ctx, k.db, request.Authenticated, "cancel", request.IdempotencyKey)
	if err != nil {
		return CancelResult{}, err
	}
	if found {
		if existing.requestDigest != requestDigest {
			return CancelResult{}, ErrNotFoundOrDenied
		}
		return CancelResult{RunID: existing.runID, Replayed: true}, nil
	}
	tx, err := k.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return CancelResult{}, fmt.Errorf("factory: begin cancellation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, runFound, err := lookupRun(ctx, tx, request.Authenticated, request.RunID); err != nil {
		return CancelResult{}, err
	} else if !runFound {
		return CancelResult{}, ErrNotFoundOrDenied
	}
	state, err := currentRunState(ctx, tx, request.Authenticated, request.RunID)
	if err != nil {
		return CancelResult{}, err
	}
	if terminalRunState(state) {
		return CancelResult{}, ErrNotFoundOrDenied
	}
	if err := appendRunState(ctx, tx, request.Authenticated, request.RunID,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANCELLED, k.clock.NowUnixMilli()); err != nil {
		return CancelResult{}, err
	}
	if err := insertIdempotency(ctx, tx, request.Authenticated, "cancel",
		request.IdempotencyKey, requestDigest, request.RunID, k.clock.NowUnixMilli()); err != nil {
		return CancelResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CancelResult{}, fmt.Errorf("factory: commit cancellation: %w", err)
	}
	return CancelResult{RunID: request.RunID}, nil
}

// revalidateIntent checks the intent's trust facts under current policy: body
// identity matches the authenticated principal, the base is well-formed, the
// approval is present, receipt-completed, and unexpired, and supporting
// evidence anchors the request. Every failure denies statically.
func (k *Kernel) revalidateIntent(ctx context.Context, request AdmitRequest) error {
	intent := request.Intent
	if intent.GetIntentId().GetValue() == "" || !isGitOID(intent.GetRepositoryGitOid()) ||
		intent.GetScopeDigest().GetHex() == "" || !isHexDigest(intent.GetScopeDigest().GetHex()) ||
		len(intent.GetSupportingEvidence()) == 0 {
		return ErrNotFoundOrDenied
	}
	requestedBy := intent.GetRequestedBy()
	if requestedBy.GetPrincipalId().GetValue() != request.Authenticated.Principal ||
		requestedBy.GetTenantId().GetValue() != request.Authenticated.Tenant {
		return ErrNotFoundOrDenied
	}
	approval := intent.GetApproval()
	if approval.GetReceipt().GetStatus() != contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED {
		return ErrNotFoundOrDenied
	}
	if expiresAt := approval.GetExpiresAt(); expiresAt != nil &&
		k.clock.NowUnixMilli() >= expiresAt.AsTime().UnixMilli() {
		return ErrNotFoundOrDenied
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// stageGoals encrypts every node goal before the ledger transaction: leaf
// goals arrive with the request, and the kernel authors deterministic goal
// descriptors for the orchestrator and review nodes.
func (k *Kernel) stageGoals(ctx context.Context, request AdmitRequest, runID string) (map[string]stagedPayload, error) {
	artifacts := make(map[string]stagedPayload, len(request.Leaves)+2)
	orchestratorGoal := fmt.Sprintf("ouroboros.stage05.goal.orchestrator.v1\nrun=%s\nintent=%s\n",
		runID, request.Intent.GetIntentId().GetValue())
	staged, err := k.stagePayload(ctx, request.Authenticated.Tenant, []byte(orchestratorGoal))
	if err != nil {
		return nil, err
	}
	artifacts[orchestratorNodeID] = staged
	for _, leaf := range request.Leaves {
		staged, err := k.stagePayload(ctx, request.Authenticated.Tenant, leaf.Goal)
		if err != nil {
			return nil, err
		}
		artifacts[leaf.NodeID] = staged
	}
	if request.Review {
		reviewGoal := fmt.Sprintf("ouroboros.stage05.goal.review.v1\nrun=%s\nintent=%s\n",
			runID, request.Intent.GetIntentId().GetValue())
		staged, err := k.stagePayload(ctx, request.Authenticated.Tenant, []byte(reviewGoal))
		if err != nil {
			return nil, err
		}
		artifacts[reviewNodeID] = staged
	}
	return artifacts, nil
}

// admissionRequestDigest binds the exact authenticated admission facts so a
// replay is only exact when every fact matches.
func admissionRequestDigest(intentDigest string, request AdmitRequest) string {
	fields := []string{intentDigest}
	for _, leaf := range request.Leaves {
		fields = append(fields, leaf.NodeID, digestBytes(leaf.Goal),
			strings.Join(leaf.OwnedPaths, "\x1f"), strings.Join(leaf.ForbiddenPaths, "\x1f"), leaf.HolderPrincipal)
	}
	if request.Review {
		fields = append(fields, "review")
	}
	return identity("ouroboros.stage05.admit-request.v1", fields...)
}

// leaseFor derives the roster issuance fact matching the compiled node's
// embedded lease exactly: same fence, identity, holder, and expiry.
func (k *Kernel) leaseFor(node compiledNode, authenticated Identity, runID string) roster.Lease {
	return roster.Lease{
		Tenant:            authenticated.Tenant,
		Principal:         authenticated.Principal,
		RunID:             runID,
		NodeID:            node.node.GetNodeId(),
		HolderPrincipalID: node.leaseHolder,
		ExpiresAtMs:       node.leaseExpiresAt,
	}
}

// insertPlanNode commits one validated plan node row with its JSON scope and
// leaf-carried route and grant facts; non-leaf rows carry none by schema check.
func insertPlanNode(ctx context.Context, tx *sql.Tx, authenticated Identity, runID string, node compiledNode) error {
	var kindText string
	switch node.node.GetKind() {
	case contractsv1.PlanNodeKind_PLAN_NODE_KIND_ORCHESTRATOR:
		kindText = "orchestrator"
	case contractsv1.PlanNodeKind_PLAN_NODE_KIND_LEAF:
		kindText = "leaf"
	case contractsv1.PlanNodeKind_PLAN_NODE_KIND_REVIEW:
		kindText = "review"
	default:
		return ErrInvalidInput
	}
	var routeProfileDigest, routeModel, routeRationale, grantActions, grantAllowedPaths, grantNonce, grantPolicyDigest any
	var grantExpiresAtMs, grantRevocationEpoch, grantCommandFence any
	if grant := node.node.GetCapabilityGrant(); node.node.GetKind() == contractsv1.PlanNodeKind_PLAN_NODE_KIND_LEAF {
		route := node.node.GetRoute()
		routeProfileDigest = route.GetProfileDigest().GetHex()
		routeModel = route.GetModelIdentity()
		routeRationale = route.GetRationaleCode()
		grantActions = jsonPaths(grant.GetActions())
		grantAllowedPaths = jsonPaths(grant.GetAllowedPaths())
		grantNonce = grant.GetNonce()
		grantExpiresAtMs = grant.GetExpiresAt().AsTime().UnixMilli()
		grantRevocationEpoch = int64(grant.GetRevocationEpoch())
		grantCommandFence = int64(grant.GetCommandFence())
		grantPolicyDigest = grant.GetPolicyDigest().GetHex()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO factory_plan_nodes
		(tenant_id,principal_id,run_id,node_id,kind,goal_artifact_id,goal_digest,owned_paths,forbidden_paths,
		 route_profile_digest,route_model_identity,route_rationale_code,grant_actions,grant_allowed_paths,
		 grant_nonce,grant_expires_at_ms,grant_revocation_epoch,grant_command_fence,grant_policy_digest)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		authenticated.Tenant, authenticated.Principal, runID, node.node.GetNodeId(), kindText,
		node.goalArtifactID, node.goalDigestHex, jsonPaths(node.node.GetOwnedPaths()),
		jsonPaths(node.node.GetForbiddenPaths()), routeProfileDigest, routeModel, routeRationale,
		grantActions, grantAllowedPaths, grantNonce, grantExpiresAtMs, grantRevocationEpoch,
		grantCommandFence, grantPolicyDigest); err != nil {
		return fmt.Errorf("factory: commit plan node: %w", err)
	}
	return nil
}

// insertGate commits one gate roster row and its initial PENDING evaluation.
func insertGate(ctx context.Context, tx *sql.Tx, authenticated Identity, runID string, gate *contractsv1.GateSpec, atMs int64) error {
	kindText, err := gateKindText(gate.GetKind())
	if err != nil {
		return err
	}
	required := 0
	if gate.GetRequired() {
		required = 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO factory_gates
		(tenant_id,principal_id,run_id,gate_id,kind,required) VALUES (?,?,?,?,?,?)`,
		authenticated.Tenant, authenticated.Principal, runID, gate.GetGateId().GetValue(), kindText, required); err != nil {
		return fmt.Errorf("factory: commit gate: %w", err)
	}
	statusText, err := gateStatusText(gate.GetStatus())
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO factory_gate_states
		(tenant_id,principal_id,run_id,gate_id,sequence,status,occurred_at_ms) VALUES (?,?,?,?,1,?,?)`,
		authenticated.Tenant, authenticated.Principal, runID, gate.GetGateId().GetValue(), statusText, atMs); err != nil {
		return fmt.Errorf("factory: commit gate state: %w", err)
	}
	return nil
}
