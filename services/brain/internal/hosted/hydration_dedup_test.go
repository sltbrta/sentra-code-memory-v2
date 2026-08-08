package hosted

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClassifyHydrationReuse(t *testing.T) {
	pool := []Passage{
		{DocumentID: "doc-a", ChunkID: "c1", Text: "hydrated"},
		{DocumentID: "doc-a", ChunkID: "c1"}, // covered by the duplicate above
		{DocumentID: "doc-a", ChunkID: "c2", Text: "   "},
		{DocumentID: "turn:s:1", Text: "chat", Channel: "turn_grep"},
		{},
	}
	got := classifyHydrationReuse(pool)
	if got.Reused != 1 || got.Skipped != 2 {
		t.Fatalf("classification=%+v", got)
	}
	if len(got.FetchIDs) != 1 || got.FetchIDs[0] != "c2" {
		t.Fatalf("fetch IDs=%v want [c2]", got.FetchIDs)
	}
}

func TestClassifyHydrationReuseDeduplicatesMissingIDs(t *testing.T) {
	got := classifyHydrationReuse([]Passage{
		{DocumentID: "doc-a", ChunkID: "c1"},
		{DocumentID: "doc-a", ChunkID: "c1"},
		{DocumentID: "doc-b", ChunkID: "c1"},
	})
	if len(got.FetchIDs) != 1 || got.FetchIDs[0] != "c1" {
		t.Fatalf("fetch IDs=%v want [c1]", got.FetchIDs)
	}
}

func TestCountAppliedHydrationHits(t *testing.T) {
	got := countAppliedHydrationHits(
		[]string{"c1", "c2", "c3"},
		[]Hit{{ChunkID: "c1", Text: "body"}, {ChunkID: "c1", Text: "duplicate"}, {ChunkID: "c2"}, {ChunkID: "other", Text: "body"}},
	)
	if got != 1 {
		t.Fatalf("applied=%d want 1", got)
	}
}

func TestUpgradePassagesFromHitsPreservesCitationMetadata(t *testing.T) {
	pool := []Passage{{
		DocumentID: "doc-a", ChunkID: "c1", Text: "short", Score: 0.5,
		SourceURI: "uri://original", Channel: "lexical",
	}}
	out := upgradePassagesFromHits(pool, []Hit{{
		ChunkID: "c1", DSID: "other-doc", Text: "longer Neon text", SourceURI: "uri://neon",
	}})
	if len(out) != 1 || out[0].Text != "longer Neon text" {
		t.Fatalf("upgrade=%+v", out)
	}
	if out[0].DocumentID != "doc-a" || out[0].ChunkID != "c1" || out[0].SourceURI != "uri://original" || out[0].Channel != "lexical" || out[0].Score != 0.5 {
		t.Fatalf("citation metadata changed: %+v", out[0])
	}
}

func TestUpgradePassagesFromHitsHandlesWhitespace(t *testing.T) {
	out := upgradePassagesFromHits(
		[]Passage{
			{DocumentID: "doc-a", ChunkID: "c1", Text: "     "},
			{DocumentID: "doc-a", ChunkID: "c2"},
		},
		[]Hit{
			{ChunkID: "c1", Text: "full"},
			{ChunkID: "c2", Text: "   "},
		},
	)
	if out[0].Text != "full" {
		t.Fatalf("whitespace text was not hydrated: %q", out[0].Text)
	}
	if out[1].Text != "" {
		t.Fatalf("whitespace hit was applied: %q", out[1].Text)
	}
}

