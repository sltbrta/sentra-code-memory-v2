package hosted

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hsParDriver is a BrainID-scoped fake Neon driver for the interactive
// hydrate∥structure section. Every query pays Delay (cancelable), records the
// query text and first (brain_id) argument, and answers by query shape:
// hydrate-by-id, sibling chunks, and path2 relationship rows. Unknown shapes
// return no rows. Rows are only served when the brain argument matches Brain,
// mirroring store-side tenant enforcement.
type hsParDriver struct {
	Brain string
	Delay time.Duration
	// Optional per-arm delays so the hydrate and structure arms can pay
	// genuinely different store latencies (Blocker 5). When zero, the shape
	// falls back to Delay. hydrate-by-id uses HydrateByIDDelay; sibling /
	// entity-stub / path2-doc-metadata hydration uses HydrateDelay; the path2
	// relationship/entity/fact SQL uses StructureDelay.
	HydrateByIDDelay time.Duration
	HydrateDelay     time.Duration
	StructureDelay   time.Duration

	mu      sync.Mutex
	queries []string
	brains  []string
	relDocs []string
}

func (d *hsParDriver) record(query string, args []driver.NamedValue) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queries = append(d.queries, query)
	if len(args) > 0 {
		if s, ok := args[0].Value.(string); ok {
			d.brains = append(d.brains, s)
		}
	}
}

func (d *hsParDriver) snapshot() ([]string, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.queries...), append([]string(nil), d.brains...)
}

// delayFor picks a shape-specific store latency so the two overlapped arms can
// run for genuinely different durations. Hydrate-by-id is a point lookup;
// sibling/entity/path2-doc hydration is the hydrate arm tail; the path2
// relationship/entity/fact SQL is the structure arm. Zero falls back to Delay.
func (d *hsParDriver) delayFor(query string) time.Duration {
	switch {
	case strings.Contains(query, "chunk_id IN ("):
		if d.HydrateByIDDelay > 0 {
			return d.HydrateByIDDelay
		}
	case strings.Contains(query, "path2_relationships"),
		strings.Contains(query, "path2_entities"),
		strings.Contains(query, "path2_facts"):
		if d.StructureDelay > 0 {
			return d.StructureDelay
		}
	default:
		// path2_chunk_metadata (path2-doc hydration) and sibling chunks hydrate
		// on the hydrate arm; everything else uses the generic hydrate delay.
		if d.HydrateDelay > 0 {
			return d.HydrateDelay
		}
	}
	return d.Delay
}

// relationshipSeedArg pulls the seed dsid array ($2) from a path2 relationship
// query so the fake store can model a seed-aware graph.
func relationshipSeedArg(args []driver.NamedValue) []string {
	if len(args) < 2 {
		return nil
	}
	if seeds, ok := args[1].Value.([]string); ok {
		return seeds
	}
	return nil
}

// relationshipPeers models a tiny 2-hop graph so the project second hop surfaces
// FRESH docs. path2QueryRelationships excludes seeds from its result, so a flat
// relDocs list would make hop2 (seeded with hop1's docs) find nothing. Here,
// ordinary HotLex corpus docs reach the configured first-hop peers (relDocs),
// and first-hop peers reach the synthetic second-hop peers docN/docP. Seeds are
// still returned; the caller excludes them.
func (d *hsParDriver) relationshipPeers(seeds []string) []string {
	firstHop := append([]string(nil), d.relDocs...)
	secondHop := []string{"docN", "docP"}
	firstSet := map[string]bool{}
	for _, p := range firstHop {
		firstSet[p] = true
	}
	hasOrdinary, hasFirstHop := false, false
	for _, s := range seeds {
		if firstSet[s] {
			hasFirstHop = true
			continue
		}
		if s == "docN" || s == "docP" {
			continue
		}
		hasOrdinary = true
	}
	var out []string
	if hasOrdinary || len(seeds) == 0 {
		out = append(out, firstHop...)
	}
	if hasFirstHop {
		out = append(out, secondHop...)
	}
	return out
}

