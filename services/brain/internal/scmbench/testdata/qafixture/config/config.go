// Package config loads runtime configuration for the qafixture server. It is
// indexed, never built.
package config

// Config aggregates the server, database, and auth settings.
type Config struct {
	Addr        string
	DSN         string
	TokenIssuer string
	QueryLimit  int
	LogLevel    string
}

// Default returns the baseline configuration used when no file is present.
func Default() Config {
	return Config{
		Addr:        "127.0.0.1:8080",
		DSN:         "memory://",
		TokenIssuer: "qafixture",
		QueryLimit:  100,
		LogLevel:    "info",
	}
}

// Validate checks that the configuration is internally consistent before the
// server starts.
func (c Config) Validate() error {
	if c.Addr == "" {
		return ErrMissingAddr
	}
	if c.DSN == "" {
		return ErrMissingDSN
	}
	if c.QueryLimit <= 0 {
		return ErrBadQueryLimit
	}
	return nil
}

// Configuration errors surfaced at startup.
var (
	ErrMissingAddr   = cfgErr("missing listen address")
	ErrMissingDSN    = cfgErr("missing database dsn")
	ErrBadQueryLimit = cfgErr("query limit must be positive")
)

type cfgErr string

func (e cfgErr) Error() string { return string(e) }
