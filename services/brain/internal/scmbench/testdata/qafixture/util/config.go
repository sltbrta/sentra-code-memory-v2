package util

// Config holds runtime configuration. It references token and query settings
// as plain configuration values (a lexical distractor for retrieval probes).
type Config struct {
	TokenName  string
	QueryLimit int
	LogLevel   string
}

// LoadConfig returns the default runtime configuration.
func LoadConfig() Config {
	return Config{TokenName: "access_token", QueryLimit: 100, LogLevel: "info"}
}
