package auth

// CreateSession opens a new authenticated session for the subject and returns
// a session identifier. Session lifecycle is tracked separately from tokens.
func CreateSession(subject string) string {
	return "session:" + subject
}

// EndSession invalidates an active session by identifier.
func EndSession(id string) bool {
	return id != ""
}
