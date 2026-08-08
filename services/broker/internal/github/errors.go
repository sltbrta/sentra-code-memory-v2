package github

import "errors"

var (
	// ErrDenied is the single non-disclosing effect denial.
	ErrDenied = errors.New("github: denied")
	// ErrConflict is a terminal publication conflict (mismatched ref/PR).
	ErrConflict = errors.New("github: terminal conflict")
	// ErrInvalidInput reports malformed broker inputs.
	ErrInvalidInput = errors.New("github: invalid input")
	// ErrAlreadyClosed reports an exact closed PR that must never duplicate.
	ErrAlreadyClosed = errors.New("github: pr already closed")
)

// Reason codes (stable, non-sensitive).
const (
	ReasonAllowed             = "allowed"
	ReasonEffectNotAuthorized = "EFFECT_NOT_AUTHORIZED"
	ReasonEffectDuplicate     = "EFFECT_DUPLICATE"
	ReasonTerminalConflict    = "TERMINAL_CONFLICT"
	ReasonPRAlreadyClosed     = "PR_ALREADY_CLOSED"
	ReasonStaleBase           = "BASE_STALE"
	ReasonPolicyDenied        = "policy_denied"
	ReasonIdentityMismatch    = "identity_mismatch"
	ReasonGrantMalformed      = "grant_malformed"
	ReasonRevoked             = "revoked"
)

// Denial carries one static internal reason code.
type Denial struct {
	// Reason is a stable non-sensitive reason code.
	Reason string
}

// Error implements error.
func (d *Denial) Error() string { return "github: denied: " + d.Reason }

// Unwrap exposes ErrDenied for errors.Is.
func (d *Denial) Unwrap() error { return ErrDenied }

// ReasonCode extracts the static internal reason from one denial error.
func ReasonCode(err error) string {
	var denial *Denial
	if errors.As(err, &denial) {
		return denial.Reason
	}
	if errors.Is(err, ErrAlreadyClosed) {
		return ReasonPRAlreadyClosed
	}
	if errors.Is(err, ErrConflict) {
		return ReasonTerminalConflict
	}
	return "denied"
}
