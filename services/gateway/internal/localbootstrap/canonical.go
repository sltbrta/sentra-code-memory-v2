package localbootstrap

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"strconv"
)

func policyDigest(config *Config) string {
	digest := sha256.New()
	writeCanonical(digest, "ouroboros.policy.v1")
	writeCanonical(digest, config.manifest.Tenant)
	writeCanonical(digest, config.manifest.Principal)
	writeCanonical(digest, config.manifest.Brain)
	writeCanonical(digest, strconv.FormatUint(config.manifest.RevocationEpoch, 10))
	for _, relationship := range config.relationships {
		writeCanonical(digest, relationship.Object)
		writeCanonical(digest, relationship.Relation)
		writeCanonical(digest, relationship.User)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func writeCanonical(destination hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	// hash.Hash.Write is specified to return a nil error; only byte count is irrelevant here.
	_, _ = destination.Write(length[:])
	_, _ = destination.Write([]byte(value))
}
