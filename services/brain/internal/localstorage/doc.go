// Package localstorage supplies durable SQLite adapters for Stage 2 artifact,
// evidence, and key-reference ports. It shares the frozen authority schema but
// never becomes a command authority and never persists artifact or key bytes.
package localstorage
