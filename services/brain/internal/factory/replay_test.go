package factory

import (
	"context"
	"errors"
	"testing"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factory/mailbox"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factory/roster"
)

// TestReplayCollapsesWithRandomArtifactIDs pins the production vault shape:
// every Put mints a fresh artifact identity, so replay collapse must be
// digest-authoritative, never artifact-identity-authoritative.
func TestReplayCollapsesWithRandomArtifactIDs(t *testing.T) {
	fixture := newTestKernel(t)
	fixture.payloads.uniqueIDs = true
	runID := admitHappy(t, fixture, "replay-random-ids")
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)

	committed, err := fixture.kernel.CommitLeafResult(context.Background(), testIdentity(), runID, "leaf-a", 1, []byte("result-a"))
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := fixture.kernel.CommitLeafResult(context.Background(), testIdentity(), runID, "leaf-a", 1, []byte("result-a"))
	if err != nil {
		t.Fatalf("exact leaf-commit replay must collapse under random artifact identities: %v", err)
	}
	if !replayed.Replayed || replayed.ArtifactID != committed.ArtifactID || replayed.Digest != committed.Digest {
		t.Fatalf("replay = %#v, want original outcome %#v", replayed, committed)
	}

	input := MailboxMessageInput{
		MessageID: "msg-1", TaskID: "leaf-b", Kind: mailbox.KindBlocked, Payload: []byte("blocked"),
	}
	first, err := fixture.kernel.SendMailboxMessage(context.Background(), testIdentity(), runID, input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.kernel.SendMailboxMessage(context.Background(), testIdentity(), runID, input)
	if err != nil {
		t.Fatalf("exact duplicate delivery must collapse under random artifact identities: %v", err)
	}
	if !second.Replayed || second.Sequence != first.Sequence {
		t.Fatalf("duplicate delivery = %#v, want original sequence %d", second, first.Sequence)
	}
	if _, err := fixture.kernel.CommitLeafResult(context.Background(), testIdentity(), runID, "leaf-a", 1, []byte("result-b")); !errors.Is(err, roster.ErrResultConflict) {
		t.Fatalf("differing second commit error = %v, want roster.ErrResultConflict", err)
	}
	conflicting := input
	conflicting.Payload = []byte("different")
	if _, err := fixture.kernel.SendMailboxMessage(context.Background(), testIdentity(), runID, conflicting); !errors.Is(err, mailbox.ErrMessageConflict) {
		t.Fatalf("conflicting resend error = %v, want mailbox.ErrMessageConflict", err)
	}
}

// TestDeniedAttemptsLeaveNoVaultObjects proves every static denial resolves
// before payload staging: a denied attempt mints zero vault objects even when
// the vault assigns random identities.
func TestDeniedAttemptsLeaveNoVaultObjects(t *testing.T) {
	fixture := newTestKernel(t)
	fixture.payloads.uniqueIDs = true
	ctx := context.Background()

	// Stale base denies before staging.
	staleRequest := admitRequest(t, "orphan-stale-base", []LeafSpec{leafSpec("leaf-a", "src/go/modify-00.go")})
	staleRequest.Intent = makeIntent(t, "intent-stale-base", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if _, err := fixture.kernel.AdmitChangeIntent(ctx, staleRequest); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("stale base error = %v", err)
	}
	if got := fixture.payloads.putCount(); got != 0 {
		t.Fatalf("stale-base denial staged %d vault objects, want 0", got)
	}

	runID := admitHappy(t, fixture, "orphan-baseline")
	baseline := fixture.payloads.putCount()

	// Unknown run, unknown task, and not-running commits deny before staging.
	if _, err := fixture.kernel.CommitLeafResult(ctx, testIdentity(), runID, "leaf-a", 1, []byte("result")); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("commit before running error = %v", err)
	}
	if _, err := fixture.kernel.SendMailboxMessage(ctx, testIdentity(), "run-absent", MailboxMessageInput{
		MessageID: "msg-x", TaskID: "leaf-a", Kind: mailbox.KindQuestion, Payload: []byte("q"),
	}); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("send to unknown run error = %v", err)
	}
	if _, err := fixture.kernel.SendMailboxMessage(ctx, testIdentity(), runID, MailboxMessageInput{
		MessageID: "msg-y", TaskID: "node-absent", Kind: mailbox.KindQuestion, Payload: []byte("q"),
	}); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("send to unknown task error = %v", err)
	}

	// Stale fence denies before staging.
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)
	fixture.clock.now += 120_000
	if _, err := fixture.kernel.CommitLeafResult(ctx, testIdentity(), runID, "leaf-a", 1, []byte("result")); !errors.Is(err, roster.ErrStaleFence) {
		t.Fatalf("stale fence error = %v", err)
	}

	// Duplicate intent under a new key denies before staging.
	duplicate := admitRequest(t, "orphan-duplicate-new-key", []LeafSpec{
		leafSpec("leaf-a", "src/go/modify-00.go"),
		leafSpec("leaf-b", "src/go/modify-01.go"),
	})
	duplicate.Intent = makeIntent(t, "intent-orphan-baseline", testBaseOID)
	if _, err := fixture.kernel.AdmitChangeIntent(ctx, duplicate); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("duplicate intent error = %v", err)
	}

	// Cancel, then terminal-run sends and commits deny before staging.
	if _, err := fixture.kernel.CancelChangeRun(ctx, CancelRequest{
		Authenticated: testIdentity(), Caller: testCaller(), RunID: runID, IdempotencyKey: "orphan-cancel",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.kernel.SendMailboxMessage(ctx, testIdentity(), runID, MailboxMessageInput{
		MessageID: "msg-z", TaskID: "leaf-a", Kind: mailbox.KindQuestion, Payload: []byte("q"),
	}); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("send to cancelled run error = %v", err)
	}
	if _, err := fixture.kernel.CommitLeafResult(ctx, testIdentity(), runID, "leaf-a", 1, []byte("result")); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("commit to cancelled run error = %v", err)
	}

	if got := fixture.payloads.putCount(); got != baseline {
		t.Fatalf("denied attempts staged vault objects: puts %d, want %d", got, baseline)
	}
}
