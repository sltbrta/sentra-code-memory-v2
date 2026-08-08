// Package localbootstrap loads the pinned, owner-only Stage 02 local runtime
// manifest. It validates filesystem custody, strict JSON, bounded authority
// facts, and deterministic configuration and policy digests before returning
// an immutable normalized configuration.
package localbootstrap
