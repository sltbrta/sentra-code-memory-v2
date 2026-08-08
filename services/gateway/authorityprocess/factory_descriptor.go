package authorityprocess

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
	broker "github.com/sltbrta/sentra-code-memory-v2/services/broker/localauthority"
)

// The Stage 05 approval descriptor is the operator-staged vault artifact one
// admitted ChangeIntent binds through its scope digest. The TUI admit CLI's
// embedded approval receipt is a placeholder: admission revalidates the
// descriptor against the authorized vault read — the descriptor's exact bytes
// must hash to the intent scope digest, its identities must equal every
// embedded intent fact, and the read itself must pass the current grant,
// policy, fence, and epoch chain. Every fact the kernel cannot carry in the
// frozen ChangeIntent — approved scope paths, the leaf decomposition, goals,
// edit directives, and the deterministic review plan — resolves from this
// ledger state, never from untrusted client bytes.

const (
	factoryDescriptorVersion     = "ouroboros.stage05.factory-approval.v1"
	factoryMaxDescriptorBytes    = 64 * 1024
	factoryMaxDescriptorPaths    = 64
	factoryMaxDescriptorLeaves   = 3
	factoryMaxDescriptorEdits    = 64
	factoryMaxDescriptorFindings = 16
	factoryMaxDescriptorText     = 2048
	factoryDescriptorProbeLength = 1
	factoryDescriptorIDPrefix    = "factory-descriptor-read"
)

var factoryNodeIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

type factoryDescriptorLeafEdit struct {
	Op      string `json:"op"`
	Path    string `json:"path"`
	OldPath string `json:"oldPath,omitempty"`
}

type factoryDescriptorLeaf struct {
	NodeID         string                      `json:"nodeId"`
	Goal           string                      `json:"goal"`
	OwnedPaths     []string                    `json:"ownedPaths"`
	ForbiddenPaths []string                    `json:"forbiddenPaths,omitempty"`
	Edits          []factoryDescriptorLeafEdit `json:"edits"`
}

type factoryDescriptorFinding struct {
	Severity    string `json:"severity"`
	Category    string `json:"category"`
	Summary     string `json:"summary"`
	Disposition string `json:"disposition"`
}

type factoryDescriptorApproval struct {
	ApprovalID            string `json:"approvalId"`
	ExpiresAtUnixSeconds  int64  `json:"expiresAtUnixSeconds"`
	RecordedAtUnixSeconds int64  `json:"recordedAtUnixSeconds"`
}

type factoryDescriptor struct {
	Version          string                     `json:"version"`
	IntentID         string                     `json:"intentId"`
	EvidenceRevision string                     `json:"evidenceRevision"`
	Approval         factoryDescriptorApproval  `json:"approval"`
	ScopePaths       []string                   `json:"scopePaths"`
	Review           bool                       `json:"review"`
	Leaves           []factoryDescriptorLeaf    `json:"leaves"`
	Findings         []factoryDescriptorFinding `json:"findings,omitempty"`
}

// errFactoryDescriptor marks any descriptor resolution failure; the adapter
// collapses it to the static non-disclosing denial at the port boundary.
var errFactoryDescriptor = errors.New("factory descriptor unavailable")

// resolveDescriptor reads and revalidates the approval descriptor bound to one
// intent: the authorized artifact read must pass the current grant, policy,
// fence, and epoch chain, the exact descriptor bytes must hash to the intent's
// scope digest, and every embedded intent fact must equal the ledger facts.
func (adapter *factoryKernelAdapter) resolveDescriptor(
	ctx context.Context, intent *contractsv1.ChangeIntent,
) (factoryDescriptor, error) {
	if intent == nil || intent.GetIntentId().GetValue() == "" ||
		len(intent.GetSupportingEvidence()) == 0 || intent.GetScopeDigest().GetHex() == "" {
		return factoryDescriptor{}, errFactoryDescriptor
	}
	evidence := intent.GetSupportingEvidence()[0]
	artifactID := evidence.GetEvidenceId().GetValue()
	if evidence.GetEvidenceId().GetNamespace() != "artifact" || artifactID == "" {
		return factoryDescriptor{}, errFactoryDescriptor
	}
	payload, err := adapter.readDescriptorArtifact(ctx, artifactID, intent.GetScopeDigest().GetHex())
	if err != nil {
		return factoryDescriptor{}, err
	}
	descriptor, err := parseFactoryDescriptor(payload)
	if err != nil {
		return factoryDescriptor{}, err
	}
	if err := revalidateDescriptor(descriptor, intent, payload, evidence); err != nil {
		return factoryDescriptor{}, err
	}
	return descriptor, nil
}