func TestHydrateTopDocsReusingPreservesWindowAndUpgrade(t *testing.T) {
	dr := &hydrationDedupDriver{rows: [][]driver.Value{
		{"c1", "richer Neon text with 2026-08-05", "uri://a"},
		{"c2", "new top-two text", "uri://a"},
		{"c3", "must not append page two", "uri://a"},
	}}
	db := openHydrationDedupTestDB(t, dr)
	pool := []Passage{
		{DocumentID: "doc-a", ChunkID: "c1", Text: "short", SourceURI: "uri://seed", Channel: "hydrate"},
		{DocumentID: "doc-b", ChunkID: "b1", Text: "other", Channel: "lexical"},
	}

	out, counts := hydrateTopDocsReusing(context.Background(), db, Config{BrainID: "tenant-1", MaxPassageChars: 2000}, pool, 1, 2, false)
	if strings.Contains(dr.lastQuery, "NOT IN") {
		t.Fatalf("sibling query changed the original window: %q", dr.lastQuery)
	}
	if len(dr.lastArgs) != 3 || dr.lastArgs[2].Value != int64(2) {
		t.Fatalf("query args=%#v", dr.lastArgs)
	}
	if len(out) != 3 || out[0].ChunkID != "c1" || out[1].ChunkID != "b1" || out[2].ChunkID != "c2" {
		t.Fatalf("order/window changed: %+v", out)
	}
	if out[0].Text != "richer Neon text with 2026-08-05" || out[0].SourceURI != "uri://seed" {
		t.Fatalf("existing chunk was not safely upgraded: %+v", out[0])
	}
	if counts.Reused != 1 || counts.Fetched != 1 {
		t.Fatalf("sibling counts=%+v want reused=1 fetched=1", counts)
	}
	for _, p := range out {
		if p.ChunkID == "c3" {
			t.Fatalf("page-two chunk appended: %+v", out)
		}
	}
}

func TestHydrateTopDocsReusingSkipsCoveredWindow(t *testing.T) {
	dr := &hydrationDedupDriver{rows: [][]driver.Value{
		{"c3", "page-two text", "uri://a"},
	}}
	db := openHydrationDedupTestDB(t, dr)
	pool := []Passage{
		{DocumentID: "doc-a", ChunkID: "c1", Text: "top one", Channel: "lexical+hydrate"},
		{DocumentID: "doc-a", ChunkID: "c2", Text: "top two", Channel: "hydrate"},
	}

	out, counts := hydrateTopDocsReusing(context.Background(), db, Config{BrainID: "tenant-1"}, pool, 1, 2, false)
	if dr.queries.Load() != 0 {
		t.Fatalf("covered top-two window issued %d sibling queries", dr.queries.Load())
	}
	if len(out) != len(pool) {
		t.Fatalf("covered top-two window grew from %d to %d: %+v", len(pool), len(out), out)
	}
	if counts.Reused != 2 || counts.Fetched != 0 {
		t.Fatalf("sibling counts=%+v want reused=2 fetched=0", counts)
	}
}

func TestHydrateTopDocsReusingDateWindowStillQueries(t *testing.T) {
	dr := &hydrationDedupDriver{rows: [][]driver.Value{
		{"c1", "top one with 2026-08-05", "uri://a"},
		{"c2", "top two with 2026-08-06", "uri://a"},
	}}
	db := openHydrationDedupTestDB(t, dr)
	pool := []Passage{
		{DocumentID: "doc-a", ChunkID: "c1", Text: "top one", Channel: "hydrate"},
		{DocumentID: "doc-a", ChunkID: "c2", Text: "top two", Channel: "hydrate"},
	}

	out, counts := hydrateTopDocsReusing(context.Background(), db, Config{BrainID: "tenant-1", MaxPassageChars: 2000}, pool, 1, 2, true)
	if dr.queries.Load() != 1 {
		t.Fatalf("date window queries=%d want 1", dr.queries.Load())
	}
	if counts.Reused != 2 || counts.Fetched != 0 || len(out) != 2 {
		t.Fatalf("date hydrate out=%+v counts=%+v", out, counts)
	}
	if out[0].Text != "top one with 2026-08-05" {
		t.Fatalf("date-rich text did not upgrade: %+v", out[0])
	}
}

func TestHydrationQueriesKeepBrainIDScope(t *testing.T) {
	dr := &hydrationDedupDriver{rows: [][]driver.Value{{"c1", "body", "uri://a"}}}
	db := openHydrationDedupTestDB(t, dr)
	_, _ = siblingChunks(context.Background(), db, Config{BrainID: "tenant-acl-9"}, "doc-a", 2)
	if dr.lastQueryBrainID != "tenant-acl-9" {
		t.Fatalf("brain scope=%q", dr.lastQueryBrainID)
	}
	if !strings.Contains(dr.lastQuery, "brain_id = $1") {
		t.Fatalf("missing BrainID predicate: %q", dr.lastQuery)
	}
}

