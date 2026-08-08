// Package audit computes and verifies tenant-scoped hash-linked audit chains.
// The chain binds each canonical event digest and sequence to its predecessor.
package audit

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
)

var ErrCorrupt = errors.New("audit: hash chain corrupt")

// Entry contains the minimum immutable facts required to verify one audit link.
type Entry struct {
	Metadata EventMetadata
	Previous string
	Digest   string
}

// EventMetadata contains every canonical event column bound by the audit chain.
type EventMetadata struct {
	Sequence         uint64
	EventID          string
	Tenant           string
	AggregateType    string
	AggregateID      string
	AggregateVersion uint64
	CommandID        string
	PayloadDigest    string
	OccurredAtMs     int64
}

// Next returns the SHA-256 audit digest for one canonical event link.
// An empty previous digest is valid only for the first link chosen by the caller.
func Next(metadata EventMetadata, previous string) (string, error) {
	if !validMetadata(metadata) {
		return "", ErrCorrupt
	}
	material := fmt.Sprintf("v1\x00%d\x00%s\x00%s\x00%s\x00%s\x00%d\x00%s\x00%s\x00%d\x00%s",
		metadata.Sequence, metadata.EventID, metadata.Tenant, metadata.AggregateType, metadata.AggregateID,
		metadata.AggregateVersion, metadata.CommandID, metadata.PayloadDigest, metadata.OccurredAtMs, previous)
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:]), nil
}

// Verify checks link order, predecessor equality, and every recomputed digest.
func Verify(entries []Entry) error {
	previous := ""
	for index, entry := range entries {
		if index > 0 && entry.Metadata.Sequence <= entries[index-1].Metadata.Sequence {
			return ErrCorrupt
		}
		if subtle.ConstantTimeCompare([]byte(entry.Previous), []byte(previous)) != 1 {
			return ErrCorrupt
		}
		digest, err := Next(entry.Metadata, previous)
		if err != nil || subtle.ConstantTimeCompare([]byte(entry.Digest), []byte(digest)) != 1 {
			return ErrCorrupt
		}
		previous = entry.Digest
	}
	return nil
}

func validMetadata(metadata EventMetadata) bool {
	return metadata.Sequence > 0 && metadata.EventID != "" && metadata.Tenant != "" &&
		metadata.AggregateType != "" && metadata.AggregateID != "" && metadata.AggregateVersion > 0 &&
		metadata.CommandID != "" && metadata.PayloadDigest != "" && metadata.OccurredAtMs > 0
}
