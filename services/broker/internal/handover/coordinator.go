package handover

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Executor runs a non-effectful leaf under a realm.
type Executor interface {
	Realm() RealmKind
	Run(ctx context.Context, checkpoint Checkpoint, fence uint64, attempt int) (string, error)
	Cleanup(ctx context.Context, taskID string) CleanupReceipt
}

// Coordinator owns fence CAS, handover state, and exactly-once completion.
type Coordinator struct {
	mu           sync.Mutex
	taskID       string
	fence        uint64
	state        HandoverState
	checkpoint   *Checkpoint
	completion   *CompletionReceipt
	revoked      bool
	cleaning     bool
	targetActive bool
	allowed      map[RealmKind]bool
	attempts     int
	sourceExec   Executor
	targetExec   Executor
	cleanups     []CleanupReceipt
}

// NewCoordinator starts a task under the local realm with fence 1.
func NewCoordinator(taskID string, source, target Executor) *Coordinator {
	return &Coordinator{
		taskID:     taskID,
		fence:      1,
		state:      StateRunningSource,
		allowed:    map[RealmKind]bool{RealmLocal: true, RealmModal: true},
		sourceExec: source,
		targetExec: target,
	}
}

// CurrentFence returns the sole committable fence.
func (c *Coordinator) CurrentFence() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fence
}

// State returns the handover lifecycle state.
func (c *Coordinator) State() HandoverState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Checkpoint prepares a portable checkpoint under the source fence.
func (c *Coordinator) Checkpoint(ctx context.Context, bundleDigest string, steps []string) (Checkpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.revoked {
		return Checkpoint{}, ErrRevoked
	}
	if c.state != StateRunningSource && c.state != StateQuiescing {
		return Checkpoint{}, ErrRejected
	}
	c.state = StateQuiescing
	cp := SealCheckpoint(Checkpoint{
		TaskID:            c.taskID,
		WorkflowVersion:   "wf-1",
		BundleDigest:      bundleDigest,
		MailboxCursor:     1,
		CompletedSteps:    append([]string(nil), steps...),
		CapabilityReqs:    []string{"compute.pure"},
		CreatedUnderFence: c.fence,
		CreatedAt:         time.Now().UTC(),
	})
	c.checkpoint = &cp
	c.state = StatePrepared
	return cp, nil
}

// Transfer advances the fence and hands to the target realm.
func (c *Coordinator) Transfer(ctx context.Context, target RealmKind) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.revoked {
		return ErrRevoked
	}
	if c.state != StatePrepared {
		return ErrRejected
	}
	if !c.allowed[target] {
		return ErrIncompatibleRealm
	}
	if target != RealmModal {
		// V1 exit is specifically local→Modal.
		return ErrIncompatibleRealm
	}
	if c.checkpoint == nil {
		return ErrRejected
	}
	// Atomic fence advance: source permanently loses commit rights.
	c.fence++
	c.state = StateFenceAdvanced
	return nil
}

// ResumeTarget runs the non-effectful leaf on Modal with at-least-once retry.
// Exactly one completion may commit under the current fence.
func (c *Coordinator) ResumeTarget(ctx context.Context) (CompletionReceipt, error) {
	c.mu.Lock()
	if c.revoked {
		c.mu.Unlock()
		return CompletionReceipt{}, ErrRevoked
	}
	if c.cleaning {
		c.mu.Unlock()
		return CompletionReceipt{}, ErrRejected
	}
	if c.completion != nil {
		// Exactly-once: return original receipt.
		dup := *c.completion
		c.mu.Unlock()
		return dup, ErrDuplicate
	}
	if c.state != StateFenceAdvanced && c.state != StateReplaying && c.state != StateRunningTarget && c.state != StatePaused {
		c.mu.Unlock()
		return CompletionReceipt{}, ErrRejected
	}
	if c.checkpoint == nil {
		c.mu.Unlock()
		return CompletionReceipt{}, ErrRejected
	}
	cp := *c.checkpoint
	fence := c.fence
	c.state = StateReplaying
	c.attempts++
	attempt := c.attempts
	exec := c.targetExec
	c.state = StateRunningTarget
	c.targetActive = true
	c.mu.Unlock()

	result, err := exec.Run(ctx, cp, fence, attempt)
	c.mu.Lock()
	c.targetActive = false
	if err != nil {
		// Never regress a terminal/revoked state to paused.
		if !c.revoked && c.completion == nil && c.state != StateCompleted && c.state != StateCancelled {
			c.state = StatePaused
		}
		c.mu.Unlock()
		return CompletionReceipt{}, err
	}
	c.mu.Unlock()

	return c.commit(fence, exec.Realm(), attempt, result)
}