func TestHydrateByChunkIDsKeepsBrainIDScopeAndOrder(t *testing.T) {
	dr := &hydrationDedupDriver{rows: [][]driver.Value{
		{"c2", "doc-a", "body two", "uri://a"},
		{"c1", "doc-a", "body one", "uri://a"},
	}}
	db := openHydrationDedupTestDB(t, dr)
	hits, err := hydrateByChunkIDs(context.Background(), db, Config{BrainID: "tenant-1"}, []string{"c1", "c2"})
	if err != nil {
		t.Fatal(err)
	}
	if dr.lastQueryBrainID != "tenant-1" || len(hits) != 2 || hits[0].ChunkID != "c1" || hits[1].ChunkID != "c2" {
		t.Fatalf("scope/order: brain=%q hits=%+v", dr.lastQueryBrainID, hits)
	}
}

func TestStampHydrationReuseDiagUsesExplicitPreHydrateCounts(t *testing.T) {
	diag := map[string]any{}
	stampHydrationReuseDiag(diag, HydrationReuseDiag{
		Fetched: 1, SiblingReused: 2, SiblingFetched: 1,
	})
	if diag["answer_hydrate_reused_n"] != 0 || diag["answer_hydrate_skipped_n"] != 0 {
		t.Fatalf("post-hydrate pool was reclassified: %v", diag)
	}
	if diag["answer_hydrate_fetched_n"] != 1 || diag["answer_hydrate_sibling_reused_n"] != 2 || diag["answer_hydrate_sibling_fetched_n"] != 1 {
		t.Fatalf("diagnostics=%v", diag)
	}
	if _, ok := diag["answer_hydrate_skip_reason"]; ok {
		t.Fatalf("empty skip reason stamped: %v", diag)
	}
}

func TestStampHydrationReuseDiagTruthfulSkipReason(t *testing.T) {
	diag := map[string]any{}
	stampHydrationReuseDiag(diag, HydrationReuseDiag{Reused: 2, SkipReason: "whole_doc_not_requested"})
	if diag["answer_hydrate_reused_n"] != 2 || diag["answer_hydrate_skip_reason"] != "whole_doc_not_requested" {
		t.Fatalf("diagnostics=%v", diag)
	}
}

type hydrationDedupDriver struct {
	queries          atomic.Int32
	lastQueryBrainID string
	lastQuery        string
	lastArgs         []driver.NamedValue
	rows             [][]driver.Value
}

type hydrationDedupConn struct{ d *hydrationDedupDriver }

func (c *hydrationDedupConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *hydrationDedupConn) Close() error                        { return nil }
func (c *hydrationDedupConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (c *hydrationDedupConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.d.queries.Add(1)
	c.d.lastQuery = query
	c.d.lastArgs = append([]driver.NamedValue(nil), args...)
	if len(args) > 0 {
		c.d.lastQueryBrainID, _ = args[0].Value.(string)
	}
	columns := []string{"chunk_id", "text_content", "source_uri"}
	if strings.Contains(query, "chunk_id IN (") {
		columns = []string{"chunk_id", "dsid", "text_content", "source_uri"}
	}
	return &hydrationDedupRows{columns: columns, values: c.d.rows}, nil
}

type hydrationDedupRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *hydrationDedupRows) Columns() []string { return r.columns }
func (r *hydrationDedupRows) Close() error      { return nil }
func (r *hydrationDedupRows) Next(dest []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

var hydrationDedupDriverID atomic.Int64

func openHydrationDedupTestDB(t *testing.T, d *hydrationDedupDriver) *sql.DB {
	t.Helper()
	name := "hosted_hydration_dedup_test_" + strconv.FormatInt(hydrationDedupDriverID.Add(1), 10)
	sql.Register(name, hydrationDedupDriverFunc(func() (driver.Conn, error) {
		return &hydrationDedupConn{d: d}, nil
	}))
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type hydrationDedupDriverFunc func() (driver.Conn, error)

func (f hydrationDedupDriverFunc) Open(string) (driver.Conn, error) { return f() }