// parseFactoryDescriptor decodes the descriptor with strict field checking and
// bounded structural validation; semantic revalidation happens against the
// intent and the kernel.
func parseFactoryDescriptor(payload []byte) (factoryDescriptor, error) {
	if len(payload) == 0 || len(payload) > factoryMaxDescriptorBytes {
		return factoryDescriptor{}, errFactoryDescriptor
	}
	descriptor := factoryDescriptor{}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&descriptor); err != nil {
		return factoryDescriptor{}, errors.Join(errFactoryDescriptor, err)
	}
	if descriptor.Version != factoryDescriptorVersion || descriptor.IntentID == "" ||
		descriptor.EvidenceRevision == "" || descriptor.Approval.ApprovalID == "" ||
		len(descriptor.ScopePaths) == 0 || len(descriptor.ScopePaths) > factoryMaxDescriptorPaths ||
		len(descriptor.Leaves) == 0 || len(descriptor.Leaves) > factoryMaxDescriptorLeaves ||
		len(descriptor.Findings) > factoryMaxDescriptorFindings {
		return factoryDescriptor{}, errFactoryDescriptor
	}
	for _, path := range descriptor.ScopePaths {
		if !validFactoryDescriptorPath(path) {
			return factoryDescriptor{}, errFactoryDescriptor
		}
	}
	for _, leaf := range descriptor.Leaves {
		if !factoryNodeIDPattern.MatchString(leaf.NodeID) ||
			len(leaf.Goal) == 0 || len(leaf.Goal) > factoryMaxDescriptorText ||
			len(leaf.OwnedPaths) == 0 || len(leaf.OwnedPaths) > factoryMaxDescriptorPaths ||
			len(leaf.ForbiddenPaths) > factoryMaxDescriptorPaths ||
			len(leaf.Edits) == 0 || len(leaf.Edits) > factoryMaxDescriptorEdits {
			return factoryDescriptor{}, errFactoryDescriptor
		}
		for _, path := range append(append([]string(nil), leaf.OwnedPaths...), leaf.ForbiddenPaths...) {
			if !validFactoryDescriptorPath(path) {
				return factoryDescriptor{}, errFactoryDescriptor
			}
		}
		for _, edit := range leaf.Edits {
			switch edit.Op {
			case "add", "modify", "delete":
				if edit.OldPath != "" {
					return factoryDescriptor{}, errFactoryDescriptor
				}
			case "rename":
				if edit.OldPath == "" {
					return factoryDescriptor{}, errFactoryDescriptor
				}
			default:
				return factoryDescriptor{}, errFactoryDescriptor
			}
			if !validFactoryDescriptorPath(edit.Path) ||
				(edit.OldPath != "" && !validFactoryDescriptorPath(edit.OldPath)) {
				return factoryDescriptor{}, errFactoryDescriptor
			}
		}
	}
	for _, finding := range descriptor.Findings {
		if !validFactoryFindingVocabulary(finding) || len(finding.Summary) == 0 ||
			len(finding.Summary) > factoryMaxDescriptorText {
			return factoryDescriptor{}, errFactoryDescriptor
		}
	}
	return descriptor, nil
}

