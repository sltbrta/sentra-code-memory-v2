package codeindex

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
)

func contentDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func fileReceipt(projection FileProjection) string {
	digest := sha256.New()
	writeDigestText(digest, projectionVersion)
	writeDigestText(digest, projection.Path)
	writeDigestText(digest, string(projection.Language))
	writeDigestText(digest, projection.ContentDigest)
	writeDigestText(digest, string(projection.Coverage))
	writeDigestText(digest, projection.DegradationCode)
	writeDigestUint(digest, uint64(len(projection.Occurrences)))
	for _, occurrence := range projection.Occurrences {
		writeDigestText(digest, string(occurrence.Language))
		writeDigestText(digest, string(occurrence.Kind))
		writeDigestText(digest, occurrence.Text)
		writeDigestText(digest, occurrence.Range.Path)
		writeDigestUint(digest, uint64(occurrence.Range.Start.Line))
		writeDigestUint(digest, uint64(occurrence.Range.Start.Column))
		writeDigestUint(digest, uint64(occurrence.Range.End.Line))
		writeDigestUint(digest, uint64(occurrence.Range.End.Column))
		writeDigestText(digest, occurrence.ContentDigest)
		writeDigestText(digest, string(occurrence.Coverage))
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

func snapshotReceipt(files []FileProjection) string {
	digest := sha256.New()
	writeDigestText(digest, projectionVersion)
	writeDigestText(digest, "snapshot")
	writeDigestUint(digest, uint64(len(files)))
	for _, file := range files {
		writeDigestText(digest, file.Path)
		writeDigestText(digest, file.ReceiptDigest)
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

func writeDigestText(digest hash.Hash, value string) {
	writeDigestUint(digest, uint64(len(value)))
	_, _ = digest.Write([]byte(value))
}

func writeDigestUint(digest hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = digest.Write(encoded[:])
}
