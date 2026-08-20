package authorityprocess

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
	broker "github.com/sltbrta/sentra-code-memory-v2/services/broker/localauthority"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/factoryapi"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Stage 05 deterministic execution and review drives. Candidate execution
// drives lazily on the first candidate read; the fresh review drives lazily on
// the first findings read once the candidate is verified. Every drive step is
// either a kernel-replayed durable fact or a deterministic derivation from
// durable state, so a crashed or restarted drive replays exactly.

// factoryGateOrder is the canonical deterministic gate evaluation order; a
// failed gate stops evaluation and leaves later gates pending.
var factoryGateOrder = []contractsv1.FactoryGateKind{
	contractsv1.FactoryGateKind_FACTORY_GATE_KIND_BUILD,
	contractsv1.FactoryGateKind_FACTORY_GATE_KIND_TEST,
	contractsv1.FactoryGateKind_FACTORY_GATE_KIND_DOCS,
	contractsv1.FactoryGateKind_FACTORY_GATE_KIND_SECURITY,
}

// factoryLeafOutcome joins one plan leaf to its sealed-runner terminal trace.
type factoryLeafOutcome struct {
	node    *contractsv1.PlanNode
	outcome broker.FactoryLeafOutcome
}

// factoryLeafFailure classifies one failed leaf execution: a stale lease,
// fence, epoch, or base leaves the run alive for a later safe point; an escape
// attempt fails the run closed with the security gate failed; an atomic
// application failure rejects the candidate with its rollback receipt.
type factoryLeafFailure struct {
	kind           string
	rollbackDigest string
}

// driveExecution runs the bounded candidate pipeline for one admitted run:
// leaves execute through the sealed runner, their results commit under the
// current fences, the atomic candidate preview is proposed, the deterministic
// gates evaluate, and the candidate verifies or rejects with rollback.
func (adapter *factoryKernelAdapter) driveExecution(
	ctx context.Context, identity brain.FactoryIdentity, plan *contractsv1.ChangePlan,
) error {
	runID := plan.GetRunId().GetValue()
	// Advance durable run state before leaf work so a crash between
	// PLANNING→READY and READY→RUNNING still re-drives past READY on the
	// next candidate read. Descriptor resolution and leaf execution follow.
	runState := plan.GetState()
	if runState == contractsv1.ChangeRunState_CHANGE_RUN_STATE_PLANNING {
		if err := adapter.kernel.TransitionRun(ctx, identity, runID,
			contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY); err != nil {
			return mapFactoryKernelError(err)
		}
		runState = contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY
	}
	if runState == contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY {
		if err := adapter.kernel.TransitionRun(ctx, identity, runID,
			contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING); err != nil {
			return mapFactoryKernelError(err)
		}
	}
	descriptor, err := adapter.resolveDescriptor(ctx, plan.GetIntent())
	if err != nil {
		return factoryapi.ErrUnknownRun
	}
	adapter.fences.load(plan)
	scripts := make(map[string]factoryDescriptorLeaf, len(descriptor.Leaves))
	for _, leaf := range descriptor.Leaves {
		scripts[leaf.NodeID] = leaf
	}
	outcomes := make([]factoryLeafOutcome, 0, 3)
	var failure *factoryLeafFailure
	for _, node := range plan.GetNodes() {
		if node.GetKind() != contractsv1.PlanNodeKind_PLAN_NODE_KIND_LEAF {
			continue
		}
		script, found := scripts[node.GetNodeId()]
		if !found {
			return fmt.Errorf("factory pipeline: %w", errFactoryPortFailure)
		}
		outcome, err := adapter.runner.ExecuteLeaf(ctx, adapter.runnerSpec(runID, node), broker.FactoryLeafScript{
			Edits:      factoryEditDirectives(script.Edits),
			ProbePaths: append([]string(nil), script.ForbiddenPaths...),
		})
		if err != nil {
			// A sealed-runner denial (stale lease/fence/epoch, base mismatch,
			// effect denial) is a non-disclosing absence of candidate, never
			// a port failure that would surface as outer request-denied.
			if errors.Is(err, broker.ErrDenied) {
				return factoryapi.ErrUnknownRun
			}
			return fmt.Errorf("factory pipeline: %w", errFactoryPortFailure)
		}
		outcomes = append(outcomes, factoryLeafOutcome{node: node, outcome: outcome})
		if outcome.State == "COMPLETED" {
			resultBytes := canonicalFactoryLeafResult(runID, node.GetNodeId(), outcome)
			if _, err := adapter.kernel.CommitLeafResult(ctx, identity, runID,
				node.GetNodeId(), node.GetLease().GetFence(), resultBytes); err != nil {
				if errors.Is(err, brain.ErrFactoryStaleFence) {
					failure = &factoryLeafFailure{kind: "stale"}
					break
				}
				return mapFactoryKernelError(err)
			}
			continue
		}
		failure = classifyFactoryLeafFailure(outcome)
		break
	}
	if failure != nil {
		return adapter.concludeFailedExecution(ctx, identity, plan, descriptor, outcomes, failure)
	}
	preview := buildFactoryPreview(plan, outcomes, adapter.configHex)
	if err := adapter.kernel.ProposeCandidate(ctx, identity, runID, preview); err != nil {
		return mapFactoryKernelError(err)
	}
	gatesPassed, err := adapter.recordFactoryGates(ctx, identity, plan, outcomes)
	if err != nil {
		return err
	}
	candidateState, found := adapter.currentCandidateState(ctx, identity, runID)
	if !found {
		return fmt.Errorf("factory pipeline: %w", errFactoryPortFailure)
	}
	if candidateState == contractsv1.CandidateState_CANDIDATE_STATE_PROPOSED {
		if err := adapter.kernel.TransitionCandidate(ctx, identity, runID,
			contractsv1.CandidateState_CANDIDATE_STATE_APPLIED, nil); err != nil {
			return mapFactoryKernelError(err)
		}
		candidateState = contractsv1.CandidateState_CANDIDATE_STATE_APPLIED
	}
	if !gatesPassed {
		if candidateState != contractsv1.CandidateState_CANDIDATE_STATE_REJECTED {
			if err := adapter.kernel.TransitionCandidate(ctx, identity, runID,
				contractsv1.CandidateState_CANDIDATE_STATE_REJECTED, &brain.FactoryRollbackReceipt{
					ReasonCode:        "candidate_rejected",
					ArtifactDigestHex: preview.GetChangeSet().GetChangeSetDigest().GetHex(),
				}); err != nil {
				return mapFactoryKernelError(err)
			}
		}
		return adapter.failRun(ctx, identity, plan, runID)
	}
	if candidateState == contractsv1.CandidateState_CANDIDATE_STATE_APPLIED {
		if err := adapter.kernel.TransitionCandidate(ctx, identity, runID,
			contractsv1.CandidateState_CANDIDATE_STATE_VERIFIED, nil); err != nil {
			return mapFactoryKernelError(err)
		}
	}
	return nil
}

