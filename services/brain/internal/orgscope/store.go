package orgscope

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"
)

// Item is one scoped memory record in the primary projection.
type Item struct {
	ID    string `json:"id"`
	Scope Scope  `json:"scope"`
	Owner string `json:"owner"` // user whose deletion request erases this item
	Text  string `json:"text"`
}

// Citation is one authorized evidence reference returned to a caller.
type Citation struct {
	ItemID   string `json:"item_id"`
	ScopeKey string `json:"scope_key"`
	Snippet  string `json:"snippet"`
}

// Citations is one authorized read result with its provenance.
type Citations struct {
	Citations []Citation `json:"citations"`
	FromCache bool       `json:"from_cache"`
	Epoch     uint64     `json:"epoch"`
}

// AuditEntry records one decision or erasure. It stores ids and counts only —
// never memory content — and survives erasure by design.
type AuditEntry struct {
	Seq       uint64    `json:"seq"`
	Kind      string    `json:"kind"` // "query", "history", "erasure", "restore", "rebuild"
	Principal string    `json:"principal,omitempty"`
	ItemIDs   []string  `json:"item_ids,omitempty"`
	Digest    string    `json:"digest,omitempty"`
	At        time.Time `json:"at"`
}

type cacheEntry struct {
	itemIDs []string
}

type sessionEntry struct {
	itemIDs []string
}

// principalArtifactKey prevents a newly provisioned lifecycle incarnation
// from inheriting cache, history, or replay artifacts created by an older one.
// TenantID is included even though a Store is single-tenant so every artifact
// has an explicit tenant binding.
type principalArtifactKey struct {
	tenantID    string
	userID      string
	incarnation uint64
}

type queryCacheKey struct {
	principal principalArtifactKey
	query     string
}

// Store holds primary items plus local search/claim/graph, cache, history, and
// replay projections. Content-returning reads re-authorize each item against
// the current authority and tombstone set. Cache, history, and replay entries
// are also tenant- and lifecycle-incarnation-bound.
type Store struct {
	mu            sync.Mutex
	auth          *Authority
	items         map[string]Item                         // primary: item id -> item
	index         map[string]map[string]struct{}          // lexical term -> item id set
	claims        map[string]map[string]struct{}          // claim term -> source item id set
	graph         map[string]struct{}                     // graph node projection by source item id
	cache         map[queryCacheKey]cacheEntry            // principal incarnation + query -> item ids
	sessions      map[principalArtifactKey][]sessionEntry // principal incarnation -> history
	replay        map[principalArtifactKey][]sessionEntry // principal incarnation -> replay artifacts
	tombstones    map[string]Tombstone                    // item id -> tombstone
	erasingScopes map[Scope]struct{}                      // deletion fences for exact scopes
	erasingOwners map[string]struct{}                     // deletion fences for owner offboarding
	audit         []AuditEntry
	auditSeq      uint64
}

// NewStore wires a store over one tenant authority.
func NewStore(auth *Authority) *Store {
	return &Store{
		auth:          auth,
		items:         make(map[string]Item),
		index:         make(map[string]map[string]struct{}),
		claims:        make(map[string]map[string]struct{}),
		graph:         make(map[string]struct{}),
		cache:         make(map[queryCacheKey]cacheEntry),
		sessions:      make(map[principalArtifactKey][]sessionEntry),
		replay:        make(map[principalArtifactKey][]sessionEntry),
		tombstones:    make(map[string]Tombstone),
		erasingScopes: make(map[Scope]struct{}),
		erasingOwners: make(map[string]struct{}),
	}
}

