package query

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factualconsistency"
)

var (
	// ErrInvalidInput reports a missing, malformed, or oversized caller or
	// configuration value before any authorization or retrieval work.
	ErrInvalidInput = errors.New("query: invalid input")
	// ErrUnknownScope is the single non-disclosing failure for an unknown,
	// revoked, or otherwise unservable source or generation scope; Status
	// also uses it for admission denial and mid-flight revocation, because a
	// status read has no abstention shape. Answer never uses it for denial: a
	// denied Ask collapses to an absent_support abstention instead. The
	// gateway maps it to the static not_found_or_denied outcome.
	ErrUnknownScope = errors.New("query: unknown source or generation scope")
	// ErrSynthesisFailed is the single fail-closed synthesis failure: provider
	// error, timeout, policy refusal, or malformed provider output. It never
	// carries provider detail and never leaks partial prose.
	ErrSynthesisFailed = errors.New("query: synthesis unavailable")
)

// Status distinguishes an answer, a disclosed partial result, and an
// abstention, mirroring the frozen AnswerStatus enum values.
type Status string

const (
	// StatusAnswered marks non-empty prose and claims with zero degraded reasons.
	StatusAnswered Status = "answered"
	// StatusPartial marks supported claims with at least one disclosed reason.
	StatusPartial Status = "partial"
	// StatusAbstained marks empty prose, zero claims, and disclosed reasons.
	StatusAbstained Status = "abstained"
)

// Reason is one member of the frozen bounded degraded-reason vocabulary.
// There is deliberately no denied_support value: denial collapses to
// absent_support byte-identical to genuinely absent support.
type Reason string

const (
	// ReasonAbsentSupport marks support that is absent, denied, or revoked.
	ReasonAbsentSupport Reason = "absent_support"
	// ReasonStaleSupport discloses a superseded pinned generation.
	ReasonStaleSupport Reason = "stale_support"
	// ReasonPartialCoverage discloses canonical revisions the projection does
	// not index, so absence is never proven over the unindexed subset.
	ReasonPartialCoverage Reason = "partial_coverage"
	// ReasonLaneDegraded discloses a candidate file whose language lane is
	// lexically degraded in the pinned generation.
	ReasonLaneDegraded Reason = "lane_degraded"
	// ReasonRetrievalUnavailable marks a rebuilding or absent projection;
	// projection absence is a coverage fact, never deletion evidence.
	ReasonRetrievalUnavailable Reason = "retrieval_unavailable"
	// ReasonCitationVerificationFailed marks discarded claims whose citation
	// anchor, digest, or support failed canonical verification.
	ReasonCitationVerificationFailed Reason = "citation_verification_failed"
	// ReasonSynthesisUnavailable marks any provider or adapter failure.
	ReasonSynthesisUnavailable Reason = "synthesis_unavailable"
)

// reasonOrder is the canonical disclosure order of the frozen vocabulary.
var reasonOrder = []Reason{
	ReasonAbsentSupport, ReasonStaleSupport, ReasonPartialCoverage, ReasonLaneDegraded,
	ReasonRetrievalUnavailable, ReasonCitationVerificationFailed, ReasonSynthesisUnavailable,
}

func (r Reason) known() bool {
	for _, reason := range reasonOrder {
		if r == reason {
			return true
		}
	}
	return false
}

// FreshnessRequirement mirrors the frozen FreshnessRequirement enum.
type FreshnessRequirement string

const (
	// FreshnessBestEffort serves a superseded pin with explicit disclosure.
	FreshnessBestEffort FreshnessRequirement = "best_effort"
	// FreshnessCompleteGeneration requires the pin to be a complete published
	// generation; v1 publications are atomic, so a superseded complete pin is
	// served with the same explicit stale disclosure as best_effort.
	FreshnessCompleteGeneration FreshnessRequirement = "complete_generation"
	// FreshnessAbstainIfStale refuses a superseded pin with stale_support.
	FreshnessAbstainIfStale FreshnessRequirement = "abstain_if_stale"
)

func (f FreshnessRequirement) valid() bool {
	switch f {
	case FreshnessBestEffort, FreshnessCompleteGeneration, FreshnessAbstainIfStale:
		return true
	default:
		return false
	}
}

// FreshnessState discloses whether the pinned generation is current.
type FreshnessState string

const (
	// FreshnessCurrent pins the current complete generation.
	FreshnessCurrent FreshnessState = "current"
	// FreshnessStaleDisclosed serves a superseded generation explicitly.
	FreshnessStaleDisclosed FreshnessState = "stale_disclosed"
	// FreshnessDegraded serves a complete generation with reduced lanes.
	FreshnessDegraded FreshnessState = "degraded"
)

// ProjectionState discloses the rebuildable search projection, never deletion.
type ProjectionState string