// concludeFailedExecution maps one failed leaf execution to its frozen run
// outcome: stale facts leave the run alive with no candidate, an escape fails
// the run closed with the security gate failed, and an atomic application
// failure rejects the proposed candidate with its rollback receipt.
func (adapter *factoryKernelAdapter) concludeFailedExecution(
	ctx context.Context,
	identity brain.FactoryIdentity,
	plan *contractsv1.ChangePlan,
	descriptor factoryDescriptor,
	outcomes []factoryLeafOutcome,
	failure *factoryLeafFailure,
) error {
	runID := plan.GetRunId().GetValue()
	switch failure.kind {
	case "stale":
		// The run stays alive at its safe point with no candidate: a stale
		// lease, fence, epoch, or base never becomes a canonical fact. The
		// candidate read surfaces the static non-disclosing denial so the
		// TUI cannot observe a port failure where no preview exists.
		return factoryapi.ErrUnknownRun
	case "escape":
		if err := adapter.recordFactoryGate(ctx, identity, plan,
			contractsv1.FactoryGateKind_FACTORY_GATE_KIND_SECURITY,
			contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_FAILED); err != nil {
			return err
		}
		return adapter.failRun(ctx, identity, plan, runID)
	default:
		preview := buildFactoryPreview(plan, outcomes, adapter.configHex)
		if len(preview.GetEdits()) > 0 {
			if err := adapter.kernel.ProposeCandidate(ctx, identity, runID, preview); err != nil {
				return mapFactoryKernelError(err)
			}
			if err := adapter.kernel.TransitionCandidate(ctx, identity, runID,
				contractsv1.CandidateState_CANDIDATE_STATE_REJECTED, &brain.FactoryRollbackReceipt{
					ReasonCode:        "candidate_rejected",
					ArtifactDigestHex: failure.rollbackDigest,
				}); err != nil {
				return mapFactoryKernelError(err)
			}
		}
		return adapter.failRun(ctx, identity, plan, runID)
	}
}

// failRun advances one run to its terminal failed state exactly once.
func (adapter *factoryKernelAdapter) failRun(
	ctx context.Context, identity brain.FactoryIdentity, plan *contractsv1.ChangePlan, runID string,
) error {
	if plan.GetState() == contractsv1.ChangeRunState_CHANGE_RUN_STATE_FAILED {
		return nil
	}
	if err := adapter.kernel.TransitionRun(ctx, identity, runID,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_FAILED); err != nil {
		return mapFactoryKernelError(err)
	}
	return nil
}

