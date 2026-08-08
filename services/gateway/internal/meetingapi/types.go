package meetingapi

import (
	"errors"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

var (
	// ErrRequestDenied marks an authenticated-context failure.
	ErrRequestDenied = errors.New("meetingapi: request denied")
	// ErrInvalidRequest marks a message that fails decoding or Protovalidate.
	ErrInvalidRequest = errors.New("meetingapi: invalid request")
	// ErrInvalidResponse marks a constructed response that fails contract validation.
	ErrInvalidResponse = errors.New("meetingapi: invalid response")
	// ErrUnknownMeeting is the non-disclosing port failure for unknown,
	// unauthorized, revoked, or purged meetings.
	ErrUnknownMeeting = errors.New("meetingapi: unknown meeting")
	// ErrIdempotencyConflict marks a reused key bound to a different request digest.
	ErrIdempotencyConflict = errors.New("meetingapi: idempotency conflict")
	// ErrInvalidConfiguration marks incomplete handler configuration.
	ErrInvalidConfiguration = errors.New("meetingapi: invalid configuration")
	errPortFailure          = errors.New("meetingapi: port failure")
)

// Principal is the authenticated gateway peer identity.
type Principal struct {
	Tenant      string
	PrincipalID string
	Session     string
}

// ImportCommand is one transcript import under the authenticated principal.
type ImportCommand struct {
	Principal Principal
	Request   *contractsv1.ImportTranscriptRequest
}

// StatusCommand is one status read under the authenticated principal.
type StatusCommand struct {
	Principal Principal
	MeetingID string
}

// QueryCommand is one meeting query under the authenticated principal.
type QueryCommand struct {
	Principal Principal
	Request   *contractsv1.QueryMeetingRequest
}

// RevokeCommand is one revoke under the authenticated principal.
type RevokeCommand struct {
	Principal      Principal
	MeetingID      string
	IdempotencyKey string
}

// PurgeCommand is one purge under the authenticated principal.
type PurgeCommand struct {
	Principal      Principal
	MeetingID      string
	IdempotencyKey string
}
