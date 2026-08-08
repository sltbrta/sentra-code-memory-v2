package hosted

import (
	"context"
	"database/sql"
	"encoding/gob"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Offline entity catalog (sentra ~70k dump spirit) — durable gob/json on HotLex
// volume so recovery does not depend on Neon RTT under burst.
//
// Layout (gob or json):
//
//	{
//	  "brain_id": "full-bench-v2",
//	  "names": { "acme corp": "Acme Corp", "inc-1234": "INC-1234" },
//	  "name_to_dsids": { "acme corp": ["dsid_…"], ... }
//	}
//
// Path resolution order:
//  1. OUROBOROS_ERB_ENTITY_GOB
//  2. $OUROBOROS_ERB_HOTLEX_PATH sibling entity-catalog.gob / .json
//  3. /hotlex/entity-catalog.gob (Modal default)

// OfflineEntityCatalog is the on-disk entity name index.
type OfflineEntityCatalog struct {
	BrainID     string              `json:"brain_id"`
	Generation  string              `json:"generation_id,omitempty"`
	Names       map[string]string   `json:"names"`         // lower → display
	NameToDSIDs map[string][]string `json:"name_to_dsids"` // lower → source dsids
	Generated   string              `json:"generated,omitempty"`

	// Lazily-built lookup indexes (#303) — unexported, never serialized.
	// Built once per catalog; Names/NameToDSIDs must not mutate afterwards.
	idxOnce sync.Once
	nameIdx *entityNameIndex
	dsidIdx *entityNameIndex
}

// offlineEntityCatalogDisk is the serialization-only shape. Keeping sync.Once
// out of this value avoids copying a live lock when an indexed catalog is
// atomically rewritten.
type offlineEntityCatalogDisk struct {
	BrainID     string              `json:"brain_id"`
	Generation  string              `json:"generation_id,omitempty"`
	Names       map[string]string   `json:"names"`
	NameToDSIDs map[string][]string `json:"name_to_dsids"`
	Generated   string              `json:"generated,omitempty"`
}

// indexes returns the seed-lookup indexes, building them on first use.
func (c *OfflineEntityCatalog) indexes() (names, dsids *entityNameIndex) {
	if c == nil {
		return nil, nil
	}
	c.idxOnce.Do(func() {
		c.nameIdx = newEntityNameIndex(mapKeyStrings(c.Names))
		c.dsidIdx = newEntityNameIndex(mapKeySliceStrings(c.NameToDSIDs))
	})
	return c.nameIdx, c.dsidIdx
}

var offlineEntityCache = &offlineEntityFileCache{}

const offlineEntityFileCheckTTL = 30 * time.Second

type offlineEntityFileCache struct {
	mu sync.Mutex

	pathConfig        entityCatalogPathConfig
	checkedGeneration string
	path              string
	size              int64
	modTime           time.Time
	fileInfo          os.FileInfo
	checked           time.Time
	cat               *OfflineEntityCatalog
	err               string
	now               func() time.Time
	resolve           func(entityCatalogPathConfig) string
	stat              func(string) (os.FileInfo, error)
}

// entityCatalogPathConfig is the cheap, comparable identity of the configured
// discovery roots. A change bypasses the file-check TTL; an unchanged value
// lets a scoped cache hit avoid path discovery and filesystem metadata calls.
type entityCatalogPathConfig struct {
	explicit string
	hotLex   string
}

func (c *Client) offlineEntityCatalog() *OfflineEntityCatalog {
	if c == nil {
		return nil
	}
	return offlineEntityCache.load(c.cfg.BrainID, c.entityCatalogGeneration())
}

// WarmEntityCatalog decodes and indexes the scoped offline generation during
// process warm-up, keeping the one-time large-catalog build off the first
// recovery query. It is fail-soft when no compatible catalog is configured.
func (c *Client) WarmEntityCatalog() (names, dsidKeys int) {
	cat := c.offlineEntityCatalog()
	if cat == nil {
		return 0, 0
	}
	nameIdx, dsidIdx := cat.indexes()
	return nameIdx.size(), dsidIdx.size()
}

func (fc *offlineEntityFileCache) clock() time.Time {
	if fc != nil && fc.now != nil {
		return fc.now().UTC()
	}
	return time.Now().UTC()
}

func currentEntityCatalogPathConfig() entityCatalogPathConfig {
	return entityCatalogPathConfig{
		explicit: strings.TrimSpace(os.Getenv("OUROBOROS_ERB_ENTITY_GOB")),
		hotLex:   strings.TrimSpace(os.Getenv("OUROBOROS_ERB_HOTLEX_PATH")),
	}
}

func (fc *offlineEntityFileCache) resolvePath(config entityCatalogPathConfig) string {
	if fc != nil && fc.resolve != nil {
		return fc.resolve(config)
	}
	return resolveEntityCatalogPath(config)
}

func (fc *offlineEntityFileCache) statPath(path string) (os.FileInfo, error) {
	if fc != nil && fc.stat != nil {
		return fc.stat(path)
	}
	return os.Stat(path)
}

func catalogMatchesScope(cat *OfflineEntityCatalog, brainID, generation string) bool {
	if cat == nil {
		return false
	}
	brainID = strings.TrimSpace(brainID)
	generation = strings.TrimSpace(generation)
	if brainID != "" {
		if got := strings.TrimSpace(cat.BrainID); got == "" || got != brainID {
			return false
		}
	}
	// Legacy catalogs did not carry a generation. They remain TTL/file-identity
	// safe; newly generated catalogs fail closed on an explicit generation
	// mismatch.
	if got := strings.TrimSpace(cat.Generation); generation != "" && got != "" && got != generation {
		return false
	}
	return true
}

func (fc *offlineEntityFileCache) load(brainID, generation string) *OfflineEntityCatalog {
	pathConfig := currentEntityCatalogPathConfig()
	generation = strings.TrimSpace(generation)
	now := fc.clock()
	fc.mu.Lock()
	defer fc.mu.Unlock()
	setErr := func(s string) {
		fc.err = s
	}
	// Path discovery can itself issue several stats. Reuse the resolved path and
	// file identity for a fresh scoped hit, but only while the environment-backed
	// discovery identity is unchanged. A generation mismatch continues below so
	// an atomic replacement for the newly pinned generation is observed now.
	if fc.pathConfig == pathConfig && !fc.checked.IsZero() {
		age := now.Sub(fc.checked)
		if age >= 0 && age < offlineEntityFileCheckTTL {
			if fc.cat == nil {
				if fc.checkedGeneration == generation {
					return nil
				}
			} else {
				if catalogMatchesScope(fc.cat, brainID, generation) {
					setErr("")
					return fc.cat
				}
				if got := strings.TrimSpace(fc.cat.BrainID); strings.TrimSpace(brainID) != "" &&
					got != strings.TrimSpace(brainID) {
					setErr("scope_mismatch")
					return nil
				}
			}
		}
	}
	path := fc.resolvePath(pathConfig)
	if path == "" {
		fc.cat = nil
		fc.path = ""
		fc.pathConfig = entityCatalogPathConfig{}
		fc.checked = time.Time{}
		setErr("no_path")
		return nil
	}
	st, err := fc.statPath(path)
	if err != nil || st.IsDir() {
		fc.cat = nil
		fc.path = path
		fc.pathConfig = pathConfig
		fc.checkedGeneration = generation
		fc.checked = now
		if err != nil {
			setErr(err.Error())
		} else {
			setErr("catalog_is_directory")
		}
		return nil
	}
	if fc.cat != nil && fc.path == path && fc.fileInfo != nil && os.SameFile(fc.fileInfo, st) &&
		fc.size == st.Size() && fc.modTime.Equal(st.ModTime()) {
		fc.pathConfig = pathConfig
		fc.checkedGeneration = generation
		fc.checked = now
		setErr("")
		if catalogMatchesScope(fc.cat, brainID, generation) {
			return fc.cat
		}
		setErr("scope_mismatch")
		return nil
	}
	cat, err := readEntityCatalogFile(path)
	fc.path = path
	fc.pathConfig = pathConfig
	fc.checkedGeneration = generation
	fc.size = st.Size()
	fc.modTime = st.ModTime()
	fc.fileInfo = st
	fc.checked = now
	if err != nil {
		fc.cat = nil
		setErr(err.Error())
		return nil
	}
	fc.cat = cat
	setErr("")
	if !catalogMatchesScope(cat, brainID, generation) {
		setErr("scope_mismatch")
		return nil
	}
	return cat
}

func resolveEntityCatalogPath(config entityCatalogPathConfig) string {
	if config.explicit != "" {
		return config.explicit
	}
	if config.hotLex != "" {
		dir := filepath.Dir(config.hotLex)
		for _, name := range []string{"entity-catalog.gob", "entity-catalog.json", "entity-catalog-full.gob"} {
			cand := filepath.Join(dir, name)
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				return cand
			}
		}
	}
	for _, cand := range []string{
		"/hotlex/entity-catalog.gob",
		"/hotlex/entity-catalog.json",
		"/hotlex/entity-catalog-full.gob",
	} {
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	return ""
}

func readEntityCatalogFile(path string) (*OfflineEntityCatalog, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cat := &OfflineEntityCatalog{}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		if err := json.NewDecoder(f).Decode(cat); err != nil {
			return nil, err
		}
	default:
		// gob
		if err := gob.NewDecoder(f).Decode(cat); err != nil {
			return nil, err
		}
	}
	if cat.Names == nil {
		cat.Names = map[string]string{}
	}
	if cat.NameToDSIDs == nil {
		cat.NameToDSIDs = map[string][]string{}
	}
	return cat, nil
}

