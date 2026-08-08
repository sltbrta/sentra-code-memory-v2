package mailbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

var (
	// ErrInvalidInput reports missing or malformed message facts.
	ErrInvalidInput = errors.New("factory/mailbox: invalid input")
	// ErrMessageConflict reports reuse of a message identity with a different
	// payload digest; canonical state is never mutated by a conflict.
	ErrMessageConflict = errors.New("factory/mailbox: message conflict")
	// ErrUnknownMessage reports an acknowledgement for an absent message.
	ErrUnknownMessage = errors.New("factory/mailbox: unknown message")
)

// Kind is the bounded typed communication vocabulary mirrored from the frozen
// MailboxKind contract enumeration.
type Kind string

// The bounded mailbox kinds; every value names the proto enumeration member of
// the same name.
const (
	KindQuestion        Kind = "QUESTION"
	KindAnswer          Kind = "ANSWER"
	KindFinding         Kind = "FINDING"
	KindEvidence        Kind = "EVIDENCE"
	KindDependencyReady Kind = "DEPENDENCY_READY"
	KindBlocked         Kind = "BLOCKED"
	KindHandover        Kind = "HANDOVER"
	KindReviewRequest   Kind = "REVIEW_REQUEST"
	KindReviewResult    Kind = "REVIEW_RESULT"
	KindCorrection      Kind = "CORRECTION"
	KindCancellation    Kind = "CANCELLATION"
)

func validKind(kind Kind) bool {
	switch kind {
	case KindQuestion, KindAnswer, KindFinding, KindEvidence, KindDependencyReady,
		KindBlocked, KindHandover, KindReviewRequest, KindReviewResult, KindCorrection, KindCancellation:
		return true
	}
	return false
}

// Executor is the narrow database handle shared by the composing kernel: it is
// satisfied by both *sql.DB and *sql.Tx, so message facts commit inside the
// caller's transaction.
type Executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Message is one durable mailbox message. Payload bytes live in the encrypted
// vault under PayloadArtifactID; only the canonical digest enters the ledger.
type Message struct {
	// Tenant and Principal scope the owning run.
	Tenant    string
	Principal string
	// RunID identifies the owning run; TaskID identifies the roster task whose
	// dense sequence this message extends.
	RunID  string
	TaskID string
	// MessageID is the replay-safe sender-authored message identity.
	MessageID string
	// Kind is the bounded typed communication purpose.
	Kind Kind
	// CorrelationID and CausationID carry causal ordering metadata; both may be
	// empty for a root message.
	CorrelationID string
	CausationID   string
	// SenderPrincipalID names the authenticated sending worker or human.
	SenderPrincipalID string
	// PayloadArtifactID and PayloadDigest pin the encrypted payload.
	PayloadArtifactID string
	PayloadDigest     string
	// ExpiresAtMs prevents stale guidance from becoming authority; zero means
	// the message never expires.
	ExpiresAtMs int64
}

// SendResult is the canonical disposition of one Send.
type SendResult struct {
	// Sequence is the dense per-task position assigned to the message.
	Sequence uint64
	// Replayed reports that an identical message was already canonical and its
	// original sequence was returned without a new transition.
	Replayed bool
}

// Received is one delivered message joined with its acknowledgement state.
type Received struct {
	// Message is the canonical message fact with its assigned sequence.
	Message Message
	// Sequence is the dense per-task position.
	Sequence uint64
	// SentAtMs is the canonical send instant.
	SentAtMs int64
	// AcknowledgedAtMs is positive once a durable acknowledgement committed.
	AcknowledgedAtMs int64
}

// Store derives ordering, dedupe, and acknowledgement from the migration 005
// insert-only facts. It is safe for concurrent use; callers serialize writers
// through the composing kernel's mutex and single connection.
type Store struct {
	clock contracts.Clock
}

// New binds the wall clock used for send and expiry evaluation.
func New(clock contracts.Clock) (*Store, error) {
	if clock == nil {
		return nil, ErrInvalidInput
	}
	return &Store{clock: clock}, nil
}

