package query

import (
	"context"
)

// EvidenceEntry is one hydrated, digest-verified evidence block: the
// statement block of one selected definition occurrence plus the canonical
// revision facts a citation binds. Lines holds the block's raw content lines
// starting at BlockStartLine.
type EvidenceEntry struct {
	Path           string
	Language       string
	RevisionID     string
	BlobOID        string
	ContentDigest  string
	BlockStartLine uint32
	Lines          []string
	DefinitionText string
}

// SynthesisRequest is the bounded model-adapter input: the admitted question
// and the verified evidence pack. Adapters receive no principal, tenant, or
// source facts beyond the evidence they must ground in.
type SynthesisRequest struct {
	Query    string
	Evidence []EvidenceEntry
	Limits   SynthesisLimits
}

// ProposedCitation references one pack entry and a one-based half-open range
// within its hydrated block. The engine re-resolves and re-verifies every
// proposal against canonical bytes before emission; a fabricated index or
// range discards the whole synthesis.
type ProposedCitation struct {
	EvidenceIndex int
	StartLine     uint32
	StartColumn   uint32
	EndLine       uint32
	EndColumn     uint32
}

// ProposedClaim is one adapter-proposed material assertion and its support.
type ProposedClaim struct {
	Statement          string
	Citations          []ProposedCitation
	ConfidencePerMille uint32
}

// Synthesis is the adapter output: bounded prose, proposed claims, and
// disclosed token consumption.
type Synthesis struct {
	Prose      string
	Claims     []ProposedClaim
	TokenUsage uint64
}

// Synthesizer generates bounded claims from the verified evidence pack.
// Implementations must fail closed: any provider or adapter error yields a
// zero Synthesis and a wrapped ErrSynthesisFailed, never partial prose, and
// must never silently fall back to another provider or billing identity.
// Context cancellation and deadline errors from the caller's context are the
// one carve-out: they are returned directly and unwrapped so the engine can
// distinguish caller cancellation from provider failure.
type Synthesizer interface {
	Synthesize(ctx context.Context, request SynthesisRequest) (Synthesis, error)
}