// classifyFactoryLeafFailure buckets one failed leaf trace by its denial
// vocabulary: stale facts, escape attempts, or atomic application failures.
func classifyFactoryLeafFailure(outcome broker.FactoryLeafOutcome) *factoryLeafFailure {
	for _, denial := range outcome.Denials {
		if strings.HasPrefix(denial.ReasonCode, "escape_") {
			return &factoryLeafFailure{kind: "escape"}
		}
	}
	for _, denial := range outcome.Denials {
		switch denial.ReasonCode {
		case "stale_lease", "stale_fence", "stale_epoch", "base_mismatch",
			"grant_expired", "grant_malformed", "identity_mismatch", "policy_denied":
			return &factoryLeafFailure{kind: "stale"}
		}
	}
	failure := &factoryLeafFailure{kind: "apply"}
	if outcome.Rollback != nil {
		failure.rollbackDigest = outcome.Rollback.ChangeSetDigestHex
	} else {
		failure.rollbackDigest = digestFactoryText("ouroboros.stage05.empty-rollback.v1")
	}
	return failure
}

// recordFactoryGates evaluates the deterministic gate roster in canonical
// order over the leaf traces and candidate facts. A failed gate stops
// evaluation and leaves the remaining roster pending, matching the frozen
// failure matrix.
func (adapter *factoryKernelAdapter) recordFactoryGates(
	ctx context.Context,
	identity brain.FactoryIdentity,
	plan *contractsv1.ChangePlan,
	outcomes []factoryLeafOutcome,
) (bool, error) {
	passed := true
	for _, kind := range factoryGateOrder {
		status := factoryPlanGateStatus(plan, kind)
		if status == contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PASSED {
			continue
		}
		if status == contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_FAILED {
			passed = false
			continue
		}
		verdict := evaluateFactoryGate(kind, outcomes)
		outcome := contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PASSED
		if !verdict {
			outcome = contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_FAILED
		}
		if err := adapter.recordFactoryGate(ctx, identity, plan, kind, outcome); err != nil {
			return false, err
		}
		if !verdict {
			passed = false
			break
		}
	}
	return passed, nil
}

// recordFactoryGate records one gate's RUNNING and terminal outcomes, skipping
// facts the durable roster already holds so a replayed drive never conflicts.
// Overlapping drives can both observe PENDING on a stale plan snapshot: the
// first advances the durable roster, and the second treats an already-advanced
// RUNNING transition as success so concurrent re-drive is not a permanent deny.
func (adapter *factoryKernelAdapter) recordFactoryGate(
	ctx context.Context,
	identity brain.FactoryIdentity,
	plan *contractsv1.ChangePlan,
	kind contractsv1.FactoryGateKind,
	outcome contractsv1.FactoryGateStatus,
) error {
	runID := plan.GetRunId().GetValue()
	gateID := factoryPlanGateID(plan, kind)
	if gateID == "" {
		return fmt.Errorf("factory pipeline: %w", errFactoryPortFailure)
	}
	if status := factoryPlanGateStatus(plan, kind); status == outcome ||
		status == contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PASSED ||
		status == contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_FAILED {
		return nil
	}
	if factoryPlanGateStatus(plan, kind) == contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PENDING {
		if err := adapter.kernel.RecordGateResult(ctx, identity, runID, gateID,
			contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_RUNNING); err != nil {
			// Peer drive already moved past PENDING (RUNNING or terminal).
			// Exact terminal re-record below is replay-safe; a conflicting
			// terminal still fails closed via ErrFactoryTransitionInvalid.
			if !errors.Is(err, brain.ErrFactoryTransitionInvalid) {
				return mapFactoryKernelError(err)
			}
		}
	}
	if err := adapter.kernel.RecordGateResult(ctx, identity, runID, gateID, outcome); err != nil {
		return mapFactoryKernelError(err)
	}
	return nil
}