// CommitUnderFence attempts a completion under an explicit fence (stale race test).
func (c *Coordinator) CommitUnderFence(fence uint64, realm RealmKind, result string) (CompletionReceipt, error) {
	return c.commit(fence, realm, 0, result)
}

func (c *Coordinator) commit(fence uint64, realm RealmKind, attempt int, result string) (CompletionReceipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.revoked {
		return CompletionReceipt{}, ErrRevoked
	}
	if fence != c.fence {
		return CompletionReceipt{}, ErrStaleFence
	}
	if c.completion != nil {
		return *c.completion, ErrDuplicate
	}
	// Executors return the pure SHA-256 hex digest; do not re-hash.
	receipt := CompletionReceipt{
		TaskID:       c.taskID,
		Fence:        fence,
		Realm:        realm,
		Attempt:      attempt,
		ResultDigest: result,
		CommittedAt:  time.Now().UTC(),
	}
	c.completion = &receipt
	c.state = StateCompleted
	return receipt, nil
}

// Revoke stops hydration/effects and cancels the handover.
func (c *Coordinator) Revoke() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revoked = true
	c.state = StateCancelled
}

// Completion returns the committed receipt if any.
func (c *Coordinator) Completion() *CompletionReceipt {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.completion == nil {
		return nil
	}
	cp := *c.completion
	return &cp
}

// Cleanup tears down source and target transient resources.
// Blocks new ResumeTarget and waits until any in-flight target run finishes
// so cleanup cannot race with resource creation.
func (c *Coordinator) Cleanup(ctx context.Context) CleanupReceipt {
	c.mu.Lock()
	c.cleaning = true
	for c.targetActive {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return CleanupReceipt{Complete: false}
		case <-time.After(5 * time.Millisecond):
		}
		c.mu.Lock()
	}
	taskID := c.taskID
	source, target := c.sourceExec, c.targetExec
	c.mu.Unlock()
	s := source.Cleanup(ctx, taskID)
	t := target.Cleanup(ctx, taskID)
	merged := CleanupReceipt{
		WorkspacesRemoved: append(s.WorkspacesRemoved, t.WorkspacesRemoved...),
		ModalAppsClosed:   append(s.ModalAppsClosed, t.ModalAppsClosed...),
		BundlesPurged:     append(s.BundlesPurged, t.BundlesPurged...),
		Complete:          s.Complete && t.Complete,
	}
	c.mu.Lock()
	c.cleanups = append(c.cleanups, merged)
	c.mu.Unlock()
	return merged
}

// Status is an operator/TUI projection of handover progress.
func (c *Coordinator) Status() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]any{
		"task_id":  c.taskID,
		"fence":    c.fence,
		"state":    string(c.state),
		"attempts": c.attempts,
		"revoked":  c.revoked,
	}
	if c.checkpoint != nil {
		out["checkpoint_digest"] = c.checkpoint.CheckpointDigest
		out["mailbox_cursor"] = c.checkpoint.MailboxCursor
	}
	if c.completion != nil {
		out["completion_digest"] = c.completion.ResultDigest
		out["completion_realm"] = string(c.completion.Realm)
	}
	return out
}

// String returns a stable debug identity.
func (c *Coordinator) String() string {
	return fmt.Sprintf("handover(%s fence=%d state=%s)", c.taskID, c.CurrentFence(), c.State())
}