// Send appends one message at the next dense per-(run, task) sequence. An exact
// resend — same tenant, principal, run, message identity, kind, task, and
// payload digest — collapses to the original sequence with Replayed; the schema
// trigger independently enforces the dense append. A resend with a different
// payload digest or kind is ErrMessageConflict and mutates nothing.
func (s *Store) Send(ctx context.Context, ex Executor, message Message) (SendResult, error) {
	if s == nil || ctx == nil || ex == nil || !validMessage(message) {
		return SendResult{}, ErrInvalidInput
	}
	existing, found, err := s.lookup(ctx, ex, message.Tenant, message.Principal, message.RunID, message.MessageID)
	if err != nil {
		return SendResult{}, err
	}
	if found {
		// The canonical digest is the authoritative replay match: artifact
		// identities are vault-assigned and may differ across exact retries.
		if existing.TaskID == message.TaskID && existing.Kind == message.Kind &&
			existing.PayloadDigest == message.PayloadDigest {
			return SendResult{Sequence: existing.Sequence, Replayed: true}, nil
		}
		return SendResult{}, ErrMessageConflict
	}
	var sequence uint64
	if err := ex.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM factory_mailbox_messages
		WHERE tenant_id=? AND principal_id=? AND run_id=? AND task_id=?`,
		message.Tenant, message.Principal, message.RunID, message.TaskID).Scan(&sequence); err != nil {
		return SendResult{}, fmt.Errorf("factory/mailbox: read task sequence: %w", err)
	}
	if _, err := ex.ExecContext(ctx, `INSERT INTO factory_mailbox_messages
		(tenant_id,principal_id,run_id,message_id,task_id,kind,sequence,correlation_id,causation_id,
		 sender_principal_id,payload_artifact_id,payload_digest,expires_at_ms,sent_at_ms)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		message.Tenant, message.Principal, message.RunID, message.MessageID, message.TaskID,
		string(message.Kind), sequence, message.CorrelationID, message.CausationID,
		message.SenderPrincipalID, message.PayloadArtifactID, message.PayloadDigest,
		nullIfZero(message.ExpiresAtMs), s.clock.NowUnixMilli()); err != nil {
		return SendResult{}, fmt.Errorf("factory/mailbox: commit message: %w", err)
	}
	return SendResult{Sequence: sequence}, nil
}

// Lookup returns one canonical message with its dense sequence. It is a
// read-only dedupe check: callers use it to collapse exact retries before
// staging a new payload.
func (s *Store) Lookup(ctx context.Context, ex Executor, tenant, principal, runID, messageID string) (Message, uint64, bool, error) {
	if s == nil || ctx == nil || ex == nil || !validID(tenant) || !validID(principal) ||
		!validID(runID) || !validID(messageID) {
		return Message{}, 0, false, ErrInvalidInput
	}
	row, found, err := s.lookup(ctx, ex, tenant, principal, runID, messageID)
	if err != nil || !found {
		return Message{}, 0, false, err
	}
	message := Message{
		Tenant: tenant, Principal: principal, RunID: runID, MessageID: messageID,
		TaskID: row.TaskID, Kind: row.Kind,
		PayloadArtifactID: row.PayloadArtifactID, PayloadDigest: row.PayloadDigest,
	}
	return message, row.Sequence, true, nil
}

