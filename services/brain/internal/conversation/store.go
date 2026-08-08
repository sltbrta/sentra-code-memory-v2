package conversation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/query"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	_ "modernc.org/sqlite"
)

// Store persists private conversation facts in the single-writer SQLite
// authority database behind the migration 004 insert-only tables. It is safe
// for concurrent use: one mutex serializes every operation, so the dense
// per-session sequence and the exactly-once completion index hold without
// cross-process locking, which the composing authority owner already enforces.
type Store struct {
	db       *sql.DB
	payloads PayloadStore
	clock    contracts.Clock
	mu       sync.Mutex
}

// Open attaches the conversation store to an already-migrated authority
// database. The path must be absolute; migration 004 must already be applied
// (the composing local authority owns migrations and the process owner lock,
// so Open takes neither). WAL, full synchronous, foreign keys, and a bounded
// busy timeout mirror the authority's own durability posture.
func Open(ctx context.Context, databasePath string, payloads PayloadStore, clock contracts.Clock) (*Store, error) {
	clean := filepath.Clean(databasePath)
	if !filepath.IsAbs(clean) || payloads == nil || clock == nil {
		return nil, ErrInvalidInput
	}
	db, err := sql.Open("sqlite", clean)
	if err != nil {
		return nil, fmt.Errorf("conversation: open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, payloads: payloads, clock: clock}
	if err := store.configure(ctx); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	if err := store.requireSchema(ctx); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	return store, nil
}

// Close releases the database handle; the authority owner lock stays with the
// composing local authority. It is idempotent and safe to call concurrently
// with in-flight operations: those already holding the store mutex commit
// first, and every later call fails closed with ErrInvalidInput.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *Store) configure(ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("conversation: configure database: %w", err)
		}
	}
	return nil
}

