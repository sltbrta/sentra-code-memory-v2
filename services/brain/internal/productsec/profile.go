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
	Grants    map[string]bool `json:"grants,omitempty"` // principal → allowed (all actions)
	// ActionGrants is principal → action → allowed, the read-only-expressible form.
	ActionGrants map[string]map[string]bool `json:"action_grants,omitempty"`
	BrainID      string                     `json:"brain_id,omitempty"`
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

// Authorize implements SEC-004/005: multi_principal authorize before corpus
// use. action is one of ask, ingest, export; deny is non-disclosing.
//
// The action argument used to be discarded (`_ = action`), so a principal
// granted read access was equally granted ingest and export and a read-only
// grant could not be expressed. It is now honoured: ActionGrants, when it
// names the principal, decides per action; a bare Grants entry still admits
// every action, which is what existing security.json files mean.
func (c Context) Authorize(action string) error {
	if c.Profile == "" || c.Profile == ProfileSingleUser {
		return nil
	}
	action = strings.ToLower(strings.TrimSpace(action))
	if action == "" {
		// An unnamed action cannot be checked against a grant, and admitting it
		// would reintroduce the hole this closes.
		return ErrDenied
	}
	p := strings.TrimSpace(c.Principal)
	if p == "" {
		return ErrDenied
	}
	owner := strings.TrimSpace(c.Owner)
	if owner != "" && p == owner {
		return nil
	}
	if actions, ok := c.ActionGrants[p]; ok {
		if actions[action] {
			return nil
		}
		return ErrDenied
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
	Profile Profile `json:"profile"`
	Owner   string  `json:"owner"`
	// Grants admits a principal for every action. It is the coarse form kept
	// for compatibility; ActionGrants is the finer one.
	Grants map[string]bool `json:"grants,omitempty"`
	// ActionGrants admits a principal for named actions only (ask, ingest,
	// export). When a principal appears here, this decides and Grants is not
	// consulted for them -- so a read-only grant is expressible, which it was
	// not while Authorize discarded its action argument entirely.
	ActionGrants map[string]map[string]bool `json:"action_grants,omitempty"`
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
			// A brand-new brain has no security.json yet, and defaulting to
			// single_user is what makes the local-first path work. But an
			// existing brain that holds a corpus and has *lost* its
			// security.json is a different situation: silently downgrading it
			// to single_user turns "delete one 0600 file" into a complete ACL
			// bypass, since single_user authorises everything.
			if brainHasContent(dir) {
				return BrainSecurity{}, fmt.Errorf(
					"%w: %s is missing from a populated brain; refusing to serve it as single_user",
					ErrDenied, securityFile)
			}
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
	p := strings.TrimSpace(principal)
	if p == "" && prof != ProfileMultiPrincipal {
		// Only the single-user profile may assume the owner. Under an ACL,
		// substituting the owner for an absent principal meant the ACL was
		// bypassed by *not* presenting an identity.
		p = s.Owner
	}
	return Context{
		Profile: prof, Principal: p, Owner: s.Owner,
		Grants: s.Grants, ActionGrants: s.ActionGrants,
	}, nil
}

// brainHasContent reports whether dir holds brain data, as opposed to being an
// empty directory a new brain is about to be created in.
func brainHasContent(dir string) bool {
	for _, name := range []string{"chunks.jsonl", "chunks.delta.jsonl", "meta.json", "hotlex.gob"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.Size() > 0 {
			return true
		}
	}
	return false
}