// evaluateFactoryGate runs one deterministic gate over the sealed leaf traces.
//
// What each gate actually proves, stated plainly because the previous comment
// overstated it and callers read FACTORY_GATE_STATUS_PASSED as an assurance:
//
//   - BUILD: every leaf's candidate applied atomically. It does NOT compile
//     anything; see HARDENING.md for the overlay-based design that would.
//   - TEST: every touched Go file parses. It does NOT run tests; same entry.
//   - DOCS: every exported declaration in every touched Go file carries a doc
//     comment. This one now checks what its name says.
//   - SECURITY: zero brokered-effect denials and every edit inside its leaf
//     scope.
//
// Non-Go edits are skipped by BUILD, TEST and DOCS, so a TypeScript or Python
// change set passes those three having been checked by nothing.
func evaluateFactoryGate(kind contractsv1.FactoryGateKind, outcomes []factoryLeafOutcome) bool {
	switch kind {
	case contractsv1.FactoryGateKind_FACTORY_GATE_KIND_BUILD:
		for _, leaf := range outcomes {
			if leaf.outcome.State != "COMPLETED" {
				return false
			}
		}
		return true
	case contractsv1.FactoryGateKind_FACTORY_GATE_KIND_TEST:
		for _, leaf := range outcomes {
			for _, edit := range leaf.outcome.Edits {
				if edit.Language != "go" || len(edit.AfterBytes) == 0 {
					continue
				}
				if _, err := parser.ParseFile(token.NewFileSet(), edit.Path, edit.AfterBytes, 0); err != nil {
					return false
				}
			}
		}
		return true
	case contractsv1.FactoryGateKind_FACTORY_GATE_KIND_DOCS:
		for _, leaf := range outcomes {
			for _, edit := range leaf.outcome.Edits {
				if edit.Language != "go" || len(edit.AfterBytes) == 0 {
					continue
				}
				if !exportedDeclarationsAreDocumented(edit.Path, edit.AfterBytes) {
					return false
				}
			}
		}
		return true
	case contractsv1.FactoryGateKind_FACTORY_GATE_KIND_SECURITY:
		for _, leaf := range outcomes {
			if len(leaf.outcome.Denials) > 0 {
				return false
			}
			for _, edit := range leaf.outcome.Edits {
				if !factoryPathWithinScope(edit.Path, leaf.node.GetOwnedPaths()) {
					return false
				}
				if edit.OldPath != "" && !factoryPathWithinScope(edit.OldPath, leaf.node.GetOwnedPaths()) {
					return false
				}
			}
		}
		return true
	default:
		return false
	}
}

// factoryShouldDriveExecution reports whether a candidate read must re-drive
// the execution pipeline. READY is included so a crash between the durable
// PLANNING→READY and READY→RUNNING commits does not leave the run stuck.
func factoryShouldDriveExecution(state contractsv1.ChangeRunState) bool {
	return state == contractsv1.ChangeRunState_CHANGE_RUN_STATE_PLANNING ||
		state == contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY ||
		state == contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING
}

// factoryShouldDriveReview reports whether a findings read must re-drive the
// review pipeline. CANDIDATE_READY is included so a crash after RETAINED but
// before COMPLETED does not leave the run stuck mid-terminal transitions.
func factoryShouldDriveReview(state contractsv1.ChangeRunState) bool {
	return state == contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING ||
		state == contractsv1.ChangeRunState_CHANGE_RUN_STATE_REVIEW ||
		state == contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANDIDATE_READY
}

// driveReview runs the bounded fresh-review pipeline for one verified
// candidate: the deterministic reviewer records and dispositions the approved
// descriptor's typed findings under a principal disjoint from every leaf grant
// initiator, retains a clean candidate, or rejects a candidate carrying an
// undisposed blocker with its rollback receipt.
func (adapter *factoryKernelAdapter) driveReview(
	ctx context.Context, identity brain.FactoryIdentity, plan *contractsv1.ChangePlan,
) error {
	runID := plan.GetRunId().GetValue()
	candidateState, found := adapter.currentCandidateState(ctx, identity, runID)
	if !found {
		// No candidate yet: a planning or running run has nothing to review.
		return nil
	}
	// Crash recovery: the candidate was already retained but the run may still
	// sit in REVIEW or CANDIDATE_READY. Finish the terminal transitions only —
	// findings and candidate transitions must not re-fire on RETAINED.
	if candidateState == contractsv1.CandidateState_CANDIDATE_STATE_RETAINED {
		return adapter.completeRetainedRun(ctx, identity, runID)
	}
	if candidateState != contractsv1.CandidateState_CANDIDATE_STATE_VERIFIED &&
		candidateState != contractsv1.CandidateState_CANDIDATE_STATE_REVIEWED {
		// Terminal rejected (or other non-reviewable) candidates stay put.
		return nil
	}
	descriptor, err := adapter.resolveDescriptor(ctx, plan.GetIntent())
	if err != nil {
		return factoryapi.ErrUnknownRun
	}
	if plan.GetState() == contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING {
		if err := adapter.kernel.TransitionRun(ctx, identity, runID,
			contractsv1.ChangeRunState_CHANGE_RUN_STATE_REVIEW); err != nil {
			return mapFactoryKernelError(err)
		}
	}
	var blocker *brain.FactoryFindingResult
	evidence := plan.GetIntent().GetSupportingEvidence()
	for _, findingSpec := range descriptor.Findings {
		draft := brain.FactoryFindingDraft{
			Severity:          factoryFindingSeverity(findingSpec.Severity),
			Category:          factoryFindingCategory(findingSpec.Category),
			Summary:           findingSpec.Summary,
			Evidence:          factoryFindingEvidence(evidence),
			ReviewerPrincipal: identity.Principal + "-factory-review",
			ReviewerSession:   identity.Session + "-factory-review",
			ReviewerFamily:    "deterministic-v1",
		}
		recorded, err := adapter.kernel.RecordFinding(ctx, identity, runID, draft)
		if err != nil {
			return mapFactoryKernelError(err)
		}
		if findingSpec.Disposition == "OPEN" {
			if findingSpec.Severity == "BLOCKER" && blocker == nil {
				blocker = &recorded
			}
			continue
		}
		if err := adapter.kernel.DisposeFinding(ctx, identity, runID, recorded.FindingID,
			factoryFindingDisposition(findingSpec.Disposition)); err != nil {
			return mapFactoryKernelError(err)
		}
	}
	if blocker != nil {
		if candidateState == contractsv1.CandidateState_CANDIDATE_STATE_VERIFIED {
			if err := adapter.kernel.TransitionCandidate(ctx, identity, runID,
				contractsv1.CandidateState_CANDIDATE_STATE_REJECTED, &brain.FactoryRollbackReceipt{
					ReasonCode:        "candidate_rejected",
					ArtifactDigestHex: digestFactoryText("ouroboros.stage05.review-rollback.v1\x00" + runID + "\x00" + blocker.FindingID),
				}); err != nil {
				return mapFactoryKernelError(err)
			}
		}
		return adapter.failRun(ctx, identity, plan, runID)
	}
	if candidateState == contractsv1.CandidateState_CANDIDATE_STATE_VERIFIED {
		if err := adapter.kernel.TransitionCandidate(ctx, identity, runID,
			contractsv1.CandidateState_CANDIDATE_STATE_REVIEWED, nil); err != nil {
			return mapFactoryKernelError(err)
		}
	}
	if err := adapter.kernel.TransitionCandidate(ctx, identity, runID,
		contractsv1.CandidateState_CANDIDATE_STATE_RETAINED, nil); err != nil {
		return mapFactoryKernelError(err)
	}
	return adapter.completeRetainedRun(ctx, identity, runID)
}

