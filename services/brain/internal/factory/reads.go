package factory

import (
	"context"
	"errors"
	"fmt"

	"buf.build/go/protovalidate"
	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// FindingsPage is one bounded page of typed findings in the canonical
// (occurred-at, finding-identity) total order.
type FindingsPage struct {
	// Findings lists only findings owned by the authorized run.
	Findings []*contractsv1.ReviewFinding
	// NextCursor continues within the same listing; empty ends the page stream.
	NextCursor string
}

// GetChangePlan reads the current typed one-layer DAG for one admitted run,
// reconstructing the frozen ChangePlan from canonical facts: the admitted
// intent hydrated from the vault, current run state, plan nodes with current
// fenced leases, and the gate roster with current statuses. Unknown,
// cross-principal, and malformed runs share the static denial. The served
// plan is re-validated through protovalidate before it leaves the kernel.
func (k *Kernel) GetChangePlan(ctx context.Context, authenticated Identity, runID string) (*contractsv1.ChangePlan, error) {
	if k == nil || ctx == nil || !validIdentity(authenticated) || runID == "" {
		return nil, ErrInvalidInput
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return nil, ErrInvalidInput
	}
	run, found, err := lookupRun(ctx, k.db, authenticated, runID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFoundOrDenied
	}
	intentBytes, err := k.hydratePayload(ctx, authenticated.Tenant, stagedPayload{
		artifactID: run.intentArtifact,
		digestHex:  run.intentDigest,
	})
	if err != nil {
		return nil, err
	}
	intent := &contractsv1.ChangeIntent{}
	if err := proto.Unmarshal(intentBytes, intent); err != nil {
		return nil, errors.Join(ErrPayloadUnavailable, err)
	}
	state, err := currentRunState(ctx, k.db, authenticated, runID)
	if err != nil {
		return nil, err
	}
	if state == contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANCELLED {
		return nil, ErrNotFoundOrDenied
	}
	nodes, edges, err := k.planNodes(ctx, authenticated, runID, run.repositoryGitOID)
	if err != nil {
		return nil, err
	}
	gates, err := k.planGates(ctx, authenticated, runID)
	if err != nil {
		return nil, err
	}
	plan := &contractsv1.ChangePlan{
		PlanId: &contractsv1.Identifier{Namespace: "factory-plan", Value: run.planID},
		RunId:  &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
		Intent: intent,
		State:  state,
		Nodes:  nodes,
		Edges:  edges,
		Gates:  gates,
	}
	validator, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("factory: build validator: %w", err)
	}
	if err := validator.Validate(plan); err != nil {
		return nil, fmt.Errorf("%w: served plan failed contract validation: %v", ErrPayloadUnavailable, err)
	}
	return plan, nil
}

// PreviewChangeSet reads the atomic candidate preview for one run: the stored
// canonical preview bytes hydrated and digest-reverified from the vault, then
// overlaid with the current candidate state, current gate statuses, and — for
// a rejected candidate — its rollback receipt. Runs without a candidate share
// the static denial.
func (k *Kernel) PreviewChangeSet(ctx context.Context, authenticated Identity, runID string) (*contractsv1.ChangeSetPreview, error) {
	if k == nil || ctx == nil || !validIdentity(authenticated) || runID == "" {
		return nil, ErrInvalidInput
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return nil, ErrInvalidInput
	}
	if _, found, err := lookupRun(ctx, k.db, authenticated, runID); err != nil {
		return nil, err
	} else if !found {
		return nil, ErrNotFoundOrDenied
	}
	candidate, err := lookupCandidate(ctx, k.db, authenticated, runID)
	if err != nil {
		return nil, err
	}
	if candidate == nil {
		return nil, ErrNotFoundOrDenied
	}
	if runState, err := currentRunState(ctx, k.db, authenticated, runID); err != nil {
		return nil, err
	} else if runState == contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANCELLED {
		return nil, ErrNotFoundOrDenied
	}
	encoded, err := k.hydratePayload(ctx, authenticated.Tenant, stagedPayload{
		artifactID: candidate.artifactID,
		digestHex:  candidate.digestHex,
	})
	if err != nil {
		return nil, err
	}
	preview := &contractsv1.ChangeSetPreview{}
	if err := proto.Unmarshal(encoded, preview); err != nil {
		return nil, errors.Join(ErrPayloadUnavailable, err)
	}
	state, found, err := currentCandidateState(ctx, k.db, authenticated, runID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrPayloadUnavailable
	}
	preview.CandidateState = state
	for _, gate := range preview.GetGates() {
		status, err := currentGateStatus(ctx, k.db, authenticated, runID, gate.GetGateId().GetValue())
		if err != nil {
			return nil, err
		}
		gate.Status = status
	}
	if state == contractsv1.CandidateState_CANDIDATE_STATE_REJECTED {
		receipt, err := k.rollbackReceipt(ctx, authenticated, runID)
		if err != nil {
			return nil, err
		}
		preview.RollbackReceipt = receipt
	}
	validator, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("factory: build validator: %w", err)
	}
	if err := validator.Validate(preview); err != nil {
		return nil, fmt.Errorf("%w: served preview failed contract validation: %v", ErrPayloadUnavailable, err)
	}
	return preview, nil
}

