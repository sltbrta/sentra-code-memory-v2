package hosted

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Entity catalog for recovery expand — production substitute for sentra's
// offline ~70k name dump.
//
// Source of truth: path2_entities on Neon (brain-scoped, always current with
// the corpus). We do NOT ship a frozen FAISS/name file; live SQL + process
// cache is more prod-ready (multi-tenant, no rebuild job for renames).
//
// Flow: match question tokens → entity display_name/slug → inject aliases
// into recovery multi-list bags (BM25 + chunk ANN).

type entityCatalogCache struct {
	mu      sync.Mutex
	byBrain map[string]*entityBrainSlot
	now     func() time.Time
	load    func(context.Context, *sql.DB, string, int) *entityBrainCache
}

// entityBrainSlot serializes refreshes for one brain without making an
// unrelated brain wait behind the live sample's four-second SQL budget.
type entityBrainSlot struct {
	mu  sync.Mutex
	cur atomic.Pointer[entityBrainCache]
}

type entityBrainCache struct {
	// lower name/slug → display form
	names  map[string]string
	loaded time.Time
	// generation pins this immutable sample to the serving projection that
	// requested it. A generation change replaces the sample even inside TTL.
	generation string
	loadErr    string

	// Lazily-built seed-lookup index (#303) — names must not mutate after load.
	idxOnce sync.Once
	idx     *entityNameIndex
}

// index returns the seed-lookup index, building it on first use.
func (bc *entityBrainCache) index() *entityNameIndex {
	if bc == nil {
		return nil
	}
	bc.idxOnce.Do(func() {
		bc.idx = newEntityNameIndex(mapKeyStrings(bc.names))
	})
	return bc.idx
}

var globalEntityCatalog = &entityCatalogCache{byBrain: map[string]*entityBrainSlot{}}

const entityCatalogTTL = 30 * time.Minute

// entityCatalogTerms returns recovery expand strings from offline gob (HotLex
// volume) unioned with live path2_entities. Fail-soft → nil.
func (c *Client) entityCatalogTerms(ctx context.Context, question string, maxN int) []string {
	if maxN < 1 {
		return nil
	}
	// 0) Offline gob/json on HotLex volume (no Neon RTT) — seed the bag.
	var out []string
	outSeen := map[string]struct{}{}
	add := func(disp string) {
		disp = strings.TrimSpace(disp)
		if disp == "" {
			return
		}
		k := strings.ToLower(disp)
		if _, d := outSeen[k]; d {
			return
		}
		outSeen[k] = struct{}{}
		out = append(out, disp)
	}
	for _, t := range offlineEntityTermsFromCatalog(c.offlineEntityCatalog(), question, maxN) {
		add(t)
		if len(out) >= maxN {
			return out
		}
	}
	// Offline gob filled the bag — skip Neon entity SQL (was 2–8s on recovery path).
	if len(out) >= maxN/2 && maxN >= 4 {
		return limitStrings(out, maxN)
	}
	if c == nil || c.db == nil {
		return limitStrings(out, maxN)
	}
	brain := strings.TrimSpace(c.cfg.BrainID)
	if brain == "" {
		return limitStrings(out, maxN)
	}
	seeds := entitySeedsFromQuestion(question)
	if len(seeds) == 0 {
		return limitStrings(out, maxN)
	}

	// 1) Live SQL match with tight budget (recovery must stay in ≤3s wall share).
	lctx, lcancel := withTimeout(ctx, 800*time.Millisecond)
	live, err := path2EntityNameLookup(lctx, c.db, brain, seeds, maxN*2)
	lcancel()
	if err == nil {
		for _, t := range live {
			add(t)
			if len(out) >= maxN {
				return out
			}
		}
	}
	if len(out) >= maxN {
		return limitStrings(out, maxN)
	}

	// 2) Process cache of frequent entity names (warm after first load).
	cache := globalEntityCatalog.getOrLoad(ctx, c.db, brain, c.entityCatalogGeneration())
	if cache == nil || len(cache.names) == 0 {
		return limitStrings(out, maxN)
	}
	idx := cache.index()
	for _, s := range seeds {
		if disp, ok := cache.names[s]; ok {
			add(disp)
			if len(out) >= maxN {
				return out
			}
		}
		// Substring match for multi-word entities via the load-time index
		// (#303) — original Contains predicate re-verified per candidate,
		// bounded fallback inside match keeps small-catalog semantics.
		for _, slug := range idx.match(s, maxN*2, func(k string) bool {
			return strings.Contains(k, s) || strings.Contains(s, k)
		}) {
			add(cache.names[slug])
			if len(out) >= maxN {
				return out
			}
		}
		if len(out) >= maxN {
			break
		}
	}
	return out
}

