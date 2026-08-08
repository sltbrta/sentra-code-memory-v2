# Keyring

This package resolves tenant-scoped current, historical, and legacy encryption
roots. Production roots live only in macOS Keychain; the package has no file or
environment fallback. Callers receive copied bytes only for the duration of a
cryptographic operation and must clear them after use.

`Memory` is a deterministic, concurrency-safe conformance adapter for tests and
isolated fixtures. It is not production persistence. Known unreadable epochs
return a typed fail-closed error so ArtifactVault can quarantine the affected
generation rather than substitute another key.

On Darwin, `DarwinKeychain` invokes the fixed `/usr/bin/security` binary with
explicit arguments. New material travels over stdin, never argv. Items are
create-only: exact-key retries succeed and conflicting material fails without
using Keychain overwrite. Tenant/key accounts use length-prefixed opaque
components. The adapter returns static errors and never includes command output
or secret material.

Acceptance label: `//services/brain/internal/keyring:keyring_test`.
