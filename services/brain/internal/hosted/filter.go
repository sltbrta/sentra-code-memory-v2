// Governed metadata-filter contract for scoped retrieval (issue #328).
//
// One reusable contract shared by every retrieval arm (lexical, dense,
// graph/structure, parent hydration) and by the retrieve cache:
//
//   - Predicates are allowlisted; unknown keys are rejected, never ignored.
//   - Values are normalized (trim/lower/dedupe/sort) so identical filters
//     share one canonical identity digest.
//   - Authorization is explicit: a FilterAuthority binds the tenant and the
//     predicates a caller may use. Malformed or unauthorized filters fail
//     closed with an error — retrieval never proceeds with a best-effort or
//     broadened filter.
//   - Document-ID pinning predicates do not exist in the allowlist at all,
//     so gold-derived filters (expected_doc_ids etc.) are unexpressible.
//   - Official/blind ERB posture additionally rejects gold-derived
//     predicates (source_types, question_type) so blind runs cannot receive
//     filters derived from dataset metadata.
//
// Filter identity is part of the retrieve cache key (cache.go) and is
// stamped into retrieval diagnostics so run receipts/manifests record
// exactly which governed filter produced a window.
package hosted

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// filterPredicateAllowlist is the complete set of expressible predicates.
// Anything not listed here is rejected by NormalizeMetadataFilter. Document-ID
// predicates are deliberately absent: pinning document IDs is gold-adjacent
// and must never be a filter input.
var filterPredicateAllowlist = map[string]struct{}{
	"tenant":          {},
	"scopes":          {},
	"source_types":    {},
	"tags":            {},
	"valid_from":      {},
	"valid_until":     {},
	"principals":      {},
	"deny_principals": {},
}

// goldDerivedFilterPredicates are predicates (or gold-field aliases) that an
// official/blind ERB run must never receive, because they can only be derived
// from dataset metadata rather than the question. In blind posture any of
// these in a raw filter fails closed.
var goldDerivedFilterPredicates = map[string]struct{}{
	"source_types":     {},
	"question_type":    {},
	"document_ids":     {},
	"expected_doc_ids": {},
	"gold_doc_ids":     {},
	"gold_answer":      {},
	"answer_facts":     {},
}

// MetadataFilter is the normalized, immutable governed filter. Construct only
// via NormalizeMetadataFilter so invariants (sorted, deduped, authorized)
// always hold.
type MetadataFilter struct {
	Tenant           string
	Scopes           []string
	SourceTypes      []string
	Tags             []string
	ValidFrom        time.Time
	ValidUntil       time.Time
	Principals       []string
	DeniedPrincipals []string
}

// FilterAuthority declares what a caller is permitted to filter on.
type FilterAuthority struct {
	// Tenant, when non-empty, binds the filter: a filter naming any other
	// tenant is rejected (no cross-tenant predicate injection).
	Tenant string
	// Blind is the official/blind ERB posture: gold-derived predicates are
	// rejected so benchmark metadata cannot steer retrieval.
	Blind bool
	// AllowedPredicates, when non-empty, further restricts the allowlist for
	// this caller (delegated/least-privilege filtering).
	AllowedPredicates []string
}

// DocMeta is the document metadata a filter is evaluated against. Arms look
// it up per passage via the client metadata provider (or derive the
// passage-local subset from SourceURI/Channel).
type DocMeta struct {
	Tenant     string
	Scope      string
	SourceType string
	Tags       []string
	ValidFrom  time.Time
	ValidUntil time.Time
	// Principals are ACL-adjacent: who may see this document.
	Principals []string
}

// IsZero reports whether the filter constrains nothing (nil-safe).
func (f *MetadataFilter) IsZero() bool {
	return f == nil ||
		(f.Tenant == "" && len(f.Scopes) == 0 && len(f.SourceTypes) == 0 &&
			len(f.Tags) == 0 && f.ValidFrom.IsZero() && f.ValidUntil.IsZero() &&
			len(f.Principals) == 0 && len(f.DeniedPrincipals) == 0)
}

// Identity returns the canonical digest of the normalized filter. It is
// stable across key order and list order, and is embedded in retrieve cache
// keys and run receipts. Zero filter → "" (key-compatible with unfiltered
// legacy requests).
func (f *MetadataFilter) Identity() string {
	if f == nil || f.IsZero() {
		return ""
	}
	parts := []string{
		"tenant=" + f.Tenant,
		"scopes=" + strings.Join(f.Scopes, ","),
		"source_types=" + strings.Join(f.SourceTypes, ","),
		"tags=" + strings.Join(f.Tags, ","),
	}
	if !f.ValidFrom.IsZero() {
		parts = append(parts, "valid_from="+f.ValidFrom.UTC().Format(time.RFC3339))
	}
	if !f.ValidUntil.IsZero() {
		parts = append(parts, "valid_until="+f.ValidUntil.UTC().Format(time.RFC3339))
	}
	parts = append(parts,
		"principals="+strings.Join(f.Principals, ","),
		"deny_principals="+strings.Join(f.DeniedPrincipals, ","),
	)
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:16])
}