// Put admits one item into the primary store and index. Writing an id that
// has been erased is rejected: tombstones block resurrection by re-ingest.
func (s *Store) Put(item Item) error {
	if !validID(item.ID) || !item.Scope.valid() || item.Text == "" || !validID(item.Owner) {
		return ErrRejected
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dead := s.tombstones[item.ID]; dead {
		return ErrRejected
	}
	if _, fenced := s.erasingScopes[item.Scope]; fenced {
		return ErrRejected
	}
	if _, fenced := s.erasingOwners[item.Owner]; fenced {
		return ErrRejected
	}
	// Replacement is projection-safe: remove every old derived occurrence
	// before indexing the current canonical item.
	_, replacing := s.items[item.ID]
	s.removeIndexedIDLocked(item.ID)
	// Any write can add a new query candidate, so cached candidate sets are no
	// longer complete. A replacement must also stop old history/replay entries
	// from aliasing the new content stored under the same id.
	s.cache = make(map[queryCacheKey]cacheEntry)
	if replacing {
		s.removePrincipalArtifactIDLocked(item.ID)
	}
	s.items[item.ID] = item
	for _, term := range tokenize(item.Text) {
		if s.index[term] == nil {
			s.index[term] = make(map[string]struct{})
		}
		if s.claims[term] == nil {
			s.claims[term] = make(map[string]struct{})
		}
		s.index[term][item.ID] = struct{}{}
		s.claims[term][item.ID] = struct{}{}
	}
	s.graph[item.ID] = struct{}{}
	return nil
}

// Query returns only citations the principal is authorized to see right now.
// The cache stores pre-authorization candidate ids and every hit is
// re-filtered through the current authority and tombstones, so revocation and
// erasure always win over cached artifacts and later grants become visible.
func (s *Store) Query(p Principal, query string) (Citations, error) {
	principalKey, err := s.activePrincipalKey(p)
	if err != nil {
		return Citations{}, err
	}
	terms := tokenize(query)
	if len(terms) == 0 {
		return Citations{}, ErrRejected
	}
	cacheKey := queryCacheKey{principal: principalKey, query: query}

	s.mu.Lock()
	entry, fromCache := s.cache[cacheKey]
	var candidates []string
	if fromCache {
		candidates = append(candidates, entry.itemIDs...)
	} else {
		seen := make(map[string]struct{})
		for _, term := range terms {
			for id := range s.index[term] {
				seen[id] = struct{}{}
			}
		}
		for id := range seen {
			candidates = append(candidates, id)
		}
		sort.Strings(candidates)
	}
	s.mu.Unlock()

	allowed, epoch, err := s.authorizedItems(p, principalKey, candidates)
	if err != nil {
		return Citations{}, err
	}

	ids := make([]string, 0, len(allowed))
	citations := make([]Citation, 0, len(allowed))
	for _, item := range allowed {
		ids = append(ids, item.ID)
		citations = append(citations, Citation{
			ItemID: item.ID, ScopeKey: item.Scope.Key(), Snippet: snippet(item.Text),
		})
	}

	// Do not attach the result to an incarnation that changed while the
	// authority pass was running.
	if !s.principalEpochStillCurrent(p, principalKey, epoch) {
		return Citations{}, ErrDenied
	}
	s.mu.Lock()
	if !fromCache {
		// Never (re)admit tombstoned ids into the cache projection: an Erase
		// concurrent with this query must leave no trace to purge again.
		s.cache[cacheKey] = cacheEntry{itemIDs: s.withoutTombstonedLocked(candidates)}
	}
	s.sessions[principalKey] = append(s.sessions[principalKey], sessionEntry{itemIDs: s.withoutTombstonedLocked(ids)})
	s.replay[principalKey] = append(s.replay[principalKey], sessionEntry{itemIDs: s.withoutTombstonedLocked(ids)})
	s.appendAuditLocked("query", p.UserID, ids, "")
	s.mu.Unlock()

	return Citations{Citations: citations, FromCache: fromCache, Epoch: epoch}, nil
}

// authorizedItems re-authorizes candidate ids and returns the items that are
// live, non-tombstoned, and allowed for the principal under one stable policy
// epoch. Each item is re-read after Resolve so an in-flight Erase always wins
// over this read, and any policy-epoch movement during the pass forces a full
// re-evaluation. If the policy will not settle, the read fails closed.
func (s *Store) authorizedItems(p Principal, principalKey principalArtifactKey, candidates []string) ([]Item, uint64, error) {
	for pass := 0; pass < maxAuthPasses; pass++ {
		epoch := s.auth.Epoch()
		currentKey, err := s.activePrincipalKey(p)
		if err != nil || currentKey != principalKey {
			return nil, 0, ErrDenied
		}
		allowed := make([]Item, 0, len(candidates))
		for _, id := range candidates {
			// Snapshot outside authority locks (authority takes its own).
			s.mu.Lock()
			item, ok := s.items[id]
			_, dead := s.tombstones[id]
			s.mu.Unlock()
			if !ok || dead {
				continue
			}
			if s.auth.Resolve(p, item.Scope) != nil {
				continue
			}
			// Revalidate after Resolve: if an Erase (or any mutation of this
			// item) landed between the snapshot and here, fail closed and
			// never serve the stale snippet.
			s.mu.Lock()
			current, live := s.items[id]
			_, dead = s.tombstones[id]
			s.mu.Unlock()
			if !live || dead || current != item {
				continue
			}
			allowed = append(allowed, current)
		}
		if s.auth.Epoch() == epoch {
			return allowed, epoch, nil
		}
	}
	return nil, 0, ErrDenied
}

func (s *Store) activePrincipalKey(p Principal) (principalArtifactKey, error) {
	if p.TenantID != s.auth.dir.TenantID() || !validID(p.UserID) {
		return principalArtifactKey{}, ErrDenied
	}
	incarnation, active := s.auth.dir.Incarnation(p.UserID)
	if !active {
		return principalArtifactKey{}, ErrDenied
	}
	return principalArtifactKey{
		tenantID: p.TenantID, userID: p.UserID, incarnation: incarnation,
	}, nil
}

func (s *Store) principalEpochStillCurrent(p Principal, principalKey principalArtifactKey, epoch uint64) bool {
	currentKey, err := s.activePrincipalKey(p)
	return err == nil && currentKey == principalKey && s.auth.Epoch() == epoch
}

// withoutTombstonedLocked filters tombstoned ids under s.mu.
func (s *Store) withoutTombstonedLocked(ids []string) []string {
	kept := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, dead := s.tombstones[id]; dead {
			continue
		}
		kept = append(kept, id)
	}
	return kept
}