type hsParConn struct{ d *hsParDriver }

func (c *hsParConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *hsParConn) Close() error                        { return nil }
func (c *hsParConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

// CheckNamedValue accepts []string seed arrays (path2 relationship ANY($2));
// everything else falls back to the default converter.
func (c *hsParConn) CheckNamedValue(nv *driver.NamedValue) error {
	if _, ok := nv.Value.([]string); ok {
		return nil
	}
	return driver.ErrSkip
}

func (c *hsParConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.d.record(query, args)
	if d := c.d.delayFor(query); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	brain := ""
	if len(args) > 0 {
		brain, _ = args[0].Value.(string)
	}
	if brain != c.d.Brain {
		// Store-side ACL: a mismatched tenant scope never sees rows.
		return &hsParRows{columns: []string{"dsid"}}, nil
	}
	switch {
	case strings.Contains(query, "chunk_id IN ("):
		cols := []string{"chunk_id", "dsid", "text_content", "source_uri"}
		var vals [][]driver.Value
		for _, a := range args[1:] {
			id, _ := a.Value.(string)
			if id == "" {
				continue
			}
			// Synthetic entity-stub IDs ("dsid#entity") have no real chunk row,
			// so hydrate-by-id never resolves them — they stay empty-text for
			// the dedicated entity-stub hydrate (mirrors real Neon).
			if strings.HasSuffix(id, "#entity") {
				continue
			}
			dsid := strings.SplitN(id, "#", 2)[0]
			vals = append(vals, []driver.Value{id, dsid, "hydrated body for " + id, "uri://" + dsid})
		}
		return &hsParRows{columns: cols, values: vals}, nil
	case strings.Contains(query, "path2_relationships"):
		// Seed-aware 2-hop graph so the project second hop surfaces FRESH docs
		// (path2QueryRelationships excludes seeds from results, so a flat relDocs
		// list would make hop2 find only hop1's docs and return nothing). Hot
		// docs reach the first-hop peers (relDocs); first-hop peers reach the
		// synthetic second-hop peers (docN, docP).
		seeds := relationshipSeedArg(args)
		rels := c.d.relationshipPeers(seeds)
		var vals [][]driver.Value
		for _, r := range rels {
			vals = append(vals, []driver.Value{r})
		}
		return &hsParRows{columns: []string{"dsid"}, values: vals}, nil
	case strings.Contains(query, "path2_entities"), strings.Contains(query, "path2_facts"):
		return &hsParRows{columns: []string{"dsid"}}, nil
	case strings.Contains(query, "path2_chunk_metadata"):
		dsid := ""
		if len(args) > 1 {
			dsid, _ = args[1].Value.(string)
		}
		vals := [][]driver.Value{
			{dsid + "#s1", "sibling one for " + dsid, "uri://" + dsid},
			{dsid + "#s2", "sibling two for " + dsid, "uri://" + dsid},
		}
		return &hsParRows{columns: []string{"chunk_id", "text_content", "source_uri"}, values: vals}, nil
	default:
		return &hsParRows{columns: []string{"dsid"}}, nil
	}
}

type hsParRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *hsParRows) Columns() []string { return r.columns }
func (r *hsParRows) Close() error      { return nil }
func (r *hsParRows) Next(dest []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

var hsParDriverID atomic.Int64

func openHSParTestDB(t *testing.T, d *hsParDriver) *sql.DB {
	t.Helper()
	name := "hosted_hs_parallel_test_" + strconv.FormatInt(hsParDriverID.Add(1), 10)
	sql.Register(name, hsParDriverFunc(func() (driver.Conn, error) {
		return &hsParConn{d: d}, nil
	}))
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type hsParDriverFunc func() (driver.Conn, error)

func (f hsParDriverFunc) Open(string) (driver.Conn, error) { return f() }

// newHSParTestClient builds a hermetic interactive client: HotLex projection
// with one strong doc, one index-only (hydrate-by-id) chunk, and eight weak
// single-token docs, plus the fake Neon store. No dense/FTS/remote CE.
func newHSParTestClient(t *testing.T, brain string, delay time.Duration, relDocs []string) (*Client, *hsParDriver) {
	t.Helper()
	dr := &hsParDriver{Brain: brain, Delay: delay, relDocs: relDocs}
	hot := NewHotLex(brain)
	hot.AddChunk("docA#c1", "docA",
		"atlas launch report primary overview atlas launch report summary atlas launch report findings", "uri://docA")
	hot.AddChunkIndexOnly("docB#c1", "docB", "atlas launch report schematic appendix beta", "uri://docB")
	for i := 0; i < 8; i++ {
		dsid := "doc" + string(rune('C'+i))
		hot.AddChunk(dsid+"#c1", dsid, "atlas program evidence record number "+strconv.Itoa(i), "uri://"+dsid)
	}
	hot.Finalize()
	c := &Client{
		cfg: Config{BrainID: brain, MaxPassageChars: 2000, RRFK: 60, LexicalLimit: 20},
		db:  openHSParTestDB(t, dr),
		hot: hot,
	}
	return c, dr
}

func hsParTestEnvs(t *testing.T) {
	t.Helper()
	t.Setenv("OUROBOROS_ERB_SKIP_DENSE", "1")
	t.Setenv("OUROBOROS_ERB_SKIP_FTS", "1")
	t.Setenv("OUROBOROS_ERB_FORCE_LEXICAL_CE", "1")
	t.Setenv("OUROBOROS_ERB_SKIP_SIBLING", "0")
	t.Setenv("OUROBOROS_ERB_FORCE_RESIDUAL", "0")
	t.Setenv("OUROBOROS_ERB_SERIAL_HYDRATE_STRUCTURE", "0")
	t.Setenv("COHERE_API_KEY", "")
	t.Setenv("CO_API_KEY", "")
	t.Setenv("ZEROENTROPY_API_KEY", "")
}

func hsParProd() ProdProfile {
	return ProdProfile{
		Enabled:            true,
		LexTimeout:         time.Second,
		LexLimit:           40,
		HydrateDocs:        4,
		HydrateChunks:      2,
		HydrateDocsMulti:   6,
		HydrateChunksMulti: 4,
		StructureMaxNeigh:  12,
		PoolK:              64,
	}
}

func windowPassageKeys(window []Passage) []string {
	keys := make([]string, 0, len(window))
	for _, p := range window {
		keys = append(keys, p.ChunkID+"|"+p.DocumentID+"|"+p.SourceURI)
	}
	return keys
}

// windowPassageFullKeys captures text/score/channel/locator so any serial-
// equivalence claim is checked on the full passage contract, not just the
// identity triple (Blocker 5). Score is rendered at 1e-6 precision so tiny
// float noise from deterministic merges does not spuriously diverge.
func windowPassageFullKeys(window []Passage) []string {
	keys := make([]string, 0, len(window))
	for _, p := range window {
		text := p.Text
		if len(text) > 48 {
			text = text[:48]
		}
		keys = append(keys, strings.Join([]string{
			p.ChunkID, p.DocumentID, p.SourceURI, p.Channel,
			text, strconv.Itoa(int(p.Score * 1e6)),
		}, "\x1f"))
	}
	return keys
}

// TestInteractiveParallelWallVsArmDiagnostics proves the hydrate and structure
// arms overlap: the section wall must be strictly below the sum of arm walls,
// and each arm reports its own time. The arms pay GENUINELY DIFFERENT store
// latencies (hydrate-by-id 180ms, path2 SQL 120ms) so the overlap margin is
// not masked by symmetric per-query delays (Blocker 5).
func TestInteractiveParallelWallVsArmDiagnostics(t *testing.T) {
	hsParTestEnvs(t)
	c, dr := newHSParTestClient(t, "tenant-par-wall", 0, []string{"docK", "docL"})
	dr.HydrateByIDDelay = 180 * time.Millisecond
	dr.StructureDelay = 120 * time.Millisecond

	window, diag, err := c.retrieveInteractive(
		context.Background(), "atlas launch report", RetrieveOptions{}, map[string]any{},
		8, 32, time.Now(), hsParProd(), "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(window) == 0 {
		t.Fatal("expected a non-empty evidence window")
	}
	if diag["hydrate_structure_parallel"] != true {
		t.Fatalf("parallel flag not stamped: %v", diag["hydrate_structure_parallel"])
	}
	hydArm, _ := diag["hydrate_arm_ms"].(int64)
	structArm, _ := diag["structure_arm_ms"].(int64)
	wall, _ := diag["hydrate_structure_wall_ms"].(int64)
	if hydArm <= 0 || structArm <= 0 || wall <= 0 {
		t.Fatalf("arm diagnostics missing: hydrate_arm_ms=%v structure_arm_ms=%v wall=%v",
			diag["hydrate_arm_ms"], diag["structure_arm_ms"], diag["hydrate_structure_wall_ms"])
	}
	if wall >= hydArm+structArm {
		t.Fatalf("no overlap: wall=%dms >= hydrate %dms + structure %dms", wall, hydArm, structArm)
	}
	// Real overlap margin: arms both paid ≥120ms store delays, so the wall must
	// beat the serial sum by a clear margin, not just timer noise.
	if wall*4 >= (hydArm+structArm)*3 {
		t.Fatalf("weak overlap: wall=%dms sum=%dms", wall, hydArm+structArm)
	}
	// Hydrate-by-id found the index-only chunk and failed nowhere.
	if n, _ := diag["hydrate_by_id_n"].(int); n < 1 {
		t.Fatalf("hydrate-by-id did not run: %v", diag["hydrate_by_id_n"])
	}
	if _, failed := diag["hydrate_by_id_error"]; failed {
		t.Fatalf("hydrate-by-id failed: %v", diag["hydrate_by_id_error"])
	}
}

// TestInteractiveParallelDeterministicOrdering proves repeated parallel runs
// under jittered store latency produce byte-identical evidence ordering and
// citation metadata: final ordering never depends on arm completion order.
func TestInteractiveParallelDeterministicOrdering(t *testing.T) {
	hsParTestEnvs(t)
	var first []string
	for i := 0; i < 10; i++ {
		c, _ := newHSParTestClient(t, "tenant-par-det", time.Duration(3+i%5)*time.Millisecond, []string{"docK"})
		window, _, err := c.retrieveInteractive(
			context.Background(), "atlas launch report", RetrieveOptions{}, map[string]any{},
			8, 32, time.Now(), hsParProd(), "",
		)
		if err != nil {
			t.Fatal(err)
		}
		keys := windowPassageKeys(window)
		if len(keys) == 0 {
			t.Fatal("empty window")
		}
		if first == nil {
			first = keys
			continue
		}
		if len(keys) != len(first) {
			t.Fatalf("run %d window size %d != first %d", i, len(keys), len(first))
		}
		for j := range first {
			if keys[j] != first[j] {
				t.Fatalf("run %d ordering diverged at %d:\nfirst=%v\nnow=%v", i, j, first, keys)
			}
		}
	}
}

// TestInteractiveParallelSerialEquivalent proves the parallel section is
// quality-equivalent to the serial ordering: same window, same citations, same
// counters, on a pool without empty entity stubs (where seeds are identical).
// Equivalence is checked on the FULL passage contract (text/score/channel/
// locator), not just the identity triple (Blocker 5).
func TestInteractiveParallelSerialEquivalent(t *testing.T) {
	hsParTestEnvs(t)
	run := func(serial bool) ([]string, map[string]any) {
		if serial {
			t.Setenv("OUROBOROS_ERB_SERIAL_HYDRATE_STRUCTURE", "1")
		} else {
			t.Setenv("OUROBOROS_ERB_SERIAL_HYDRATE_STRUCTURE", "0")
		}
		c, _ := newHSParTestClient(t, "tenant-par-equiv", 10*time.Millisecond, []string{"docK", "docL"})
		window, diag, err := c.retrieveInteractive(
			context.Background(), "atlas launch report", RetrieveOptions{}, map[string]any{},
			8, 32, time.Now(), hsParProd(), "",
		)
		if err != nil {
			t.Fatal(err)
		}
		return windowPassageFullKeys(window), diag
	}
	parKeys, parDiag := run(false)
	serKeys, serDiag := run(true)
	if len(parKeys) == 0 || len(parKeys) != len(serKeys) {
		t.Fatalf("window sizes differ: parallel=%d serial=%d", len(parKeys), len(serKeys))
	}
	for i := range parKeys {
		if parKeys[i] != serKeys[i] {
			t.Fatalf("full passage diverged at %d:\nparallel=%v\nserial=%v", i, parKeys, serKeys)
		}
	}
	for _, k := range []string{"passage_count", "hydrate_by_id_n", "smf_facts_injected", "structure_sql_promoted"} {
		if parDiag[k] != serDiag[k] {
			t.Fatalf("diag %q differs: parallel=%v serial=%v", k, parDiag[k], serDiag[k])
		}
	}
	if parDiag["hydrate_structure_parallel"] != true || serDiag["hydrate_structure_parallel"] != false {
		t.Fatalf("mode flags wrong: parallel=%v serial=%v",
			parDiag["hydrate_structure_parallel"], serDiag["hydrate_structure_parallel"])
	}
}

// TestInteractiveParallelArmDeadlinesBounded proves a stalled store cannot
// stretch the section past its per-arm budgets, and that arm diagnostics still
// report under deadline pressure.
func TestInteractiveParallelArmDeadlinesBounded(t *testing.T) {
	hsParTestEnvs(t)
	t.Setenv("OUROBOROS_ERB_HYDRATE_TIMEOUT_MS", "120")
	t.Setenv("OUROBOROS_BRAIN_STRUCTURE_SQL_MS", "150")
	c, _ := newHSParTestClient(t, "tenant-par-deadline", 600*time.Millisecond, []string{"docK"})

	start := time.Now()
	window, diag, err := c.retrieveInteractive(
		context.Background(), "atlas launch report", RetrieveOptions{}, map[string]any{},
		8, 32, time.Now(), hsParProd(), "",
	)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(window) == 0 {
		t.Fatal("stalled arms must degrade, not empty the window")
	}
	// Serial stall would be ≥5 delayed queries × 600ms; budgets keep each arm
	// to a few hundred ms and overlap keeps the wall near max(arm), not sum.
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("section not deadline-bounded: took %v", elapsed)
	}
	if wall, _ := diag["hydrate_structure_wall_ms"].(int64); wall <= 0 || wall > 2000 {
		t.Fatalf("wall diag out of bounds: %v", diag["hydrate_structure_wall_ms"])
	}
	if diag["hydrate_by_id_n"] == nil {
		t.Fatalf("hydrate diagnostics missing under deadline: %v", diag)
	}
}

// TestInteractiveParallelPrecanceledContext proves a done context short-
// circuits before fanout and never spawns the overlapped section.
func TestInteractiveParallelPrecanceledContext(t *testing.T) {
	hsParTestEnvs(t)
	c, dr := newHSParTestClient(t, "tenant-par-cancel", time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, diag, err := c.retrieveInteractive(ctx, "atlas launch report", RetrieveOptions{}, map[string]any{},
		8, 32, time.Now(), hsParProd(), "")
	if err == nil {
		t.Fatal("precanceled context must fail retrieve")
	}
	if diag["retrieval_context_done_before_fanout"] != true {
		t.Fatalf("expected pre-fanout short-circuit, diag=%v", diag)
	}
	if queries, _ := dr.snapshot(); len(queries) != 0 {
		t.Fatalf("precanceled retrieve issued %d store queries", len(queries))
	}
}

// TestInteractiveParallelKeepsBrainIDScope proves every store query issued by
// both overlapped arms stays scoped to the client's BrainID: parallelism never
// widens the authorization boundary (#285 reuse relies on this).
func TestInteractiveParallelKeepsBrainIDScope(t *testing.T) {
	hsParTestEnvs(t)
	const brain = "tenant-par-acl"
	c, dr := newHSParTestClient(t, brain, 2*time.Millisecond, []string{"docK"})

	_, _, err := c.retrieveInteractive(
		context.Background(), "atlas launch report", RetrieveOptions{}, map[string]any{},
		8, 32, time.Now(), hsParProd(), "",
	)
	if err != nil {
		t.Fatal(err)
	}
	queries, brains := dr.snapshot()
	if len(queries) == 0 {
		t.Fatal("expected store queries from hydrate/structure arms")
	}
	// Robustness (Blocker 5): both arms must issue real store work, and every
	// query must carry the tenant brain_id as its first bind argument.
	hasHydrateByID, hasPath2 := false, false
	for _, q := range queries {
		if strings.Contains(q, "chunk_id IN (") {
			hasHydrateByID = true
		}
		if strings.Contains(q, "path2_relationships") {
			hasPath2 = true
		}
	}
	if !hasHydrateByID {
		t.Fatalf("hydrate arm issued no hydrate-by-id query: %v", queries)
	}
	if !hasPath2 {
		t.Fatalf("structure arm issued no path2 query: %v", queries)
	}
	if len(queries) < 2 {
		t.Fatalf("expected multiple store queries, got %d: %v", len(queries), queries)
	}
	if len(brains) != len(queries) {
		t.Fatalf("brain arg count %d != query count %d", len(brains), len(queries))
	}
	for i, b := range brains {
		if b != brain {
			t.Fatalf("query %d escaped BrainID scope: got %q want %q (%q)", i, b, brain, queries[i])
		}
	}
}

// withOfflineEntityCatalog installs a scoped offline entity catalog on disk for
// brain/gen, swaps in a fresh file cache, and returns a restore func. A question
// that matches a catalog name produces entity_catalog_offline hits that fuse
// into the interactive pool as empty-text stubs (Blocker 3 regression setup).
func withOfflineEntityCatalog(t *testing.T, brain, gen string, names map[string]string, nameToDSIDs map[string][]string) func() {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "entity-catalog.gob")
	t.Setenv("OUROBOROS_ERB_ENTITY_GOB", path)
	cat := &OfflineEntityCatalog{
		BrainID:     brain,
		Generation:  gen,
		Names:       names,
		NameToDSIDs: nameToDSIDs,
	}
	if err := WriteOfflineEntityCatalog(path, cat); err != nil {
		t.Fatal(err)
	}
	oldCache := offlineEntityCache
	offlineEntityCache = &offlineEntityFileCache{}
	return func() { offlineEntityCache = oldCache }
}

// TestInteractiveParallelOfflineEntityStubSeedSafety is the #276 review
// regression for Blocker 3. With offline entity stubs in the pool, pre-hydrate
// top-6 seeds can carry entity DSIDs that post-hydrate (serial) seeds displace
// with ordinary docs. The default path must fall back to serial so seeds match
// legacy exactly; the full passage contract must equal the env-forced serial
// run, and the fallback must be reported honestly in diagnostics.
func TestInteractiveParallelOfflineEntityStubSeedSafety(t *testing.T) {
	hsParTestEnvs(t)
	const brain = "tenant-par-entity"
	restore := withOfflineEntityCatalog(t, brain, "gen-live",
		map[string]string{"atlas": "Atlas Entity"},
		map[string][]string{"atlas": {"doc-atlas-entity"}})
	defer restore()

	run := func(forceSerial bool) ([]string, map[string]any) {
		if forceSerial {
			t.Setenv("OUROBOROS_ERB_SERIAL_HYDRATE_STRUCTURE", "1")
		} else {
			t.Setenv("OUROBOROS_ERB_SERIAL_HYDRATE_STRUCTURE", "0")
		}
		c, dr := newHSParTestClient(t, brain, 5*time.Millisecond, []string{"docK"})
		c.hot.Generation = "gen-live"
		_ = dr
		window, diag, err := c.retrieveInteractive(
			context.Background(), "atlas launch report", RetrieveOptions{}, map[string]any{},
			8, 32, time.Now(), hsParProd(), "",
		)
		if err != nil {
			t.Fatal(err)
		}
		return windowPassageFullKeys(window), diag
	}
	defKeys, defDiag := run(false)
	serKeys, _ := run(true)
	if len(defKeys) == 0 {
		t.Fatal("empty window")
	}
	// The fallback to serial must be honest: no claimed parallelism, and the
	// reason must name the seed-safety guard.
	if defDiag["hydrate_structure_parallel"] != false {
		t.Fatalf("entity-stub pool must not claim parallel: %v", defDiag["hydrate_structure_parallel"])
	}
	if got := defDiag["hydrate_structure_serial_reason"]; got != "offline_entity_stub_seed_safety" {
		t.Fatalf("serial reason wrong: got %v", got)
	}
	// Entity stubs must actually be present+hydrated, proving the case fired.
	if n, _ := defDiag["entity_stub_hydrate_n"].(int); n < 1 {
		t.Fatalf("entity stub did not enter the pool: entity_stub_hydrate_n=%v", defDiag["entity_stub_hydrate_n"])
	}
	// Seed safety: default (fallback serial) == env-forced serial on the full
	// passage contract.
	if len(defKeys) != len(serKeys) {
		t.Fatalf("fallback vs serial size differ: default=%d serial=%d", len(defKeys), len(serKeys))
	}
	for i := range defKeys {
		if defKeys[i] != serKeys[i] {
			t.Fatalf("entity-stub seed diverged at %d:\ndefault=%v\nserial=%v", i, defKeys, serKeys)
		}
	}
}

// TestInteractiveParallelOfflineEntityStubStableSeeds overlaps the remaining
// sibling hydrate with structure after the bounded entity-stub seed phase proves
// path2 seed IDs did not change. The result must remain byte-equivalent to the
// forced serial path.
func TestInteractiveParallelOfflineEntityStubStableSeeds(t *testing.T) {
	hsParTestEnvs(t)
	const brain = "tenant-par-entity-stable"
	restore := withOfflineEntityCatalog(t, brain, "gen-live",
		map[string]string{"atlas": "Atlas Entity"},
		map[string][]string{"atlas": {"doc-atlas-entity"}})
	defer restore()

	run := func(forceSerial bool) ([]string, map[string]any) {
		if forceSerial {
			t.Setenv("OUROBOROS_ERB_SERIAL_HYDRATE_STRUCTURE", "1")
		} else {
			t.Setenv("OUROBOROS_ERB_SERIAL_HYDRATE_STRUCTURE", "0")
		}
		c, dr := newHSParTestClient(t, brain, 0, []string{"docK"})
		dr.HydrateDelay = 20 * time.Millisecond
		dr.StructureDelay = 30 * time.Millisecond
		hot := NewHotLex(brain)
		for i := 0; i < 7; i++ {
			hot.AddChunk("docA#c"+strconv.Itoa(i), "docA",
				"atlas launch report primary overview record "+strconv.Itoa(i), "uri://docA")
		}
		hot.AddChunk("docB#c1", "docB", "atlas program appendix", "uri://docB")
		hot.Generation = "gen-live"
		hot.Finalize()
		c.hot = hot

		window, diag, err := c.retrieveInteractive(
			context.Background(), "atlas launch report", RetrieveOptions{}, map[string]any{},
			8, 32, time.Now(), hsParProd(), "",
		)
		if err != nil {
			t.Fatal(err)
		}
		return windowPassageFullKeys(window), diag
	}

	parallelKeys, parallelDiag := run(false)
	serialKeys, _ := run(true)
	if parallelDiag["hydrate_structure_parallel"] != true {
		t.Fatalf("stable entity seeds did not overlap hydrate and structure: %v", parallelDiag)
	}
	if parallelDiag["entity_stub_seed_ids_unchanged"] != true {
		t.Fatalf("stable seed comparison not reported: %v", parallelDiag)
	}
	if _, ok := parallelDiag["hydrate_structure_serial_reason"]; ok {
		t.Fatalf("stable entity seeds unnecessarily fell back to serial: %v", parallelDiag)
	}
	hydArm, _ := parallelDiag["hydrate_arm_ms"].(int64)
	structArm, _ := parallelDiag["structure_arm_ms"].(int64)
	wall, _ := parallelDiag["hydrate_structure_wall_ms"].(int64)
	if hydArm <= 0 || structArm <= 0 || wall >= hydArm+structArm {
		t.Fatalf("stable entity tail did not overlap: hydrate=%dms structure=%dms wall=%dms", hydArm, structArm, wall)
	}
	if len(parallelKeys) != len(serialKeys) {
		t.Fatalf("parallel vs serial size differ: parallel=%d serial=%d", len(parallelKeys), len(serialKeys))
	}
	for i := range parallelKeys {
		if parallelKeys[i] != serialKeys[i] {
			t.Fatalf("stable entity seed output diverged at %d:\nparallel=%v\nserial=%v", i, parallelKeys, serialKeys)
		}
	}
}

// TestInteractiveParallelProjectishHop2Ordering exercises the project second
// hop (ticket↔wiki↔PR) under the overlapped structure arm and proves the final
// evidence ordering is deterministic across repeated runs with jittered store
// latency (Blocker 5). hop2 must run and stay ordered inside the arm.
func TestInteractiveParallelProjectishHop2Ordering(t *testing.T) {
	hsParTestEnvs(t)
	const brain = "tenant-par-hop2"
	var first []string
	for i := 0; i < 8; i++ {
		c, _ := newHSParTestClient(t, brain, time.Duration(2+i%4)*time.Millisecond,
			[]string{"docK", "docL", "docM"})
		window, diag, err := c.retrieveInteractive(
			context.Background(), "atlas launch report",
			RetrieveOptions{QuestionType: "project_related"}, map[string]any{},
			8, 32, time.Now(), hsParProd(), "",
		)
		if err != nil {
			t.Fatal(err)
		}
		if diag["hydrate_structure_parallel"] != true {
			t.Fatalf("projectish run not parallel: %v", diag["hydrate_structure_parallel"])
		}
		if diag["structure_project_hop2"] != true {
			t.Fatalf("hop2 did not run on projectish question: %v", diag)
		}
		keys := windowPassageFullKeys(window)
		if len(keys) == 0 {
			t.Fatal("empty window")
		}
		if first == nil {
			first = keys
			continue
		}
		if len(keys) != len(first) {
			t.Fatalf("run %d window size %d != first %d", i, len(keys), len(first))
		}
		for j := range first {
			if keys[j] != first[j] {
				t.Fatalf("run %d hop2 ordering diverged at %d:\nfirst=%v\nnow=%v", i, j, first, keys)
			}
		}
	}
}

// TestInteractiveParallelFlagFalseWithoutDB proves the section is honest about
// real overlap: with no durable store there is no hydrate arm to overlap, so
// structure runs synchronously and hydrate_structure_parallel must be false
// (Blocker 2). Behavior is otherwise unchanged (window still produced).
func TestInteractiveParallelFlagFalseWithoutDB(t *testing.T) {
	hsParTestEnvs(t)
	c, _ := newHSParTestClient(t, "tenant-par-nodb", time.Millisecond, nil)
	c.db = nil
	window, diag, err := c.retrieveInteractive(
		context.Background(), "atlas launch report", RetrieveOptions{}, map[string]any{},
		8, 32, time.Now(), hsParProd(), "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(window) == 0 {
		t.Fatal("expected a window even without a durable store")
	}
	if diag["hydrate_structure_parallel"] != false {
		t.Fatalf("no-store path must not claim parallel: %v", diag["hydrate_structure_parallel"])
	}
	if _, ok := diag["hydrate_structure_serial_reason"]; ok {
		t.Fatalf("no-store synchronous path must not report a serial reason: %v", diag["hydrate_structure_serial_reason"])
	}
}
