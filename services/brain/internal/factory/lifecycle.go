package factory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factory/roster"
)

// TransitionRun advances one admitted run along the bounded lifecycle:
// planning → ready → running → review → candidate-ready → completed, with
// failure reachable from every non-terminal state. Cancellation records only
// through CancelChangeRun. A completion requires a retained candidate. The
// transition is validated and appended atomically; the schema independently
// enforces dense sequences and terminal finality.
func (k *Kernel) TransitionRun(ctx context.Context, authenticated Identity, runID string, next contractsv1.ChangeRunState) error {
	if k == nil || ctx == nil || !validIdentity(authenticated) || runID == "" {
		return ErrInvalidInput
	}
	if _, err := runStateText(next); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return ErrInvalidInput
	}
	tx, err := k.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("factory: begin run transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, found, err := lookupRun(ctx, tx, authenticated, runID); err != nil {
		return err
	} else if !found {
		return ErrNotFoundOrDenied
	}
	current, err := currentRunState(ctx, tx, authenticated, runID)
	if err != nil {
		return err
	}
	if next == contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANCELLED || !validRunTransition(current, next) {
		return ErrTransitionInvalid
	}
	if next == contractsv1.ChangeRunState_CHANGE_RUN_STATE_COMPLETED {
		candidate, found, err := currentCandidateState(ctx, tx, authenticated, runID)
		if err != nil {
			return err
		}
		if !found || candidate != contractsv1.CandidateState_CANDIDATE_STATE_RETAINED {
			return ErrTransitionInvalid
		}
	}
	if err := appendRunState(ctx, tx, authenticated, runID, next, k.clock.NowUnixMilli()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("factory: commit run transition: %w", err)
	}
	return nil
}

// CommitLeafResult records the canonical result of one leased leaf under its
// current unexpired fence. The run must be running: a revoked or otherwise
// terminal run denies statically, an expired or regressed fence is
// roster.ErrStaleFence and never becomes canonical, an exact replay returns
// the original commit, and a differing second commit conflicts. Static
// denials and exact replays resolve before any payload is staged, so they
// never leave unreferenced vault objects.
func (k *Kernel) CommitLeafResult(
	ctx context.Context, authenticated Identity, runID, nodeID string, fence uint64, result []byte,
) (roster.Result, error) {
	if k == nil || ctx == nil || !validIdentity(authenticated) || runID == "" || nodeID == "" ||
		fence == 0 || len(result) == 0 {
		return roster.Result{}, ErrInvalidInput
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return roster.Result{}, ErrInvalidInput
	}
	if _, found, err := lookupRun(ctx, k.db, authenticated, runID); err != nil {
		return roster.Result{}, err
	} else if !found {
		return roster.Result{}, ErrNotFoundOrDenied
	}
	state, err := currentRunState(ctx, k.db, authenticated, runID)
	if err != nil {
		return roster.Result{}, err
	}
	if state != contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING {
		return roster.Result{}, ErrNotFoundOrDenied
	}
	if _, err := k.roster.Authorize(ctx, k.db, authenticated.Tenant, authenticated.Principal, runID, nodeID, fence); err != nil {
		return roster.Result{}, err
	}
	resultDigest := digestBytes(result)
	existing, found, err := k.roster.Result(ctx, k.db, authenticated.Tenant, authenticated.Principal, runID, nodeID)
	if err != nil {
		return roster.Result{}, err
	}
	if found {
		if existing.Lease.Fence == fence && existing.Digest == resultDigest {
			existing.Replayed = true
			return existing, nil
		}
		return roster.Result{}, roster.ErrResultConflict
	}
	staged, err := k.stagePayload(ctx, authenticated.Tenant, result)
	if err != nil {
		return roster.Result{}, err
	}
	tx, err := k.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return roster.Result{}, fmt.Errorf("factory: begin leaf commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	committed, err := k.roster.CommitResult(ctx, tx, roster.Result{
		Lease:      roster.Lease{Tenant: authenticated.Tenant, Principal: authenticated.Principal, RunID: runID, NodeID: nodeID, Fence: fence},
		ArtifactID: staged.artifactID,
		Digest:     staged.digestHex,
	})
	if err != nil {
		return roster.Result{}, err
	}
	if err := tx.Commit(); err != nil {
		return roster.Result{}, fmt.Errorf("factory: commit leaf result: %w", err)
	}
	return committed, nil
}