// completeRetainedRun finishes the terminal run transitions after a candidate
// is RETAINED: REVIEW→CANDIDATE_READY→COMPLETED. Idempotent for COMPLETED and
// for re-entry after a crash between those steps.
func (adapter *factoryKernelAdapter) completeRetainedRun(
	ctx context.Context, identity brain.FactoryIdentity, runID string,
) error {
	current, err := adapter.kernel.GetChangePlan(ctx, identity, runID)
	if err != nil {
		return mapFactoryKernelError(err)
	}
	if current.GetState() == contractsv1.ChangeRunState_CHANGE_RUN_STATE_REVIEW {
		if err := adapter.kernel.TransitionRun(ctx, identity, runID,
			contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANDIDATE_READY); err != nil {
			return mapFactoryKernelError(err)
		}
		current.State = contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANDIDATE_READY
	}
	if current.GetState() == contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANDIDATE_READY {
		if err := adapter.kernel.TransitionRun(ctx, identity, runID,
			contractsv1.ChangeRunState_CHANGE_RUN_STATE_COMPLETED); err != nil {
			return mapFactoryKernelError(err)
		}
	}
	return nil
}

// currentCandidateState resolves the durable candidate state, or false when
// the run has no candidate.
func (adapter *factoryKernelAdapter) currentCandidateState(
	ctx context.Context, identity brain.FactoryIdentity, runID string,
) (contractsv1.CandidateState, bool) {
	preview, err := adapter.kernel.PreviewChangeSet(ctx, identity, runID)
	if err != nil {
		return contractsv1.CandidateState_CANDIDATE_STATE_UNSPECIFIED, false
	}
	return preview.GetCandidateState(), true
}

// runnerSpec attenuates one served plan leaf into the sealed-runner spec: the
// same lease, fence, scope, base, expiry, and epoch the kernel issued.
func (adapter *factoryKernelAdapter) runnerSpec(runID string, node *contractsv1.PlanNode) broker.FactoryLeafSpec {
	grant := node.GetCapabilityGrant()
	lease := grant.GetLease()
	return broker.FactoryLeafSpec{
		RunID:          runID,
		NodeID:         node.GetNodeId(),
		OwnedPaths:     append([]string(nil), node.GetOwnedPaths()...),
		ForbiddenPaths: append([]string(nil), node.GetForbiddenPaths()...),
		BaseGitOID:     grant.GetRepositoryGitOid(),
		Identity:       adapter.identity,
		Grant: broker.FactoryLeafGrant{
			GrantID:          grant.GetGrantId().GetValue(),
			Initiator:        grant.GetInitiator().GetPrincipalId().GetValue(),
			Tenant:           grant.GetInitiator().GetTenantId().GetValue(),
			TaskID:           grant.GetTaskId().GetValue(),
			RunID:            grant.GetWorkflowId().GetValue(),
			LeaseID:          lease.GetLeaseId().GetValue(),
			LeaseHolder:      lease.GetHolder().GetPrincipalId().GetValue(),
			LeaseFence:       lease.GetFence(),
			LeaseExpiresAt:   lease.GetExpiresAt().AsTime(),
			AllowedPaths:     append([]string(nil), grant.GetAllowedPaths()...),
			RepositoryGitOID: grant.GetRepositoryGitOid(),
			Nonce:            grant.GetNonce(),
			RevocationEpoch:  grant.GetRevocationEpoch(),
			ExpiresAt:        grant.GetExpiresAt().AsTime(),
			PolicyDigestHex:  grant.GetPolicyDigest().GetHex(),
			CommandFence:     grant.GetCommandFence(),
		},
		ChangeSetID:    "changeset-" + runID + "-" + node.GetNodeId(),
		IdempotencyKey: "leaf-" + runID + "-" + node.GetNodeId(),
		Now:            adapter.now(),
	}
}

