package multimodal

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"unicode/utf8"
)

func identity(domain string, fields ...string) string {
	hasher := sha256.New()
	writeIdentityField(hasher, domain)
	for _, field := range fields {
		writeIdentityField(hasher, field)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func digestText(value string) string {
	return digestBytes([]byte(value))
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

func validBoundedID(value string) bool {
	if value == "" || len(value) > 512 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validIdentity(identity Identity) bool {
	return validBoundedID(identity.Tenant) &&
		validBoundedID(identity.Principal) &&
		validBoundedID(identity.Session)
}
