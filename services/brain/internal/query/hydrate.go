package query

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeindex"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ingestion"
)

// errIntegrity marks a canonical hydration or citation verification failure:
// the hydrated bytes, manifest facts, or proposed anchor disagree, so the
// affected evidence is discarded and disclosed as citation_verification_failed.
var errIntegrity = errors.New("query: citation verification failed")

// hydrateEntry binds one selected definition occurrence to its canonical
// bytes. Every manifest fact is reverified against the hydrated content — the
// projection is a hint, never evidence — so a projection/manifest/content
// disagreement discards the entry instead of serving it.
func hydrateEntry(
	snapshot Snapshot,
	file codeindex.FileProjection,
	occurrence codeindex.Occurrence,
	maxBlockLines int,
) (EvidenceEntry, error) {
	var revision *ingestion.FileRevision
	for index := range snapshot.Revisions {
		if snapshot.Revisions[index].Path == file.Path {
			revision = &snapshot.Revisions[index]
			break
		}
	}
	if revision == nil {
		return EvidenceEntry{}, fmt.Errorf("%w: %s has no canonical revision", errIntegrity, file.Path)
	}
	hydrated, exists := snapshot.Projection.Files[file.Path]
	if !exists {
		return EvidenceEntry{}, fmt.Errorf("%w: %s has no hydrated bytes", errIntegrity, file.Path)
	}
	sum := sha256.Sum256(hydrated.Content)
	contentDigest := hex.EncodeToString(sum[:])
	if contentDigest != revision.ContentDigest {
		return EvidenceEntry{}, fmt.Errorf("%w: %s content digest mismatch", errIntegrity, file.Path)
	}
	if gitBlobOID(hydrated.Content) != revision.BlobOID {
		return EvidenceEntry{}, fmt.Errorf("%w: %s blob identity mismatch", errIntegrity, file.Path)
	}
	if file.ContentDigest != "sha256:"+contentDigest {
		return EvidenceEntry{}, fmt.Errorf("%w: %s projection digest mismatch", errIntegrity, file.Path)
	}
	block := blockLines(string(hydrated.Content), occurrence.Range.Start.Line, maxBlockLines)
	if len(block) == 0 {
		return EvidenceEntry{}, fmt.Errorf("%w: %s block outside hydrated content", errIntegrity, file.Path)
	}
	return EvidenceEntry{
		Path:           file.Path,
		Language:       string(file.Language),
		RevisionID:     revision.RevisionID,
		BlobOID:        revision.BlobOID,
		ContentDigest:  contentDigest,
		BlockStartLine: occurrence.Range.Start.Line,
		Lines:          block,
		DefinitionText: occurrence.Text,
	}, nil
}

// gitBlobOID recomputes the canonical Git blob identity for hydrated bytes.
func gitBlobOID(content []byte) string {
	sum := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(content))), content...))
	return hex.EncodeToString(sum[:])
}

// resolveSupportingText selects the exact hydrated bytes one proposed
// one-based half-open citation range names within its evidence entry, and
// computes their canonical SHA-256 digest. Multi-line ranges join selected
// lines with a single newline, matching the hydrated content exactly. All
// bounds are compared in the uint32/uint64 domain so extreme coordinates can
// never overflow into a passing check on any architecture.
func resolveSupportingText(entry EvidenceEntry, proposed ProposedCitation) ([]byte, string, error) {
	if proposed.StartLine == 0 || proposed.StartColumn == 0 || proposed.EndLine == 0 || proposed.EndColumn == 0 ||
		proposed.StartLine > proposed.EndLine ||
		(proposed.StartLine == proposed.EndLine && proposed.StartColumn >= proposed.EndColumn) {
		return nil, "", fmt.Errorf("%w: non-forward range", errIntegrity)
	}
	blockEnd := uint64(entry.BlockStartLine) + uint64(len(entry.Lines)) - 1
	if len(entry.Lines) == 0 || uint64(proposed.StartLine) < uint64(entry.BlockStartLine) ||
		uint64(proposed.EndLine) > blockEnd {
		return nil, "", fmt.Errorf("%w: range outside hydrated block", errIntegrity)
	}
	first := entry.Lines[proposed.StartLine-entry.BlockStartLine]
	last := entry.Lines[proposed.EndLine-entry.BlockStartLine]
	if uint64(proposed.StartColumn) > uint64(len(first))+1 ||
		uint64(proposed.EndColumn) > uint64(len(last))+1 {
		return nil, "", fmt.Errorf("%w: column outside hydrated line", errIntegrity)
	}
	var selected string
	if proposed.StartLine == proposed.EndLine {
		selected = first[proposed.StartColumn-1 : proposed.EndColumn-1]
	} else {
		var builder strings.Builder
		builder.WriteString(first[proposed.StartColumn-1:])
		for line := proposed.StartLine + 1; line < proposed.EndLine; line++ {
			builder.WriteString("\n")
			builder.WriteString(entry.Lines[line-entry.BlockStartLine])
		}
		builder.WriteString("\n")
		builder.WriteString(last[:proposed.EndColumn-1])
		selected = builder.String()
	}
	sum := sha256.Sum256([]byte(selected))
	return []byte(selected), hex.EncodeToString(sum[:]), nil
}

// evidenceID derives the opaque, deterministic evidence identity for one
// verified citation: the same pinned evidence and range always produce the
// same identity, and different ranges of one revision never collide.
func evidenceID(entry EvidenceEntry, proposed ProposedCitation) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf(
		"ouroboros.stage04.evidence.v1\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d",
		entry.RevisionID, entry.Path,
		proposed.StartLine, proposed.StartColumn, proposed.EndLine, proposed.EndColumn,
	)))
	return hex.EncodeToString(sum[:])
}