func (s *Store) requireSchema(ctx context.Context) error {
	var applied int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM schema_migrations WHERE version=4`).Scan(&applied); err != nil {
		return errors.Join(ErrSchemaUnsupported, fmt.Errorf("conversation: inspect migrations: %w", err))
	}
	if applied != 1 {
		return ErrSchemaUnsupported
	}
	return nil
}

// Admit atomically commits the user turn and its query-idempotency record, or
// returns the original admission for an exact idempotent retry. A reused key
// with a different request digest returns ErrIdempotencyConflict and mutates
// nothing. The payload is staged into the encrypted vault before the
// transaction so the schema only ever holds the artifact identity and digest;
// a transaction failure leaves an unreferenced immutable artifact, never a
// metadata row without bytes.
func (s *Store) Admit(ctx context.Context, admission Admission) (AdmissionResult, error) {
	if s == nil || ctx == nil {
		return AdmissionResult{}, ErrInvalidInput
	}
	if err := validAdmission(admission); err != nil {
		return AdmissionResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return AdmissionResult{}, ErrInvalidInput
	}
	digest := requestDigest(admission)
	queryIDValue := queryID(admission.Principal.Tenant, admission.Principal.Principal, admission.IdempotencyKey)
	existing, found, err := s.lookupAdmission(ctx, admission.Principal, admission.IdempotencyKey)
	if err != nil {
		return AdmissionResult{}, err
	}
	if found {
		if existing.requestDigest != digest {
			return AdmissionResult{}, ErrIdempotencyConflict
		}
		return AdmissionResult{QueryID: queryIDValue, UserTurnID: existing.userTurnID, Replayed: true}, nil
	}
	encoded, err := marshalUserPayload(admission.Text)
	if err != nil {
		return AdmissionResult{}, err
	}
	artifactID, err := s.payloads.Put(ctx, admission.Principal.Tenant, encoded)
	if err != nil {
		return AdmissionResult{}, fmt.Errorf("conversation: stage user payload: %w", err)
	}
	summary, err := s.insertAdmission(ctx, admission, digest, artifactID, payloadDigest(encoded))
	if err != nil {
		return AdmissionResult{}, err
	}
	return AdmissionResult{QueryID: queryIDValue, UserTurnID: summary.userTurnID, Replayed: summary.replayed}, nil
}

// Complete appends the exactly-once assistant completion for one admitted
// query. An active completion carries the full grounded result; a failed one
// is visibly failed and never read as fact. An exact completion retry returns
// the original turn with Replayed set; a differing second completion returns
// ErrCompletionConflict, and the schema's partial unique index enforces
// exactly-once even against a racing process.
func (s *Store) Complete(ctx context.Context, completion Completion) (CompletionResult, error) {
	if s == nil || ctx == nil {
		return CompletionResult{}, ErrInvalidInput
	}
	if err := validCompletion(completion); err != nil {
		return CompletionResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return CompletionResult{}, ErrInvalidInput
	}
	return s.completeLocked(ctx, completion)
}

func (s *Store) completeLocked(ctx context.Context, completion Completion) (CompletionResult, error) {
	principal := query.Principal{Tenant: completion.Tenant, Principal: completion.Principal}
	admission, found, err := s.lookupAdmission(ctx, principal, completion.IdempotencyKey)
	if err != nil {
		return CompletionResult{}, err
	}
	if !found {
		return CompletionResult{}, ErrUnknownAdmission
	}
	encoded, err := marshalCompletionPayload(completion.Result)
	if err != nil {
		return CompletionResult{}, err
	}
	status := StatusActive
	if completion.Failed {
		status = StatusFailed
	}
	digest := payloadDigest(encoded)
	existing, existingFound, err := s.lookupCompletion(ctx, completion.Tenant, completion.Principal, completion.IdempotencyKey)
	if err != nil {
		return CompletionResult{}, err
	}
	if existingFound {
		if existing.status == status && existing.payloadDigest == digest {
			return CompletionResult{AssistantTurnID: existing.turnID, Sequence: existing.sequence, Replayed: true}, nil
		}
		return CompletionResult{}, ErrCompletionConflict
	}
	artifactID, err := s.payloads.Put(ctx, completion.Tenant, encoded)
	if err != nil {
		return CompletionResult{}, fmt.Errorf("conversation: stage assistant payload: %w", err)
	}
	return s.insertCompletion(ctx, completion, admission, status, artifactID, digest)
}

// Resolve returns the original outcome of one admitted idempotency key: the
// admitted identities, and once the exactly-once completion commits, its
// status and — for an active completion — the hydrated grounded result. A key
// that was never admitted returns ErrUnknownAdmission.
func (s *Store) Resolve(ctx context.Context, tenant, principal, idempotencyKey string) (Resolution, error) {
	if s == nil || ctx == nil || !validBoundedID(tenant) ||
		!validBoundedID(principal) || !validIdempotencyKey(idempotencyKey) {
		return Resolution{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return Resolution{}, ErrInvalidInput
	}
	admission, found, err := s.lookupAdmission(ctx, query.Principal{Tenant: tenant, Principal: principal}, idempotencyKey)
	if err != nil {
		return Resolution{}, err
	}
	if !found {
		return Resolution{}, ErrUnknownAdmission
	}
	resolution := Resolution{
		QueryID:    queryID(tenant, principal, idempotencyKey),
		UserTurnID: admission.userTurnID,
		SessionID:  admission.sessionID,
	}
	completion, completionFound, err := s.lookupCompletion(ctx, tenant, principal, idempotencyKey)
	if err != nil {
		return Resolution{}, err
	}
	if !completionFound {
		return resolution, nil
	}
	resolution.Completed = true
	resolution.Status = completion.status
	resolution.AssistantTurnID = completion.turnID
	if completion.status == StatusActive {
		payload, err := s.hydrate(ctx, tenant, completion.payloadArtifactID, completion.payloadDigest)
		if err != nil {
			return Resolution{}, err
		}
		if payload.Result == nil {
			return Resolution{}, ErrPayloadUnavailable
		}
		resolution.Result = domainResult(payload.Result)
	}
	return resolution, nil
}

// History returns the authenticated principal's own committed turns in the
// frozen (occurred_at_ms, turn_id) total order, paginated through an opaque
// cursor. Turns from other principals and other tenants are never selected,
// so cross-principal history is structurally inexpressible. Every payload is
// reverified against its canonical digest during hydration; an unreadable or
// corrupt payload fails the whole page rather than silently skipping a turn.
func (s *Store) History(ctx context.Context, tenant, principal, after string, limit uint32) (Page, error) {
	if s == nil || ctx == nil || !validBoundedID(tenant) || !validBoundedID(principal) ||
		limit == 0 || limit > MaxHistoryPage {
		return Page{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return Page{}, ErrInvalidInput
	}
	occurredAfter, turnAfter, err := decodeCursor(after)
	if err != nil {
		return Page{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT session_id,turn_id,sequence_in_session,role,status,
		COALESCE(idempotency_key,''),payload_artifact_id,payload_digest,occurred_at_ms
		FROM conversation_turns
		WHERE tenant_id=? AND principal_id=? AND (occurred_at_ms > ? OR (occurred_at_ms = ? AND turn_id > ?))
		ORDER BY occurred_at_ms, turn_id LIMIT ?`,
		tenant, principal, occurredAfter, occurredAfter, turnAfter, int64(limit)+1)
	if err != nil {
		return Page{}, fmt.Errorf("conversation: history query: %w", err)
	}
	defer rows.Close()
	page := Page{Turns: make([]Turn, 0, limit)}
	for rows.Next() {
		var turn Turn
		var artifactID, digest string
		if err := rows.Scan(&turn.SessionID, &turn.TurnID, &turn.Sequence, &turn.Role, &turn.Status,
			&turn.IdempotencyKey, &artifactID, &digest, &turn.OccurredAtMs); err != nil {
			return Page{}, fmt.Errorf("conversation: history scan: %w", err)
		}
		if uint32(len(page.Turns)) == limit {
			last := page.Turns[len(page.Turns)-1]
			page.NextCursor = encodeCursor(last.OccurredAtMs, last.TurnID)
			return page, rows.Err()
		}
		payload, err := s.hydrate(ctx, tenant, artifactID, digest)
		if err != nil {
			return Page{}, err
		}
		if err := applyPayload(&turn, payload); err != nil {
			return Page{}, err
		}
		page.Turns = append(page.Turns, turn)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("conversation: history rows: %w", err)
	}
	return page, nil
}