// History replays the principal's session history, re-filtered through the
// current authority and tombstones so revoked or erased memory never
// reappears via replay artifacts.
func (s *Store) History(p Principal) ([]Citation, error) {
	principalKey, err := s.activePrincipalKey(p)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	entries := append([]sessionEntry(nil), s.sessions[principalKey]...)
	s.mu.Unlock()

	var candidates []string
	seen := make(map[string]struct{})
	for _, e := range entries {
		for _, id := range e.itemIDs {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			candidates = append(candidates, id)
		}
	}

	allowed, epoch, err := s.authorizedItems(p, principalKey, candidates)
	if err != nil {
		return nil, err
	}
	out := make([]Citation, 0, len(allowed))
	ids := make([]string, 0, len(allowed))
	for _, item := range allowed {
		ids = append(ids, item.ID)
		out = append(out, Citation{ItemID: item.ID, ScopeKey: item.Scope.Key(), Snippet: snippet(item.Text)})
	}
	if !s.principalEpochStillCurrent(p, principalKey, epoch) {
		return nil, ErrDenied
	}
	s.mu.Lock()
	s.appendAuditLocked("history", p.UserID, ids, "")
	s.mu.Unlock()
	return out, nil
}

// SearchClaims searches the local claim projection and reauthorizes every
// source item before returning a citation.
func (s *Store) SearchClaims(p Principal, query string) ([]Citation, error) {
	principalKey, err := s.activePrincipalKey(p)
	if err != nil {
		return nil, err
	}
	terms := tokenize(query)
	if len(terms) == 0 {
		return nil, ErrRejected
	}
	s.mu.Lock()
	seen := make(map[string]struct{})
	for _, term := range terms {
		for id := range s.claims[term] {
			seen[id] = struct{}{}
		}
	}
	candidates := sortedIDs(seen)
	s.mu.Unlock()
	return s.citationsFor(p, principalKey, candidates, "claims")
}

