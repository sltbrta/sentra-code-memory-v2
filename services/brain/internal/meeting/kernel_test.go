package meeting

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate/schema"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	testTenant    = "tenant-a"
	testPrincipal = "principal-a"
	testSession   = "session-a"
)

type fakePayloads struct {
	mu      sync.Mutex
	objects map[string][]byte
	purged  map[string]bool
}

func newFakePayloads() *fakePayloads {
	return &fakePayloads{objects: make(map[string][]byte), purged: make(map[string]bool)}
}

func (f *fakePayloads) Put(_ context.Context, _ string, payload []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "artifact-" + digestBytes(payload)
	f.objects[id] = append([]byte(nil), payload...)
	return id, nil
}

func (f *fakePayloads) Get(_ context.Context, _, artifactID string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.purged[artifactID] {
		return nil, errors.New("purged")
	}
	payload, found := f.objects[artifactID]
	if !found {
		return nil, errors.New("missing")
	}
	return append([]byte(nil), payload...), nil
}

func (f *fakePayloads) Purge(_ context.Context, _, artifactID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purged[artifactID] = true
	delete(f.objects, artifactID)
	return nil
}

type testClock struct{ now int64 }

func (c *testClock) NowUnixMilli() int64 { return c.now }

func testIdentity() Identity {
	return Identity{Tenant: testTenant, Principal: testPrincipal, Session: testSession}
}

func openTestKernel(t *testing.T) (*Kernel, *fakePayloads) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "authority.db")
	authority, err := localstate.OpenWithMigrations(ctx, path, schema.Migrations(), localstate.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.OpenSession(ctx, contracts.MappedIdentityFact{
		Principal:   contracts.Identifier{Namespace: "principal", Value: testPrincipal},
		Tenant:      contracts.Identifier{Namespace: "tenant", Value: testTenant},
		Session:     contracts.Identifier{Namespace: "session", Value: testSession},
		Credentials: contracts.PeerCredentials{UID: 501, PID: 4242},
	}); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	payloads := newFakePayloads()
	kernel, err := Open(ctx, Config{
		DatabasePath: path,
		Payloads:     payloads,
		Clock:        &testClock{now: 1_700_000_000_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernel.Close() })
	return kernel, payloads
}

func validImportRequest(key string, partial bool) *contractsv1.ImportTranscriptRequest {
	return &contractsv1.ImportTranscriptRequest{
		Caller: &contractsv1.UntrustedMeetingCaller{
			RequestedPrincipal: &contractsv1.AuthenticatedPrincipalRef{
				PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: testPrincipal},
				TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: testTenant},
				SessionId:   &contractsv1.Identifier{Namespace: "session", Value: testSession},
			},
			RequestedSession: &contractsv1.Identifier{Namespace: "session", Value: testSession},
		},
		Title:       "Sprint planning",
		SourceScope: "fixture-meeting",
		StartedAt:   timestamppb.New(time.Unix(1_700_000_000, 0).UTC()),
		EndedAt:     timestamppb.New(time.Unix(1_700_000_900, 0).UTC()),
		Retention: &contractsv1.ImportRetentionPolicy{
			RawMediaRetention:   "7D",
			ScreenshotRetention: "OFF",
			DerivativeRetention: "30D",
		},
		ParticipantNotifyAcknowledged: true,
		Segments: []*contractsv1.TranscriptSegmentInput{
			{StartMillis: 0, EndMillis: 5_000, Text: "We will ship the billing service next week.", SpeakerLabel: "s1"},
			{StartMillis: 5_000, EndMillis: 12_000, Text: "Action item: notify the finance team.", SpeakerLabel: "s2"},
		},
		Partial:        partial,
		IdempotencyKey: key,
	}
}