// RecoverInterrupted marks every admitted-but-uncompleted query as failed,
// appending one visibly failed assistant turn per interrupted admission. It
// runs once at process restart before the query surface serves; the
// exactly-once completion index makes it idempotent and safe to repeat.
func (s *Store) RecoverInterrupted(ctx context.Context) (int, error) {
	if s == nil || ctx == nil {
		return 0, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return 0, ErrInvalidInput
	}
	rows, err := s.db.QueryContext(ctx, `SELECT i.tenant_id, i.principal_id, i.idempotency_key
		FROM conversation_query_idempotency i
		WHERE NOT EXISTS (
			SELECT 1 FROM conversation_turns t
			WHERE t.tenant_id=i.tenant_id AND t.principal_id=i.principal_id AND t.idempotency_key=i.idempotency_key
		)
		ORDER BY i.created_at_ms, i.idempotency_key`)
	if err != nil {
		return 0, fmt.Errorf("conversation: interrupted query scan: %w", err)
	}
	type interrupted struct{ tenant, principal, key string }
	pending := make([]interrupted, 0)
	for rows.Next() {
		var row interrupted
		if err := rows.Scan(&row.tenant, &row.principal, &row.key); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("conversation: interrupted scan row: %w", err)
		}
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("conversation: interrupted scan rows: %w", err)
	}
	_ = rows.Close()
	recovered := 0
	for _, row := range pending {
		result, err := s.completeLocked(ctx, Completion{
			Tenant: row.tenant, Principal: row.principal, IdempotencyKey: row.key, Failed: true,
		})
		if err != nil {
			return recovered, fmt.Errorf("conversation: recover interrupted query: %w", err)
		}
		if !result.Replayed {
			recovered++
		}
	}
	return recovered, nil
}

func (s *Store) hydrate(ctx context.Context, tenant, artifactID, expectedDigest string) (storedPayload, error) {
	encoded, err := s.payloads.Get(ctx, tenant, artifactID)
	if err != nil {
		return storedPayload{}, errors.Join(ErrPayloadUnavailable, err)
	}
	if payloadDigest(encoded) != expectedDigest {
		return storedPayload{}, ErrPayloadUnavailable
	}
	return unmarshalPayload(encoded)
}

