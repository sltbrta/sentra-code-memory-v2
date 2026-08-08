package factory

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"google.golang.org/protobuf/proto"
)

// stagedPayload joins one vault-staged payload's identity to its canonical
// digest for ledger rows.
type stagedPayload struct {
	artifactID string
	digestHex  string
}

// runRow is the immutable admission fact of one run.
type runRow struct {
	tenant           string
	principal        string
	runID            string
	sessionID        string
	intentID         string
	intentDigest     string
	intentArtifact   string
	repositoryGitOID string
	planID           string
	admittedAtMs     int64
}

// lookupRun returns the run scoped exactly to the authenticated principal;
// absence is indistinguishable from denial at the boundary.
func lookupRun(ctx context.Context, ex sqlExecutor, identity Identity, runID string) (runRow, bool, error) {
	row := runRow{}
	err := ex.QueryRowContext(ctx, `SELECT session_id,intent_id,intent_digest,intent_artifact_id,
		repository_git_oid,plan_id,admitted_at_ms
		FROM factory_runs
		WHERE tenant_id=? AND principal_id=? AND run_id=?`,
		identity.Tenant, identity.Principal, runID).
		Scan(&row.sessionID, &row.intentID, &row.intentDigest, &row.intentArtifact,
			&row.repositoryGitOID, &row.planID, &row.admittedAtMs)
	if errors.Is(err, sql.ErrNoRows) {
		return runRow{}, false, nil
	}
	if err != nil {
		return runRow{}, false, fmt.Errorf("factory: read run: %w", err)
	}
	row.tenant = identity.Tenant
	row.principal = identity.Principal
	row.runID = runID
	return row, true, nil
}

// sqlExecutor is the narrow handle satisfied by *sql.DB and *sql.Tx.
type sqlExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// currentRunState returns the latest appended lifecycle state of one run.
func currentRunState(ctx context.Context, ex sqlExecutor, identity Identity, runID string) (contractsv1.ChangeRunState, error) {
	var stateText string
	err := ex.QueryRowContext(ctx, `SELECT state FROM factory_run_states
		WHERE tenant_id=? AND principal_id=? AND run_id=?
		ORDER BY sequence DESC LIMIT 1`,
		identity.Tenant, identity.Principal, runID).Scan(&stateText)
	if errors.Is(err, sql.ErrNoRows) {
		return contractsv1.ChangeRunState_CHANGE_RUN_STATE_UNSPECIFIED, ErrNotFoundOrDenied
	}
	if err != nil {
		return contractsv1.ChangeRunState_CHANGE_RUN_STATE_UNSPECIFIED, fmt.Errorf("factory: read run state: %w", err)
	}
	return runStateFromText(stateText)
}

// appendRunState inserts the next dense lifecycle transition; the schema
// trigger independently enforces density and terminal finality.
func appendRunState(
	ctx context.Context, ex sqlExecutor, identity Identity, runID string,
	state contractsv1.ChangeRunState, atMs int64,
) error {
	stateText, err := runStateText(state)
	if err != nil {
		return err
	}
	var sequence uint64
	if err := ex.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM factory_run_states
		WHERE tenant_id=? AND principal_id=? AND run_id=?`,
		identity.Tenant, identity.Principal, runID).Scan(&sequence); err != nil {
		return fmt.Errorf("factory: read run sequence: %w", err)
	}
	if _, err := ex.ExecContext(ctx, `INSERT INTO factory_run_states
		(tenant_id,principal_id,run_id,sequence,state,occurred_at_ms) VALUES (?,?,?,?,?,?)`,
		identity.Tenant, identity.Principal, runID, sequence, stateText, atMs); err != nil {
		return fmt.Errorf("factory: append run state: %w", err)
	}
	return nil
}

// idempotencyRow is one recorded admission or cancellation outcome.
type idempotencyRow struct {
	requestDigest string
	runID         string
}

func lookupIdempotency(
	ctx context.Context, ex sqlExecutor, identity Identity, operation, key string,
) (idempotencyRow, bool, error) {
	row := idempotencyRow{}
	err := ex.QueryRowContext(ctx, `SELECT request_digest, run_id FROM factory_idempotency
		WHERE tenant_id=? AND principal_id=? AND operation=? AND idempotency_key=?`,
		identity.Tenant, identity.Principal, operation, key).Scan(&row.requestDigest, &row.runID)
	if errors.Is(err, sql.ErrNoRows) {
		return idempotencyRow{}, false, nil
	}
	if err != nil {
		return idempotencyRow{}, false, fmt.Errorf("factory: read idempotency record: %w", err)
	}
	return row, true, nil
}

func insertIdempotency(
	ctx context.Context, ex sqlExecutor, identity Identity, operation, key, requestDigest, runID string, atMs int64,
) error {
	if _, err := ex.ExecContext(ctx, `INSERT INTO factory_idempotency
		(tenant_id,principal_id,operation,idempotency_key,request_digest,run_id,created_at_ms)
		VALUES (?,?,?,?,?,?,?)`,
		identity.Tenant, identity.Principal, operation, key, requestDigest, runID, atMs); err != nil {
		return fmt.Errorf("factory: commit idempotency record: %w", err)
	}
	return nil
}

// stagePayload encrypts and publishes one payload, returning its identity and
// canonical digest. Payload staging happens before the ledger transaction so
// the schema only ever holds verified references; a failed transaction leaves
// an unreferenced immutable artifact, never a metadata row without bytes.
func (k *Kernel) stagePayload(ctx context.Context, tenant string, payload []byte) (stagedPayload, error) {
	if len(payload) == 0 {
		return stagedPayload{}, ErrInvalidInput
	}
	artifactID, err := k.payloads.Put(ctx, tenant, payload)
	if err != nil {
		return stagedPayload{}, fmt.Errorf("factory: stage payload: %w", err)
	}
	if artifactID == "" {
		return stagedPayload{}, ErrPayloadUnavailable
	}
	return stagedPayload{artifactID: artifactID, digestHex: digestBytes(payload)}, nil
}

// hydratePayload reads and reverifies one staged payload against its canonical
// digest; unreadable or corrupt payloads fail closed.
func (k *Kernel) hydratePayload(ctx context.Context, tenant string, staged stagedPayload) ([]byte, error) {
	payload, err := k.payloads.Get(ctx, tenant, staged.artifactID)
	if err != nil {
		return nil, errors.Join(ErrPayloadUnavailable, err)
	}
	if digestBytes(payload) != staged.digestHex {
		return nil, ErrPayloadUnavailable
	}
	return payload, nil
}

// encodeCursor pins one opaque pagination position.
func encodeCursor(occurredAtMs int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(occurredAtMs, 10) + "\n" + id))
}

// decodeCursor resolves one opaque pagination position; malformed cursors are
// invalid input, never silently restarted listings.
func decodeCursor(cursor string) (int64, string, error) {
	if cursor == "" {
		return 0, "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, "", ErrInvalidInput
	}
	parts := strings.SplitN(string(decoded), "\n", 2)
	if len(parts) != 2 || parts[1] == "" {
		return 0, "", ErrInvalidInput
	}
	occurredAtMs, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", ErrInvalidInput
	}
	return occurredAtMs, parts[1], nil
}

// marshalDeterministic binds one canonical proto message to exact bytes.
func marshalDeterministic(message proto.Message) ([]byte, error) {
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("factory: marshal canonical message: %w", err)
	}
	return encoded, nil
}
