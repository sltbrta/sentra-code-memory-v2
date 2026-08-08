package factory

import (
	"context"
	"errors"
	"testing"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factory/mailbox"
)

func TestMailboxDuplicateDeliveryCollapsesAcrossKernelBoundary(t *testing.T) {
	fixture := newTestKernel(t)
	runID := admitHappy(t, fixture, "mailbox-dedupe")
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)

	input := MailboxMessageInput{
		MessageID: "msg-1", TaskID: "leaf-a", Kind: mailbox.KindBlocked, Payload: []byte("blocked on leaf-b"),
	}
	first, err := fixture.kernel.SendMailboxMessage(context.Background(), testIdentity(), runID, input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Sequence != 1 || first.Replayed {
		t.Fatalf("first send = %#v", first)
	}
	second, err := fixture.kernel.SendMailboxMessage(context.Background(), testIdentity(), runID, input)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || second.Sequence != first.Sequence {
		t.Fatalf("duplicate delivery did not collapse: %#v", second)
	}
	conflicting := input
	conflicting.Payload = []byte("different payload")
	if _, err := fixture.kernel.SendMailboxMessage(context.Background(), testIdentity(), runID, conflicting); !errors.Is(err, mailbox.ErrMessageConflict) {
		t.Fatalf("conflicting reuse error = %v, want mailbox.ErrMessageConflict", err)
	}
	pending, err := fixture.kernel.PendingMailboxMessages(context.Background(), testIdentity(), runID, "leaf-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Message.Kind != mailbox.KindBlocked {
		t.Fatalf("pending = %#v", pending)
	}
	if replayed, err := fixture.kernel.AcknowledgeMailboxMessage(context.Background(), testIdentity(), runID, "msg-1"); err != nil || replayed {
		t.Fatalf("ack = %v %v", replayed, err)
	}
	if replayed, err := fixture.kernel.AcknowledgeMailboxMessage(context.Background(), testIdentity(), runID, "msg-1"); err != nil || !replayed {
		t.Fatalf("repeat ack = %v %v, want replayed", replayed, err)
	}
}

func TestMailboxDeniesTerminalRunsAndUnknownTasks(t *testing.T) {
	fixture := newTestKernel(t)
	runID := admitHappy(t, fixture, "mailbox-denials")
	input := MailboxMessageInput{
		MessageID: "msg-1", TaskID: "leaf-absent", Kind: mailbox.KindQuestion, Payload: []byte("question"),
	}
	if _, err := fixture.kernel.SendMailboxMessage(context.Background(), testIdentity(), runID, input); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("unknown task error = %v, want ErrNotFoundOrDenied", err)
	}
	if _, err := fixture.kernel.CancelChangeRun(context.Background(), CancelRequest{
		Authenticated: testIdentity(), Caller: testCaller(), RunID: runID, IdempotencyKey: "cancel-mailbox",
	}); err != nil {
		t.Fatal(err)
	}
	input.TaskID = "leaf-a"
	if _, err := fixture.kernel.SendMailboxMessage(context.Background(), testIdentity(), runID, input); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("terminal run error = %v, want ErrNotFoundOrDenied", err)
	}
	other := Identity{Tenant: testTenant, Principal: "principal-2", Session: testSession}
	if _, err := fixture.kernel.PendingMailboxMessages(context.Background(), other, runID, "leaf-a"); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("cross-principal pending error = %v, want ErrNotFoundOrDenied", err)
	}
}