// RecordGateResult appends one evaluation outcome to the run's gate roster.
// Gates progress PENDING → RUNNING → PASSED/FAILED and terminal outcomes are
// final; the schema enforces both. The run must be admitted and non-terminal.
func (k *Kernel) RecordGateResult(
	ctx context.Context, authenticated Identity, runID, gateID string, status contractsv1.FactoryGateStatus,
) error {
	if k == nil || ctx == nil || !validIdentity(authenticated) || runID == "" || gateID == "" {
		return ErrInvalidInput
	}
	if status != contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_RUNNING &&
		status != contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PASSED &&
		status != contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_FAILED {
		return ErrInvalidInput
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return ErrInvalidInput
	}
	tx, err := k.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("factory: begin gate result: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, found, err := lookupRun(ctx, tx, authenticated, runID); err != nil {
		return err
	} else if !found {
		return ErrNotFoundOrDenied
	}
	state, err := currentRunState(ctx, tx, authenticated, runID)
	if err != nil {
		return err
	}
	if terminalRunState(state) {
		return ErrNotFoundOrDenied
	}
	var gateCount int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM factory_gates
		WHERE tenant_id=? AND principal_id=? AND run_id=? AND gate_id=?`,
		authenticated.Tenant, authenticated.Principal, runID, gateID).Scan(&gateCount); err != nil {
		return fmt.Errorf("factory: read gate: %w", err)
	}
	if gateCount != 1 {
		return ErrNotFoundOrDenied
	}
	current, err := currentGateStatus(ctx, tx, authenticated, runID, gateID)
	if err != nil {
		return err
	}
	if current == contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PASSED ||
		current == contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_FAILED {
		if current == status {
			// An exact re-record of the terminal outcome is a replay.
			return nil
		}
		return ErrTransitionInvalid
	}
	if status == contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_RUNNING &&
		current != contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PENDING {
		return ErrTransitionInvalid
	}
	if err := appendGateState(ctx, tx, authenticated, runID, gateID, status, k.clock.NowUnixMilli()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("factory: commit gate result: %w", err)
	}
	return nil
}

// ProposeCandidate records the run's atomic candidate preview. The preview is
// validated against the frozen ChangeSetPreview shape: normalized unique
// post-image and pre-image paths, per-language obligations covering exactly
// the touched lanes, gate identifiers unique within the roster, and the exact
// base binding — the candidate base must equal the admitted intent's approved
// Git base. Every edit path must attenuate one leaf's owned scope; anything
// else is an escape and denies before any canonical fact commits. The full
// preview bytes persist in the encrypted vault; SQLite holds only the
// candidate identity and digest. Re-proposing an identical candidate replays.
func (k *Kernel) ProposeCandidate(ctx context.Context, authenticated Identity, runID string, preview *contractsv1.ChangeSetPreview) error {
	if k == nil || ctx == nil || !validIdentity(authenticated) || runID == "" || preview == nil {
		return ErrInvalidInput
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return ErrInvalidInput
	}
	run, found, err := lookupRun(ctx, k.db, authenticated, runID)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFoundOrDenied
	}
	state, err := currentRunState(ctx, k.db, authenticated, runID)
	if err != nil {
		return err
	}
	if state != contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING &&
		state != contractsv1.ChangeRunState_CHANGE_RUN_STATE_REVIEW {
		return ErrNotFoundOrDenied
	}
	scopes, err := k.leafScopes(ctx, k.db, authenticated, runID)
	if err != nil {
		return err
	}
	if err := k.validatePreview(preview, run, scopes); err != nil {
		return err
	}
	if err := k.requirePreviewGatesMatchRoster(ctx, k.db, authenticated, runID, preview); err != nil {
		return err
	}
	encoded, err := marshalDeterministic(preview)
	if err != nil {
		return err
	}
	staged, err := k.stagePayload(ctx, authenticated.Tenant, encoded)
	if err != nil {
		return err
	}
	tx, err := k.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("factory: begin candidate proposal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	existing, err := lookupCandidate(ctx, tx, authenticated, runID)
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.digestHex == staged.digestHex {
			return nil
		}
		return ErrNotFoundOrDenied
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO factory_candidates
		(tenant_id,principal_id,run_id,change_set_id,candidate_artifact_id,candidate_digest,proposed_at_ms)
		VALUES (?,?,?,?,?,?,?)`,
		authenticated.Tenant, authenticated.Principal, runID, preview.GetChangeSet().GetChangeSetId().GetValue(),
		staged.artifactID, staged.digestHex, k.clock.NowUnixMilli()); err != nil {
		return fmt.Errorf("factory: commit candidate: %w", err)
	}
	if err := appendCandidateState(ctx, tx, authenticated, runID,
		contractsv1.CandidateState_CANDIDATE_STATE_PROPOSED, k.clock.NowUnixMilli()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("factory: commit candidate proposal: %w", err)
	}
	return nil
}