// applyPayload folds one hydrated payload into its history row, enforcing the
// frozen role shape: user turns carry text, active assistant turns carry an
// answer, failed assistant turns carry neither.
func applyPayload(turn *Turn, payload storedPayload) error {
	switch {
	case turn.Role == RoleUser && turn.Status == StatusActive && payload.Result == nil && payload.Text != "":
		turn.Text = payload.Text
		return nil
	case turn.Role == RoleAssistant && turn.Status == StatusActive && payload.Result != nil && payload.Text == "":
		turn.Answer = domainAnswer(&payload.Result.Answer)
		return nil
	case turn.Role == RoleAssistant && turn.Status == StatusFailed && payload.Result == nil && payload.Text == "":
		return nil
	default:
		return ErrPayloadUnavailable
	}
}

type admissionRow struct {
	requestDigest string
	sessionID     string
	userTurnID    string
}

func (s *Store) lookupAdmission(
	ctx context.Context, principal query.Principal, idempotencyKey string,
) (admissionRow, bool, error) {
	row := admissionRow{}
	err := s.db.QueryRowContext(ctx, `SELECT request_digest, session_id, user_turn_id
		FROM conversation_query_idempotency
		WHERE tenant_id=? AND principal_id=? AND idempotency_key=?`,
		principal.Tenant, principal.Principal, idempotencyKey).
		Scan(&row.requestDigest, &row.sessionID, &row.userTurnID)
	if errors.Is(err, sql.ErrNoRows) {
		return admissionRow{}, false, nil
	}
	if err != nil {
		return admissionRow{}, false, fmt.Errorf("conversation: read admitted query: %w", err)
	}
	return row, true, nil
}

type completionRow struct {
	turnID            string
	sequence          uint64
	status            Status
	payloadArtifactID string
	payloadDigest     string
}

func (s *Store) lookupCompletion(
	ctx context.Context, tenant, principal, idempotencyKey string,
) (completionRow, bool, error) {
	row := completionRow{}
	err := s.db.QueryRowContext(ctx, `SELECT turn_id, sequence_in_session, status, payload_artifact_id, payload_digest
		FROM conversation_turns
		WHERE tenant_id=? AND principal_id=? AND idempotency_key=?`,
		tenant, principal, idempotencyKey).
		Scan(&row.turnID, &row.sequence, &row.status, &row.payloadArtifactID, &row.payloadDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return completionRow{}, false, nil
	}
	if err != nil {
		return completionRow{}, false, fmt.Errorf("conversation: read completion: %w", err)
	}
	return row, true, nil
}

type admissionInsert struct {
	userTurnID string
	replayed   bool
}

// insertAdmission commits the user turn and idempotency row in one
// serializable transaction. The dense-append trigger independently enforces
// the computed next sequence. A racing admission already visible to the
// transaction snapshot is reclassified by the in-transaction re-read as an
// exact replay or a digest conflict without mutation; a conflict landing
// after the snapshot (unreachable in-process, where the store mutex and the
// single connection serialize writers) is rejected by the primary key, which
// guarantees a duplicate can never commit and surfaces as a raw wrapped
// error.
func (s *Store) insertAdmission(
	ctx context.Context, admission Admission, digest, artifactID, digestHex string,
) (admissionInsert, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return admissionInsert{}, fmt.Errorf("conversation: begin admission: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var existingDigest, userTurn string
	lookupErr := tx.QueryRowContext(ctx, `SELECT request_digest, user_turn_id
		FROM conversation_query_idempotency
		WHERE tenant_id=? AND principal_id=? AND idempotency_key=?`,
		admission.Principal.Tenant, admission.Principal.Principal, admission.IdempotencyKey).
		Scan(&existingDigest, &userTurn)
	if lookupErr == nil {
		if existingDigest != digest {
			return admissionInsert{}, ErrIdempotencyConflict
		}
		return admissionInsert{userTurnID: userTurn, replayed: true}, nil
	}
	if !errors.Is(lookupErr, sql.ErrNoRows) {
		return admissionInsert{}, fmt.Errorf("conversation: re-read admitted query: %w", lookupErr)
	}
	var sessionCount int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sessions
		WHERE tenant_id=? AND principal_id=? AND session_id=?`,
		admission.Principal.Tenant, admission.Principal.Principal, admission.Principal.Session).
		Scan(&sessionCount); err != nil {
		return admissionInsert{}, fmt.Errorf("conversation: verify session: %w", err)
	}
	if sessionCount != 1 {
		return admissionInsert{}, ErrUnknownSession
	}
	var sequence uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence_in_session),0)+1
		FROM conversation_turns WHERE tenant_id=? AND principal_id=? AND session_id=?`,
		admission.Principal.Tenant, admission.Principal.Principal, admission.Principal.Session).
		Scan(&sequence); err != nil {
		return admissionInsert{}, fmt.Errorf("conversation: read session sequence: %w", err)
	}
	turnID := userTurnID(admission.Principal.Tenant, admission.Principal.Principal, admission.Principal.Session, sequence)
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,idempotency_key,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES (?,?,?,?,?,'user','active',NULL,?,?,?)`,
		admission.Principal.Tenant, admission.Principal.Principal, admission.Principal.Session,
		turnID, sequence, artifactID, digestHex, s.clock.NowUnixMilli()); err != nil {
		return admissionInsert{}, fmt.Errorf("conversation: commit user turn: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_query_idempotency
		(tenant_id,principal_id,idempotency_key,request_digest,session_id,user_turn_id,created_at_ms)
		VALUES (?,?,?,?,?,?,?)`,
		admission.Principal.Tenant, admission.Principal.Principal, admission.IdempotencyKey,
		digest, admission.Principal.Session, turnID, s.clock.NowUnixMilli()); err != nil {
		return admissionInsert{}, fmt.Errorf("conversation: commit idempotency record: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return admissionInsert{}, fmt.Errorf("conversation: commit admission: %w", err)
	}
	return admissionInsert{userTurnID: turnID}, nil
}