// Predicates returns the sorted names of active predicates (receipt-safe).
func (f *MetadataFilter) Predicates() []string {
	if f == nil {
		return nil
	}
	var out []string
	if f.Tenant != "" {
		out = append(out, "tenant")
	}
	if len(f.Scopes) > 0 {
		out = append(out, "scopes")
	}
	if len(f.SourceTypes) > 0 {
		out = append(out, "source_types")
	}
	if len(f.Tags) > 0 {
		out = append(out, "tags")
	}
	if !f.ValidFrom.IsZero() {
		out = append(out, "valid_from")
	}
	if !f.ValidUntil.IsZero() {
		out = append(out, "valid_until")
	}
	if len(f.Principals) > 0 {
		out = append(out, "principals")
	}
	if len(f.DeniedPrincipals) > 0 {
		out = append(out, "deny_principals")
	}
	sort.Strings(out)
	return out
}

func normStringList(v any, key string) ([]string, error) {
	raw, ok := v.([]any)
	if !ok {
		// Tolerate []string (non-JSON callers).
		if ss, ok2 := v.([]string); ok2 {
			raw = make([]any, len(ss))
			for i, s := range ss {
				raw[i] = s
			}
		} else {
			return nil, fmt.Errorf("filter %q must be a list of strings", key)
		}
	}
	seen := map[string]struct{}{}
	var out []string
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("filter %q entries must be strings", key)
		}
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			return nil, fmt.Errorf("filter %q contains an empty entry", key)
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("filter %q must not be empty", key)
	}
	sort.Strings(out)
	return out, nil
}

func normFilterTime(v any, key string) (time.Time, error) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, fmt.Errorf("filter %q must be an RFC3339 or YYYY-MM-DD string", key)
	}
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("filter %q is not a valid timestamp: %q", key, s)
}

// NormalizeMetadataFilter validates and normalizes a raw predicate map under
// the given authority. It fails closed: any unknown key, malformed value,
// unauthorized predicate, tenant mismatch, or (in blind posture) gold-derived
// predicate returns an error and no filter. A nil/empty raw map returns
// (nil, nil) — filtering is opt-in.
func NormalizeMetadataFilter(raw map[string]any, auth FilterAuthority) (*MetadataFilter, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	allowed := auth.AllowedPredicates
	f := &MetadataFilter{}
	for key, v := range raw {
		k := strings.ToLower(strings.TrimSpace(key))
		if auth.Blind {
			if _, gold := goldDerivedFilterPredicates[k]; gold {
				return nil, fmt.Errorf("filter predicate %q is gold-derived and rejected in blind mode", k)
			}
		}
		if _, ok := filterPredicateAllowlist[k]; !ok {
			return nil, fmt.Errorf("filter predicate %q is not allowlisted", k)
		}
		if len(allowed) > 0 {
			ok := false
			for _, a := range allowed {
				if strings.EqualFold(strings.TrimSpace(a), k) {
					ok = true
					break
				}
			}
			if !ok {
				return nil, fmt.Errorf("filter predicate %q is not authorized for this caller", k)
			}
		}
		switch k {
		case "tenant":
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("filter %q must be a string", k)
			}
			s = strings.ToLower(strings.TrimSpace(s))
			if s == "" {
				return nil, fmt.Errorf("filter %q must not be empty", k)
			}
			if auth.Tenant != "" && s != strings.ToLower(strings.TrimSpace(auth.Tenant)) {
				return nil, fmt.Errorf("filter tenant %q does not match the authorized tenant", s)
			}
			f.Tenant = s
		case "scopes":
			l, err := normStringList(v, k)
			if err != nil {
				return nil, err
			}
			f.Scopes = l
		case "source_types":
			l, err := normStringList(v, k)
			if err != nil {
				return nil, err
			}
			f.SourceTypes = l
		case "tags":
			l, err := normStringList(v, k)
			if err != nil {
				return nil, err
			}
			f.Tags = l
		case "principals":
			l, err := normStringList(v, k)
			if err != nil {
				return nil, err
			}
			f.Principals = l
		case "deny_principals":
			l, err := normStringList(v, k)
			if err != nil {
				return nil, err
			}
			f.DeniedPrincipals = l
		case "valid_from":
			t, err := normFilterTime(v, k)
			if err != nil {
				return nil, err
			}
			f.ValidFrom = t
		case "valid_until":
			t, err := normFilterTime(v, k)
			if err != nil {
				return nil, err
			}
			f.ValidUntil = t
		}
	}
	if !f.ValidFrom.IsZero() && !f.ValidUntil.IsZero() && f.ValidUntil.Before(f.ValidFrom) {
		return nil, fmt.Errorf("filter valid_until precedes valid_from")
	}
	if f.IsZero() {
		return nil, nil
	}
	return f, nil
}