// Pending lists every unexpired message for one task in dense sequence order,
// joined with acknowledgement state. Expired messages stay canonical but are
// never delivered as current guidance.
func (s *Store) Pending(ctx context.Context, ex Executor, tenant, principal, runID, taskID string) ([]Received, error) {
	if s == nil || ctx == nil || ex == nil || !validID(tenant) || !validID(principal) ||
		!validID(runID) || !validID(taskID) {
		return nil, ErrInvalidInput
	}
	rows, err := ex.QueryContext(ctx, `SELECT m.message_id,m.kind,m.sequence,m.correlation_id,m.causation_id,
		m.sender_principal_id,m.payload_artifact_id,m.payload_digest,COALESCE(m.expires_at_ms,0),m.sent_at_ms,
		COALESCE(a.acked_at_ms,0)
		FROM factory_mailbox_messages m
		LEFT JOIN factory_mailbox_acks a
		ON a.tenant_id=m.tenant_id AND a.principal_id=m.principal_id AND a.run_id=m.run_id
		AND a.message_id=m.message_id
		WHERE m.tenant_id=? AND m.principal_id=? AND m.run_id=? AND m.task_id=?
		ORDER BY m.sequence`,
		tenant, principal, runID, taskID)
	if err != nil {
		return nil, fmt.Errorf("factory/mailbox: pending query: %w", err)
	}
	defer rows.Close()
	now := s.clock.NowUnixMilli()
	received := make([]Received, 0)
	for rows.Next() {
		var one Received
		var kind string
		if err := rows.Scan(&one.Message.MessageID, &kind, &one.Sequence, &one.Message.CorrelationID,
			&one.Message.CausationID, &one.Message.SenderPrincipalID, &one.Message.PayloadArtifactID,
			&one.Message.PayloadDigest, &one.Message.ExpiresAtMs, &one.SentAtMs, &one.AcknowledgedAtMs); err != nil {
			return nil, fmt.Errorf("factory/mailbox: pending scan: %w", err)
		}
		one.Message.Tenant = tenant
		one.Message.Principal = principal
		one.Message.RunID = runID
		one.Message.TaskID = taskID
		one.Message.Kind = Kind(kind)
		if one.Message.ExpiresAtMs > 0 && now >= one.Message.ExpiresAtMs {
			continue
		}
		received = append(received, one)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("factory/mailbox: pending rows: %w", err)
	}
	return received, nil
}

// Acknowledge commits one durable acknowledgement. Repeat acknowledgements of
// the same message are replay-safe and return Replayed set.
func (s *Store) Acknowledge(ctx context.Context, ex Executor, tenant, principal, runID, messageID string) (replayed bool, err error) {
	if s == nil || ctx == nil || ex == nil || !validID(tenant) || !validID(principal) ||
		!validID(runID) || !validID(messageID) {
		return false, ErrInvalidInput
	}
	if _, found, err := s.lookup(ctx, ex, tenant, principal, runID, messageID); err != nil {
		return false, err
	} else if !found {
		return false, ErrUnknownMessage
	}
	var acked int
	if err := ex.QueryRowContext(ctx, `SELECT count(*) FROM factory_mailbox_acks
		WHERE tenant_id=? AND principal_id=? AND run_id=? AND message_id=?`,
		tenant, principal, runID, messageID).Scan(&acked); err != nil {
		return false, fmt.Errorf("factory/mailbox: read acknowledgement: %w", err)
	}
	if acked == 1 {
		return true, nil
	}
	if _, err := ex.ExecContext(ctx, `INSERT INTO factory_mailbox_acks
		(tenant_id,principal_id,run_id,message_id,acked_at_ms) VALUES (?,?,?,?,?)`,
		tenant, principal, runID, messageID, s.clock.NowUnixMilli()); err != nil {
		return false, fmt.Errorf("factory/mailbox: commit acknowledgement: %w", err)
	}
	return false, nil
}

type lookupRow struct {
	TaskID            string
	Kind              Kind
	Sequence          uint64
	PayloadArtifactID string
	PayloadDigest     string
}

func (s *Store) lookup(ctx context.Context, ex Executor, tenant, principal, runID, messageID string) (lookupRow, bool, error) {
	row := lookupRow{}
	err := ex.QueryRowContext(ctx, `SELECT task_id,kind,sequence,payload_artifact_id,payload_digest
		FROM factory_mailbox_messages
		WHERE tenant_id=? AND principal_id=? AND run_id=? AND message_id=?`,
		tenant, principal, runID, messageID).
		Scan(&row.TaskID, (*string)(&row.Kind), &row.Sequence, &row.PayloadArtifactID, &row.PayloadDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return lookupRow{}, false, nil
	}
	if err != nil {
		return lookupRow{}, false, fmt.Errorf("factory/mailbox: read message: %w", err)
	}
	return row, true, nil
}

func validMessage(message Message) bool {
	return validID(message.Tenant) && validID(message.Principal) && validID(message.RunID) &&
		validID(message.TaskID) && validID(message.MessageID) && validKind(message.Kind) &&
		validID(message.SenderPrincipalID) && validID(message.PayloadArtifactID) &&
		validHexDigest(message.PayloadDigest) && message.ExpiresAtMs >= 0 &&
		len(message.CorrelationID) <= 512 && len(message.CausationID) <= 512
}

func validID(value string) bool {
	if value == "" || len(value) > 512 {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func nullIfZero(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}
