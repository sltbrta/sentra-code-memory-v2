package orgscope

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

// Tombstone marks one erased item id. Tombstones are authoritative: they
// survive backup, restore, and projection rebuild, and they block re-ingest
// of the same id.
type Tombstone struct {
	ItemID string    `json:"item_id"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// LocalStoreErasureCoverage names the exact content-bearing substrate set
// verified by ErasureReceipt.Complete. It intentionally excludes retained
// audit metadata and caller-owned Backup values.
const LocalStoreErasureCoverage = "orgscope_store_content_projections_v1"

var localErasureProjections = []string{
	"primary", "index", "claims", "graph", "cache", "session", "replay",
}

// ErasureReceipt reports one erasure across the locally managed,
// content-bearing projections named by Coverage. Complete is not a claim about
// retained audit metadata, exported backup values, or production substrates.
type ErasureReceipt struct {
	TenantID    string         `json:"tenant_id"`
	Coverage    string         `json:"coverage"`
	ItemIDs     []string       `json:"item_ids"`
	Projections map[string]int `json:"projections"` // purge counts per projection
	Receipt     Receipt        `json:"receipt"`     // policy-ordered revocation receipt
	StartedAt   time.Time      `json:"started_at"`
	CompletedAt time.Time      `json:"completed_at"`
	Complete    bool           `json:"complete"`
}

// Erase tombstones the given item ids and purges them from the primary
// store, search/claim indexes, graph nodes, query cache, session history, and
// replay artifacts. Ids that are unknown are still tombstoned so they can
// never be (re)ingested later.
func (s *Store) Erase(reason string, itemIDs ...string) (ErasureReceipt, error) {
	if reason == "" || len(itemIDs) == 0 {
		return ErasureReceipt{}, ErrRejected
	}
	seenIDs := make(map[string]struct{}, len(itemIDs))
	for _, id := range itemIDs {
		if !validID(id) {
			return ErasureReceipt{}, ErrRejected
		}
		if _, duplicate := seenIDs[id]; duplicate {
			return ErasureReceipt{}, ErrRejected
		}
		seenIDs[id] = struct{}{}
	}
	now := s.auth.dir.clock().UTC()

	s.mu.Lock()
	counts := make(map[string]int, len(localErasureProjections))
	for _, projection := range localErasureProjections {
		counts[projection] = 0
	}
	erase := make(map[string]struct{}, len(itemIDs))
	var digests []string
	for _, id := range itemIDs {
		erase[id] = struct{}{}
		if _, ok := s.tombstones[id]; !ok {
			s.tombstones[id] = Tombstone{ItemID: id, Reason: reason, At: now}
		}
		if item, ok := s.items[id]; ok {
			digests = append(digests, digestOf(item.Text))
			delete(s.items, id)
			counts["primary"]++
		}
	}
	for term, ids := range s.index {
		for id := range erase {
			if _, ok := ids[id]; ok {
				delete(ids, id)
				counts["index"]++
			}
		}
		if len(ids) == 0 {
			delete(s.index, term)
		}
	}
	for term, ids := range s.claims {
		for id := range erase {
			if _, ok := ids[id]; ok {
				delete(ids, id)
				counts["claims"]++
			}
		}
		if len(ids) == 0 {
			delete(s.claims, term)
		}
	}
	for id := range erase {
		if _, ok := s.graph[id]; ok {
			delete(s.graph, id)
			counts["graph"]++
		}
	}
	for key, entry := range s.cache {
		kept, removed := withoutIDs(entry.itemIDs, erase)
		if removed > 0 {
			counts["cache"] += removed
			s.cache[key] = cacheEntry{itemIDs: kept}
		}
	}
	for user, entries := range s.sessions {
		for i, e := range entries {
			kept, removed := withoutIDs(e.itemIDs, erase)
			if removed > 0 {
				counts["session"] += removed
				entries[i] = sessionEntry{itemIDs: kept}
			}
		}
		s.sessions[user] = entries
	}
	for user, entries := range s.replay {
		for i, e := range entries {
			kept, removed := withoutIDs(e.itemIDs, erase)
			if removed > 0 {
				counts["replay"] += removed
				entries[i] = sessionEntry{itemIDs: kept}
			}
		}
		s.replay[user] = entries
	}
	sorted := append([]string(nil), itemIDs...)
	sort.Strings(sorted)
	s.appendAuditLocked("erasure", "", sorted, digestJoin(digests))
	s.mu.Unlock()

	receipt := s.auth.dir.receiptFor(ReceiptErasure, "store", joinIDs(sorted))
	leaks := s.VerifyErasure(itemIDs...)
	return ErasureReceipt{
		TenantID: s.auth.dir.TenantID(), Coverage: LocalStoreErasureCoverage,
		ItemIDs: sorted, Projections: counts, Receipt: receipt,
		StartedAt: now, CompletedAt: s.auth.dir.clock().UTC(),
		Complete: len(leaks.Leaks) == 0,
	}, nil
}

// EraseScope erases every item in one exact scope without widening to sibling
// individual/team scopes.
func (s *Store) EraseScope(reason string, scope Scope) (ErasureReceipt, error) {
	if reason == "" || !scope.valid() {
		return ErasureReceipt{}, ErrRejected
	}
	s.mu.Lock()
	// Fence the scope before taking the snapshot so a concurrent Put cannot
	// land between enumeration and erasure. The fence is permanent: a deleted
	// scope must be explicitly reprovisioned by a higher-level policy owner.
	_, wasFenced := s.erasingScopes[scope]
	s.erasingScopes[scope] = struct{}{}
	var ids []string
	for id, item := range s.items {
		if item.Scope == scope {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	if len(ids) == 0 {
		if !wasFenced {
			s.mu.Lock()
			delete(s.erasingScopes, scope)
			s.mu.Unlock()
		}
		return ErasureReceipt{}, ErrDenied
	}
	sort.Strings(ids)
	return s.Erase(reason, ids...)
}

// EraseOwner erases every item owned by a user (offboarding deletion request).
func (s *Store) EraseOwner(reason, userID string) (ErasureReceipt, error) {
	if reason == "" || !validID(userID) {
		return ErasureReceipt{}, ErrRejected
	}
	s.mu.Lock()
	// Fence the owner before taking the snapshot so offboarding cannot race a
	// new write into a supposedly complete deletion request.
	_, wasFenced := s.erasingOwners[userID]
	s.erasingOwners[userID] = struct{}{}
	var ids []string
	for id, item := range s.items {
		if item.Owner == userID {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	if len(ids) == 0 {
		if !wasFenced {
			s.mu.Lock()
			delete(s.erasingOwners, userID)
			s.mu.Unlock()
		}
		return ErasureReceipt{}, ErrDenied
	}
	sort.Strings(ids)
	return s.Erase(reason, ids...)
}

// LeakReport is the projection scan result for erased ids.
type LeakReport struct {
	Checked int      `json:"checked"`
	ItemIDs []string `json:"item_ids"`
	Leaks   []string `json:"leaks"` // "projection:item_id" occurrences
}

// VerifyErasure scans every locally managed content-bearing projection for the
// given ids and reports each occurrence. Retained audit metadata and
// caller-owned Backup values are outside this scan by design.
func (s *Store) VerifyErasure(itemIDs ...string) LeakReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	checkedIDs := append([]string(nil), itemIDs...)
	sort.Strings(checkedIDs)
	report := LeakReport{Checked: len(itemIDs), ItemIDs: checkedIDs}
	for _, id := range itemIDs {
		if _, ok := s.items[id]; ok {
			report.Leaks = append(report.Leaks, "primary:"+id)
		}
		for _, ids := range s.index {
			if _, ok := ids[id]; ok {
				report.Leaks = append(report.Leaks, "index:"+id)
				break
			}
		}
		for _, ids := range s.claims {
			if _, ok := ids[id]; ok {
				report.Leaks = append(report.Leaks, "claims:"+id)
				break
			}
		}
		if _, ok := s.graph[id]; ok {
			report.Leaks = append(report.Leaks, "graph:"+id)
		}
		for _, entry := range s.cache {
			if containsID(entry.itemIDs, id) {
				report.Leaks = append(report.Leaks, "cache:"+id)
				break
			}
		}
		for _, entries := range s.sessions {
			hit := false
			for _, e := range entries {
				if containsID(e.itemIDs, id) {
					report.Leaks = append(report.Leaks, "session:"+id)
					hit = true
					break
				}
			}
			if hit {
				break
			}
		}
		for _, entries := range s.replay {
			hit := false
			for _, e := range entries {
				if containsID(e.itemIDs, id) {
					report.Leaks = append(report.Leaks, "replay:"+id)
					hit = true
					break
				}
			}
			if hit {
				break
			}
		}
	}
	return report
}

// RebuildProjections drops and rebuilds search, claim, graph, and query-cache
// projections from the primary store, honoring tombstones. Session and replay
// history are retained but re-filtered on read. Rebuild can never resurrect
// erased memory.
func (s *Store) RebuildProjections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rebuildProjectionsLocked()
}

func (s *Store) rebuildProjectionsLocked() {
	s.index = make(map[string]map[string]struct{})
	s.claims = make(map[string]map[string]struct{})
	s.graph = make(map[string]struct{})
	s.cache = make(map[queryCacheKey]cacheEntry)
	for id, item := range s.items {
		if _, dead := s.tombstones[id]; dead {
			delete(s.items, id)
			continue
		}
		for _, term := range tokenize(item.Text) {
			if s.index[term] == nil {
				s.index[term] = make(map[string]struct{})
			}
			if s.claims[term] == nil {
				s.claims[term] = make(map[string]struct{})
			}
			s.index[term][id] = struct{}{}
			s.claims[term][id] = struct{}{}
		}
		s.graph[id] = struct{}{}
	}
	s.appendAuditLocked("rebuild", "", nil, "")
}

// Backup is one caller-owned restorable value: items plus the authoritative
// tombstone set at snapshot time. The Store does not track or erase copies of
// returned Backup values.
type Backup struct {
	TenantID   string      `json:"tenant_id"`
	Items      []Item      `json:"items"`
	Tombstones []Tombstone `json:"tombstones"`
	Digest     string      `json:"digest"`
}

// CreateBackup snapshots the primary store and tombstones.
func (s *Store) CreateBackup() (Backup, error) {
	s.mu.Lock()
	items := make([]Item, 0, len(s.items))
	for _, item := range s.items {
		items = append(items, item)
	}
	stones := make([]Tombstone, 0, len(s.tombstones))
	for _, t := range s.tombstones {
		stones = append(stones, t)
	}
	s.mu.Unlock()
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	sort.Slice(stones, func(i, j int) bool { return stones[i].ItemID < stones[j].ItemID })
	digest, err := canonicalBackupDigest(s.auth.dir.TenantID(), items, stones)
	if err != nil {
		return Backup{}, err
	}
	return Backup{
		TenantID:   s.auth.dir.TenantID(),
		Items:      items,
		Tombstones: stones,
		Digest:     digest,
	}, nil
}

// canonicalBackupDigest hashes the canonical (id-sorted) backup payload. The
// input slices are not mutated; nil and empty slices digest identically.
func canonicalBackupDigest(tenantID string, items []Item, stones []Tombstone) (string, error) {
	cItems := make([]Item, len(items))
	copy(cItems, items)
	cStones := make([]Tombstone, len(stones))
	copy(cStones, stones)
	sort.Slice(cItems, func(i, j int) bool { return cItems[i].ID < cItems[j].ID })
	sort.Slice(cStones, func(i, j int) bool { return cStones[i].ItemID < cStones[j].ItemID })
	raw, err := json.Marshal(struct {
		TenantID   string
		Items      []Item
		Tombstones []Tombstone
	}{tenantID, cItems, cStones})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Restore loads a same-tenant backup. The digest is recomputed over the
// canonical backup contents and must match exactly, detecting accidental or
// unrecomputed modification; it is not an authenticity signature. The live
// tombstone set and the backup's
// tombstone set are unioned first, and only non-tombstoned items are loaded,
// so a pre-erasure backup cannot resurrect data in a Store that already has
// the tombstone. A fresh Store needs a post-erasure backup or an external
// erasure ledger. Derived projections and histories are dropped and rebuilt
// atomically so old item ids cannot alias restored content. Restore is a
// trusted internal operation; this package does not model operator auth.
func (s *Store) Restore(b Backup) error {
	if b.Digest == "" || b.TenantID != s.auth.dir.TenantID() {
		return ErrRejected
	}
	want, err := canonicalBackupDigest(b.TenantID, b.Items, b.Tombstones)
	if err != nil || b.Digest != want || !validBackupContents(b) {
		return ErrRejected
	}
	s.mu.Lock()
	for _, t := range b.Tombstones {
		if _, ok := s.tombstones[t.ItemID]; !ok {
			s.tombstones[t.ItemID] = t
		}
	}
	s.items = make(map[string]Item)
	for _, item := range b.Items {
		if _, dead := s.tombstones[item.ID]; dead {
			continue
		}
		s.items[item.ID] = item
	}
	s.sessions = make(map[principalArtifactKey][]sessionEntry)
	s.replay = make(map[principalArtifactKey][]sessionEntry)
	s.appendAuditLocked("restore", "", nil, b.Digest)
	s.rebuildProjectionsLocked()
	s.mu.Unlock()
	return nil
}

// validBackupContents rejects malformed and ambiguous canonical records before
// they can enter primary or tombstone authority.
func validBackupContents(b Backup) bool {
	itemIDs := make(map[string]struct{}, len(b.Items))
	for _, item := range b.Items {
		if !validID(item.ID) || !item.Scope.valid() || !validID(item.Owner) || item.Text == "" {
			return false
		}
		if _, duplicate := itemIDs[item.ID]; duplicate {
			return false
		}
		itemIDs[item.ID] = struct{}{}
	}
	stoneIDs := make(map[string]struct{}, len(b.Tombstones))
	for _, stone := range b.Tombstones {
		if !validID(stone.ItemID) || stone.Reason == "" || stone.At.IsZero() {
			return false
		}
		if _, duplicate := stoneIDs[stone.ItemID]; duplicate {
			return false
		}
		stoneIDs[stone.ItemID] = struct{}{}
	}
	return true
}

// Tombstones returns a copy of the authoritative tombstone set.
func (s *Store) Tombstones() []Tombstone {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Tombstone, 0, len(s.tombstones))
	for _, t := range s.tombstones {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ItemID < out[j].ItemID })
	return out
}

func withoutIDs(ids []string, drop map[string]struct{}) ([]string, int) {
	kept := ids[:0:0]
	removed := 0
	for _, id := range ids {
		if _, ok := drop[id]; ok {
			removed++
			continue
		}
		kept = append(kept, id)
	}
	return kept, removed
}

func containsID(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func joinIDs(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += id
	}
	return out
}

func digestJoin(digests []string) string {
	if len(digests) == 0 {
		return ""
	}
	sort.Strings(digests)
	sum := sha256.Sum256([]byte(joinIDs(digests)))
	return hex.EncodeToString(sum[:])
}
