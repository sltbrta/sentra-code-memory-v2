package roster

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

func writeIdentityField(hasher hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = hasher.Write(size[:])
	_, _ = hasher.Write([]byte(value))
}
