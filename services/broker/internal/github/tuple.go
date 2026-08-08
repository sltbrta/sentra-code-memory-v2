package github

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

const (
	// HeadRefPrefix is the deterministic branch namespace for Tracer 001.
	HeadRefPrefix = "refs/heads/ouroboros/tracer-001/"
	// TupleDomain binds canonical publication-tuple digests.
	TupleDomain = "ouroboros.stage06.github.publication-tuple.v1"
	// ContentDomain binds title/body digests.
	ContentDomain = "ouroboros.stage06.github.pr-content.v1"
)

// HeadRef returns the deterministic head ref for one publication tuple:
// refs/heads/ouroboros/tracer-001/<first-24-lowercase-hex-of-SHA256(canonical-tuple)>.
func HeadRef(tuple PublicationTuple) string {
	digest := TupleDigest(tuple)
	return HeadRefPrefix + digest.Hex[:24]
}

// TupleDigest binds the immutable publication tuple.
func TupleDigest(tuple PublicationTuple) contracts.Digest {
	type projection struct {
		Domain               string `json:"domain"`
		TenantID             string `json:"tenantId"`
		InstallationID       string `json:"installationId"`
		RepositoryOwner      string `json:"repositoryOwner"`
		RepositoryName       string `json:"repositoryName"`
		BaseRef              string `json:"baseRef"`
		BaseCommitOID        string `json:"baseCommitOid"`
		HeadCommitOID        string `json:"headCommitOid"`
		ChangeSetDigest      string `json:"changeSetDigest"`
		EffectApprovalDigest string `json:"effectApprovalDigest"`
		PolicyDigest         string `json:"policyDigest"`
		ConfigDigest         string `json:"configDigest"`
	}
	payload, _ := json.Marshal(projection{
		Domain:               TupleDomain,
		TenantID:             tuple.TenantID,
		InstallationID:       tuple.InstallationID,
		RepositoryOwner:      tuple.RepositoryOwner,
		RepositoryName:       tuple.RepositoryName,
		BaseRef:              tuple.BaseRef,
		BaseCommitOID:        tuple.BaseCommitOID,
		HeadCommitOID:        tuple.HeadCommitOID,
		ChangeSetDigest:      tuple.ChangeSetDigest.Hex,
		EffectApprovalDigest: tuple.EffectApprovalDigest.Hex,
		PolicyDigest:         tuple.PolicyDigest.Hex,
		ConfigDigest:         tuple.ConfigDigest.Hex,
	})
	return digestBytes(payload)
}

// ContentDigest binds deterministic title/body bytes.
func ContentDigest(content PRContent) contracts.Digest {
	type projection struct {
		Domain string `json:"domain"`
		Title  string `json:"title"`
		Body   string `json:"body"`
	}
	payload, _ := json.Marshal(projection{
		Domain: ContentDomain,
		Title:  content.Title,
		Body:   content.Body,
	})
	return digestBytes(payload)
}

// RepositoryFullName returns owner/name.
func RepositoryFullName(tuple PublicationTuple) string {
	return tuple.RepositoryOwner + "/" + tuple.RepositoryName
}

// BranchName strips the refs/heads/ prefix from a head ref.
func BranchName(headRef string) string {
	return strings.TrimPrefix(headRef, "refs/heads/")
}

func digestBytes(content []byte) contracts.Digest {
	sum := sha256.Sum256(content)
	return contracts.Digest{Algorithm: "sha256", Hex: hex.EncodeToString(sum[:])}
}

func validGitOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return false
	}
	return true
}

func validDigest(d contracts.Digest) bool {
	if d.Algorithm != "sha256" || len(d.Hex) != 64 {
		return false
	}
	for _, character := range d.Hex {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return false
	}
	return true
}

func validateTuple(tuple PublicationTuple) error {
	if tuple.TenantID == "" || tuple.RepositoryOwner == "" || tuple.RepositoryName == "" {
		return fmt.Errorf("%w: repository identity incomplete", ErrInvalidInput)
	}
	if tuple.BaseRef == "" {
		return fmt.Errorf("%w: base ref missing", ErrInvalidInput)
	}
	if !validGitOID(tuple.BaseCommitOID) || !validGitOID(tuple.HeadCommitOID) {
		return fmt.Errorf("%w: commit oid malformed", ErrInvalidInput)
	}
	if !validDigest(tuple.ChangeSetDigest) || !validDigest(tuple.EffectApprovalDigest) ||
		!validDigest(tuple.PolicyDigest) || !validDigest(tuple.ConfigDigest) {
		return fmt.Errorf("%w: digest malformed", ErrInvalidInput)
	}
	return nil
}

func containsAction(actions []string, want string) bool {
	for _, action := range actions {
		if action == want {
			return true
		}
	}
	return false
}

func hasForbiddenAction(actions []string) bool {
	for _, action := range actions {
		for _, forbidden := range ForbiddenActions {
			if action == forbidden {
				return true
			}
		}
		lower := strings.ToLower(action)
		if strings.Contains(lower, "merge") || strings.Contains(lower, "deploy") ||
			strings.Contains(lower, "force_push") || strings.Contains(lower, "promote") {
			return true
		}
	}
	return false
}
