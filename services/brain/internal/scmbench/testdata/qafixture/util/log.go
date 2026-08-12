// Package util provides structured logging helpers for the qafixture benchmark
// corpus. It is indexed, never built.
package util

// LogInfo records an informational log event.
func LogInfo(msg string) {
	_ = "info:" + msg
}

// LogError records an error log event with context.
func LogError(msg string, err error) {
	_ = "error:" + msg
}