func TestImportQueryRevokePurgeMatrix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernel, payloads := openTestKernel(t)

	imported, err := kernel.ImportTranscript(ctx, ImportCommand{
		Identity: testIdentity(), Request: validImportRequest("import-1", false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if imported.State != contractsv1.MeetingLifecycleState_MEETING_LIFECYCLE_STATE_READY {
		t.Fatalf("state = %v", imported.State)
	}
	// Exact duplicate import replays without a second payload.
	replay, err := kernel.ImportTranscript(ctx, ImportCommand{
		Identity: testIdentity(), Request: validImportRequest("import-1", false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if replay.MeetingSessionId.Value != imported.MeetingSessionId.Value {
		t.Fatalf("replay meeting = %s, want %s", replay.MeetingSessionId.Value, imported.MeetingSessionId.Value)
	}
	// Conflicting reuse of the key is non-disclosing denial.
	conflict := validImportRequest("import-1", false)
	conflict.Title = "Different"
	if _, err := kernel.ImportTranscript(ctx, ImportCommand{Identity: testIdentity(), Request: conflict}); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("conflict err = %v", err)
	}

	status, err := kernel.GetMeetingStatus(ctx, StatusCommand{
		Identity: testIdentity(), MeetingID: imported.MeetingSessionId.Value,
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.Title != "Sprint planning" || status.TimelineEndMillis != 12_000 {
		t.Fatalf("status = %#v", status)
	}

	answered, err := kernel.QueryMeeting(ctx, QueryCommand{
		Identity: testIdentity(),
		Request: &contractsv1.QueryMeetingRequest{
			Caller:           validImportRequest("x", false).Caller,
			MeetingSessionId: imported.MeetingSessionId,
			Query:            "billing service",
			TimeRange:        &contractsv1.MeetingTimeRange{StartMillis: 0, EndMillis: 6_000},
			IdempotencyKey:   "query-1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if answered.Answer.Status != contractsv1.MeetingAnswerStatus_MEETING_ANSWER_STATUS_ANSWERED &&
		answered.Answer.Status != contractsv1.MeetingAnswerStatus_MEETING_ANSWER_STATUS_PARTIAL {
		t.Fatalf("answer status = %v", answered.Answer.Status)
	}
	if len(answered.Answer.Claims) == 0 || answered.Answer.Claims[0].Citations[0].EndMillis != 5_000 {
		t.Fatalf("timed citation missing: %#v", answered.Answer)
	}
	if answered.Answer.FactualConsistency == nil ||
		answered.Answer.FactualConsistency.Status != contractsv1.FactualConsistencyStatus_FACTUAL_CONSISTENCY_STATUS_UNKNOWN ||
		answered.Answer.FactualConsistency.TotalClaimCount != uint32(len(answered.Answer.Claims)) {
		t.Fatalf("factual consistency = %+v", answered.Answer.FactualConsistency)
	}

	// Cross-principal is non-disclosing denial.
	other := testIdentity()
	other.Principal = "principal-b"
	if _, err := kernel.GetMeetingStatus(ctx, StatusCommand{Identity: other, MeetingID: imported.MeetingSessionId.Value}); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("cross-principal err = %v", err)
	}

	if _, err := kernel.RevokeMeeting(ctx, RevokeCommand{
		Identity: testIdentity(), MeetingID: imported.MeetingSessionId.Value, IdempotencyKey: "revoke-1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := kernel.QueryMeeting(ctx, QueryCommand{
		Identity: testIdentity(),
		Request: &contractsv1.QueryMeetingRequest{
			Caller:           validImportRequest("x", false).Caller,
			MeetingSessionId: imported.MeetingSessionId,
			Query:            "billing",
			IdempotencyKey:   "query-revoked",
		},
	}); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("revoked query err = %v", err)
	}

	purged, err := kernel.PurgeMeeting(ctx, PurgeCommand{
		Identity: testIdentity(), MeetingID: imported.MeetingSessionId.Value, IdempotencyKey: "purge-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if purged.State != contractsv1.MeetingLifecycleState_MEETING_LIFECYCLE_STATE_PURGED || purged.PurgedArtifactCount != 1 {
		t.Fatalf("purge = %#v", purged)
	}
	if len(payloads.objects) != 0 {
		t.Fatalf("payloads remained after purge: %#v", payloads.objects)
	}
	// Exact purge replay.
	if _, err := kernel.PurgeMeeting(ctx, PurgeCommand{
		Identity: testIdentity(), MeetingID: imported.MeetingSessionId.Value, IdempotencyKey: "purge-1",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPartialImportAndMissingSupport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernel, _ := openTestKernel(t)
	imported, err := kernel.ImportTranscript(ctx, ImportCommand{
		Identity: testIdentity(), Request: validImportRequest("import-partial", true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if imported.State != contractsv1.MeetingLifecycleState_MEETING_LIFECYCLE_STATE_PARTIAL {
		t.Fatalf("state = %v", imported.State)
	}
	answer, err := kernel.QueryMeeting(ctx, QueryCommand{
		Identity: testIdentity(),
		Request: &contractsv1.QueryMeetingRequest{
			Caller:           validImportRequest("x", false).Caller,
			MeetingSessionId: imported.MeetingSessionId,
			Query:            "billing",
			IdempotencyKey:   "query-partial",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Answer.Status != contractsv1.MeetingAnswerStatus_MEETING_ANSWER_STATUS_PARTIAL {
		t.Fatalf("status = %v", answer.Answer.Status)
	}
	if !strings.Contains(strings.Join(answer.Answer.DegradedReasons, ","), "partial_coverage") {
		t.Fatalf("reasons = %v", answer.Answer.DegradedReasons)
	}
	absent, err := kernel.QueryMeeting(ctx, QueryCommand{
		Identity: testIdentity(),
		Request: &contractsv1.QueryMeetingRequest{
			Caller:           validImportRequest("x", false).Caller,
			MeetingSessionId: imported.MeetingSessionId,
			Query:            "quantum fusion reactor",
			IdempotencyKey:   "query-absent",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if absent.Answer.Status != contractsv1.MeetingAnswerStatus_MEETING_ANSWER_STATUS_ABSTAINED {
		t.Fatalf("absent status = %v", absent.Answer.Status)
	}
	if absent.Answer.FactualConsistency == nil ||
		absent.Answer.FactualConsistency.Status != contractsv1.FactualConsistencyStatus_FACTUAL_CONSISTENCY_STATUS_ABSTAINED {
		t.Fatalf("absent factual consistency = %+v", absent.Answer.FactualConsistency)
	}
}