// insertCompletion appends the assistant turn in one serializable transaction,
// extending the admitting session densely. A concurrent completion already
// visible to the transaction snapshot is reclassified by the in-transaction
// re-read as an exact replay or a conflict; a conflict landing after the
// snapshot (unreachable in-process, where the store mutex and the single
// connection serialize writers) is rejected by the partial unique index,
// which guarantees exactly-once completion and surfaces as a raw wrapped
// error.
func (s *Store) insertCompletion(
	ctx context.Context, completion Completion, admission admissionRow, status Status, artifactID, digest string,
) (CompletionResult, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return CompletionResult{}, fmt.Errorf("conversation: begin completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var existingTurn, existingDigest, existingStatus string
	var existingSequence uint64
	lookupErr := tx.QueryRowContext(ctx, `SELECT turn_id, sequence_in_session, status, payload_digest
		FROM conversation_turns
		WHERE tenant_id=? AND principal_id=? AND idempotency_key=?`,
		completion.Tenant, completion.Principal, completion.IdempotencyKey).
		Scan(&existingTurn, &existingSequence, &existingStatus, &existingDigest)
	if lookupErr == nil {
		if existingStatus == string(status) && existingDigest == digest {
			return CompletionResult{AssistantTurnID: existingTurn, Sequence: existingSequence, Replayed: true}, nil
		}
		return CompletionResult{}, ErrCompletionConflict
	}
	if !errors.Is(lookupErr, sql.ErrNoRows) {
		return CompletionResult{}, fmt.Errorf("conversation: re-read completion: %w", lookupErr)
	}
	var sequence uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence_in_session),0)+1
		FROM conversation_turns WHERE tenant_id=? AND principal_id=? AND session_id=?`,
		completion.Tenant, completion.Principal, admission.sessionID).
		Scan(&sequence); err != nil {
		return CompletionResult{}, fmt.Errorf("conversation: read session sequence: %w", err)
	}
	turnID := assistantTurnID(completion.Tenant, completion.Principal, completion.IdempotencyKey)
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_turns
		(tenant_id,principal_id,session_id,turn_id,sequence_in_session,role,status,idempotency_key,
		 payload_artifact_id,payload_digest,occurred_at_ms)
		VALUES (?,?,?,?,?,'assistant',?,?,?,?,?)`,
		completion.Tenant, completion.Principal, admission.sessionID, turnID, sequence,
		string(status), completion.IdempotencyKey, artifactID, digest, s.clock.NowUnixMilli()); err != nil {
		return CompletionResult{}, fmt.Errorf("conversation: commit assistant turn: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return CompletionResult{}, fmt.Errorf("conversation: commit completion: %w", err)
	}
	return CompletionResult{AssistantTurnID: turnID, Sequence: sequence}, nil
}
