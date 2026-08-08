package factory

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
)

// identity derives a deterministic opaque identity from a domain separator and
// length-prefixed fields, mirroring the Stage 03 authority identity chains.
func identity(domain string, fields ...string) string {
	hasher := sha256.New()
	writeIdentityField(hasher, domain)
	for _, field := range fields {
		writeIdentityField(hasher, field)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// digestBytes returns the canonical lowercase hex SHA-256 of one byte payload.
func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func writeIdentityField(hasher hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = hasher.Write(size[:])
	_, _ = hasher.Write([]byte(value))
}

func isHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func isGitOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// validPrincipalID mirrors the Identifier value boundary for principal and
// worker identities carried into canonical rows: non-empty, bounded, and free
// of control characters. Validating at the kernel surface keeps malformed
// identities from failing deep inside a committing transaction after payload
// staging.
func validPrincipalID(value string) bool {
	if value == "" || len(value) > 512 {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}
