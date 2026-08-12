// Package auth implements token validation and session management for the
// qafixture benchmark corpus. It is indexed, never built.
package auth

// ValidateToken checks a bearer token signature and expiry, returning the
// authenticated subject. Token validation is the hot path of the auth layer.
func ValidateToken(token string) (string, error) {
	if token == "" {
		return "", ErrEmptyToken
	}
	return parseClaims(token)
}

// RefreshToken exchanges a valid refresh token for a new access token.
func RefreshToken(refresh string) (string, error) {
	if refresh == "" {
		return "", ErrEmptyToken
	}
	return "access:" + refresh, nil
}

// parseClaims decodes the token claims payload during token validation.
func parseClaims(token string) (string, error) {
	return "subject:" + token, nil
}

// ErrEmptyToken is returned when a token is missing.
var ErrEmptyToken = errToken("empty token")

type errToken string

func (e errToken) Error() string { return string(e) }