// buildFactoryPreview authors the deterministic atomic candidate preview from
// the applied leaf traces: normalized per-file edits, per-language
// obligations, the plan's gate roster pending evaluation, and digest-pinned
// patch and rollback artifacts. Every fact derives from durable state and the
// run identity, so a replayed proposal binds the identical digest.
func buildFactoryPreview(
	plan *contractsv1.ChangePlan,
	outcomes []factoryLeafOutcome,
	configurationDigest string,
) *contractsv1.ChangeSetPreview {
	runID := plan.GetRunId().GetValue()
	base := plan.GetIntent().GetRepositoryGitOid()
	edits := make([]*contractsv1.PreviewEdit, 0, 8)
	touched := make(map[contractsv1.CodeLanguage]struct{})
	for _, leaf := range outcomes {
		for _, applied := range leaf.outcome.Edits {
			language := factoryPreviewLanguage(applied.Language)
			edit := &contractsv1.PreviewEdit{
				Path:      applied.Path,
				Operation: factoryPreviewOperation(applied.Op),
				Language:  language,
			}
			switch applied.Op {
			case "add":
				edit.AfterDigest = &contractsv1.Digest{Algorithm: "sha256", Hex: applied.AfterDigestHex}
			case "delete":
				edit.BeforeDigest = &contractsv1.Digest{Algorithm: "sha256", Hex: applied.BeforeDigestHex}
			case "modify":
				edit.BeforeDigest = &contractsv1.Digest{Algorithm: "sha256", Hex: applied.BeforeDigestHex}
				edit.AfterDigest = &contractsv1.Digest{Algorithm: "sha256", Hex: applied.AfterDigestHex}
			case "rename":
				oldPath := applied.OldPath
				edit.OldPath = &oldPath
				edit.BeforeDigest = &contractsv1.Digest{Algorithm: "sha256", Hex: applied.BeforeDigestHex}
				edit.AfterDigest = &contractsv1.Digest{Algorithm: "sha256", Hex: applied.AfterDigestHex}
			}
			touched[language] = struct{}{}
			edits = append(edits, edit)
		}
	}
	languages := make([]contractsv1.CodeLanguage, 0, len(touched))
	for language := range touched {
		languages = append(languages, language)
	}
	sort.Slice(languages, func(left, right int) bool { return languages[left] < languages[right] })
	obligations := make([]*contractsv1.LanguageObligation, 0, len(languages))
	for _, language := range languages {
		obligations = append(obligations, &contractsv1.LanguageObligation{
			Language:      language,
			Impact:        &contractsv1.ImpactReceipt{BaseGitOid: base},
			DocsRequired:  true,
			TestsRequired: true,
		})
	}
	setDigest := factoryChangeSetDigest(base, edits)
	rollbackDigest := digestFactoryText("ouroboros.stage05.rollback-artifact.v1\x00" + base + "\x00" + setDigest)
	recordedAt := plan.GetIntent().GetApproval().GetReceipt().GetRecordedAt()
	if recordedAt == nil {
		recordedAt = timestamppb.New(time.UnixMilli(1_000_000).UTC())
	}
	return &contractsv1.ChangeSetPreview{
		ChangeSet: &contractsv1.ChangeSet{
			ChangeSetId: &contractsv1.Identifier{Namespace: "factory-candidate", Value: "candidate-" + runID},
			BaseGitOid:  base,
			PatchArtifact: &contractsv1.ArtifactRef{
				ArtifactId:    &contractsv1.Identifier{Namespace: "artifact", Value: "patch-" + runID},
				ContentDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: setDigest},
				TenantId: &contractsv1.Identifier{
					Namespace: "tenant", Value: plan.GetIntent().GetRequestedBy().GetTenantId().GetValue(),
				},
			},
			ChangeSetDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: setDigest},
			ValidationReceipts: []*contractsv1.Receipt{{
				ReceiptId:   &contractsv1.Identifier{Namespace: "receipt", Value: "validation-" + runID},
				Status:      contractsv1.ReceiptStatus_RECEIPT_STATUS_ACCEPTED,
				ReasonCode:  "candidate_proposed",
				OperationId: &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
				Causal: &contractsv1.CausalContext{
					CorrelationId: &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
					CausationId:   &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
					TraceId:       &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
				},
				RecordedAt:          recordedAt,
				ConfigurationDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: configurationDigest},
			}},
			RollbackArtifact: &contractsv1.ArtifactRef{
				ArtifactId:    &contractsv1.Identifier{Namespace: "artifact", Value: "rollback-" + runID},
				ContentDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: rollbackDigest},
				TenantId: &contractsv1.Identifier{
					Namespace: "tenant", Value: plan.GetIntent().GetRequestedBy().GetTenantId().GetValue(),
				},
			},
		},
		CandidateState:     contractsv1.CandidateState_CANDIDATE_STATE_PROPOSED,
		Edits:              edits,
		Obligations:        obligations,
		Gates:              factoryPreviewGates(plan),
		ExpectedBaseGitOid: base,
	}
}

