// Package localstate owns the single-writer SQLite authority ledger.
// Commands, idempotency, events, receipts, audit links, outbox, and watermarks
// commit together before acknowledgement; artifact bytes never enter this store.
package localstate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/audit"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/deletion"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/eventkernel"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	_ "modernc.org/sqlite"
)

var (
	// ErrInvalidInput reports missing or malformed authority facts.
	ErrInvalidInput = errors.New("localstate: invalid input")
	// ErrIdentityMismatch reports an authenticated session conflict.
	ErrIdentityMismatch = errors.New("localstate: identity mismatch")
	// ErrIdempotencyConflict reports reuse of a key with different authenticated facts.
	ErrIdempotencyConflict = errors.New("localstate: idempotency conflict")
	// ErrReservationRequired reports finalize without a matching accepted reservation.
	ErrReservationRequired = errors.New("localstate: accepted reservation required")
	// ErrAuthorityOwned reports that another process owns the SQLite authority path.
	ErrAuthorityOwned = errors.New("localstate: authority already owned")
	// ErrAggregateConflict reports a non-consecutive aggregate version.
	ErrAggregateConflict = errors.New("localstate: aggregate version conflict")
)

// Mutation is one canonical local authority commit request.
type Mutation struct {
	Command    contracts.CommandRecord
	Events     []MutationEvent
	Receipt    contracts.Receipt
	Projection string
	Deletion   *deletion.Request
	PurgeNow   bool
}

// MutationEvent binds the frozen event record to Stage 02's explicit implicit schema version.
// Type must equal the persisted aggregate namespace and SchemaVersion must be exactly one.
type MutationEvent struct {
	Type          string
	SchemaVersion uint64
	Record        contracts.EventRecord
}

// Execution is the canonical or replayed command disposition.
type Execution struct {
	Receipt  contracts.Receipt
	Replayed bool
}

// Reservation is the canonical durable command state returned by Reserve.
// Receipt is populated only for completed commands; accepted commands are
// resumable and carry no completion receipt.
type Reservation struct {
	// Command is the original canonical command, including its session and identity.
	Command contracts.CommandRecord
	// Status is accepted for resumable work or completed for a canonical receipt.
	Status string
	// Receipt is non-zero only when Status is completed.
	Receipt contracts.Receipt
	// Replayed is false only for the call that first persisted this reservation.
	Replayed bool
}

// Store serializes writes to one WAL database while allowing stable SQL reads.
type Store struct {
	db    *sql.DB
	clock contracts.Clock
	owner *authorityOwner
	mu    sync.Mutex
}

// Migration is one immutable ordered schema transition. Version numbers must
// start at one and be consecutive; SQL is applied once in its own transaction.
type Migration struct {
	Version int
	SQL     string
}

// Open configures and migrates one local SQLite authority database.
// The path must be absolute. Open retains an exclusive process owner lock until
// Close and returns ErrAuthorityOwned while another Store owns the same path.
func Open(ctx context.Context, databasePath, migrationSQL string, clock contracts.Clock) (*Store, error) {
	return OpenWithMigrations(ctx, databasePath, []Migration{{Version: 1, SQL: migrationSQL}}, clock)
}

// OpenWithMigrations configures and owns one authority database, then applies
// each consecutive migration monotonically. A completed lower version is never
// rewritten or rolled back when a later version fails.
func OpenWithMigrations(ctx context.Context, databasePath string, migrations []Migration, clock contracts.Clock) (*Store, error) {
	if strings.TrimSpace(databasePath) == "" || !validMigrations(migrations) || clock == nil {
		return nil, ErrInvalidInput
	}
	owner, canonicalPath, err := acquireAuthorityOwner(databasePath)
	if err != nil {
		return nil, err
	}
	if err := createAuthorityDatabase(canonicalPath); err != nil {
		return nil, errors.Join(err, owner.close())
	}
	db, err := sql.Open("sqlite", canonicalPath)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("localstate: open database: %w", err), owner.close())
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, clock: clock, owner: owner}
	if err := store.configure(ctx); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	if err := store.migrate(ctx, migrations); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	return store, nil
}

