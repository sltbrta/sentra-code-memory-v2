package meetingapi

import (
	"context"
	"testing"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeKernel struct {
	importResult *contractsv1.ImportTranscriptSuccess
	importErr    error
}

func (f *fakeKernel) ImportTranscript(context.Context, ImportCommand) (*contractsv1.ImportTranscriptSuccess, error) {
	return f.importResult, f.importErr
}
func (f *fakeKernel) MeetingStatus(context.Context, StatusCommand) (*contractsv1.GetMeetingStatusSuccess, error) {
	return nil, ErrUnknownMeeting
}
func (f *fakeKernel) QueryMeeting(context.Context, QueryCommand) (*contractsv1.QueryMeetingSuccess, error) {
	return nil, ErrUnknownMeeting
}
func (f *fakeKernel) RevokeMeeting(context.Context, RevokeCommand) (*contractsv1.RevokeMeetingSuccess, error) {
	return nil, ErrUnknownMeeting
}
func (f *fakeKernel) PurgeMeeting(context.Context, PurgeCommand) (*contractsv1.PurgeMeetingSuccess, error) {
	return nil, ErrUnknownMeeting
}

type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

func testPeer() localauthority.PeerContext {
	return localauthority.PeerContext{
		Identity: shared.MappedIdentityFact{
			Principal: shared.Identifier{Namespace: "principal", Value: "principal-a"},
			Tenant:    shared.Identifier{Namespace: "tenant", Value: "tenant-a"},
			Session:   shared.Identifier{Namespace: "session", Value: "session-a"},
		},
	}
}

func validCaller() *contractsv1.UntrustedMeetingCaller {
	return &contractsv1.UntrustedMeetingCaller{
		RequestedPrincipal: &contractsv1.AuthenticatedPrincipalRef{
			PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: "principal-a"},
			TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: "tenant-a"},
			SessionId:   &contractsv1.Identifier{Namespace: "session", Value: "session-a"},
		},
		RequestedSession: &contractsv1.Identifier{Namespace: "session", Value: "session-a"},
	}
}

func TestHandlerImportSuccessAndDenial(t *testing.T) {
	t.Parallel()
	handler, err := NewHandler(Config{
		Kernel: &fakeKernel{importResult: &contractsv1.ImportTranscriptSuccess{
			MeetingSessionId: &contractsv1.Identifier{Namespace: "meeting-session", Value: "m1"},
			State:            contractsv1.MeetingLifecycleState_MEETING_LIFECYCLE_STATE_READY,
			SegmentCount:     1,
			Retention: &contractsv1.ImportRetentionPolicy{
				RawMediaRetention: "7D", ScreenshotRetention: "OFF", DerivativeRetention: "30D",
			},
			NotifyReminderRecorded: true,
			SourceScope:            "fixture-meeting",
		}},
		Clock:               fixedClock{at: time.Unix(1_700_000_000, 0).UTC()},
		ConfigurationDigest: shared.Digest{Algorithm: "sha256", Hex: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := &contractsv1.ImportTranscriptRequest{
		Caller:      validCaller(),
		Title:       "Sprint planning",
		SourceScope: "fixture-meeting",
		StartedAt:   timestamppb.New(time.Unix(1_700_000_000, 0).UTC()),
		EndedAt:     timestamppb.New(time.Unix(1_700_000_900, 0).UTC()),
		Retention: &contractsv1.ImportRetentionPolicy{
			RawMediaRetention: "7D", ScreenshotRetention: "OFF", DerivativeRetention: "30D",
		},
		ParticipantNotifyAcknowledged: true,
		Segments: []*contractsv1.TranscriptSegmentInput{
			{StartMillis: 0, EndMillis: 1000, Text: "hello"},
		},
		IdempotencyKey: "k1",
	}
	response, err := handler.ImportTranscript(context.Background(), testPeer(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetSuccess() == nil || response.GetSuccess().MeetingSessionId.Value != "m1" {
		t.Fatalf("response = %#v", response)
	}

	denied, err := NewHandler(Config{
		Kernel:              &fakeKernel{importErr: ErrUnknownMeeting},
		Clock:               fixedClock{at: time.Unix(1_700_000_000, 0).UTC()},
		ConfigurationDigest: shared.Digest{Algorithm: "sha256", Hex: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	})
	if err != nil {
		t.Fatal(err)
	}
	denial, err := denied.ImportTranscript(context.Background(), testPeer(), request)
	if err != nil {
		t.Fatal(err)
	}
	if denial.GetError() == nil || denial.GetError().Code != "not_found_or_denied" {
		t.Fatalf("denial = %#v", denial)
	}
}

func TestHandlerRejectsPeerMismatchAndMissingNotify(t *testing.T) {
	t.Parallel()
	handler, err := NewHandler(Config{
		Kernel:              &fakeKernel{},
		Clock:               fixedClock{at: time.Unix(1_700_000_000, 0).UTC()},
		ConfigurationDigest: shared.Digest{Algorithm: "sha256", Hex: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := &contractsv1.ImportTranscriptRequest{
		Caller:      validCaller(),
		Title:       "Sprint planning",
		SourceScope: "fixture-meeting",
		StartedAt:   timestamppb.New(time.Unix(1_700_000_000, 0).UTC()),
		EndedAt:     timestamppb.New(time.Unix(1_700_000_900, 0).UTC()),
		Retention: &contractsv1.ImportRetentionPolicy{
			RawMediaRetention: "7D", ScreenshotRetention: "OFF", DerivativeRetention: "30D",
		},
		ParticipantNotifyAcknowledged: false,
		Segments: []*contractsv1.TranscriptSegmentInput{
			{StartMillis: 0, EndMillis: 1000, Text: "hello"},
		},
		IdempotencyKey: "k1",
	}
	if _, err := handler.ImportTranscript(context.Background(), testPeer(), request); err != ErrInvalidRequest {
		t.Fatalf("missing notify err = %v", err)
	}
	request.ParticipantNotifyAcknowledged = true
	request.Caller.RequestedPrincipal.PrincipalId.Value = "other"
	if _, err := handler.ImportTranscript(context.Background(), testPeer(), request); err != ErrRequestDenied {
		t.Fatalf("mismatch err = %v", err)
	}
}
