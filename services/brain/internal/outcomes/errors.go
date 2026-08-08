package outcomes

import "errors"

var (
	// ErrInvalidInput reports malformed admission inputs.
	ErrInvalidInput = errors.New("outcomes: invalid input")
	// ErrSanitizationFailed rejects elevated model output or secret/raw material.
	ErrSanitizationFailed = errors.New("outcomes: sanitization failed")
	// ErrConflict reports mismatched reuse of an outcome identity.
	ErrConflict = errors.New("outcomes: conflict")
)

// Reason codes (stable, non-sensitive).
const (
	ReasonAllowed                 = "allowed"
	ReasonOutcomeSanitizationFail = "OUTCOME_SANITIZATION_FAILED"
	ReasonAuthorityClassInvalid   = "authority_class_invalid"
	ReasonRawTraceNotSeparated    = "raw_trace_not_separated"
	ReasonDuplicateMismatch       = "outcome_duplicate_mismatch"
)