func createAuthorityDatabase(databasePath string) error {
	file, err := os.OpenFile(databasePath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("localstate: create authority database: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		return errors.Join(fmt.Errorf("localstate: secure authority database: %w", err), file.Close())
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("localstate: close created authority database: %w", err)
	}
	return nil
}

// Close releases the database handle and exclusive process owner lock.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var databaseErr error
	if s.db != nil {
		databaseErr = s.db.Close()
		s.db = nil
	}
	ownerErr := s.owner.close()
	s.owner = nil
	return errors.Join(databaseErr, ownerErr)
}

// OpenSession persists an operating-system-authenticated session or verifies an exact retry.
func (s *Store) OpenSession(ctx context.Context, identity contracts.MappedIdentityFact) error {
	if s == nil || !validID(identity.Principal, "principal") || !validID(identity.Tenant, "tenant") ||
		!validID(identity.Session, "session") || identity.Credentials.PID == 0 {
		return ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions
		(session_id,principal_id,tenant_id,peer_uid,opened_at_ms,closed_at_ms)
		VALUES (?,?,?,?,?,NULL) ON CONFLICT(session_id) DO NOTHING`, identity.Session.Value,
		identity.Principal.Value, identity.Tenant.Value, identity.Credentials.UID, s.clock.NowUnixMilli())
	if err != nil {
		return fmt.Errorf("localstate: open session: %w", err)
	}
	var principal, tenant string
	var uid uint32
	if err := s.db.QueryRowContext(ctx, `SELECT principal_id,tenant_id,peer_uid FROM sessions WHERE session_id=?`, identity.Session.Value).
		Scan(&principal, &tenant, &uid); err != nil {
		return fmt.Errorf("localstate: verify session: %w", err)
	}
	if principal != identity.Principal.Value || tenant != identity.Tenant.Value || uid != identity.Credentials.UID {
		return ErrIdentityMismatch
	}
	return nil
}

// SessionWatermark verifies the exact persisted authenticated session and
// returns the tenant's latest canonical event sequence. It never infers a
// current session from principal or tenant scope.
func (s *Store) SessionWatermark(ctx context.Context, identity contracts.MappedIdentityFact) (uint64, error) {
	if s == nil || !validID(identity.Principal, "principal") || !validID(identity.Tenant, "tenant") ||
		!validID(identity.Session, "session") {
		return 0, ErrInvalidInput
	}
	var principal, tenant string
	var uid uint32
	if err := s.db.QueryRowContext(ctx, `SELECT principal_id,tenant_id,peer_uid FROM sessions WHERE session_id=?`,
		identity.Session.Value).Scan(&principal, &tenant, &uid); err != nil {
		return 0, ErrIdentityMismatch
	}
	if principal != identity.Principal.Value || tenant != identity.Tenant.Value || uid != identity.Credentials.UID {
		return 0, ErrIdentityMismatch
	}
	var watermark uint64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM events WHERE tenant_id=?`,
		identity.Tenant.Value).Scan(&watermark); err != nil {
		return 0, fmt.Errorf("localstate: session watermark: %w", err)
	}
	return watermark, nil
}

