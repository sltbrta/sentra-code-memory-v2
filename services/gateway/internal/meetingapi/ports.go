package meetingapi

import (
	"context"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

// Kernel is the bounded Stage 07 meeting-authority port.
type Kernel interface {
	ImportTranscript(ctx context.Context, command ImportCommand) (*contractsv1.ImportTranscriptSuccess, error)
	MeetingStatus(ctx context.Context, command StatusCommand) (*contractsv1.GetMeetingStatusSuccess, error)
	QueryMeeting(ctx context.Context, command QueryCommand) (*contractsv1.QueryMeetingSuccess, error)
	RevokeMeeting(ctx context.Context, command RevokeCommand) (*contractsv1.RevokeMeetingSuccess, error)
	PurgeMeeting(ctx context.Context, command PurgeCommand) (*contractsv1.PurgeMeetingSuccess, error)
}

// Clock supplies receipt time without ambient time.Now.
type Clock interface {
	Now() time.Time
}