// GetReviewFindings pages typed findings for one admitted run in the canonical
// (occurred-at, finding-identity) order through an opaque cursor. Every
// payload is reverified against its canonical digest during hydration; an
// unreadable or corrupt payload fails the whole page rather than silently
// skipping a finding.
func (k *Kernel) GetReviewFindings(
	ctx context.Context, authenticated Identity, runID, after string, limit uint32,
) (FindingsPage, error) {
	if k == nil || ctx == nil || !validIdentity(authenticated) || runID == "" || limit == 0 || limit > 100 {
		return FindingsPage{}, ErrInvalidInput
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return FindingsPage{}, ErrInvalidInput
	}
	if _, found, err := lookupRun(ctx, k.db, authenticated, runID); err != nil {
		return FindingsPage{}, err
	} else if !found {
		return FindingsPage{}, ErrNotFoundOrDenied
	}
	if state, err := currentRunState(ctx, k.db, authenticated, runID); err != nil {
		return FindingsPage{}, err
	} else if state == contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANCELLED {
		return FindingsPage{}, ErrNotFoundOrDenied
	}
	occurredAfter, idAfter, err := decodeCursor(after)
	if err != nil {
		return FindingsPage{}, err
	}
	var lastIncludedOccurredAtMs int64
	rows, err := k.db.QueryContext(ctx, `SELECT f.finding_id,f.severity,f.category,f.reviewer_principal_id,
		f.reviewer_session_id,f.reviewer_family,f.payload_artifact_id,f.payload_digest,f.evidence_count,
		f.occurred_at_ms,COALESCE(d.disposition,''),COALESCE(d.receipt_id,''),
		COALESCE(d.receipt_reason_code,''),COALESCE(d.dispositioned_at_ms,0)
		FROM factory_findings f
		LEFT JOIN factory_finding_dispositions d
		ON d.tenant_id=f.tenant_id AND d.principal_id=f.principal_id AND d.run_id=f.run_id
		AND d.finding_id=f.finding_id
		WHERE f.tenant_id=? AND f.principal_id=? AND f.run_id=?
		AND (f.occurred_at_ms > ? OR (f.occurred_at_ms = ? AND f.finding_id > ?))
		ORDER BY f.occurred_at_ms, f.finding_id LIMIT ?`,
		authenticated.Tenant, authenticated.Principal, runID, occurredAfter, occurredAfter, idAfter, int64(limit)+1)
	if err != nil {
		return FindingsPage{}, fmt.Errorf("factory: findings query: %w", err)
	}
	defer rows.Close()
	page := FindingsPage{Findings: make([]*contractsv1.ReviewFinding, 0, limit)}
	for rows.Next() {
		var (
			findingID, severity, category, reviewerPrincipal, reviewerSession, reviewerFamily string
			artifactID, payloadDigest, disposition, receiptID, receiptReason                  string
			evidenceCount                                                                     int
			occurredAtMs, dispositionedAtMs                                                   int64
		)
		if err := rows.Scan(&findingID, &severity, &category, &reviewerPrincipal, &reviewerSession,
			&reviewerFamily, &artifactID, &payloadDigest, &evidenceCount, &occurredAtMs,
			&disposition, &receiptID, &receiptReason, &dispositionedAtMs); err != nil {
			return FindingsPage{}, fmt.Errorf("factory: findings scan: %w", err)
		}
		if uint32(len(page.Findings)) == limit {
			lastIncluded := page.Findings[len(page.Findings)-1]
			page.NextCursor = encodeCursor(lastIncludedOccurredAtMs, lastIncluded.GetFindingId().GetValue())
			return page, rows.Err()
		}
		payloadBytes, err := k.hydratePayload(ctx, authenticated.Tenant, stagedPayload{
			artifactID: artifactID,
			digestHex:  payloadDigest,
		})
		if err != nil {
			return FindingsPage{}, err
		}
		payload, err := unmarshalFindingPayload(payloadBytes)
		if err != nil {
			return FindingsPage{}, err
		}
		finding, err := k.buildFinding(authenticated.Tenant, runID, findingID, severity, category, reviewerPrincipal,
			reviewerSession, reviewerFamily, payload, disposition, receiptID, receiptReason, dispositionedAtMs)
		if err != nil {
			return FindingsPage{}, err
		}
		page.Findings = append(page.Findings, finding)
		lastIncludedOccurredAtMs = occurredAtMs
	}
	if err := rows.Err(); err != nil {
		return FindingsPage{}, fmt.Errorf("factory: findings rows: %w", err)
	}
	return page, nil
}

