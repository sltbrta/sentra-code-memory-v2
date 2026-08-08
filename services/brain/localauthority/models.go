// Package localauthority composes the Stage 2 SQLite ledger with encrypted
// artifact and evidence ports. It exposes domain requests rather than transport
// messages so the Unix gateway remains a replaceable outer adapter.
package localauthority

import (
	"context"
	"errors"

	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

var (
	// ErrDenied is the single static result for authorization, existence,
	// integrity, revocation, and audit failures visible to an outer boundary.
	ErrDenied = errors.New("local authority runtime: denied")
	// ErrInvalid reports malformed injected configuration or domain requests.
	ErrInvalid = errors.New("local authority runtime: invalid request")
	// ErrUnavailable reports a local database, object-root, or key-provider
	// startup failure without exposing implementation details to outer adapters.
	ErrUnavailable = errors.New("local authority runtime: unavailable")
)

// Identifier is a namespaced opaque identifier.
type Identifier = shared.Identifier

// Digest binds immutable bytes to a named digest algorithm.
type Digest = shared.Digest

// KeyReference identifies one tenant-scoped key epoch without carrying secret
// material. It is public so outer composition packages need no internal import.
type KeyReference = shared.KeyReference

// Identity is a peer-authenticated authority identity.
type Identity = shared.MappedIdentityFact

// Clock supplies deterministic authority time.
type Clock = shared.Clock

// Authorization records a non-sensitive current policy decision.
type Authorization struct {
	Allowed         bool
	ReasonCode      string
	RevocationEpoch uint64
}

// AuthorizeFunc reauthorizes an exact action and evidence resource immediately
// before an effect. Implementations must evaluate current policy and stale epochs.
type AuthorizeFunc func(context.Context, Identity, string, Identifier) (Authorization, error)

// Artifact identifies one immutable generation and its encrypted layout.
type Artifact struct {
	ID                 Identifier
	Tenant             Identifier
	Digest             Digest
	Generation         uint64
	ExpectedGeneration uint64
	KeyEpoch           uint64
	Length             uint64
	FrameCount         uint32
}

// Command is the canonical authenticated command envelope required by SQLite
// idempotency and audit state.
type Command struct {
	ID             Identifier
	Type           string
	IdempotencyKey string
	PayloadDigest  Digest
	Fence          uint64
}

// ExecuteRequest is one authorized artifact command. Range fields are used only
// for reads; PurgeNow is used only for deletion.
type ExecuteRequest struct {
	Identity  Identity
	Command   Command
	Artifact  Artifact
	Authorize AuthorizeFunc
	Offset    uint64
	Length    uint64
	PurgeNow  bool
}

// Result is a static command disposition plus optional bounded read material.
// A denied valid command may carry a canonical rejected receipt without a Go
// error. Bytes are freshly allocated plaintext and callers must discard them
// promptly.
type Result struct {
	Receipt         shared.Receipt
	Authorization   Authorization
	Artifact        Artifact
	Bytes           []byte
	RangeDigest     Digest
	NextOffset      uint64
	Replayed        bool
	RecordedAtMilli int64
	// ConfigurationDigest pins the non-secret runtime configuration.
	ConfigurationDigest Digest
}

// Status is bounded current session state. Watermark may be zero when no
// canonical command has committed.
type Status struct {
	Identity            Identity
	Receipt             shared.Receipt
	Watermark           uint64
	RevocationEpoch     uint64
	ObservedAtMilli     int64
	ConfigurationDigest Digest
}
