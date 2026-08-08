package productsec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Profile selects residual product ACL mode (SEC-007).
type Profile string

const (
	ProfileSingleUser     Profile = "single_user"
	ProfileMultiPrincipal Profile = "multi_principal"
)

// Context is the acting security context for product ask/ingest.
type Context struct {
	Profile   Profile         `json:"profile"`
	Principal string          `json:"principal"`
	Owner     string          `json:"owner"`
	Grants    map[string]bool `json:"grants,omitempty"` // principal → allowed
	BrainID   string          `json:"brain_id,omitempty"`
}

// ParseProfile returns ProfileSingleUser for empty/unknown lean defaults.
func ParseProfile(s string) Profile {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case string(ProfileMultiPrincipal), "multi", "acl":
		return ProfileMultiPrincipal
	default:
		return ProfileSingleUser
	}
}

// Authorize implements SEC-004/005: multi_principal authorize before corpus use.
// action is free-form (ask|ingest|export); deny is non-disclosing for multi.
func (c Context) Authorize(action string) error {
	_ = action
	if c.Profile == "" || c.Profile == ProfileSingleUser {
		return nil
	}
	p := strings.TrimSpace(c.Principal)
	if p == "" {
		return ErrDenied
	}
	owner := strings.TrimSpace(c.Owner)
	if owner != "" && p == owner {
		return nil
	}
	if c.Grants != nil && c.Grants[p] {
		return nil
	}
	return ErrDenied
}

// ErrDenied is the non-disclosing multi_principal failure (SEC-004).
var ErrDenied = fmt.Errorf("productsec: denied")

// BrainSecurity is durable security metadata under the brain dir.
type BrainSecurity struct {
	Profile Profile         `json:"profile"`
	Owner   string          `json:"owner"`
	Grants  map[string]bool `json:"grants,omitempty"`
	// EvidenceDigest is sha256 of primary chunks.jsonl (immutable check for gardener).
	EvidenceDigest string `json:"evidence_digest,omitempty"`
	// VaultCapable marks durability profile (SEC-001).
	VaultCapable bool `json:"vault_capable"`
}

const securityFile = "security.json"

// LoadSecurity reads brain security.json or defaults single_user owner=local.
func LoadSecurity(dir string) (BrainSecurity, error) {
	path := filepath.Join(dir, securityFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return BrainSecurity{
				Profile: ProfileSingleUser, Owner: "local", VaultCapable: true,
			}, nil
		}
		return BrainSecurity{}, err
	}
	var s BrainSecurity
	if err := json.Unmarshal(raw, &s); err != nil {
		return BrainSecurity{}, err
	}
	if s.Profile == "" {
		s.Profile = ProfileSingleUser
	}
	if s.Owner == "" {
		s.Owner = "local"
	}
	s.VaultCapable = true
	return s, nil
}

// SaveSecurity writes security.json (0600).
func SaveSecurity(dir string, s BrainSecurity) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	s.VaultCapable = true
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, securityFile), raw, 0o600)
}

// DigestFile returns hex sha256 of path contents (empty file → empty digest).
func DigestFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// UpdateEvidenceDigest recomputes digest of chunks.jsonl into security.json.
func UpdateEvidenceDigest(dir string) (string, error) {
	s, err := LoadSecurity(dir)
	if err != nil {
		return "", err
	}
	d, err := DigestFile(filepath.Join(dir, "chunks.jsonl"))
	if err != nil {
		return "", err
	}
	s.EvidenceDigest = d
	if err := SaveSecurity(dir, s); err != nil {
		return "", err
	}
	return d, nil
}

// ContextFromBrain builds a Context for a principal against brain security.
func ContextFromBrain(dir, principal string, profileOverride Profile) (Context, error) {
	s, err := LoadSecurity(dir)
	if err != nil {
		return Context{}, err
	}
	prof := s.Profile
	if profileOverride != "" {
		prof = profileOverride
	}
	p := principal
	if p == "" {
		p = s.Owner
	}
	return Context{
		Profile: prof, Principal: p, Owner: s.Owner, Grants: s.Grants,
	}, nil
}