// TransitionCandidate advances the atomic candidate along its bounded
// all-or-nothing lifecycle. Verified, reviewed, and retained states require
// every required gate passed on the isolated candidate; rejection is reachable
// from every non-terminal state and must carry its rollback receipt in the
// same atomic commit, so a rejected candidate can never exist without
// deterministic rollback facts.
func (k *Kernel) TransitionCandidate(
	ctx context.Context, authenticated Identity, runID string, next contractsv1.CandidateState, rollback *RollbackReceipt,
) error {
	if k == nil || ctx == nil || !validIdentity(authenticated) || runID == "" {
		return ErrInvalidInput
	}
	if _, err := candidateStateText(next); err != nil {
		return err
	}
	if next == contractsv1.CandidateState_CANDIDATE_STATE_PROPOSED {
		return ErrInvalidInput
	}
	isRejection := next == contractsv1.CandidateState_CANDIDATE_STATE_REJECTED
	if isRejection != (rollback != nil) {
		return ErrInvalidInput
	}
	if rollback != nil && (!isHexDigest(rollback.ArtifactDigestHex) || rollback.ReasonCode == "") {
		return ErrInvalidInput
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return ErrInvalidInput
	}
	tx, err := k.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("factory: begin candidate transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, found, err := lookupRun(ctx, tx, authenticated, runID); err != nil {
		return err
	} else if !found {
		return ErrNotFoundOrDenied
	}
	runState, err := currentRunState(ctx, tx, authenticated, runID)
	if err != nil {
		return err
	}
	if terminalRunState(runState) {
		return ErrNotFoundOrDenied
	}
	current, found, err := currentCandidateState(ctx, tx, authenticated, runID)
	if err != nil {
		return err
	}
	if !found || !validCandidateTransition(current, next) {
		return ErrTransitionInvalid
	}
	if next == contractsv1.CandidateState_CANDIDATE_STATE_VERIFIED ||
		next == contractsv1.CandidateState_CANDIDATE_STATE_REVIEWED ||
		next == contractsv1.CandidateState_CANDIDATE_STATE_RETAINED {
		if err := k.requireRequiredGatesPassed(ctx, tx, authenticated, runID); err != nil {
			return err
		}
	}
	if isRejection {
		if _, err := tx.ExecContext(ctx, `INSERT INTO factory_rollback_receipts
			(tenant_id,principal_id,run_id,receipt_id,reason_code,rollback_artifact_digest,recorded_at_ms)
			VALUES (?,?,?,?,?,?,?)`,
			authenticated.Tenant, authenticated.Principal, runID,
			identity("ouroboros.stage05.rollback-receipt.v1", authenticated.Tenant, authenticated.Principal, runID),
			rollback.ReasonCode, rollback.ArtifactDigestHex, k.clock.NowUnixMilli()); err != nil {
			return fmt.Errorf("factory: commit rollback receipt: %w", err)
		}
	}
	if err := appendCandidateState(ctx, tx, authenticated, runID, next, k.clock.NowUnixMilli()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("factory: commit candidate transition: %w", err)
	}
	return nil
}