func (c *Client) entityCatalogGeneration() string {
	if c == nil {
		return ""
	}
	if c.hot != nil {
		if generation := strings.TrimSpace(c.hot.Generation); generation != "" {
			return generation
		}
	}
	return strings.TrimSpace(c.GenerationID())
}

func (ec *entityCatalogCache) clock() time.Time {
	if ec != nil && ec.now != nil {
		return ec.now().UTC()
	}
	return time.Now().UTC()
}

func entityBrainCacheFresh(cur *entityBrainCache, generation string, now time.Time) bool {
	if cur == nil || cur.generation != generation {
		return false
	}
	age := now.Sub(cur.loaded)
	return age >= 0 && age < entityCatalogTTL
}

func (ec *entityCatalogCache) getOrLoad(
	ctx context.Context, db *sql.DB, brainID, generation string,
) *entityBrainCache {
	slot := ec.brainSlot(brainID)
	now := ec.clock()
	if cur := slot.cur.Load(); entityBrainCacheFresh(cur, generation, now) {
		return cur
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	now = ec.clock()
	if cur := slot.cur.Load(); entityBrainCacheFresh(cur, generation, now) {
		return cur
	}
	loader := ec.load
	if loader == nil {
		loader = loadEntityBrainSample
	}
	loaded := loader(ctx, db, brainID, 8000)
	if loaded == nil {
		loaded = &entityBrainCache{names: map[string]string{}}
	}
	loaded.loaded = ec.clock()
	loaded.generation = generation
	slot.cur.Store(loaded)
	return loaded
}

func (ec *entityCatalogCache) brainSlot(brainID string) *entityBrainSlot {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	if ec.byBrain == nil {
		ec.byBrain = map[string]*entityBrainSlot{}
	}
	slot := ec.byBrain[brainID]
	if slot == nil {
		slot = &entityBrainSlot{}
		ec.byBrain[brainID] = slot
	}
	return slot
}

func (ec *entityCatalogCache) current(brainID, generation string) *entityBrainCache {
	if ec == nil {
		return nil
	}
	ec.mu.Lock()
	slot := ec.byBrain[brainID]
	ec.mu.Unlock()
	if slot == nil {
		return nil
	}
	cur := slot.cur.Load()
	if cur == nil || cur.generation != generation {
		return nil
	}
	return cur
}

func entityNameIndexDiagnostics(ix *entityNameIndex) map[string]any {
	s := ix.stats()
	m := ix.metrics()
	return map[string]any{
		"keys":                      m.Keys,
		"tokens":                    m.Tokens,
		"grams":                     m.Grams,
		"token_postings":            m.TokenPostings,
		"gram_postings":             m.GramPostings,
		"logical_payload_bytes":     m.PayloadBytes,
		"candidates_cumulative":     s.Candidates,
		"matches_cumulative":        s.IndexMatches,
		"fallback_scans_cumulative": s.FallbackScans,
		"fallback_skips_cumulative": s.FallbackSkips,
		"truncations_cumulative":    s.Truncations,
	}
}

// entityCatalogIndexDiagnostics exposes only aggregate shape/work counters.
// It contains no query text, entity names, document IDs, ACL data, or gold IDs.
func (c *Client) entityCatalogIndexDiagnostics() map[string]any {
	if c == nil {
		return nil
	}
	out := map[string]any{}
	if cat := c.offlineEntityCatalog(); cat != nil {
		names, dsids := cat.indexes()
		out["offline_names"] = entityNameIndexDiagnostics(names)
		out["offline_dsids"] = entityNameIndexDiagnostics(dsids)
	}
	if live := globalEntityCatalog.current(strings.TrimSpace(c.cfg.BrainID), c.entityCatalogGeneration()); live != nil {
		out["live_names"] = entityNameIndexDiagnostics(live.index())
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func loadEntityBrainSample(ctx context.Context, db *sql.DB, brainID string, limit int) *entityBrainCache {
	out := &entityBrainCache{names: map[string]string{}, loaded: time.Now().UTC()}
	if db == nil || limit < 1 {
		return out
	}
	lctx, cancel := withTimeout(ctx, 4*time.Second)
	defer cancel()
	// SMF shape: display_name + slug.
	q := `
SELECT COALESCE(display_name, ''), COALESCE(slug, '')
FROM path2_entities
WHERE brain_id = $1
  AND (display_name IS NOT NULL OR slug IS NOT NULL)
ORDER BY
  CASE WHEN COALESCE(display_name, slug) ~
    '([[:alpha:]][[:alnum:]_-]*[[:digit:]]|[[:digit:]][[:alnum:]_-]*[[:alpha:]]|_)'
    THEN 0 ELSE 1 END,
  length(COALESCE(display_name, slug)) DESC
LIMIT $2`
	rows, err := db.QueryContext(lctx, q, brainID, limit)
	if err != nil {
		// Legacy name column.
		q2 := `
SELECT COALESCE(name, ''), '' FROM path2_entities
WHERE brain_id = $1
ORDER BY
  CASE WHEN name ~
    '([[:alpha:]][[:alnum:]_-]*[[:digit:]]|[[:digit:]][[:alnum:]_-]*[[:alpha:]]|_)'
    THEN 0 ELSE 1 END,
  length(name) DESC
LIMIT $2`
		rows, err = db.QueryContext(lctx, q2, brainID, limit)
		if err != nil {
			out.loadErr = err.Error()
			return out
		}
	}
	defer rows.Close()
	for rows.Next() {
		var name, slug string
		if err := rows.Scan(&name, &slug); err != nil {
			continue
		}
		name = strings.TrimSpace(name)
		slug = strings.TrimSpace(slug)
		if name != "" {
			out.names[strings.ToLower(name)] = name
		}
		if slug != "" {
			out.names[strings.ToLower(slug)] = pickNonEmpty(name, slug)
		}
	}
	return out
}

func path2EntityNameLookup(ctx context.Context, db *sql.DB, brainID string, seeds []string, maxN int) ([]string, error) {
	if db == nil || len(seeds) == 0 {
		return nil, nil
	}
	lctx, cancel := withTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	// Exact + tsv match on entities; return display names for recovery bags.
	q := `
SELECT DISTINCT COALESCE(NULLIF(e.display_name, ''), e.slug) AS nm
FROM path2_entities e
WHERE e.brain_id = $1
  AND (
    lower(e.display_name) = ANY($2::text[])
    OR lower(e.slug) = ANY($2::text[])
    OR e.tsv @@ plainto_tsquery('english', $3)
  )
  AND COALESCE(NULLIF(e.display_name, ''), e.slug) IS NOT NULL
LIMIT $4`
	bag := strings.Join(seeds, " ")
	rows, err := db.QueryContext(lctx, q, brainID, seeds, bag, maxN)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	seen := map[string]struct{}{}
	for rows.Next() {
		var nm string
		if err := rows.Scan(&nm); err != nil {
			continue
		}
		nm = strings.TrimSpace(nm)
		if nm == "" {
			continue
		}
		k := strings.ToLower(nm)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, nm)
	}
	return out, rows.Err()
}

func pickNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func limitStrings(xs []string, n int) []string {
	if n > 0 && len(xs) > n {
		return xs[:n]
	}
	return xs
}

// recoveryQueriesForClient is the product recovery bag builder: static dynamic
// bags + entity catalog + optional PRF seeds.
func (c *Client) recoveryQueriesForClient(ctx context.Context, question string, seedTexts []string, maxN int) []string {
	if maxN <= 0 {
		maxN = 10
	}
	base := recoveryQueriesDynamic(ctx, question, seedTexts, maxN+6)
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if len(s) < 3 {
			return
		}
		k := strings.ToLower(s)
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		out = append(out, s)
	}
	// Keep the original question first, then retain rare identifiers and a
	// bounded entity-alias share before generic rewrites fill the cap. The old
	// append-at-tail order commonly truncated every catalog term, making the
	// indexed catalog unreachable from the live recovery fan-out.
	if len(base) > 0 {
		add(base[0])
	}
	entityBudget := maxN / 3
	if entityBudget < 1 {
		entityBudget = 1
	}
	if entityBudget > 4 {
		entityBudget = 4
	}
	entityTerms := c.entityCatalogTerms(ctx, question, entityBudget)
	// Reserve the aliases that actually exist before identifiers are admitted:
	// a question with many ticket-like tokens must not consume their slots.
	reservedEntities := minInt(len(entityTerms), maxInt(0, maxN-len(out)))
	identifierLimit := maxN - len(out) - reservedEntities
	for _, id := range extractIdentifiers(question) {
		if identifierLimit <= 0 {
			break
		}
		before := len(out)
		add(id)
		if len(out) > before {
			identifierLimit--
		}
	}
	for _, t := range entityTerms {
		add(t)
	}
	if len(base) > 1 {
		for _, b := range base[1:] {
			add(b)
		}
	}
	// Pair an alias with the question head only when the primary terms leave
	// room; this is useful for BM25 without sacrificing identifier retention.
	for _, t := range entityTerms {
		// Pair entity with question head for BM25.
		if toks := contentTokens(question); len(toks) > 0 {
			head := strings.Join(toks[:minInt(4, len(toks))], " ")
			add(t + " " + head)
		}
	}
	if len(out) > maxN {
		out = out[:maxN]
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