// factoryPreviewGates rebuilds the plan's gate roster with pending statuses
// for the candidate proposal; identities and kinds match the roster exactly.
func factoryPreviewGates(plan *contractsv1.ChangePlan) []*contractsv1.GateSpec {
	gates := make([]*contractsv1.GateSpec, 0, len(plan.GetGates()))
	for _, gate := range plan.GetGates() {
		gates = append(gates, &contractsv1.GateSpec{
			GateId:   gate.GetGateId(),
			Kind:     gate.GetKind(),
			Required: gate.GetRequired(),
			Status:   contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PENDING,
		})
	}
	return gates
}

// factoryChangeSetDigest binds the exact edit set to its base.
func factoryChangeSetDigest(base string, edits []*contractsv1.PreviewEdit) string {
	fields := []string{"ouroboros.stage05.changeset.v1", base}
	for _, edit := range edits {
		fields = append(fields, edit.GetPath(), edit.GetOldPath(),
			strconv.Itoa(int(edit.GetOperation())),
			edit.GetBeforeDigest().GetHex(), edit.GetAfterDigest().GetHex())
	}
	return digestFactoryText(strings.Join(fields, "\x1f"))
}

func digestFactoryText(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// canonicalFactoryLeafResult serializes one completed leaf trace for the
// durable roster commit; the exact bytes replay on a re-driven leaf.
func canonicalFactoryLeafResult(runID, nodeID string, outcome broker.FactoryLeafOutcome) []byte {
	type editFact struct {
		Op          string `json:"op"`
		Path        string `json:"path"`
		OldPath     string `json:"oldPath,omitempty"`
		AfterDigest string `json:"afterDigest"`
	}
	facts := struct {
		Version string     `json:"version"`
		RunID   string     `json:"runId"`
		NodeID  string     `json:"nodeId"`
		State   string     `json:"state"`
		Edits   []editFact `json:"edits"`
	}{
		Version: "ouroboros.stage05.leaf-result.v1",
		RunID:   runID, NodeID: nodeID, State: outcome.State,
		Edits: make([]editFact, 0, len(outcome.Edits)),
	}
	for _, applied := range outcome.Edits {
		facts.Edits = append(facts.Edits, editFact{
			Op: applied.Op, Path: applied.Path, OldPath: applied.OldPath, AfterDigest: applied.AfterDigestHex,
		})
	}
	encoded, err := json.Marshal(facts)
	if err != nil {
		return []byte("ouroboros.stage05.leaf-result.v1\x00" + runID + "\x00" + nodeID)
	}
	return encoded
}

func factoryEditDirectives(edits []factoryDescriptorLeafEdit) []broker.FactoryEditDirective {
	directives := make([]broker.FactoryEditDirective, 0, len(edits))
	for _, edit := range edits {
		directives = append(directives, broker.FactoryEditDirective{
			Op: edit.Op, Path: edit.Path, OldPath: edit.OldPath,
		})
	}
	return directives
}

func factoryPlanGateID(plan *contractsv1.ChangePlan, kind contractsv1.FactoryGateKind) string {
	for _, gate := range plan.GetGates() {
		if gate.GetKind() == kind {
			return gate.GetGateId().GetValue()
		}
	}
	return ""
}

func factoryPlanGateStatus(
	plan *contractsv1.ChangePlan, kind contractsv1.FactoryGateKind,
) contractsv1.FactoryGateStatus {
	for _, gate := range plan.GetGates() {
		if gate.GetKind() == kind {
			return gate.GetStatus()
		}
	}
	return contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_UNSPECIFIED
}

func factoryPathWithinScope(path string, scopes []string) bool {
	for _, scope := range scopes {
		if path == scope || strings.HasPrefix(path, scope+"/") {
			return true
		}
	}
	return false
}

func factoryPreviewOperation(op string) contractsv1.PreviewEditOperation {
	switch op {
	case "add":
		return contractsv1.PreviewEditOperation_PREVIEW_EDIT_OPERATION_ADD
	case "delete":
		return contractsv1.PreviewEditOperation_PREVIEW_EDIT_OPERATION_DELETE
	case "rename":
		return contractsv1.PreviewEditOperation_PREVIEW_EDIT_OPERATION_RENAME
	default:
		return contractsv1.PreviewEditOperation_PREVIEW_EDIT_OPERATION_MODIFY
	}
}

func factoryPreviewLanguage(language string) contractsv1.CodeLanguage {
	switch language {
	case "go":
		return contractsv1.CodeLanguage_CODE_LANGUAGE_GO
	case "typescript":
		return contractsv1.CodeLanguage_CODE_LANGUAGE_TYPESCRIPT
	case "python":
		return contractsv1.CodeLanguage_CODE_LANGUAGE_PYTHON
	case "rust":
		return contractsv1.CodeLanguage_CODE_LANGUAGE_RUST
	case "java":
		return contractsv1.CodeLanguage_CODE_LANGUAGE_JAVA
	default:
		return contractsv1.CodeLanguage_CODE_LANGUAGE_UNSPECIFIED
	}
}

func factoryFindingSeverity(severity string) contractsv1.ReviewSeverity {
	switch severity {
	case "MINOR":
		return contractsv1.ReviewSeverity_REVIEW_SEVERITY_MINOR
	case "MAJOR":
		return contractsv1.ReviewSeverity_REVIEW_SEVERITY_MAJOR
	case "BLOCKER":
		return contractsv1.ReviewSeverity_REVIEW_SEVERITY_BLOCKER
	default:
		return contractsv1.ReviewSeverity_REVIEW_SEVERITY_INFO
	}
}

func factoryFindingCategory(category string) contractsv1.ReviewCategory {
	switch category {
	case "SECURITY":
		return contractsv1.ReviewCategory_REVIEW_CATEGORY_SECURITY
	case "DATA_INTEGRITY":
		return contractsv1.ReviewCategory_REVIEW_CATEGORY_DATA_INTEGRITY
	case "DOCS":
		return contractsv1.ReviewCategory_REVIEW_CATEGORY_DOCS
	case "TESTS":
		return contractsv1.ReviewCategory_REVIEW_CATEGORY_TESTS
	default:
		return contractsv1.ReviewCategory_REVIEW_CATEGORY_CORRECTNESS
	}
}

func factoryFindingDisposition(disposition string) contractsv1.FindingDisposition {
	if disposition == "FIXED" {
		return contractsv1.FindingDisposition_FINDING_DISPOSITION_FIXED
	}
	return contractsv1.FindingDisposition_FINDING_DISPOSITION_DISMISSED_WITH_EVIDENCE
}

func factoryFindingEvidence(evidence []*contractsv1.EvidenceRef) []*contractsv1.EvidenceRef {
	if len(evidence) == 0 {
		return nil
	}
	return []*contractsv1.EvidenceRef{{
		EvidenceId:       evidence[0].GetEvidenceId(),
		SourceRevisionId: evidence[0].GetSourceRevisionId(),
	}}
}

// exportedDeclarationsAreDocumented reports whether every exported declaration
// in a Go source file carries a doc comment.
//
// The DOCS gate previously asked whether the file contained the two characters
// "//" anywhere, which any file with a single inline comment satisfies -- so a
// change set could be promoted to VERIFIED with "documentation" proven by a
// `// TODO` on line 40. This checks what the gate's name claims.
//
// Files that do not parse are left to the TEST gate rather than double-reported
// here, and a file with no exported declarations passes vacuously, which is
// correct: there is nothing it was required to document.
func exportedDeclarationsAreDocumented(path string, source []byte) bool {
	file, err := parser.ParseFile(token.NewFileSet(), path, source, parser.ParseComments)
	if err != nil {
		return true
	}
	for _, decl := range file.Decls {
		switch node := decl.(type) {
		case *ast.FuncDecl:
			if node.Name.IsExported() && node.Doc == nil {
				return false
			}
		case *ast.GenDecl:
			if node.Tok != token.TYPE && node.Tok != token.CONST && node.Tok != token.VAR {
				continue
			}
			// A grouped declaration may carry one doc comment for the group.
			if node.Doc != nil {
				continue
			}
			for _, spec := range node.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					if spec.Name.IsExported() && spec.Doc == nil {
						return false
					}
				case *ast.ValueSpec:
					if spec.Doc != nil {
						continue
					}
					for _, name := range spec.Names {
						if name.IsExported() {
							return false
						}
					}
				}
			}
		}
	}
	return true
}