// WriteOfflineEntityCatalog atomically replaces gob (or json if path ends
// .json), then syncs the containing directory so a successful return includes
// durability of the renamed directory entry.
func WriteOfflineEntityCatalog(path string, cat *OfflineEntityCatalog) error {
	return writeOfflineEntityCatalog(path, cat, syncEntityCatalogDirectory)
}

func writeOfflineEntityCatalog(path string, cat *OfflineEntityCatalog, syncDirectory func(string) error) error {
	if cat == nil {
		return os.ErrInvalid
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".entity-catalog-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	committed := false
	defer func() {
		_ = f.Close()
		if !committed {
			_ = os.Remove(tmp)
		}
	}()
	// Do not mutate or copy the locks of a catalog whose lazy indexes may be
	// serving concurrently.
	encCat := offlineEntityCatalogDisk{
		BrainID:     cat.BrainID,
		Generation:  cat.Generation,
		Names:       cat.Names,
		NameToDSIDs: cat.NameToDSIDs,
		Generated:   time.Now().UTC().Format(time.RFC3339),
	}
	if err := f.Chmod(0o644); err != nil {
		return err
	}
	var encodeErr error
	if strings.EqualFold(filepath.Ext(path), ".json") {
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		encodeErr = enc.Encode(&encCat)
	} else {
		encodeErr = gob.NewEncoder(f).Encode(&encCat)
	}
	if encodeErr != nil {
		return encodeErr
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	committed = true
	return syncDirectory(dir)
}

func syncEntityCatalogDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

// DumpEntityCatalogFromDB builds an offline catalog from path2_entities (Neon).
// Cap limits memory; identifier-shaped names are retained first, then longer
// names (more specific), without accepting any query/evaluation inputs.
func DumpEntityCatalogFromDB(ctx context.Context, db *sql.DB, brainID string, limit int) (*OfflineEntityCatalog, error) {
	if db == nil {
		return nil, os.ErrInvalid
	}
	if limit <= 0 {
		limit = 80000
	}
	brainID = strings.TrimSpace(brainID)
	if brainID == "" {
		return nil, os.ErrInvalid
	}
	lctx, cancel := withTimeout(ctx, 120*time.Second)
	defer cancel()
	q := `
SELECT COALESCE(display_name, ''), COALESCE(slug, ''),
       COALESCE(array_to_string(source_dsids, ','), '')
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
		// Legacy without source_dsids array_to_string.
		q2 := `
SELECT COALESCE(name, ''), '', ''
FROM path2_entities WHERE brain_id = $1
ORDER BY
  CASE WHEN name ~
    '([[:alpha:]][[:alnum:]_-]*[[:digit:]]|[[:digit:]][[:alnum:]_-]*[[:alpha:]]|_)'
    THEN 0 ELSE 1 END,
  length(name) DESC
LIMIT $2`
		rows, err = db.QueryContext(lctx, q2, brainID, limit)
		if err != nil {
			return nil, err
		}
	}
	defer rows.Close()
	cat := &OfflineEntityCatalog{
		BrainID:     brainID,
		Names:       map[string]string{},
		NameToDSIDs: map[string][]string{},
	}
	for rows.Next() {
		var name, slug, dsidsCSV string
		if err := rows.Scan(&name, &slug, &dsidsCSV); err != nil {
			continue
		}
		name = strings.TrimSpace(name)
		slug = strings.TrimSpace(slug)
		var dsids []string
		for _, d := range strings.Split(dsidsCSV, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				dsids = append(dsids, d)
			}
		}
		add := func(key, disp string) {
			key = strings.ToLower(strings.TrimSpace(key))
			if len(key) < 3 {
				return
			}
			if disp == "" {
				disp = key
			}
			if _, ok := cat.Names[key]; !ok {
				cat.Names[key] = disp
			}
			if len(dsids) > 0 {
				cat.NameToDSIDs[key] = uniqueStringsStable(append(cat.NameToDSIDs[key], dsids...))
			}
		}
		add(name, name)
		if slug != "" && !strings.EqualFold(slug, name) {
			add(slug, pickNonEmpty(name, slug))
		}
	}
	return cat, rows.Err()
}

func (c *Client) scopedOfflineEntityTerms(question string, maxN int) []string {
	if c == nil {
		return nil
	}
	return offlineEntityTermsFromCatalog(c.offlineEntityCatalog(), question, maxN)
}

// offlineEntityTermsFromCatalog matches seeds via the catalog index (#303)
// instead of scanning every catalog key per seed. Exact hits come first per
// seed; indexed candidates are verified with the original substring predicate
// so accepted matches are identical to the old scan, in deterministic
// longest-key-first order (the old map-iteration order was random).
func offlineEntityTermsFromCatalog(cat *OfflineEntityCatalog, question string, maxN int) []string {
	if cat == nil || maxN < 1 {
		return nil
	}
	seeds := entitySeedsFromQuestion(question)
	if len(seeds) == 0 {
		return nil
	}
	nameIdx, _ := cat.indexes()
	var out []string
	seen := map[string]struct{}{}
	add := func(disp string) {
		// Guard empty display values: a catalog whose stored keys are not
		// already normalized (e.g. hand-written JSON with "Acme Corp") makes
		// cat.Names[<normalized index key>] miss, and display values may be
		// blank — never emit an empty recovery term.
		disp = strings.TrimSpace(disp)
		if disp == "" {
			return
		}
		k := strings.ToLower(disp)
		if _, d := seen[k]; d {
			return
		}
		seen[k] = struct{}{}
		out = append(out, disp)
	}
	for _, s := range seeds {
		if disp, ok := cat.Names[s]; ok {
			add(disp)
			if len(out) >= maxN {
				break
			}
		}
		for _, slug := range nameIdx.match(s, maxN*2, func(k string) bool {
			return strings.Contains(k, s) || (len(s) >= 5 && strings.Contains(s, k))
		}) {
			add(cat.Names[slug])
			if len(out) >= maxN {
				return limitStrings(out, maxN)
			}
		}
		if len(out) >= maxN {
			break
		}
	}
	return limitStrings(out, maxN)
}

func (c *Client) scopedOfflineEntityDSIDs(question string, maxN int) []string {
	if c == nil {
		return nil
	}
	return offlineEntityDSIDsFromCatalog(c.offlineEntityCatalog(), question, maxN)
}

// offlineEntityDSIDsFromCatalog resolves seeds → dsids via the catalog index
// (#303) instead of scanning every NameToDSIDs key per seed. Exact key hits
// come first per seed; substring matches keep the original predicate and are
// emitted in deterministic longest-key-first order.
func offlineEntityDSIDsFromCatalog(cat *OfflineEntityCatalog, question string, maxN int) []string {
	if cat == nil || maxN < 1 {
		return nil
	}
	seeds := entitySeedsFromQuestion(question)
	_, dsidIdx := cat.indexes()
	var out []string
	seen := map[string]struct{}{}
	addAll := func(ds []string) bool {
		for _, d := range ds {
			if _, ok := seen[d]; ok {
				continue
			}
			seen[d] = struct{}{}
			out = append(out, d)
			if len(out) >= maxN {
				return true
			}
		}
		return false
	}
	for _, s := range seeds {
		if addAll(cat.NameToDSIDs[s]) {
			return out
		}
		// substring match on keys, via index
		for _, key := range dsidIdx.match(s, maxN*2, func(k string) bool {
			return strings.Contains(k, s) || (len(s) >= 5 && strings.Contains(s, k))
		}) {
			if addAll(cat.NameToDSIDs[key]) {
				return out
			}
		}
	}
	return out
}

func entitySeedsFromQuestion(question string) []string {
	var seeds []string
	seen := map[string]struct{}{}
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if len(s) < 3 {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		seeds = append(seeds, s)
	}
	for _, id := range extractIdentifiers(question) {
		add(id)
	}
	for _, t := range contentTokens(question) {
		if len(t) >= 4 {
			add(t)
		}
	}
	if len(seeds) > 12 {
		seeds = seeds[:12]
	}
	return seeds
}

func (c *Client) scopedOfflineEntityHits(question string, maxN int) []Hit {
	if c == nil {
		return nil
	}
	return offlineEntityHitsFromCatalog(c.offlineEntityCatalog(), question, maxN)
}

func offlineEntityHitsFromCatalog(cat *OfflineEntityCatalog, question string, maxN int) []Hit {
	ids := offlineEntityDSIDsFromCatalog(cat, question, maxN)
	if len(ids) == 0 {
		return nil
	}
	out := make([]Hit, 0, len(ids))
	for i, id := range ids {
		out = append(out, Hit{
			DSID:    id,
			ChunkID: id + "#entity",
			Score:   1.0 / float64(i+1),
			Channel: "entity_catalog_offline",
			Text:    "", // hydrate later by DSID (synthetic chunk_id)
		})
	}
	return out
}

// hydrateOfflineEntityStubs replaces empty entity_catalog_offline passages with
// real Neon sibling chunks. Synthetic chunk_id "dsid#entity" never hydrates by_id.
const offlineEntityHydrateConcurrency = 4

// mergeOfflineEntityStubHydrates preserves the old tail placement of hydrated
// entity passages. Keeping unresolved stubs visible preserves document identity
// for structure/citation floors when a bounded hydrate misses its deadline.
func mergeOfflineEntityStubHydrates(
	pool []Passage, need []string, hydrated [][]Passage,
) []Passage {
	byDSID := make(map[string][]Passage, len(need))
	for i, dsid := range need {
		if i < len(hydrated) {
			byDSID[dsid] = hydrated[i]
		}
	}
	out := make([]Passage, 0, len(pool)+len(need))
	for _, p := range pool {
		if strings.Contains(p.Channel, "entity_catalog_offline") && strings.TrimSpace(p.Text) == "" {
			continue
		}
		out = append(out, p)
	}
	for _, dsid := range need {
		hits := byDSID[dsid]
		if len(hits) == 0 {
			out = append(out, Passage{
				DocumentID: dsid,
				ChunkID:    dsid + "#entity",
				Channel:    "entity_catalog_offline",
				Score:      0.2,
			})
			continue
		}
		out = append(out, hits...)
	}
	return out
}

func offlineEntityStubIDs(pool []Passage) []string {
	var need []string
	seen := map[string]struct{}{}
	for _, p := range pool {
		if !strings.Contains(p.Channel, "entity_catalog_offline") ||
			strings.TrimSpace(p.Text) != "" || p.DocumentID == "" {
			continue
		}
		if _, ok := seen[p.DocumentID]; ok {
			continue
		}
		seen[p.DocumentID] = struct{}{}
		need = append(need, p.DocumentID)
	}
	return need
}

func hydrateOfflineEntityStubs(
	ctx context.Context, db *sql.DB, cfg Config, pool []Passage, perDoc int,
) []Passage {
	if db == nil || len(pool) == 0 {
		return pool
	}
	if perDoc < 1 {
		perDoc = 2
	}
	need := offlineEntityStubIDs(pool)
	if len(need) == 0 {
		return pool
	}
	return hydrateOfflineEntityStubsWith(ctx, cfg, pool, need,
		func(fetchCtx context.Context, dsid string) ([]Hit, error) {
			return siblingChunks(fetchCtx, db, cfg, dsid, perDoc)
		},
	)
}

func hydrateOfflineEntityStubsWith(
	ctx context.Context,
	cfg Config,
	pool []Passage,
	need []string,
	fetch func(context.Context, string) ([]Hit, error),
) []Passage {
	hydrated := make([][]Passage, len(need))
	sem := make(chan struct{}, offlineEntityHydrateConcurrency)
	var wg sync.WaitGroup
	for i, dsid := range need {
		wg.Add(1)
		go func(i int, dsid string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			hits, err := fetch(ctx, dsid)
			if err != nil || len(hits) == 0 {
				return
			}
			passages := make([]Passage, 0, len(hits))
			for _, h := range hits {
				passages = append(passages, Passage{
					DocumentID: dsid,
					Text:       clipPassageText(h.Text, storagePassageChars(cfg.MaxPassageChars)),
					Score:      0.45,
					ChunkID:    h.ChunkID,
					SourceURI:  h.SourceURI,
					Channel:    "entity_catalog_offline",
				})
			}
			hydrated[i] = passages
		}(i, dsid)
	}
	wg.Wait()
	return mergeOfflineEntityStubHydrates(pool, need, hydrated)
}
