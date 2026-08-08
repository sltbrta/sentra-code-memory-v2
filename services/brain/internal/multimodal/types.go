package multimodal

import (
	"context"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

// PayloadStore is the narrow encrypted-payload port original bytes and evidence
// JSON persist behind. SQLite never holds payload bytes — only the returned
// opaque artifact identity and the canonical SHA-256 digest. Implementations
// must scope payloads by tenant and make reads fail closed.
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

// AdmitCommand admits one multimodal envelope under the authenticated scope.
// Payload is the plaintext original bytes. When empty, the kernel may resolve
// bytes from a local-path source_object_id for residual TUI admission.
type AdmitCommand struct {
	Identity Identity
	Request  *contractsv1.AdmitMultimodalSourceRequest
	Payload  []byte
	// ForcePartial marks at least one required lane incomplete so public state
	// surfaces PARTIAL_READY (residual proof of partial readiness vocabulary).
	ForcePartial bool
}

// StatusCommand reads one multimodal source under the authenticated scope.
type StatusCommand struct {
	Identity Identity
	SourceID string
}

// EvidenceCommand pages modality-native anchors for one admitted source.
type EvidenceCommand struct {
	Identity Identity
	SourceID string
	PageSize uint32
	After    string
}

// RevokeCommand denies one multimodal source under the authenticated scope.
type RevokeCommand struct {
	Identity       Identity
	SourceID       string
	IdempotencyKey string
}

// PurgeCommand purges one multimodal source's lineage under the authenticated scope.
type PurgeCommand struct {
	Identity       Identity
	SourceID       string
	IdempotencyKey string
}

// Config binds a Kernel to its durable authority path and encrypted payload port.
type Config struct {
	// DatabasePath is the absolute path to the already-migrated authority DB.
	DatabasePath string
	// Payloads is the encrypted original/evidence payload port.
	Payloads PayloadStore
	// Clock supplies commit times.
	Clock Clock
}

// evidenceBody is the encrypted vault body for derived multimodal anchors.
type evidenceBody struct {
	Version string             `json:"version"`
	Items   []evidenceBodyItem `json:"items"`
	Lanes   []laneBody         `json:"lanes"`
}

type evidenceBodyItem struct {
	EvidenceID       string `json:"evidence_id"`
	SourceRevisionID string `json:"source_revision_id"`
	// AnchorKind is one of bytes, text, page, audio.
	AnchorKind string `json:"anchor_kind"`
	// Anchor fields are kind-specific; unused fields stay zero.
	StartByte      uint64 `json:"start_byte,omitempty"`
	EndByte        uint64 `json:"end_byte,omitempty"`
	PageNumber     uint32 `json:"page_number,omitempty"`
	LeftPerMille   uint32 `json:"left_per_mille,omitempty"`
	RightPerMille  uint32 `json:"right_per_mille,omitempty"`
	TopPerMille    uint32 `json:"top_per_mille,omitempty"`
	BottomPerMille uint32 `json:"bottom_per_mille,omitempty"`
	StartMillis    uint64 `json:"start_millis,omitempty"`
	EndMillis      uint64 `json:"end_millis,omitempty"`
	SupportDigest  string `json:"support_digest,omitempty"`
	Authority      string `json:"authority"`
}

type laneBody struct {
	Lane             string `json:"lane"`
	State            string `json:"state"`
	Required         bool   `json:"required"`
	CoveragePerMille uint32 `json:"coverage_per_mille"`
}

const (
	payloadVersion     = "ouroboros.stage11.evidence.v1"
	extractorPinHex    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	localPathNamespace = "local-path"
)
