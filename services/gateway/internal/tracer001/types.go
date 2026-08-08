package tracer001

import (
	"errors"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

var (
	// ErrRequestDenied marks an authenticated-context failure: unmapped peer
	// or body identity mismatch. Transport maps it to request-denied.
	ErrRequestDenied = errors.New("tracer001: request denied")
	// ErrInvalidRequest marks decode or Protovalidate failure before authority.
	ErrInvalidRequest = errors.New("tracer001: invalid request")
	// ErrInvalidResponse marks a constructed response that fails contract
	// validation — a defect in a composed port or in this package.
	ErrInvalidResponse = errors.New("tracer001: invalid response")
	// ErrUnknownScope is the non-disclosing port failure for unknown,
	// unauthorized, stale, or revoked runs and principals. Ports must return
	// it without existence detail.
	ErrUnknownScope = errors.New("tracer001: unknown scope")
	// ErrIdempotencyConflict marks a reused idempotency key bound to a
	// different request digest.
	ErrIdempotencyConflict = errors.New("tracer001: idempotency conflict")
	// ErrInvalidConfiguration marks incomplete handler configuration.
	ErrInvalidConfiguration = errors.New("tracer001: invalid configuration")
	// errPortFailure marks any other port failure; never wraps port text.
	errPortFailure = errors.New("tracer001: port failure")
)

// Principal is derived exclusively from the authenticated peer.
type Principal struct {
	Tenant      string
	PrincipalID string
	Session     string
}

// Step names the fixed Tracer 001 public path operations.
type Step string

const (
	StepSession Step = "session"
	StepIngest  Step = "ingest"
	StepAsk     Step = "ask"
	StepIntent  Step = "intent"
	StepPlan    Step = "plan"
	StepReview  Step = "review"
	StepDraftPR Step = "draft-pr"
	StepOutcome Step = "outcome"
)

// PathRequest is one untrusted step advance. Nested contract messages are
// protovalidated before any port call. QueryText is only for ask/outcome and
// is never echoed on denial.
type PathRequest struct {
	Caller             *contractsv1.AuthenticatedPrincipalRef
	RequestedSession   *contractsv1.Identifier
	RunID              *contractsv1.Identifier
	ManifestDigest     *contractsv1.Digest
	ConfigDigest       *contractsv1.Digest
	IdempotencyKey     string
	QueryText          string
	SourceID           *contractsv1.Identifier
	GenerationID       *contractsv1.Identifier
	ActiveVariant      contractsv1.TracerVariantKind
	BaseGitOID         string
	ScopeDigest        *contractsv1.Digest
	EffectApprovalHex  string
	ChangeSetDigestHex string
}

// PathResponse is the public step outcome: completed success or static denial.
type PathResponse struct {
	Receipt *contractsv1.Receipt
	Error   *contractsv1.PublicError
	Run     *contractsv1.TracerRun
	Step    *contractsv1.TracerStepReceipt
	DraftPR *contractsv1.DraftPrReceipt
	Outcome *contractsv1.OutcomeFact
}

// PathSuccess is the domain result the Path port returns on allow.
type PathSuccess struct {
	Run     *contractsv1.TracerRun
	Step    *contractsv1.TracerStepReceipt
	DraftPR *contractsv1.DraftPrReceipt
	Outcome *contractsv1.OutcomeFact
}

// StepCommand is the authenticated command passed to the Path port.
type StepCommand struct {
	Principal Principal
	Step      Step
	Request   PathRequest
}
