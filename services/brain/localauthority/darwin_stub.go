//go:build !darwin

package localauthority

import "context"

// OpenDarwin fails closed on platforms without the macOS Keychain security
// binary. It performs no filesystem or secret-provider side effects.
func OpenDarwin(context.Context, DarwinConfig) (*Runtime, error) {
	return nil, ErrUnavailable
}