// buildFinding reconstructs one served typed finding; exactly dispositioned
// findings carry a disposition receipt, and dismissal carries its evidence.
func (k *Kernel) buildFinding(
	tenant, runID, findingID, severity, category, reviewerPrincipal, reviewerSession, reviewerFamily string,
	payload findingPayload, disposition, receiptID, receiptReason string, dispositionedAtMs int64,
) (*contractsv1.ReviewFinding, error) {
	severityEnum, err := severityFromText(severity)
	if err != nil {
		return nil, err
	}
	categoryEnum, err := categoryFromText(category)
	if err != nil {
		return nil, err
	}
	finding := &contractsv1.ReviewFinding{
		FindingId: &contractsv1.Identifier{Namespace: "factory-finding", Value: findingID},
		RunId:     &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
		Severity:  severityEnum,
		Category:  categoryEnum,
		Summary:   payload.Summary,
		Reviewer: &contractsv1.AuthenticatedPrincipalRef{
			PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: reviewerPrincipal},
			TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: tenant},
			SessionId:   &contractsv1.Identifier{Namespace: "session", Value: reviewerSession},
		},
		ReviewerFamily: reviewerFamily,
	}
	for _, evidence := range payload.Evidence {
		reference := &contractsv1.EvidenceRef{
			EvidenceId:       &contractsv1.Identifier{Namespace: evidence.EvidenceNamespace, Value: evidence.EvidenceValue},
			SourceRevisionId: &contractsv1.Identifier{Namespace: evidence.RevisionNamespace, Value: evidence.RevisionValue},
		}
		if evidence.AnchorDigestHex != "" {
			reference.AnchorDigest = &contractsv1.Digest{Algorithm: evidence.AnchorAlgorithm, Hex: evidence.AnchorDigestHex}
		}
		finding.Evidence = append(finding.Evidence, reference)
	}
	if disposition == "" {
		finding.Disposition = contractsv1.FindingDisposition_FINDING_DISPOSITION_OPEN
		return finding, nil
	}
	dispositionEnum, err := dispositionFromText(disposition)
	if err != nil {
		return nil, err
	}
	finding.Disposition = dispositionEnum
	finding.DispositionReceipt = k.dispositionReceipt(runID, findingID, receiptID, receiptReason, dispositionedAtMs)
	return finding, nil
}

