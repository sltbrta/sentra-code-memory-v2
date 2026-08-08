package localstorage

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/artifactvault"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/evidenceledger"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

const (
	maxIdentifierBytes = 512
	maxLocatorBytes    = 1024
	maxArtifactBytes   = 1 << 40
	maxFrameBytes      = 16 * 1024 * 1024
	maxFrameCount      = 1_048_576
)

func validID(id contracts.Identifier, namespace string) bool {
	return id.Namespace == namespace && id.Value != "" && len(id.Value) <= maxIdentifierBytes &&
		!strings.ContainsRune(id.Value, '\x00')
}

func validLocator(locator string) bool {
	return locator != "" && len(locator) <= maxLocatorBytes && !strings.ContainsRune(locator, '\x00')
}

func validDigest(digest contracts.Digest) bool {
	if digest.Algorithm != "sha256" || len(digest.Hex) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(digest.Hex)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == digest.Hex
}

func validManifest(manifest contracts.ArtifactManifest) bool {
	return validID(manifest.Tenant, "tenant") && validID(manifest.Artifact, "artifact") &&
		validDigest(manifest.Digest) && manifest.Generation > 0 && manifest.KeyEpoch > 0 &&
		manifest.Length > 0 && manifest.Length <= maxArtifactBytes && manifest.FrameCount > 0 &&
		manifest.FrameCount <= maxFrameCount
}

func validFrames(manifest contracts.ArtifactManifest, frames []artifactvault.FrameRecord) bool {
	if len(frames) != int(manifest.FrameCount) {
		return false
	}
	var offset uint64
	for index, frame := range frames {
		if frame.Index != uint32(index) || frame.Offset != offset || frame.Length == 0 ||
			frame.Length > maxFrameBytes || !validDigest(frame.ObjectDigest) {
			return false
		}
		offset += uint64(frame.Length)
	}
	return offset == manifest.Length
}

func sameManifest(left, right contracts.ArtifactManifest) bool {
	return left == right
}

func validEvidence(record evidenceledger.Record) bool {
	return validID(record.Tenant, "tenant") && validID(record.Brain, "brain") &&
		validID(record.Evidence, "evidence") && validID(record.Artifact, "artifact") &&
		record.Generation > 0 && record.Anchor != "" && len(record.Anchor) <= 4096 &&
		!strings.ContainsRune(record.Anchor, '\x00') && validDigest(record.Digest)
}

func validLineage(edge evidenceledger.Lineage) bool {
	return validID(edge.Tenant, "tenant") && validID(edge.Brain, "brain") &&
		validID(edge.Parent, "evidence") && validID(edge.Child, "evidence") &&
		edge.Parent != edge.Child && edge.Relation != "" && len(edge.Relation) <= 128 &&
		!strings.ContainsRune(edge.Relation, '\x00')
}
