package authorityprocess

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"math"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
)

const operationFingerprintPurpose = "ouroboros.local-authority.operation.v1"

// OperationFingerprint binds the authenticated principal and tenant, command
// type and fence, and every typed artifact-operation fact. Session is omitted
// deliberately so an exact retry may resume under a newly authenticated
// session. The encoding is purpose-prefixed and length-delimits every string;
// integers use fixed-width big-endian encoding and booleans use one byte.
func OperationFingerprint(
	identity brain.Identity,
	request *contractsv1.ExecuteAuthorityCommandRequest,
) (brain.Digest, error) {
	if request == nil || request.Command == nil || request.Command.Causal == nil {
		return brain.Digest{}, errRequestDenied
	}
	encoder := fingerprintEncoder{hash: sha256.New()}
	if !encoder.string(operationFingerprintPurpose) ||
		!encoder.identifier(identity.Tenant.Namespace, identity.Tenant.Value) ||
		!encoder.identifier(identity.Principal.Namespace, identity.Principal.Value) ||
		!encoder.string(request.Command.CommandType) {
		return brain.Digest{}, errRequestDenied
	}
	encoder.uint64(request.Command.Causal.Fence)

	switch operation := request.ArtifactCommand.(type) {
	case *contractsv1.ExecuteAuthorityCommandRequest_ArtifactAdmit:
		if operation.ArtifactAdmit == nil || request.Command.CommandType != "artifact.admit" ||
			!encoder.string("artifact.admit") || !encoder.artifact(operation.ArtifactAdmit.Artifact) {
			return brain.Digest{}, errRequestDenied
		}
		encoder.uint64(operation.ArtifactAdmit.ExpectedGeneration)
		encoder.uint64(operation.ArtifactAdmit.DeclaredLength)
		encoder.uint32(operation.ArtifactAdmit.FrameCount)
	case *contractsv1.ExecuteAuthorityCommandRequest_ArtifactRead:
		if operation.ArtifactRead == nil || request.Command.CommandType != "artifact.read" ||
			!encoder.string("artifact.read") || !encoder.artifact(operation.ArtifactRead.Artifact) {
			return brain.Digest{}, errRequestDenied
		}
		encoder.uint64(operation.ArtifactRead.Generation)
		encoder.uint64(operation.ArtifactRead.Offset)
		encoder.uint64(operation.ArtifactRead.Length)
	case *contractsv1.ExecuteAuthorityCommandRequest_ArtifactDelete:
		if operation.ArtifactDelete == nil || request.Command.CommandType != "artifact.delete" ||
			!encoder.string("artifact.delete") || !encoder.artifact(operation.ArtifactDelete.Artifact) {
			return brain.Digest{}, errRequestDenied
		}
		encoder.uint64(operation.ArtifactDelete.ExpectedGeneration)
		encoder.boolean(operation.ArtifactDelete.PurgeAfterTombstone)
	default:
		return brain.Digest{}, errRequestDenied
	}
	return brain.Digest{Algorithm: "sha256", Hex: hex.EncodeToString(encoder.hash.Sum(nil))}, nil
}

func matchesFingerprint(supplied *contractsv1.Digest, computed brain.Digest) bool {
	if supplied == nil || supplied.Algorithm != computed.Algorithm || len(supplied.Hex) != sha256.Size*2 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(supplied.Hex), []byte(computed.Hex)) == 1
}

type fingerprintEncoder struct{ hash hash.Hash }

func (encoder fingerprintEncoder) string(value string) bool {
	if len(value) > math.MaxUint32 {
		return false
	}
	encoder.uint32(uint32(len(value)))
	_, _ = encoder.hash.Write([]byte(value))
	return true
}

func (encoder fingerprintEncoder) identifier(namespace, value string) bool {
	return encoder.string(namespace) && encoder.string(value)
}

func (encoder fingerprintEncoder) artifact(value *contractsv1.ArtifactRef) bool {
	return value != nil && value.ArtifactId != nil && value.TenantId != nil && value.ContentDigest != nil &&
		encoder.identifier(value.TenantId.Namespace, value.TenantId.Value) &&
		encoder.identifier(value.ArtifactId.Namespace, value.ArtifactId.Value) &&
		encoder.string(value.ContentDigest.Algorithm) && encoder.string(value.ContentDigest.Hex)
}

func (encoder fingerprintEncoder) uint64(value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = encoder.hash.Write(encoded[:])
}

func (encoder fingerprintEncoder) uint32(value uint32) {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	_, _ = encoder.hash.Write(encoded[:])
}

func (encoder fingerprintEncoder) boolean(value bool) {
	encoded := byte(0)
	if value {
		encoded = 1
	}
	_, _ = encoder.hash.Write([]byte{encoded})
}