// planNodes reconstructs the served node set with current fenced leases and
// the orchestrator-originated edge set; every leaf grant stays pinned to the
// admitted intent base.
func (k *Kernel) planNodes(
	ctx context.Context, authenticated Identity, runID, repositoryGitOID string,
) ([]*contractsv1.PlanNode, []*contractsv1.PlanEdge, error) {
	rows, err := k.db.QueryContext(ctx, `SELECT node_id,kind,goal_digest,owned_paths,forbidden_paths,
		route_profile_digest,route_model_identity,route_rationale_code,grant_actions,grant_allowed_paths,
		grant_nonce,grant_expires_at_ms,grant_revocation_epoch,grant_command_fence,grant_policy_digest
		FROM factory_plan_nodes
		WHERE tenant_id=? AND principal_id=? AND run_id=? ORDER BY node_id`,
		authenticated.Tenant, authenticated.Principal, runID)
	if err != nil {
		return nil, nil, fmt.Errorf("factory: plan nodes query: %w", err)
	}
	defer rows.Close()
	type nodeRow struct {
		nodeID, kind, goalDigest, ownedJSON, forbiddenJSON              string
		routeProfile, routeModel, routeRationale                        *string
		grantActionsJSON, grantPathsJSON, grantNonce, grantPolicyDigest *string
		grantExpiresAtMs, grantEpoch, grantFence                        *int64
	}
	// Rows are buffered before any lease lookup so the single-writer connection
	// never nests a second query inside an open cursor.
	buffered := make([]nodeRow, 0, 5)
	for rows.Next() {
		var row nodeRow
		if err := rows.Scan(&row.nodeID, &row.kind, &row.goalDigest, &row.ownedJSON, &row.forbiddenJSON,
			&row.routeProfile, &row.routeModel, &row.routeRationale, &row.grantActionsJSON, &row.grantPathsJSON,
			&row.grantNonce, &row.grantExpiresAtMs, &row.grantEpoch, &row.grantFence, &row.grantPolicyDigest); err != nil {
			return nil, nil, fmt.Errorf("factory: plan node scan: %w", err)
		}
		buffered = append(buffered, row)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("factory: plan node rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, fmt.Errorf("factory: plan node close: %w", err)
	}
	nodes := make([]*contractsv1.PlanNode, 0, len(buffered))
	edges := make([]*contractsv1.PlanEdge, 0, len(buffered))
	for _, row := range buffered {
		nodeID, kind, goalDigest, ownedJSON, forbiddenJSON := row.nodeID, row.kind, row.goalDigest, row.ownedJSON, row.forbiddenJSON
		routeProfile, routeModel, routeRationale := row.routeProfile, row.routeModel, row.routeRationale
		grantActionsJSON, grantPathsJSON, grantNonce, grantPolicyDigest := row.grantActionsJSON, row.grantPathsJSON, row.grantNonce, row.grantPolicyDigest
		grantExpiresAtMs, grantEpoch, grantFence := row.grantExpiresAtMs, row.grantEpoch, row.grantFence
		node := &contractsv1.PlanNode{
			NodeId:     nodeID,
			GoalDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: goalDigest},
		}
		switch kind {
		case "orchestrator":
			node.Kind = contractsv1.PlanNodeKind_PLAN_NODE_KIND_ORCHESTRATOR
		case "review":
			node.Kind = contractsv1.PlanNodeKind_PLAN_NODE_KIND_REVIEW
		case "leaf":
			node.Kind = contractsv1.PlanNodeKind_PLAN_NODE_KIND_LEAF
			if routeProfile == nil || routeModel == nil || routeRationale == nil || grantNonce == nil ||
				grantExpiresAtMs == nil || grantEpoch == nil || grantFence == nil || grantPolicyDigest == nil {
				return nil, nil, ErrPayloadUnavailable
			}
			owned, err := decodePaths(ownedJSON)
			if err != nil {
				return nil, nil, err
			}
			forbidden, err := decodePaths(forbiddenJSON)
			if err != nil {
				return nil, nil, err
			}
			node.OwnedPaths = owned
			node.ForbiddenPaths = forbidden
			node.Route = &contractsv1.ModelRoute{
				ProfileDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: *routeProfile},
				ModelIdentity: *routeModel,
				RationaleCode: *routeRationale,
			}
			lease, leaseFound, err := k.roster.Current(ctx, k.db, authenticated.Tenant, authenticated.Principal, runID, nodeID)
			if err != nil {
				return nil, nil, err
			}
			if !leaseFound {
				return nil, nil, ErrPayloadUnavailable
			}
			grantActions, err := decodePaths(*grantActionsJSON)
			if err != nil {
				return nil, nil, err
			}
			grantPaths, err := decodePaths(*grantPathsJSON)
			if err != nil {
				return nil, nil, err
			}
			holderRef := &contractsv1.AuthenticatedPrincipalRef{
				PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: lease.HolderPrincipalID},
				TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: authenticated.Tenant},
			}
			leaseProto := &contractsv1.Lease{
				LeaseId:   &contractsv1.Identifier{Namespace: "lease", Value: lease.LeaseID},
				Holder:    holderRef,
				Fence:     lease.Fence,
				ExpiresAt: timestamppb.New(unixMillis(lease.ExpiresAtMs)),
			}
			node.Lease = leaseProto
			node.CapabilityGrant = &contractsv1.CapabilityGrant{
				GrantId: &contractsv1.Identifier{
					Namespace: "factory-grant",
					Value:     identity("ouroboros.stage05.grant.v1", runID, nodeID),
				},
				Initiator: &contractsv1.AuthenticatedPrincipalRef{
					PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: authenticated.Principal},
					TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: authenticated.Tenant},
					SessionId:   &contractsv1.Identifier{Namespace: "session", Value: authenticated.Session},
				},
				TaskId:           &contractsv1.Identifier{Namespace: "factory-task", Value: nodeID},
				WorkflowId:       &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
				Lease:            leaseProto,
				Actions:          grantActions,
				Resources:        []*contractsv1.Identifier{{Namespace: "tenant", Value: authenticated.Tenant}},
				RepositoryGitOid: repositoryGitOID,
				AllowedPaths:     grantPaths,
				Nonce:            *grantNonce,
				RevocationEpoch:  uint64(*grantEpoch),
				ExpiresAt:        timestamppb.New(unixMillis(*grantExpiresAtMs)),
				PolicyDigest:     &contractsv1.Digest{Algorithm: "sha256", Hex: *grantPolicyDigest},
				CommandFence:     uint64(*grantFence),
			}
		default:
			return nil, nil, ErrPayloadUnavailable
		}
		nodes = append(nodes, node)
		if node.Kind != contractsv1.PlanNodeKind_PLAN_NODE_KIND_ORCHESTRATOR {
			edges = append(edges, &contractsv1.PlanEdge{FromNodeId: orchestratorNodeID, ToNodeId: nodeID})
		}
	}
	return nodes, edges, nil
}