// Reserve durably claims a command before an external idempotent effect runs.
// Exact retries return the original canonical command. Accepted retries are
// resumable; completed retries also return the original receipt.
func (s *Store) Reserve(ctx context.Context, command contracts.CommandRecord) (Reservation, error) {
	if s == nil || !validCommand(command) {
		return Reservation{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return Reservation{}, fmt.Errorf("localstate: begin command reservation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	canonical, commandFound, err := lookupCanonicalCommand(ctx, tx, command.Command.Value)
	if err != nil {
		return Reservation{}, err
	}
	if commandFound && canonical != command {
		return Reservation{}, ErrIdempotencyConflict
	}
	reservation, found, err := lookupReservation(ctx, tx, command)
	if err != nil {
		return Reservation{}, err
	}
	if found {
		if commandFound && reservation.Command != command {
			return Reservation{}, ErrIdempotencyConflict
		}
		reservation.Replayed = true
		return reservation, nil
	}
	if commandFound {
		return Reservation{}, ErrIdempotencyConflict
	}
	if err := insertCommand(ctx, tx, command, s.clock.NowUnixMilli()); err != nil {
		return Reservation{}, err
	}
	if err := tx.Commit(); err != nil {
		return Reservation{}, fmt.Errorf("localstate: commit command reservation: %w", err)
	}
	return Reservation{Command: command, Status: "accepted"}, nil
}

// Finalize atomically completes an exact accepted canonical reservation.
// Exact completed retries return the stored receipt; unreserved, rejected, or
// mismatched commands fail before any canonical completion state is appended.
func (s *Store) Finalize(ctx context.Context, mutation Mutation) (Execution, error) {
	if s == nil || !validMutation(mutation) {
		return Execution{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return Execution{}, fmt.Errorf("localstate: begin command finalize: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	canonical, commandFound, err := lookupCanonicalCommand(ctx, tx, mutation.Command.Command.Value)
	if err != nil {
		return Execution{}, err
	}
	if commandFound && canonical != mutation.Command {
		return Execution{}, ErrIdempotencyConflict
	}
	reservation, found, err := lookupReservation(ctx, tx, mutation.Command)
	if err != nil {
		return Execution{}, err
	}
	if !found || reservation.Status == "rejected" {
		return Execution{}, ErrReservationRequired
	}
	if reservation.Command != mutation.Command {
		return Execution{}, ErrIdempotencyConflict
	}
	if reservation.Status == "completed" {
		return Execution{Receipt: reservation.Receipt, Replayed: true}, nil
	}
	receipt, err := completeMutation(ctx, tx, mutation, s.clock)
	if err != nil {
		return Execution{}, err
	}
	if err := tx.Commit(); err != nil {
		return Execution{}, fmt.Errorf("localstate: commit command finalize: %w", err)
	}
	return Execution{Receipt: receipt}, nil
}

// Execute commits one command unit or returns its exact canonical replay.
// Conflicting idempotency reuse returns ErrIdempotencyConflict without mutation.
func (s *Store) Execute(ctx context.Context, mutation Mutation) (Execution, error) {
	if s == nil || !validMutation(mutation) {
		return Execution{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return Execution{}, fmt.Errorf("localstate: begin command: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	canonical, commandFound, err := lookupCanonicalCommand(ctx, tx, mutation.Command.Command.Value)
	if err != nil {
		return Execution{}, err
	}
	if commandFound && canonical != mutation.Command {
		return Execution{}, ErrIdempotencyConflict
	}
	reservation, found, err := lookupReservation(ctx, tx, mutation.Command)
	if err != nil {
		return Execution{}, err
	}
	if found && reservation.Status == "completed" {
		return Execution{Receipt: reservation.Receipt, Replayed: true}, nil
	}
	if found && (reservation.Status != "accepted" || reservation.Command != mutation.Command) {
		return Execution{}, ErrIdempotencyConflict
	}
	if !found {
		if commandFound {
			return Execution{}, ErrIdempotencyConflict
		}
		if err := insertCommand(ctx, tx, mutation.Command, s.clock.NowUnixMilli()); err != nil {
			return Execution{}, err
		}
	}
	receipt, err := completeMutation(ctx, tx, mutation, s.clock)
	if err != nil {
		return Execution{}, err
	}
	if err := tx.Commit(); err != nil {
		return Execution{}, fmt.Errorf("localstate: commit command: %w", err)
	}
	return Execution{Receipt: receipt}, nil
}

// Replay returns the ordered immutable event view used by deterministic projectors.
func (s *Store) Replay(ctx context.Context, tenant contracts.Identifier) ([]eventkernel.Event, error) {
	if s == nil || !validID(tenant, "tenant") {
		return nil, ErrInvalidInput
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id,aggregate_type,aggregate_id,aggregate_version,payload_digest
		FROM events WHERE tenant_id=? ORDER BY sequence`, tenant.Value)
	if err != nil {
		return nil, fmt.Errorf("localstate: replay query: %w", err)
	}
	defer rows.Close()
	events := make([]eventkernel.Event, 0)
	for rows.Next() {
		var event eventkernel.Event
		if err := rows.Scan(&event.ID, &event.AggregateType, &event.AggregateID, &event.AggregateVersion, &event.PayloadDigest); err != nil {
			return nil, fmt.Errorf("localstate: replay scan: %w", err)
		}
		event.Type = event.AggregateType
		event.SchemaVersion = 1
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("localstate: replay rows: %w", err)
	}
	return events, nil
}

// AggregateVersion returns the exact tenant-scoped aggregate version from the
// primary-key-backed authority projection. An absent aggregate is version zero.
func (s *Store) AggregateVersion(ctx context.Context, tenant, aggregate contracts.Identifier) (uint64, error) {
	if s == nil || !validID(tenant, "tenant") || aggregate.Namespace == "" || aggregate.Value == "" {
		return 0, ErrInvalidInput
	}
	var version uint64
	err := s.db.QueryRowContext(ctx, `SELECT version FROM aggregate_versions
		WHERE tenant_id=? AND aggregate_type=? AND aggregate_id=?`,
		tenant.Value, aggregate.Namespace, aggregate.Value).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("localstate: aggregate version: %w", err)
	}
	return version, nil
}

// VerifyAudit hydrates canonical event digests and verifies every tenant audit link.
func (s *Store) VerifyAudit(ctx context.Context, tenant contracts.Identifier) error {
	if s == nil || !validID(tenant, "tenant") {
		return ErrInvalidInput
	}
	var eventCount, auditCount uint64
	if err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM events WHERE tenant_id=?),
		(SELECT count(*) FROM audit_log WHERE tenant_id=?)`, tenant.Value, tenant.Value).Scan(&eventCount, &auditCount); err != nil {
		return fmt.Errorf("localstate: audit coverage: %w", err)
	}
	if eventCount != auditCount {
		return audit.ErrCorrupt
	}
	if eventCount == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.sequence,e.event_id,e.tenant_id,e.aggregate_type,e.aggregate_id,
		e.aggregate_version,e.command_id,e.payload_digest,e.occurred_at_ms,COALESCE(a.previous_digest,''),a.event_digest
		FROM audit_log a JOIN events e ON e.tenant_id=a.tenant_id AND e.sequence=a.sequence
		WHERE a.tenant_id=? ORDER BY a.sequence`, tenant.Value)
	if err != nil {
		return fmt.Errorf("localstate: audit query: %w", err)
	}
	defer rows.Close()
	entries := make([]audit.Entry, 0)
	for rows.Next() {
		entry := audit.Entry{}
		if err := rows.Scan(&entry.Metadata.Sequence, &entry.Metadata.EventID, &entry.Metadata.Tenant,
			&entry.Metadata.AggregateType, &entry.Metadata.AggregateID, &entry.Metadata.AggregateVersion,
			&entry.Metadata.CommandID, &entry.Metadata.PayloadDigest, &entry.Metadata.OccurredAtMs,
			&entry.Previous, &entry.Digest); err != nil {
			return fmt.Errorf("localstate: audit scan: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("localstate: audit rows: %w", err)
	}
	if uint64(len(entries)) != eventCount {
		return audit.ErrCorrupt
	}
	if err := audit.Verify(entries); err != nil {
		return err
	}
	var checkpointSequence uint64
	var checkpointDigest string
	if err := s.db.QueryRowContext(ctx, `SELECT event_sequence,audit_digest FROM checkpoints
		WHERE checkpoint_id=? AND tenant_id=?`, auditCheckpointID(tenant.Value), tenant.Value).
		Scan(&checkpointSequence, &checkpointDigest); err != nil {
		return audit.ErrCorrupt
	}
	head := entries[len(entries)-1]
	if checkpointSequence != head.Metadata.Sequence || checkpointDigest != head.Digest {
		return audit.ErrCorrupt
	}
	return nil
}

// ArtifactState returns the current local deletion state without exposing existence cross-tenant.
func (s *Store) ArtifactState(ctx context.Context, tenant, artifact contracts.Identifier, generation uint64) (string, error) {
	if s == nil || !validID(tenant, "tenant") || !validID(artifact, "artifact") || generation == 0 {
		return "", ErrInvalidInput
	}
	var state string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM artifact_manifests
		WHERE tenant_id=? AND artifact_id=? AND generation=?`, tenant.Value, artifact.Value, generation).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrInvalidInput
		}
		return "", fmt.Errorf("localstate: artifact state: %w", err)
	}
	return state, nil
}

func (s *Store) configure(ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("localstate: configure database: %w", err)
		}
	}
	return nil
}

func (s *Store) migrate(ctx context.Context, migrations []Migration) error {
	for _, migration := range migrations {
		applied, err := s.migrationApplied(ctx, migration.Version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		if err := s.applyMigration(ctx, migration); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrationApplied(ctx context.Context, version int) (bool, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master
		WHERE type='table' AND name='schema_migrations'`).Scan(&exists); err != nil {
		return false, fmt.Errorf("localstate: inspect migrations: %w", err)
	}
	if exists == 0 {
		return false, nil
	}
	var applied int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version=?`, version).
		Scan(&applied); err != nil {
		return false, fmt.Errorf("localstate: read migration: %w", err)
	}
	return applied == 1, nil
}

func (s *Store) applyMigration(ctx context.Context, migration Migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("localstate: begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
		return fmt.Errorf("localstate: apply migration %d: %w", migration.Version, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at_ms) VALUES (?,?)`,
		migration.Version, s.clock.NowUnixMilli()); err != nil {
		return fmt.Errorf("localstate: record migration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("localstate: commit migration: %w", err)
	}
	return nil
}

func validMigrations(migrations []Migration) bool {
	if len(migrations) == 0 {
		return false
	}
	for index, migration := range migrations {
		if migration.Version != index+1 || strings.TrimSpace(migration.SQL) == "" {
			return false
		}
	}
	return true
}

func validMutation(mutation Mutation) bool {
	command := mutation.Command
	if !validCommand(command) || mutation.Receipt.Status == "" || mutation.Receipt.ReasonCode == "" {
		return false
	}
	if len(mutation.Events) == 0 {
		return false
	}
	for _, mutationEvent := range mutation.Events {
		event := mutationEvent.Record
		if !validID(event.Event, "event") || event.Aggregate.Namespace == "" || event.Aggregate.Value == "" ||
			event.Version == 0 || mutationEvent.Type != event.Aggregate.Namespace || mutationEvent.SchemaVersion != 1 ||
			event.PayloadDigest.Algorithm != "sha256" || event.PayloadDigest.Hex == "" {
			return false
		}
	}
	return true
}

func validCommand(command contracts.CommandRecord) bool {
	return validID(command.Command, "command") && validID(command.Tenant, "tenant") &&
		validID(command.Principal, "principal") && validID(command.Session, "session") &&
		command.CommandType != "" && command.IdempotencyKey != "" && command.AuthenticatedDigest.Algorithm == "sha256" &&
		command.AuthenticatedDigest.Hex != "" && command.Fence > 0
}

func validID(identifier contracts.Identifier, namespace string) bool {
	return identifier.Namespace == namespace && identifier.Value != ""
}

func receiptID(commandID string) string { return "receipt:" + commandID }

func auditCheckpointID(tenant string) string { return "audit-head:" + tenant }

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// SystemClock provides production wall-clock values to the local ledger.
type SystemClock struct{}

// NowUnixMilli returns the current UTC Unix millisecond timestamp.
func (SystemClock) NowUnixMilli() int64 { return time.Now().UTC().UnixMilli() }