const (
	// ProjectionReady serves the pinned generation projection.
	ProjectionReady ProjectionState = "ready"
	// ProjectionRebuilding is rebuilding after restart or reconcile.
	ProjectionRebuilding ProjectionState = "rebuilding"
	// ProjectionAbsent has no projection; canonical revisions remain.
	ProjectionAbsent ProjectionState = "absent"
)

// GenerationState mirrors the frozen GenerationState publication values.
type GenerationState string

const (
	// GenerationReady marks a complete generation with full P5 coverage.
	GenerationReady GenerationState = "ready"
	// GenerationDegraded marks a complete generation with a degraded lane.
	GenerationDegraded GenerationState = "degraded"
)

// LaneReadiness reports one P5 language lane's publication disposition.
type LaneReadiness struct {
	Language   string
	Coverage   string
	ReasonCode string
}

// Principal is the authenticated, gateway-supplied caller identity. The
// engine never accepts it as untrusted body input; the composing layer
// cross-checks body identity against the authenticated peer before calling.
type Principal struct {
	Tenant    string
	Principal string
	Session   string
}

// Query is one admitted grounded question pinned to one generation. The
// engine is stateless: QueryID is server-authored by the composing layer and
// echoed into the answer, and IdempotencyKey is validated but never persisted
// here; the conversation store owns admission and replay.
type Query struct {
	QueryID        string
	Principal      Principal
	SourceID       string
	GenerationID   string
	Text           string
	Freshness      FreshnessRequirement
	IdempotencyKey string
}

func (q Query) validate(limits Limits) error {
	if q.QueryID == "" || len(q.QueryID) > limits.MaxIdentifierLength ||
		q.SourceID == "" || len(q.SourceID) > limits.MaxIdentifierLength ||
		q.GenerationID == "" || len(q.GenerationID) > limits.MaxIdentifierLength {
		return fmt.Errorf("%w: identity fields", ErrInvalidInput)
	}
	if q.Principal.Tenant == "" || q.Principal.Principal == "" || q.Principal.Session == "" ||
		len(q.Principal.Tenant) > limits.MaxIdentifierLength ||
		len(q.Principal.Principal) > limits.MaxIdentifierLength ||
		len(q.Principal.Session) > limits.MaxIdentifierLength {
		return fmt.Errorf("%w: principal", ErrInvalidInput)
	}
	if utf8.RuneCountInString(q.Text) == 0 || utf8.RuneCountInString(q.Text) > limits.MaxQueryLength ||
		len(strings.TrimSpace(q.Text)) == 0 {
		return fmt.Errorf("%w: query text", ErrInvalidInput)
	}
	if !q.Freshness.valid() {
		return fmt.Errorf("%w: freshness", ErrInvalidInput)
	}
	if q.IdempotencyKey == "" || len(q.IdempotencyKey) > limits.MaxIdentifierLength ||
		strings.TrimSpace(q.IdempotencyKey) != q.IdempotencyKey {
		return fmt.Errorf("%w: idempotency key", ErrInvalidInput)
	}
	for _, character := range q.IdempotencyKey {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("%w: idempotency key characters", ErrInvalidInput)
		}
	}
	return nil
}

// Citation binds one claim to exact authorized committed-code evidence with a
// one-based, half-open line/column range and the canonical SHA-256 digest of
// the hydrated supporting bytes the range selects.
type Citation struct {
	EvidenceID           string
	SourceRevisionID     string
	GitOID               string
	Path                 string
	StartLine            uint32
	StartColumn          uint32
	EndLine              uint32
	EndColumn            uint32
	SupportingTextDigest string
}

// Claim is one bounded material answer assertion and its verified support.
// Claims are always model proposals and never promote to direct source; the
// composing gateway sets AUTHORITY_CLASS_MODEL_PROPOSAL on every emission.
type Claim struct {
	ClaimID            string
	Statement          string
	Citations          []Citation
	ConfidencePerMille uint32
}

// Answer is the bounded grounded result: answered, partial, or abstained,
// composed exactly as the frozen query_answer.status_consistency rule states.
type Answer struct {
	QueryID            string
	Status             Status
	Prose              string
	Claims             []Claim
	DegradedReasons    []Reason
	TokenUsage         uint64
	FactualConsistency factualconsistency.Result
}

// Freshness pins one complete generation and discloses its state, the
// authorization epoch used for hydration decisions, and the observation time.
type Freshness struct {
	GenerationID    string
	Sequence        uint64
	CommitOID       string
	TreeOID         string
	GenerationState GenerationState
	State           FreshnessState
	ACLEpoch        uint64
	ObservedAt      time.Time
}

// Coverage discloses canonical versus indexed revision counts; indexed never
// exceeds canonical, and an absent projection reports zero indexed revisions
// while canonical facts remain.
type Coverage struct {
	CanonicalRevisionCount uint64
	IndexedRevisionCount   uint64
}