// planGates reconstructs the served gate roster with current statuses.
func (k *Kernel) planGates(ctx context.Context, authenticated Identity, runID string) ([]*contractsv1.GateSpec, error) {
	rows, err := k.db.QueryContext(ctx, `SELECT gate_id,kind,required FROM factory_gates
		WHERE tenant_id=? AND principal_id=? AND run_id=? ORDER BY gate_id`,
		authenticated.Tenant, authenticated.Principal, runID)
	if err != nil {
		return nil, fmt.Errorf("factory: gates query: %w", err)
	}
	defer rows.Close()
	type gateRow struct {
		gateID   string
		kindText string
		required int
	}
	// Rows are buffered before status lookups so the single-writer connection
	// never nests a second query inside an open cursor.
	buffered := make([]gateRow, 0, 4)
	for rows.Next() {
		var row gateRow
		if err := rows.Scan(&row.gateID, &row.kindText, &row.required); err != nil {
			return nil, fmt.Errorf("factory: gate scan: %w", err)
		}
		buffered = append(buffered, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("factory: gate rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("factory: gate close: %w", err)
	}
	gates := make([]*contractsv1.GateSpec, 0, len(buffered))
	for _, row := range buffered {
		kind, err := gateKindFromText(row.kindText)
		if err != nil {
			return nil, err
		}
		status, err := currentGateStatus(ctx, k.db, authenticated, runID, row.gateID)
		if err != nil {
			return nil, err
		}
		gates = append(gates, &contractsv1.GateSpec{
			GateId:   &contractsv1.Identifier{Namespace: "factory-gate", Value: row.gateID},
			Kind:     kind,
			Required: row.required == 1,
			Status:   status,
		})
	}
	return gates, nil
}

// rollbackReceipt rebuilds the served rollback receipt for a rejected
// candidate.
func (k *Kernel) rollbackReceipt(ctx context.Context, authenticated Identity, runID string) (*contractsv1.Receipt, error) {
	var receiptID, reasonCode, artifactDigest string
	var recordedAtMs int64
	err := k.db.QueryRowContext(ctx, `SELECT receipt_id,reason_code,rollback_artifact_digest,recorded_at_ms
		FROM factory_rollback_receipts
		WHERE tenant_id=? AND principal_id=? AND run_id=?`,
		authenticated.Tenant, authenticated.Principal, runID).
		Scan(&receiptID, &reasonCode, &artifactDigest, &recordedAtMs)
	if err != nil {
		return nil, errors.Join(ErrPayloadUnavailable, err)
	}
	return &contractsv1.Receipt{
		ReceiptId:   &contractsv1.Identifier{Namespace: "receipt", Value: receiptID},
		Status:      contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED,
		ReasonCode:  reasonCode,
		OperationId: &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
		Causal: &contractsv1.CausalContext{
			CorrelationId: &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
			CausationId:   &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
			TraceId:       &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
		},
		RecordedAt:          timestamppb.New(unixMillis(recordedAtMs)),
		ConfigurationDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: k.policyDigestHex},
		Evidence: []*contractsv1.EvidenceRef{{
			EvidenceId:       &contractsv1.Identifier{Namespace: "artifact", Value: artifactDigest},
			SourceRevisionId: &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
		}},
	}, nil
}