// Graph returns authorized nodes from the local graph projection. Nodes are
// source-item keyed, so graph traversal can never outrun item authorization.
func (s *Store) Graph(p Principal) ([]Citation, error) {
	principalKey, err := s.activePrincipalKey(p)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	candidates := sortedIDs(s.graph)
	s.mu.Unlock()
	return s.citationsFor(p, principalKey, candidates, "graph")
}

// Replay returns session replay artifacts through current authorization and
// tombstone checks. It is intentionally distinct from History so erasure
// coverage cannot accidentally omit a replay substrate.
func (s *Store) Replay(p Principal) ([]Citation, error) {
	principalKey, err := s.activePrincipalKey(p)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	entries := append([]sessionEntry(nil), s.replay[principalKey]...)
	s.mu.Unlock()
	return s.citationsFor(p, principalKey, uniqueEntryIDs(entries), "replay")
}

func (s *Store) citationsFor(p Principal, principalKey principalArtifactKey, candidates []string, auditKind string) ([]Citation, error) {
	allowed, epoch, err := s.authorizedItems(p, principalKey, candidates)
	if err != nil {
		return nil, err
	}
	out := make([]Citation, 0, len(allowed))
	ids := make([]string, 0, len(allowed))
	for _, item := range allowed {
		ids = append(ids, item.ID)
		out = append(out, Citation{ItemID: item.ID, ScopeKey: item.Scope.Key(), Snippet: snippet(item.Text)})
	}
	if !s.principalEpochStillCurrent(p, principalKey, epoch) {
		return nil, ErrDenied
	}
	s.mu.Lock()
	s.appendAuditLocked(auditKind, p.UserID, ids, "")
	s.mu.Unlock()
	return out, nil
}

func uniqueEntryIDs(entries []sessionEntry) []string {
	seen := make(map[string]struct{})
	for _, entry := range entries {
		for _, id := range entry.itemIDs {
			seen[id] = struct{}{}
		}
	}
	return sortedIDs(seen)
}

func sortedIDs(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (s *Store) removeIndexedIDLocked(itemID string) {
	for term, ids := range s.index {
		delete(ids, itemID)
		if len(ids) == 0 {
			delete(s.index, term)
		}
	}
	for term, ids := range s.claims {
		delete(ids, itemID)
		if len(ids) == 0 {
			delete(s.claims, term)
		}
	}
	delete(s.graph, itemID)
}

func (s *Store) removePrincipalArtifactIDLocked(itemID string) {
	drop := map[string]struct{}{itemID: {}}
	for principal, entries := range s.sessions {
		for i, entry := range entries {
			kept, _ := withoutIDs(entry.itemIDs, drop)
			entries[i] = sessionEntry{itemIDs: kept}
		}
		s.sessions[principal] = entries
	}
	for principal, entries := range s.replay {
		for i, entry := range entries {
			kept, _ := withoutIDs(entry.itemIDs, drop)
			entries[i] = sessionEntry{itemIDs: kept}
		}
		s.replay[principal] = entries
	}
}

// Audit returns a deep copy of the append-only audit log: mutating a
// returned entry (including its ItemIDs) can never alter the retained log.
func (s *Store) Audit() []AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AuditEntry, len(s.audit))
	for i, entry := range s.audit {
		entry.ItemIDs = append([]string(nil), entry.ItemIDs...)
		out[i] = entry
	}
	return out
}

// appendAuditLocked records one entry under s.mu; content is never stored.
func (s *Store) appendAuditLocked(kind, principal string, itemIDs []string, digest string) {
	s.auditSeq++
	ids := append([]string(nil), itemIDs...)
	sort.Strings(ids)
	s.audit = append(s.audit, AuditEntry{
		Seq: s.auditSeq, Kind: kind, Principal: principal,
		ItemIDs: ids, Digest: digest, At: s.auth.dir.clock().UTC(),
	})
}

func tokenize(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !('a' <= r && r <= 'z') && !('0' <= r && r <= '9')
	})
	seen := make(map[string]struct{}, len(fields))
	var out []string
	for _, f := range fields {
		if _, dup := seen[f]; dup {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	return out
}

func snippet(text string) string {
	const max = 64
	if len(text) <= max {
		return text
	}
	return text[:max]
}

func digestOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
