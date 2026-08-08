package meeting

import (
	"context"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

// PayloadStore is the narrow encrypted-payload port transcript bytes persist
// behind. SQLite never holds prose — only the returned opaque artifact identity
// and the canonical SHA-256 digest. Implementations must scope payloads by
// tenant and make reads fail closed.
type PayloadStore interface {
	// Put encrypts and publishes one immutable payload, returning its opaque
	// artifact identity.
	Put(ctx context.Context, tenant string, payload []byte) (artifactID string, err error)
	// Get returns the authenticated plaintext of one published payload.
	Get(ctx context.Context, tenant, artifactID string) (payload []byte, err error)
	// Purge immediately denies and physically purges one payload artifact.
	Purge(ctx context.Context, tenant, artifactID string) error
}

// Clock supplies commit times without ambient time.Now.
type Clock interface {
	// NowUnixMilli returns the current wall-clock instant in milliseconds.
	NowUnixMilli() int64
}

// Identity is the authenticated principal scope derived exclusively from the
// gateway peer — never from request bodies.
type Identity struct {
	Tenant    string
	Principal string
	Session   string
}

// ImportCommand admits one timestamped transcript under the authenticated scope.
type ImportCommand struct {
	Identity Identity
	Request  *contractsv1.ImportTranscriptRequest
}

// StatusCommand reads one meeting under the authenticated scope.
type StatusCommand struct {
	Identity  Identity
	MeetingID string
}

// QueryCommand answers one question against one admitted meeting.
type QueryCommand struct {
	Identity Identity
	Request  *contractsv1.QueryMeetingRequest
}

// RevokeCommand denies one meeting under the authenticated scope.
type RevokeCommand struct {
	Identity       Identity
	MeetingID      string
	IdempotencyKey string
}

// PurgeCommand purges one meeting's lineage under the authenticated scope.
type PurgeCommand struct {
	Identity       Identity
	MeetingID      string
	IdempotencyKey string
}

// Config binds a Kernel to its durable authority path and encrypted payload port.
type Config struct {
	// DatabasePath is the absolute path to the already-migrated authority DB.
	DatabasePath string
	// Payloads is the encrypted transcript payload port.
	Payloads PayloadStore
	// Clock supplies commit times.
	Clock Clock
}

// transcriptPayload is the encrypted vault body for one admitted meeting.
type transcriptPayload struct {
	Version  string                     `json:"version"`
	Title    string                     `json:"title"`
	Segments []transcriptPayloadSegment `json:"segments"`
}

type transcriptPayloadSegment struct {
	StartMillis  uint64 `json:"start_millis"`
	EndMillis    uint64 `json:"end_millis"`
	Text         string `json:"text"`
	SpeakerLabel string `json:"speaker_label,omitempty"`
}

const payloadVersion = "ouroboros.stage07.transcript.v1"
