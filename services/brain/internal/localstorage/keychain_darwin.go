//go:build darwin

package localstorage

import "github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/keyring"

// Keep the secret-material adapter one-way: SQLite supplies references and the
// existing Darwin resolver alone loads their Keychain values.
var _ keyring.ReferenceSource = (*KeyReferences)(nil)
