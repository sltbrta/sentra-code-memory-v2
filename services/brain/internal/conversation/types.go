package conversation

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/query"
)

var (
	// ErrInvalidInput reports a missing, malformed, or oversized admission,
	// completion, cursor, or configuration value before any mutation.
	ErrInvalidInput = errors.New("conversation: invalid input")
	// ErrIdempotencyConflict reports reuse of an admitted idempotency key with
	// a different request digest. Nothing is mutated.
	ErrIdempotencyConflict = errors.New("conversation: idempotency conflict")
	// ErrUnknownAdmission reports a completion or resolution for an
	// idempotency key that was never admitted.
	ErrUnknownAdmission = errors.New("conversation: unknown admitted query")
	// ErrCompletionConflict reports a second completion for one admitted query
	// whose outcome differs from the canonical first completion.
	ErrCompletionConflict = errors.New("conversation: completion conflict")
	// ErrUnknownSession reports admission under a session the canonical
	// session ledger has never opened; the schema foreign key rejects it.
	ErrUnknownSession = errors.New("conversation: unknown session")
	// ErrSchemaUnsupported reports a database that has not applied migration
	// 004; the store fails closed rather than creating shadow tables.
	ErrSchemaUnsupported = errors.New("conversation: migration 004 schema unavailable")
	// ErrPayloadUnavailable reports an unreadable, missing, or
	// digest-mismatched vault payload during hydration.
	ErrPayloadUnavailable = errors.New("conversation: payload unavailable")
)

const (
	// maxIdentifierLength bounds every opaque identity the store persists.
	maxIdentifierLength = 512
	// MaxQueryLength is the frozen AskRequest query-text bound in runes.
	MaxQueryLength = 8192
	// MaxPayloadBytes bounds one serialized turn payload; the frozen claim,
	// citation, and prose bounds keep real payloads far below it.
	MaxPayloadBytes = 1 << 20
	// MaxHistoryPage is the frozen GetHistory page bound.
	MaxHistoryPage = 100
)

// Role distinguishes an authenticated principal's input from synthesized
// assistant output, mirroring the frozen ConversationRole values.
type Role string

const (
	// RoleUser is an authenticated principal's own input turn.
	RoleUser Role = "user"
	// RoleAssistant is model proposal output, never source fact.
	RoleAssistant Role = "assistant"
)

// Status is the terminal commit-time lifecycle of one turn, mirroring the
// frozen ConversationTurnStatus values. No transition exists after commit.
type Status string

const (
	// StatusActive is a completed committed turn.
	StatusActive Status = "active"
	// StatusFailed is an interrupted turn that is never read as fact.
	StatusFailed Status = "failed"
)

// Admission is one authenticated Ask admission: the user turn plus the
// idempotency record committed atomically before any engine work. The
// composing layer derives Principal exclusively from the authenticated peer;
// body identity is cross-checked upstream and never trusted here.
type Admission struct {
	Principal      query.Principal
	SourceID       string
	GenerationID   string
	Text           string
	Freshness      query.FreshnessRequirement
	IdempotencyKey string
}

// AdmissionResult is the durable admission disposition. An exact idempotent
// replay returns the originally admitted identities with Replayed set and
// commits nothing.
type AdmissionResult struct {
	QueryID    string
	UserTurnID string
	Replayed   bool
}

// Completion is the exactly-once assistant outcome for one admitted query,
// keyed by (tenant, principal, idempotency key). Result carries the complete
// grounded engine result — answer, freshness, coverage, and projection — so
// an idempotent replay reconstructs the original outcome byte-faithfully.
// Failed appends a
// visibly failed turn with no answer; exactly one of Result and Failed is set.
// The owning session is resolved from the admission record, never from the
// caller, so a completion always extends the session that admitted the query.
type Completion struct {
	Tenant         string
	Principal      string
	IdempotencyKey string
	Result         *query.Result
	Failed         bool
}

// CompletionResult is the durable completion disposition.
type CompletionResult struct {
	AssistantTurnID string
	Sequence        uint64
	Replayed        bool
}

// Resolution resolves one admitted idempotency key to its original outcome:
// the admitted identities plus, once present, the exactly-once completion.
type Resolution struct {
	QueryID         string
	UserTurnID      string
	SessionID       string
	Completed       bool
	Status          Status
	Result          *query.Result
	AssistantTurnID string
}

// Turn is one hydrated private history entry. A user turn carries Text and no
// answer; an active assistant turn carries Answer and no text; a failed
// assistant turn carries neither.
type Turn struct {
	TurnID         string
	SessionID      string
	Sequence       uint64
	Role           Role
	Status         Status
	OccurredAtMs   int64
	Text           string
	Answer         *query.Answer
	IdempotencyKey string
}

// Page is one bounded history page with its opaque continuation cursor.
type Page struct {
	Turns      []Turn
	NextCursor string
}

// validAdmission bounds admission input before any vault or database effect,
// mirroring the engine's own input rules so an admitted query always passes
// engine validation.
func validAdmission(admission Admission) error {
	if err := validIdentity(admission.Principal); err != nil {
		return err
	}
	if !validBoundedID(admission.SourceID) || !validBoundedID(admission.GenerationID) {
		return ErrInvalidInput
	}
	if !validQueryText(admission.Text) || !validFreshness(admission.Freshness) ||
		!validIdempotencyKey(admission.IdempotencyKey) {
		return ErrInvalidInput
	}
	return nil
}

func validIdentity(principal query.Principal) error {
	if !validBoundedID(principal.Tenant) || !validBoundedID(principal.Principal) || !validBoundedID(principal.Session) {
		return ErrInvalidInput
	}
	return nil
}

func validBoundedID(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && len(value) <= maxIdentifierLength
}

func validQueryText(value string) bool {
	runes := utf8.RuneCountInString(value)
	return utf8.ValidString(value) && runes > 0 && runes <= MaxQueryLength && strings.TrimSpace(value) != ""
}

func validFreshness(value query.FreshnessRequirement) bool {
	switch value {
	case query.FreshnessBestEffort, query.FreshnessCompleteGeneration, query.FreshnessAbstainIfStale:
		return true
	default:
		return false
	}
}

func validIdempotencyKey(value string) bool {
	if value == "" || len(value) > maxIdentifierLength || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validCompletion(completion Completion) error {
	if !validBoundedID(completion.Tenant) || !validBoundedID(completion.Principal) ||
		!validIdempotencyKey(completion.IdempotencyKey) {
		return ErrInvalidInput
	}
	if (completion.Result == nil) == !completion.Failed {
		return ErrInvalidInput
	}
	return nil
}
