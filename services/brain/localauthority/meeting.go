package localauthority

import (
	"context"
	"errors"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/meeting"
)

// Public aliases over the bounded Stage 07 meeting kernel shapes. They let the
// composing gateway command wire the production meeting surface without
// importing brain-internal packages.
type (
	MeetingKernel        = meeting.Kernel
	MeetingIdentity      = meeting.Identity
	MeetingImportCommand = meeting.ImportCommand
	MeetingStatusCommand = meeting.StatusCommand
	MeetingQueryCommand  = meeting.QueryCommand
	MeetingRevokeCommand = meeting.RevokeCommand
	MeetingPurgeCommand  = meeting.PurgeCommand
)

var (
	// ErrMeetingNotFoundOrDenied is the single static non-disclosing kernel denial.
	ErrMeetingNotFoundOrDenied = meeting.ErrNotFoundOrDenied
	// ErrMeetingInvalidInput marks malformed kernel boundary facts.
	ErrMeetingInvalidInput = meeting.ErrInvalidInput
)

// MeetingSurface is the Stage 07 meeting-transcript surface over one durable
// authority runtime. The kernel owns migration 006 facts and encrypted
// transcript payloads; this type only composes and closes.
type MeetingSurface struct {
	runtime *Runtime
	kernel  *meeting.Kernel
}

// OpenMeetingSurface composes the Stage 07 meeting kernel over one durable
// runtime. It fails closed without a durable payload vault, a database path,
// or the conversation payload port (the same encrypted vault Stage 04 uses).
func (r *Runtime) OpenMeetingSurface(ctx context.Context) (*MeetingSurface, error) {
	if r == nil || ctx == nil {
		return nil, ErrInvalid
	}
	if r.databasePath == "" || r.conversationPayloads == nil || r.clock == nil {
		return nil, ErrInvalid
	}
	kernel, err := meeting.Open(ctx, meeting.Config{
		DatabasePath: r.databasePath,
		Payloads:     r.conversationPayloads,
		Clock:        r.clock,
	})
	if err != nil {
		if errors.Is(err, meeting.ErrSchemaUnsupported) || errors.Is(err, meeting.ErrInvalidInput) {
			return nil, ErrInvalid
		}
		return nil, ErrUnavailable
	}
	return &MeetingSurface{runtime: r, kernel: kernel}, nil
}

// Kernel returns the composed meeting kernel.
func (s *MeetingSurface) Kernel() *MeetingKernel {
	if s == nil {
		return nil
	}
	return s.kernel
}

// Close releases the meeting kernel handle.
func (s *MeetingSurface) Close() error {
	if s == nil || s.kernel == nil {
		return nil
	}
	return s.kernel.Close()
}