// revalidateDescriptor proves the ledger descriptor is exactly the approval
// the intent embeds: the digest binds the bytes, and every identity the
// client supplied must equal the ledger facts. Any divergence denies
// statically, so client bytes never substitute for ledger state.
func revalidateDescriptor(
	descriptor factoryDescriptor,
	intent *contractsv1.ChangeIntent,
	payload []byte,
	evidence *contractsv1.EvidenceRef,
) error {
	digest := sha256.Sum256(payload)
	scopeDigestHex := hex.EncodeToString(digest[:])
	approval := intent.GetApproval()
	if scopeDigestHex != intent.GetScopeDigest().GetHex() ||
		scopeDigestHex != approval.GetScopeDigest().GetHex() ||
		scopeDigestHex != approval.GetReceipt().GetConfigurationDigest().GetHex() ||
		descriptor.IntentID != intent.GetIntentId().GetValue() ||
		descriptor.EvidenceRevision != evidence.GetSourceRevisionId().GetValue() ||
		descriptor.Approval.ApprovalID != approval.GetApprovalId().GetValue() ||
		descriptor.Approval.ExpiresAtUnixSeconds != approval.GetExpiresAt().AsTime().Unix() ||
		descriptor.Approval.RecordedAtUnixSeconds != approval.GetReceipt().GetRecordedAt().AsTime().Unix() {
		return errFactoryDescriptor
	}
	return nil
}

func validFactoryDescriptorPath(path string) bool {
	if path == "" || len(path) > 512 || path[0] == '/' || path[len(path)-1] == '/' {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." || segment == ".git" {
			return false
		}
	}
	return true
}

func validFactoryFindingVocabulary(finding factoryDescriptorFinding) bool {
	switch finding.Severity {
	case "INFO", "MINOR", "MAJOR", "BLOCKER":
	default:
		return false
	}
	switch finding.Category {
	case "CORRECTNESS", "SECURITY", "DATA_INTEGRITY", "DOCS", "TESTS":
	default:
		return false
	}
	switch finding.Disposition {
	case "OPEN", "FIXED", "DISMISSED_WITH_EVIDENCE":
	default:
		return false
	}
	return true
}

// descriptorGrant resolves the manifest-issued read grant covering the
// descriptor artifact; the bootstrap manifest is the operator policy that
// authorizes the factory approval surface.
func (adapter *factoryKernelAdapter) descriptorGrant(artifactID string) (broker.Grant, bool) {
	if adapter == nil || adapter.config == nil {
		return broker.Grant{}, false
	}
	for _, issued := range adapter.config.IssuedGrants() {
		// Bootstrap grants pin the evidence namespace; the public ChangeIntent
		// and ArtifactRef still address the same id under the artifact
		// namespace on the wire.
		if issued.Action != "artifact.read" || issued.Evidence.Namespace != "evidence" ||
			issued.Evidence.Value != artifactID {
			continue
		}
		return broker.Grant{
			ID:              issued.ID,
			IDNamespace:     "grant",
			Principal:       broker.Identifier{Namespace: "principal", Value: adapter.config.Principal()},
			Tenant:          broker.Identifier{Namespace: "tenant", Value: adapter.config.Tenant()},
			PolicyDigest:    broker.Digest{Algorithm: "sha256", Hex: adapter.config.PolicyDigest()},
			Actions:         []string{issued.Action},
			Resources:       []broker.Identifier{{Namespace: issued.Evidence.Namespace, Value: issued.Evidence.Value}},
			Limits:          issued.Limits,
			Fence:           issued.Fence,
			RevocationEpoch: issued.RevocationEpoch,
			ExpiresAt:       issued.ExpiresAt,
			Nonce:           issued.Nonce,
		}, true
	}
	return broker.Grant{}, false
}

// readDescriptorArtifact hydrates the descriptor through the real Stage 02
// authorized artifact read: a one-byte probe resolves the canonical manifest
// length, then the exact bounded read hydrates and digest-reverifies the
// payload. Every read re-runs the current grant, capability, policy, fence,
// and epoch chain, so a revoked or superseded approval fails closed.
func (adapter *factoryKernelAdapter) readDescriptorArtifact(
	ctx context.Context, artifactID, scopeDigestHex string,
) ([]byte, error) {
	grant, found := adapter.descriptorGrant(artifactID)
	if !found {
		return nil, errFactoryDescriptor
	}
	probe, err := adapter.executeDescriptorRead(ctx, grant, artifactID, scopeDigestHex, factoryDescriptorProbeLength)
	if err != nil {
		return nil, err
	}
	length := probe.Artifact.Length
	clear(probe.Bytes)
	if length == 0 || length > factoryMaxDescriptorBytes {
		return nil, errFactoryDescriptor
	}
	result, err := adapter.executeDescriptorRead(ctx, grant, artifactID, scopeDigestHex, length)
	if err != nil {
		return nil, err
	}
	defer clear(result.Bytes)
	digest := sha256.Sum256(result.Bytes)
	if hex.EncodeToString(digest[:]) != scopeDigestHex {
		return nil, errFactoryDescriptor
	}
	return append([]byte(nil), result.Bytes...), nil
}

