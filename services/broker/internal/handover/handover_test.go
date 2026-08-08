package handover_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/handover"
)

func newCoord() (*handover.Coordinator, *handover.LocalExecutor, *handover.ModalExecutor) {
	local := handover.NewLocalExecutor()
	modal := handover.NewModalExecutor()
	return handover.NewCoordinator("task-1", local, modal), local, modal
}

func TestLocalToModalExactlyOnce(t *testing.T) {
	ctx := context.Background()
	coord, _, _ := newCoord()
	cp, err := coord.Checkpoint(ctx, "bundle-abc", []string{"step-a"})
	if err != nil {
		t.Fatal(err)
	}
	if cp.CheckpointDigest == "" || cp.MailboxCursor != 1 {
		t.Fatalf("checkpoint = %+v", cp)
	}
	sourceFence := coord.CurrentFence()
	if err := coord.Transfer(ctx, handover.RealmModal); err != nil {
		t.Fatal(err)
	}
	if coord.CurrentFence() != sourceFence+1 {
		t.Fatalf("fence = %d", coord.CurrentFence())
	}
	// Stale source fence cannot commit.
	if _, err := coord.CommitUnderFence(sourceFence, handover.RealmLocal, "stale"); !errors.Is(err, handover.ErrStaleFence) {
		t.Fatalf("stale = %v", err)
	}
	receipt, err := coord.ResumeTarget(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Realm != handover.RealmModal || receipt.Fence != coord.CurrentFence() {
		t.Fatalf("receipt = %+v", receipt)
	}
	// Duplicate resume returns original completion (exactly-once).
	dup, err := coord.ResumeTarget(ctx)
	if !errors.Is(err, handover.ErrDuplicate) {
		t.Fatalf("dup err = %v", err)
	}
	if dup.ResultDigest != receipt.ResultDigest {
		t.Fatalf("dup digest mismatch")
	}
	cleanup := coord.Cleanup(ctx)
	if !cleanup.Complete || len(cleanup.ModalAppsClosed) == 0 {
		t.Fatalf("cleanup = %+v", cleanup)
	}
}

func TestStaleFenceRaceAndIncompatibleRealm(t *testing.T) {
	ctx := context.Background()
	coord, _, _ := newCoord()
	if _, err := coord.Checkpoint(ctx, "b", nil); err != nil {
		t.Fatal(err)
	}
	if err := coord.Transfer(ctx, handover.RealmLocal); !errors.Is(err, handover.ErrIncompatibleRealm) {
		t.Fatalf("local target = %v", err)
	}
	if err := coord.Transfer(ctx, handover.RealmModal); err != nil {
		t.Fatal(err)
	}
	// Two racers: only current fence wins.
	fence := coord.CurrentFence()
	if _, err := coord.CommitUnderFence(fence+9, handover.RealmModal, "x"); !errors.Is(err, handover.ErrStaleFence) {
		t.Fatalf("future fence = %v", err)
	}
	if _, err := coord.CommitUnderFence(fence, handover.RealmModal, "ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.CommitUnderFence(fence, handover.RealmModal, "again"); !errors.Is(err, handover.ErrDuplicate) {
		t.Fatalf("second commit = %v", err)
	}
}

func TestRevokeAndRecovery(t *testing.T) {
	ctx := context.Background()
	coord, _, modal := newCoord()
	if _, err := coord.Checkpoint(ctx, "b", []string{"s1"}); err != nil {
		t.Fatal(err)
	}
	if err := coord.Transfer(ctx, handover.RealmModal); err != nil {
		t.Fatal(err)
	}
	modal.FailNext()
	if _, err := coord.ResumeTarget(ctx); err == nil {
		t.Fatal("expected transient failure")
	}
	if coord.State() != handover.StatePaused {
		t.Fatalf("state = %s", coord.State())
	}
	// Recovery under same fence succeeds once.
	if _, err := coord.ResumeTarget(ctx); err != nil {
		t.Fatal(err)
	}

	// Revoke path.
	coord2, _, _ := newCoord()
	if _, err := coord2.Checkpoint(ctx, "b2", nil); err != nil {
		t.Fatal(err)
	}
	coord2.Revoke()
	if err := coord2.Transfer(ctx, handover.RealmModal); !errors.Is(err, handover.ErrRevoked) {
		t.Fatalf("transfer after revoke = %v", err)
	}
}

func TestStatusSurface(t *testing.T) {
	ctx := context.Background()
	coord, _, _ := newCoord()
	if _, err := coord.Checkpoint(ctx, "bundle", nil); err != nil {
		t.Fatal(err)
	}
	if err := coord.Transfer(ctx, handover.RealmModal); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.ResumeTarget(ctx); err != nil {
		t.Fatal(err)
	}
	status := coord.Status()
	if status["state"] != string(handover.StateCompleted) {
		t.Fatalf("status = %+v", status)
	}
	if status["completion_realm"] != string(handover.RealmModal) {
		t.Fatalf("realm = %v", status["completion_realm"])
	}
}