// Result is the complete engine output for one Ask: the answer plus the
// freshness, coverage, and projection disclosures the gateway maps onto
// AskSuccess, QueryFreshness, and QueryCoverage.
type Result struct {
	Answer     Answer
	Freshness  Freshness
	Coverage   Coverage
	Projection ProjectionState
}

// SourceStatus composes the authorized GetStatus view: the current complete
// generation's freshness, coverage, and projection truth for one source.
type SourceStatus struct {
	SourceID   string
	Freshness  Freshness
	Coverage   Coverage
	Projection ProjectionState
}

// Limits bounds every engine input and output dimension. Defaults follow the
// frozen contract bounds; smaller values are honored, larger values are not.
type Limits struct {
	MaxQueryLength        int
	MaxIdentifierLength   int
	MaxCandidates         int
	MaxBlockLines         int
	MaxEvidenceEntries    int
	MaxEvidenceEntryBytes int
	MaxEvidencePackBytes  int
	MaxReasons            int
	Synthesis             SynthesisLimits
	FactualConsistency    factualconsistency.Limits
}

// SynthesisLimits carries the frozen claim and prose bounds every synthesizer
// output is verified against.
type SynthesisLimits struct {
	MaxClaims            int
	MaxCitationsPerClaim int
	MaxStatementBytes    int
	MaxProseBytes        int
}

// DefaultLimits returns the contract-aligned engine bounds. The 64 KiB pack
// ceiling stays under the frozen 16,000-token evidence budget at four bytes
// per token.
func DefaultLimits() Limits {
	return Limits{
		MaxQueryLength:        8192,
		MaxIdentifierLength:   512,
		MaxCandidates:         64,
		MaxBlockLines:         64,
		MaxEvidenceEntries:    16,
		MaxEvidenceEntryBytes: 4096,
		MaxEvidencePackBytes:  64 * 1024,
		MaxReasons:            8,
		Synthesis: SynthesisLimits{
			MaxClaims:            64,
			MaxCitationsPerClaim: 16,
			MaxStatementBytes:    4096,
			MaxProseBytes:        16384,
		},
		FactualConsistency: factualconsistency.DefaultLimits(),
	}
}

var hardLimits = DefaultLimits()

func (l Limits) validate() error {
	values := []struct {
		name string
		got  int
		max  int
	}{
		{"query length", l.MaxQueryLength, hardLimits.MaxQueryLength},
		{"identifier length", l.MaxIdentifierLength, hardLimits.MaxIdentifierLength},
		{"candidates", l.MaxCandidates, hardLimits.MaxCandidates},
		{"block lines", l.MaxBlockLines, hardLimits.MaxBlockLines},
		{"evidence entries", l.MaxEvidenceEntries, hardLimits.MaxEvidenceEntries},
		{"evidence entry bytes", l.MaxEvidenceEntryBytes, hardLimits.MaxEvidenceEntryBytes},
		{"evidence pack bytes", l.MaxEvidencePackBytes, hardLimits.MaxEvidencePackBytes},
		{"reasons", l.MaxReasons, hardLimits.MaxReasons},
		{"claims", l.Synthesis.MaxClaims, hardLimits.Synthesis.MaxClaims},
		{"citations per claim", l.Synthesis.MaxCitationsPerClaim, hardLimits.Synthesis.MaxCitationsPerClaim},
		{"statement bytes", l.Synthesis.MaxStatementBytes, hardLimits.Synthesis.MaxStatementBytes},
		{"prose bytes", l.Synthesis.MaxProseBytes, hardLimits.Synthesis.MaxProseBytes},
		{"factual consistency claims", l.FactualConsistency.MaxClaims, hardLimits.FactualConsistency.MaxClaims},
		{"factual consistency supports", l.FactualConsistency.MaxSupportsPerClaim, hardLimits.FactualConsistency.MaxSupportsPerClaim},
		{"factual consistency statement bytes", l.FactualConsistency.MaxStatementBytes, hardLimits.FactualConsistency.MaxStatementBytes},
		{"factual consistency support bytes", l.FactualConsistency.MaxSupportBytes, hardLimits.FactualConsistency.MaxSupportBytes},
		{"factual consistency total bytes", l.FactualConsistency.MaxTotalBytes, hardLimits.FactualConsistency.MaxTotalBytes},
	}
	for _, value := range values {
		if value.got <= 0 || value.got > value.max {
			return fmt.Errorf("%w: %s limit", ErrInvalidInput, value.name)
		}
	}
	if l.MaxCandidates < l.MaxEvidenceEntries {
		return fmt.Errorf("%w: candidates below evidence entries limit", ErrInvalidInput)
	}
	if l.FactualConsistency.Timeout <= 0 || l.FactualConsistency.Timeout > hardLimits.FactualConsistency.Timeout {
		return fmt.Errorf("%w: factual consistency timeout", ErrInvalidInput)
	}
	return nil
}