// factoryDescriptorReadCommandIDs derives the command ID and idempotency key
// for one authorized descriptor read. Both fold artifact ID, scope digest, and
// length so a revised descriptor under the same artifact ID never collides with
// a prior read's reservation (same length, different bytes).
func factoryDescriptorReadCommandIDs(artifactID, scopeDigestHex string, length uint64) (commandID, idempotencyKey string) {
	fingerprint := sha256.Sum256([]byte(fmt.Sprintf(
		"ouroboros.stage05.factory-descriptor-read.v1\x00%s\x00%s\x00%d", artifactID, scopeDigestHex, length)))
	token := hex.EncodeToString(fingerprint[:])
	return factoryDescriptorIDPrefix + "-" + token, factoryDescriptorIDPrefix + ":" + token
}

// executeDescriptorRead performs one authorized bounded artifact read through
// the durable runtime, mirroring the gateway command adapter's grant and
// authorization wiring exactly.
func (adapter *factoryKernelAdapter) executeDescriptorRead(
	ctx context.Context, grant broker.Grant, artifactID, scopeDigestHex string, length uint64,
) (brain.Result, error) {
	commandID, idempotencyKey := factoryDescriptorReadCommandIDs(artifactID, scopeDigestHex, length)
	fingerprint := sha256.Sum256([]byte(fmt.Sprintf(
		"ouroboros.stage05.factory-descriptor-read.v1\x00%s\x00%s\x00%d", artifactID, scopeDigestHex, length)))
	// Bootstrap artifact.read grants meter only bytes (never frames); usage
	// keys must match issued limits exactly.
	usage := map[string]uint64{"bytes": length}
	result, err := adapter.runtime.Execute(ctx, brain.ExecuteRequest{
		Identity: adapter.identity,
		Command: brain.Command{
			ID:             brain.Identifier{Namespace: "command", Value: commandID},
			Type:           "artifact.read",
			IdempotencyKey: idempotencyKey,
			PayloadDigest:  brain.Digest{Algorithm: "sha256", Hex: hex.EncodeToString(fingerprint[:])},
			Fence:          grant.Fence,
		},
		Artifact: brain.Artifact{
			ID:         brain.Identifier{Namespace: "artifact", Value: artifactID},
			Tenant:     adapter.identity.Tenant,
			Digest:     brain.Digest{Algorithm: "sha256", Hex: scopeDigestHex},
			Generation: 1,
			KeyEpoch:   adapter.keyEpoch,
		},
		Offset: 0,
		Length: length,
		Authorize: func(ctx context.Context, mapped brain.Identity, action string, resource brain.Identifier) (brain.Authorization, error) {
			// Runtime remaps artifact → evidence via evidenceID before this
			// callback; grant resources stay on the bootstrap evidence namespace.
			use := broker.NewUse(action, resource, grant.Fence, grant.RevocationEpoch, grant.Nonce, adapter.now())
			use.Usage = cloneUsage(usage)
			decision, err := adapter.broker.Authorize(ctx, mapped, grant, use)
			return brain.Authorization{
				Allowed: decision.Allowed, ReasonCode: decision.ReasonCode,
				RevocationEpoch: decision.RevocationEpoch,
			}, err
		},
	})
	if err != nil || result.Receipt.Status == "rejected" || !result.Authorization.Allowed {
		return brain.Result{}, errFactoryDescriptor
	}
	return result, nil
}