// RollbackReceipt carries the deterministic discard facts of a rejected
// candidate.
type RollbackReceipt struct {
	// ReasonCode is a stable non-sensitive rejection reason.
	ReasonCode string
	// ArtifactDigestHex pins the frozen rollback artifact.
	ArtifactDigestHex string
}

// candidateRow is the stored candidate identity and digest.
type candidateRow struct {
	changeSetID string
	artifactID  string
	digestHex   string
}

func lookupCandidate(ctx context.Context, ex sqlExecutor, authenticated Identity, runID string) (*candidateRow, error) {
	row := candidateRow{}
	err := ex.QueryRowContext(ctx, `SELECT change_set_id,candidate_artifact_id,candidate_digest
		FROM factory_candidates
		WHERE tenant_id=? AND principal_id=? AND run_id=?`,
		authenticated.Tenant, authenticated.Principal, runID).
		Scan(&row.changeSetID, &row.artifactID, &row.digestHex)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("factory: read candidate: %w", err)
	}
	return &row, nil
}

func currentCandidateState(
	ctx context.Context, ex sqlExecutor, authenticated Identity, runID string,
) (contractsv1.CandidateState, bool, error) {
	var stateText string
	err := ex.QueryRowContext(ctx, `SELECT state FROM factory_candidate_states
		WHERE tenant_id=? AND principal_id=? AND run_id=?
		ORDER BY sequence DESC LIMIT 1`,
		authenticated.Tenant, authenticated.Principal, runID).Scan(&stateText)
	if errors.Is(err, sql.ErrNoRows) {
		return contractsv1.CandidateState_CANDIDATE_STATE_UNSPECIFIED, false, nil
	}
	if err != nil {
		return contractsv1.CandidateState_CANDIDATE_STATE_UNSPECIFIED, false, fmt.Errorf("factory: read candidate state: %w", err)
	}
	state, err := candidateStateFromText(stateText)
	if err != nil {
		return contractsv1.CandidateState_CANDIDATE_STATE_UNSPECIFIED, false, err
	}
	return state, true, nil
}

func appendCandidateState(
	ctx context.Context, ex sqlExecutor, authenticated Identity, runID string, state contractsv1.CandidateState, atMs int64,
) error {
	stateText, err := candidateStateText(state)
	if err != nil {
		return err
	}
	var sequence uint64
	if err := ex.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM factory_candidate_states
		WHERE tenant_id=? AND principal_id=? AND run_id=?`,
		authenticated.Tenant, authenticated.Principal, runID).Scan(&sequence); err != nil {
		return fmt.Errorf("factory: read candidate sequence: %w", err)
	}
	if _, err := ex.ExecContext(ctx, `INSERT INTO factory_candidate_states
		(tenant_id,principal_id,run_id,sequence,state,occurred_at_ms) VALUES (?,?,?,?,?,?)`,
		authenticated.Tenant, authenticated.Principal, runID, sequence, stateText, atMs); err != nil {
		return fmt.Errorf("factory: append candidate state: %w", err)
	}
	return nil
}

func currentGateStatus(
	ctx context.Context, ex sqlExecutor, authenticated Identity, runID, gateID string,
) (contractsv1.FactoryGateStatus, error) {
	var statusText string
	err := ex.QueryRowContext(ctx, `SELECT status FROM factory_gate_states
		WHERE tenant_id=? AND principal_id=? AND run_id=? AND gate_id=?
		ORDER BY sequence DESC LIMIT 1`,
		authenticated.Tenant, authenticated.Principal, runID, gateID).Scan(&statusText)
	if errors.Is(err, sql.ErrNoRows) {
		return contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_UNSPECIFIED, ErrNotFoundOrDenied
	}
	if err != nil {
		return contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_UNSPECIFIED, fmt.Errorf("factory: read gate status: %w", err)
	}
	return gateStatusFromText(statusText)
}

func appendGateState(
	ctx context.Context, ex sqlExecutor, authenticated Identity, runID, gateID string,
	status contractsv1.FactoryGateStatus, atMs int64,
) error {
	statusText, err := gateStatusText(status)
	if err != nil {
		return err
	}
	var sequence uint64
	if err := ex.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM factory_gate_states
		WHERE tenant_id=? AND principal_id=? AND run_id=? AND gate_id=?`,
		authenticated.Tenant, authenticated.Principal, runID, gateID).Scan(&sequence); err != nil {
		return fmt.Errorf("factory: read gate sequence: %w", err)
	}
	if _, err := ex.ExecContext(ctx, `INSERT INTO factory_gate_states
		(tenant_id,principal_id,run_id,gate_id,sequence,status,occurred_at_ms) VALUES (?,?,?,?,?,?,?)`,
		authenticated.Tenant, authenticated.Principal, runID, gateID, sequence, statusText, atMs); err != nil {
		return fmt.Errorf("factory: append gate state: %w", err)
	}
	return nil
}

