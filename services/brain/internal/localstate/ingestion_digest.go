// This file computes canonical authenticated payload digests for Stage 03
// ingestion mutations. Length-delimited fields and sorted set-like collections
// make operation identity unambiguous and stable across runtime restarts.
package localstate

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"sort"
	"strconv"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// GenerationPublicationDigest returns the canonical SHA-256 payload digest a
// caller must place in Command.AuthenticatedDigest before PublishGeneration.
// It hashes every authority-relevant field except command-envelope identity.
func GenerationPublicationDigest(publication GenerationPublication) contracts.Digest {
	digester := newCanonicalDigester("ouroboros.stage03.generation-publication.v1")
	digester.scope(publication.Scope)
	digester.fields(
		publication.Source.RepositoryID,
		publication.Source.ConfigurationDigest,
		publication.Source.IgnorePolicyDigest,
		publication.Source.ApprovedRootID,
		strconv.FormatUint(publication.Source.ACLEpoch, 10),
		publication.Snapshot.SnapshotID,
		publication.Snapshot.CommitOID,
		publication.Snapshot.TreeOID,
		publication.Snapshot.PolicyDigest,
		publication.Snapshot.SnapshotDigest,
		publication.GenerationID,
		strconv.FormatUint(publication.Sequence, 10),
		publication.ExpectedCurrentGenerationID,
		publication.State,
		strconv.FormatUint(publication.SourceWatermark, 10),
	)
	revisions := append([]IngestionRevisionMetadata(nil), publication.Revisions...)
	sort.Slice(revisions, func(left, right int) bool {
		if revisions[left].RevisionID == revisions[right].RevisionID {
			return revisions[left].SourceObjectID < revisions[right].SourceObjectID
		}
		return revisions[left].RevisionID < revisions[right].RevisionID
	})
	digester.field(strconv.Itoa(len(revisions)))
	for _, revision := range revisions {
		digester.fields(
			revision.RevisionID,
			revision.SourceObjectID,
			revision.PathDigest,
			revision.GitBlobOID,
			revision.ContentDigest,
			strconv.FormatInt(revision.ByteLength, 10),
			revision.EntryKind,
			revision.MediaType,
			revision.Language,
			revision.PredecessorRevisionID,
		)
	}
	readiness := append([]IngestionReadiness(nil), publication.Readiness...)
	sort.Slice(readiness, func(left, right int) bool { return readiness[left].Language < readiness[right].Language })
	digester.field(strconv.Itoa(len(readiness)))
	for _, lane := range readiness {
		digester.fields(lane.Language, lane.Coverage, lane.ReasonCode)
	}
	return digester.digest()
}

// IngestionRevocationDigest returns the canonical SHA-256 payload digest a
// caller must place in Command.AuthenticatedDigest before source revocation.
func IngestionRevocationDigest(request IngestionRevocation) contracts.Digest {
	digester := newCanonicalDigester("ouroboros.stage03.ingestion-revocation.v1")
	digester.scope(request.Scope)
	digester.fields(
		request.ExpectedCurrentGenerationID,
		strconv.FormatUint(request.RevocationEpoch, 10),
		request.ReasonCode,
	)
	return digester.digest()
}

func authenticatedDigestMatches(actual, expected contracts.Digest) bool {
	return actual.Algorithm == "sha256" && expected.Algorithm == "sha256" &&
		subtle.ConstantTimeCompare([]byte(actual.Hex), []byte(expected.Hex)) == 1
}

type canonicalDigester struct{ hash hash.Hash }

func newCanonicalDigester(domain string) *canonicalDigester {
	digester := &canonicalDigester{hash: sha256.New()}
	digester.field(domain)
	return digester
}

func (digester *canonicalDigester) scope(scope IngestionScope) {
	digester.fields(
		scope.Tenant.Namespace,
		scope.Tenant.Value,
		scope.Brain.Namespace,
		scope.Brain.Value,
		scope.SourceID,
	)
}

func (digester *canonicalDigester) fields(values ...string) {
	for _, value := range values {
		digester.field(value)
	}
}

func (digester *canonicalDigester) field(value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = digester.hash.Write(length[:])
	_, _ = digester.hash.Write([]byte(value))
}

func (digester *canonicalDigester) digest() contracts.Digest {
	return contracts.Digest{Algorithm: "sha256", Hex: hex.EncodeToString(digester.hash.Sum(nil))}
}
