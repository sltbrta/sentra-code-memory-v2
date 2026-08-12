package util

// AppError is a classified application error carrying a stable code suitable
// for structured logging and client-facing responses.
type AppError struct {
	Code string
	Msg  string
	Err  error
}

// Error renders the message, including the wrapped cause when present.
func (e *AppError) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Msg + ": " + e.Err.Error()
	}
	return e.Code + ": " + e.Msg
}

// Unwrap exposes the wrapped cause for errors.Is/As.
func (e *AppError) Unwrap() error { return e.Err }

// Wrap attaches a code and message to an underlying error. A nil cause returns
// nil so callers can wrap unconditionally.
func Wrap(err error, code, msg string) *AppError {
	if err == nil {
		return nil
	}
	return &AppError{Code: code, Msg: msg, Err: err}
}

// NewAppError builds an AppError without a cause.
func NewAppError(code, msg string) *AppError {
	return &AppError{Code: code, Msg: msg}
}