// requireRequiredGatesPassed enforces the frozen atomicity rule: no candidate
// may verify, review, or retain behind a failed or incomplete required gate.
func (k *Kernel) requireRequiredGatesPassed(ctx context.Context, ex sqlExecutor, authenticated Identity, runID string) error {
	rows, err := ex.QueryContext(ctx, `SELECT g.gate_id,
		(SELECT s.status FROM factory_gate_states s
		 WHERE s.tenant_id=g.tenant_id AND s.principal_id=g.principal_id AND s.run_id=g.run_id
		 AND s.gate_id=g.gate_id ORDER BY s.sequence DESC LIMIT 1) AS current
		FROM factory_gates g
		WHERE g.tenant_id=? AND g.principal_id=? AND g.run_id=? AND g.required=1`,
		authenticated.Tenant, authenticated.Principal, runID)
	if err != nil {
		return fmt.Errorf("factory: read required gates: %w", err)
	}
	defer rows.Close()
	gates := 0
	for rows.Next() {
		var gateID, status string
		if err := rows.Scan(&gateID, &status); err != nil {
			return fmt.Errorf("factory: scan required gates: %w", err)
		}
		gates++
		if status != "PASSED" {
			return ErrTransitionInvalid
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("factory: required gate rows: %w", err)
	}
	if gates != 4 {
		return ErrTransitionInvalid
	}
	return nil
}

// requirePreviewGatesMatchRoster enforces that the preview's gate roster is
// exactly the roster the kernel authored at admission, so no preview can
// smuggle substitute gate identities or drop a required gate.
func (k *Kernel) requirePreviewGatesMatchRoster(
	ctx context.Context, ex sqlExecutor, authenticated Identity, runID string, preview *contractsv1.ChangeSetPreview,
) error {
	for _, gate := range preview.GetGates() {
		var required int
		err := ex.QueryRowContext(ctx, `SELECT required FROM factory_gates
			WHERE tenant_id=? AND principal_id=? AND run_id=? AND gate_id=?`,
			authenticated.Tenant, authenticated.Principal, runID, gate.GetGateId().GetValue()).Scan(&required)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFoundOrDenied
		}
		if err != nil {
			return fmt.Errorf("factory: read gate roster: %w", err)
		}
		if (required == 1) != gate.GetRequired() {
			return fmt.Errorf("%w: gate %s requiredness differs from the run roster", ErrPlanInvalid, gate.GetGateId().GetValue())
		}
	}
	return nil
}

// leafScopes returns every leaf's owned write scope for candidate escape
// checks.
func (k *Kernel) leafScopes(ctx context.Context, ex sqlExecutor, authenticated Identity, runID string) ([]string, error) {
	rows, err := ex.QueryContext(ctx, `SELECT owned_paths FROM factory_plan_nodes
		WHERE tenant_id=? AND principal_id=? AND run_id=? AND kind='leaf'`,
		authenticated.Tenant, authenticated.Principal, runID)
	if err != nil {
		return nil, fmt.Errorf("factory: read leaf scopes: %w", err)
	}
	defer rows.Close()
	scopes := make([]string, 0)
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, fmt.Errorf("factory: scan leaf scope: %w", err)
		}
		paths, err := decodePaths(encoded)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, paths...)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("factory: leaf scope rows: %w", err)
	}
	return scopes, nil
}