// Allows evaluates one document's metadata against the filter, failing
// closed: when a predicate is active and the document metadata lacks the
// corresponding field, the document is denied rather than passed through.
// Returns the denial reason ("" when allowed) for receipts.
func (f *MetadataFilter) Allows(meta DocMeta) (bool, string) {
	if f == nil || f.IsZero() {
		return true, ""
	}
	if f.Tenant != "" && !strings.EqualFold(meta.Tenant, f.Tenant) {
		return false, "tenant"
	}
	if len(f.Scopes) > 0 {
		scope := strings.ToLower(strings.TrimSpace(meta.Scope))
		ok := false
		for _, s := range f.Scopes {
			if s == scope {
				ok = true
				break
			}
		}
		if !ok {
			return false, "scope"
		}
	}
	if len(f.SourceTypes) > 0 {
		st := strings.ToLower(strings.TrimSpace(meta.SourceType))
		ok := false
		for _, s := range f.SourceTypes {
			if s == st {
				ok = true
				break
			}
		}
		if !ok {
			return false, "source_type"
		}
	}
	if len(f.Tags) > 0 {
		have := map[string]struct{}{}
		for _, t := range meta.Tags {
			have[strings.ToLower(strings.TrimSpace(t))] = struct{}{}
		}
		for _, want := range f.Tags {
			if _, ok := have[want]; !ok {
				return false, "tags"
			}
		}
	}
	if !f.ValidFrom.IsZero() || !f.ValidUntil.IsZero() {
		// Time-validity predicate: a document with no validity metadata
		// cannot prove it is inside the window → deny (fail closed).
		if meta.ValidFrom.IsZero() && meta.ValidUntil.IsZero() {
			return false, "validity_unknown"
		}
		if !f.ValidUntil.IsZero() && !meta.ValidFrom.IsZero() && meta.ValidFrom.After(f.ValidUntil) {
			return false, "validity_window"
		}
		if !f.ValidFrom.IsZero() && !meta.ValidUntil.IsZero() && meta.ValidUntil.Before(f.ValidFrom) {
			return false, "validity_window"
		}
	}
	if len(f.DeniedPrincipals) > 0 {
		for _, p := range meta.Principals {
			p = strings.ToLower(strings.TrimSpace(p))
			for _, d := range f.DeniedPrincipals {
				if p == d {
					return false, "principal_denied"
				}
			}
		}
	}
	if len(f.Principals) > 0 {
		ok := false
		for _, p := range meta.Principals {
			p = strings.ToLower(strings.TrimSpace(p))
			for _, want := range f.Principals {
				if p == want {
					ok = true
					break
				}
			}
			if ok {
				break
			}
		}
		if !ok {
			return false, "principal"
		}
	}
	return true, ""
}

// docMetaFromPassage derives the passage-local metadata subset when no
// document metadata provider is wired. Source type comes from the SourceURI
// scheme (slack://… → slack) or falls back to the retrieval channel.
// Tenant/scope/tags/validity/principals stay empty, so filters requiring
// them fail closed rather than pass on absence.
func docMetaFromPassage(p Passage) DocMeta {
	st := ""
	if uri := strings.TrimSpace(p.SourceURI); uri != "" {
		if i := strings.Index(uri, "://"); i > 0 {
			st = strings.ToLower(uri[:i])
		}
	}
	if st == "" {
		st = strings.ToLower(strings.TrimSpace(p.Channel))
	}
	return DocMeta{SourceType: st}
}

// FilterPassages applies one authorized filter to a merged pool. Every arm
// (lexical, dense, structure/graph, hydrate) funnels through this single
// choke point so the same authorized filter is applied identically regardless
// of which arm surfaced a passage. meta resolves document metadata per
// passage; when it is nil the passage-local subset is used. Returns the
// surviving passages (order preserved) and the number dropped.
func FilterPassages(ps []Passage, f *MetadataFilter, meta func(Passage) DocMeta) ([]Passage, int) {
	if f == nil || f.IsZero() || len(ps) == 0 {
		return ps, 0
	}
	out := make([]Passage, 0, len(ps))
	dropped := 0
	for _, p := range ps {
		m := docMetaFromPassage(p)
		if meta != nil {
			m = meta(p)
		}
		if ok, _ := f.Allows(m); ok {
			out = append(out, p)
		} else {
			dropped++
		}
	}
	return out, dropped
}

// blindFilterMode mirrors the engine blind posture (official ⇒ blind) so the
// hosted layer fails closed on gold-derived filters even if a caller forgets
// to set FilterAuthority.Blind explicitly.
func blindFilterMode() bool {
	return envTruthy("OUROBOROS_ERB_OFFICIAL", false) ||
		envTruthy("OUROBOROS_ERB_OFFICIAL_JUDGE", false) ||
		envTruthy("OUROBOROS_ERB_BLIND_PLAN", false)
}
