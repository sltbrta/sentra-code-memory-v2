package factory

import (
	"context"
	"database/sql"
	"fmt"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factory/mailbox"
)

// MailboxMessageInput is one durable message the authenticated principal sends
// to one roster task of a live run. Payload bytes persist in the encrypted
// vault; the ledger holds only their digest.
type MailboxMessageInput struct {
	// MessageID is the replay-safe sender-authored message identity.
	MessageID string
	// TaskID identifies the roster task whose dense sequence extends.
	TaskID string
	// Kind is the bounded typed communication purpose.
	Kind mailbox.Kind
	// CorrelationID and CausationID carry causal ordering metadata and may be
	// empty for a root message.
	CorrelationID string
	CausationID   string
	// Payload is the bounded message body staged into the vault.
	Payload []byte
	// ExpiresAtMs prevents stale guidance from becoming authority; zero never
	// expires.
	ExpiresAtMs int64
}

// SendMailboxMessage durably sends one message to a roster task of a live
// run. Duplicate delivery collapses: an exact resend returns the original
// dense sequence with Replayed; a same-identity send with different payload
// conflicts as mailbox.ErrMessageConflict. Messages to unknown runs, unknown
// tasks, or terminal runs deny statically. Static denials and exact replays
// resolve before any payload is staged, so they never leave unreferenced
// vault objects.
func (k *Kernel) SendMailboxMessage(
	ctx context.Context, authenticated Identity, runID string, input MailboxMessageInput,
) (mailbox.SendResult, error) {
	if k == nil || ctx == nil || !validIdentity(authenticated) || runID == "" || len(input.Payload) == 0 {
		return mailbox.SendResult{}, ErrInvalidInput
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return mailbox.SendResult{}, ErrInvalidInput
	}
	if err := k.requireLiveRun(ctx, k.db, authenticated, runID); err != nil {
		return mailbox.SendResult{}, err
	}
	if err := k.requirePlanNode(ctx, k.db, authenticated, runID, input.TaskID); err != nil {
		return mailbox.SendResult{}, err
	}
	payloadDigest := digestBytes(input.Payload)
	existing, sequence, found, err := k.mailbox.Lookup(ctx, k.db, authenticated.Tenant, authenticated.Principal, runID, input.MessageID)
	if err != nil {
		return mailbox.SendResult{}, err
	}
	if found {
		if existing.TaskID == input.TaskID && existing.Kind == input.Kind && existing.PayloadDigest == payloadDigest {
			return mailbox.SendResult{Sequence: sequence, Replayed: true}, nil
		}
		return mailbox.SendResult{}, mailbox.ErrMessageConflict
	}
	staged, err := k.stagePayload(ctx, authenticated.Tenant, input.Payload)
	if err != nil {
		return mailbox.SendResult{}, err
	}
	tx, err := k.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return mailbox.SendResult{}, fmt.Errorf("factory: begin message send: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := k.requireLiveRun(ctx, tx, authenticated, runID); err != nil {
		return mailbox.SendResult{}, err
	}
	if err := k.requirePlanNode(ctx, tx, authenticated, runID, input.TaskID); err != nil {
		return mailbox.SendResult{}, err
	}
	result, err := k.mailbox.Send(ctx, tx, mailbox.Message{
		Tenant:            authenticated.Tenant,
		Principal:         authenticated.Principal,
		RunID:             runID,
		TaskID:            input.TaskID,
		MessageID:         input.MessageID,
		Kind:              input.Kind,
		CorrelationID:     input.CorrelationID,
		CausationID:       input.CausationID,
		SenderPrincipalID: authenticated.Principal,
		PayloadArtifactID: staged.artifactID,
		PayloadDigest:     staged.digestHex,
		ExpiresAtMs:       input.ExpiresAtMs,
	})
	if err != nil {
		return mailbox.SendResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return mailbox.SendResult{}, fmt.Errorf("factory: commit message send: %w", err)
	}
	return result, nil
}

// PendingMailboxMessages lists every unexpired message for one roster task of
// one admitted run in dense sequence order, joined with acknowledgement state.
func (k *Kernel) PendingMailboxMessages(
	ctx context.Context, authenticated Identity, runID, taskID string,
) ([]mailbox.Received, error) {
	if k == nil || ctx == nil || !validIdentity(authenticated) || runID == "" || taskID == "" {
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
	if err := k.requireNotCancelled(ctx, k.db, authenticated, runID); err != nil {
		return nil, err
	}
	return k.mailbox.Pending(ctx, k.db, authenticated.Tenant, authenticated.Principal, runID, taskID)
}

// AcknowledgeMailboxMessage commits one durable acknowledgement for a message
// of an admitted run; repeat acknowledgements are replay-safe.
func (k *Kernel) AcknowledgeMailboxMessage(
	ctx context.Context, authenticated Identity, runID, messageID string,
) (bool, error) {
	if k == nil || ctx == nil || !validIdentity(authenticated) || runID == "" || messageID == "" {
		return false, ErrInvalidInput
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return false, ErrInvalidInput
	}
	if _, found, err := lookupRun(ctx, k.db, authenticated, runID); err != nil {
		return false, err
	} else if !found {
		return false, ErrNotFoundOrDenied
	}
	tx, err := k.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return false, fmt.Errorf("factory: begin acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := k.requireNotCancelled(ctx, tx, authenticated, runID); err != nil {
		return false, err
	}
	replayed, err := k.mailbox.Acknowledge(ctx, tx, authenticated.Tenant, authenticated.Principal, runID, messageID)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("factory: commit acknowledgement: %w", err)
	}
	return replayed, nil
}

// requireLiveRun proves the run exists in scope and is not terminal; anything
// else shares the static denial.
func (k *Kernel) requireLiveRun(ctx context.Context, ex sqlExecutor, authenticated Identity, runID string) error {
	if _, found, err := lookupRun(ctx, ex, authenticated, runID); err != nil {
		return err
	} else if !found {
		return ErrNotFoundOrDenied
	}
	state, err := currentRunState(ctx, ex, authenticated, runID)
	if err != nil {
		return err
	}
	if terminalRunState(state) {
		return ErrNotFoundOrDenied
	}
	return nil
}

// requireNotCancelled proves the run was not revoked; delivery and
// acknowledgement of guidance stop at revocation exactly like the public
// reads.
func (k *Kernel) requireNotCancelled(ctx context.Context, ex sqlExecutor, authenticated Identity, runID string) error {
	state, err := currentRunState(ctx, ex, authenticated, runID)
	if err != nil {
		return err
	}
	if state == contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANCELLED {
		return ErrNotFoundOrDenied
	}
	return nil
}

// requirePlanNode proves the named node belongs to the run's plan.
func (k *Kernel) requirePlanNode(ctx context.Context, ex sqlExecutor, authenticated Identity, runID, nodeID string) error {
	var count int
	if err := ex.QueryRowContext(ctx, `SELECT count(*) FROM factory_plan_nodes
		WHERE tenant_id=? AND principal_id=? AND run_id=? AND node_id=?`,
		authenticated.Tenant, authenticated.Principal, runID, nodeID).Scan(&count); err != nil {
		return fmt.Errorf("factory: read plan node: %w", err)
	}
	if count != 1 {
		return ErrNotFoundOrDenied
	}
	return nil
}
