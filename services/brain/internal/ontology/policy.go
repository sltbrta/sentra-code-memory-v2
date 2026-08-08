package ontology

import (
	_ "embed"
	"os"
	"strings"
	"sync"
)

//go:embed packs/default.yaml
var defaultPackYAML []byte

// PredicatePolicy controls which claim predicates may hold multiple objects
// without forming a conflict (contest) attack.
type PredicatePolicy struct {
	// MultiValued lists predicates (lowercase) that do not contest on object mismatch.
	MultiValued map[string]struct{}
	// Version is the pack version string when loaded.
	Version string
	// Source is "embed", "file", or "default".
	Source string
}

// DefaultPredicatePolicy is used when the pack file is missing or unreadable.
func DefaultPredicatePolicy() PredicatePolicy {
	return PredicatePolicy{
		MultiValued: map[string]struct{}{
			"tags":       {},
			"aliases":    {},
			"keywords":   {},
			"related_to": {},
		},
		Version: "ontology.predicate.v0",
		Source:  "default",
	}
}

// LoadPredicatePolicy loads multi-valued predicate policy from path, or the
// embedded default pack when path is empty. On any parse/read failure returns Default.
func LoadPredicatePolicy(path string) PredicatePolicy {
	if path != "" {
		raw, err := os.ReadFile(path)
		if err == nil {
			if p, ok := parsePredicatePolicyYAML(raw); ok {
				p.Source = "file"
				return p
			}
		}
		// Fall through to embed / default.
	}
	if p, ok := parsePredicatePolicyYAML(defaultPackYAML); ok {
		p.Source = "embed"
		return p
	}
	return DefaultPredicatePolicy()
}

// parsePredicatePolicyYAML is a minimal line-oriented parser for the pack shape:
//
//	version: ...
//	multi_valued_predicates:
//	  - tags
//	  - aliases
//
// No external YAML dependency (leanness).
func parsePredicatePolicyYAML(raw []byte) (PredicatePolicy, bool) {
	p := PredicatePolicy{MultiValued: map[string]struct{}{}}
	lines := strings.Split(string(raw), "\n")
	inList := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if strings.HasPrefix(trim, "version:") {
			p.Version = strings.TrimSpace(strings.TrimPrefix(trim, "version:"))
			p.Version = strings.Trim(p.Version, `"'`)
			inList = false
			continue
		}
		if strings.HasPrefix(trim, "multi_valued_predicates:") {
			inList = true
			// inline form: multi_valued_predicates: [a, b]
			rest := strings.TrimSpace(strings.TrimPrefix(trim, "multi_valued_predicates:"))
			if rest != "" && strings.HasPrefix(rest, "[") {
				rest = strings.Trim(rest, "[]")
				for _, part := range strings.Split(rest, ",") {
					part = strings.ToLower(strings.TrimSpace(strings.Trim(part, `"'`)))
					if part != "" {
						p.MultiValued[part] = struct{}{}
					}
				}
				inList = false
			}
			continue
		}
		if inList && strings.HasPrefix(trim, "-") {
			item := strings.TrimSpace(strings.TrimPrefix(trim, "-"))
			item = strings.ToLower(strings.Trim(item, `"'`))
			if item != "" {
				p.MultiValued[item] = struct{}{}
			}
			continue
		}
		// Non-list key ends list section.
		if inList && strings.Contains(trim, ":") && !strings.HasPrefix(trim, "-") {
			inList = false
		}
	}
	if len(p.MultiValued) == 0 {
		return PredicatePolicy{}, false
	}
	if p.Version == "" {
		p.Version = "ontology.predicate.v0"
	}
	return p, true
}

// IsMultiValued reports whether predicate may hold many objects without contest.
func (p PredicatePolicy) IsMultiValued(predicate string) bool {
	if len(p.MultiValued) == 0 {
		return false
	}
	_, ok := p.MultiValued[strings.ToLower(strings.TrimSpace(predicate))]
	return ok
}

var (
	globalPolicyMu sync.RWMutex
	globalPolicy   = LoadPredicatePolicy("")
)

// ActivePredicatePolicy returns the process-wide policy (embed/default at init).
func ActivePredicatePolicy() PredicatePolicy {
	globalPolicyMu.RLock()
	defer globalPolicyMu.RUnlock()
	return globalPolicy
}

// SetActivePredicatePolicy replaces the process-wide policy (tests / pack reload).
func SetActivePredicatePolicy(p PredicatePolicy) {
	globalPolicyMu.Lock()
	defer globalPolicyMu.Unlock()
	if p.MultiValued == nil {
		p.MultiValued = map[string]struct{}{}
	}
	globalPolicy = p
}

// IsMultiValuedPredicate is a package helper over ActivePredicatePolicy.
func IsMultiValuedPredicate(predicate string) bool {
	return ActivePredicatePolicy().IsMultiValued(predicate)
}

// ResetPredicatePolicyForTest reloads the embedded default (test helper).
func ResetPredicatePolicyForTest() {
	SetActivePredicatePolicy(LoadPredicatePolicy(""))
}
