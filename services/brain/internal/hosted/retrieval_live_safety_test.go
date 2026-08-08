package hosted

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type retrievalSafetyDriver struct {
	queries    atomic.Int32
	ftsQueries atomic.Int32
	query      func(context.Context, string) (driver.Rows, error)
}

type retrievalSafetyConn struct{ d *retrievalSafetyDriver }

func (c *retrievalSafetyConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *retrievalSafetyConn) Close() error                        { return nil }
func (c *retrievalSafetyConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (c *retrievalSafetyConn) QueryContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.d.queries.Add(1)
	if strings.Contains(query, "ts_rank") {
		c.d.ftsQueries.Add(1)
	}
	return c.d.query(ctx, query)
}

type retrievalSafetyRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *retrievalSafetyRows) Columns() []string { return r.columns }
func (r *retrievalSafetyRows) Close() error      { return nil }
func (r *retrievalSafetyRows) Next(dest []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

var retrievalSafetyDriverID atomic.Int64

func openRetrievalSafetyDB(t *testing.T, d *retrievalSafetyDriver) *sql.DB {
	t.Helper()
	name := "hosted_retrieval_safety_" + strconv.FormatInt(retrievalSafetyDriverID.Add(1), 10)
	sql.Register(name, driver.Driver(driverFunc(func() (driver.Conn, error) {
		return &retrievalSafetyConn{d: d}, nil
	})))
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type driverFunc func() (driver.Conn, error)

func (f driverFunc) Open(string) (driver.Conn, error) { return f() }

func emptyRetrievalRows() driver.Rows {
	return &retrievalSafetyRows{columns: []string{"chunk_id", "dsid", "text_content", "source_uri", "rank"}}
}

func lexicalRetrievalRows() driver.Rows {
	return &retrievalSafetyRows{
		columns: []string{"chunk_id", "dsid", "text_content", "source_uri", "rank"},
		values: [][]driver.Value{
			{"relevant#0", "relevant", "Atlas recovery objective is fifteen minutes.", "fixture://relevant", 9.0},
			{"noise#0", "noise", "Cafeteria menu and picnic schedule.", "fixture://noise", 1.0},
		},
	}
}

func setRetrievalSafetyEnv(t *testing.T) {
	t.Helper()
	t.Setenv("OUROBOROS_ERB_SKIP_DENSE", "1")
	t.Setenv("OUROBOROS_ERB_SKIP_FTS", "0")
	t.Setenv("OUROBOROS_ERB_FORCE_FTS", "0")
	t.Setenv("OUROBOROS_ERB_FORCE_RESIDUAL", "0")
	t.Setenv("OUROBOROS_ERB_QUALITY_RESIDUAL", "0")
	t.Setenv("OUROBOROS_ERB_OFFICIAL", "0")
	t.Setenv("OUROBOROS_ERB_OFFICIAL_JUDGE", "0")
	t.Setenv("OUROBOROS_ERB_BENCHMAX", "0")
	t.Setenv("OUROBOROS_ERB_BENCH_MAX", "0")
	t.Setenv("OUROBOROS_ERB_QUALITY", "0")
	t.Setenv("OUROBOROS_ERB_PROD", "1")
}

func TestOfficialAndForcedFTSRemainBounded(t *testing.T) {
	setRetrievalSafetyEnv(t)
	t.Setenv("OUROBOROS_ERB_OFFICIAL", "1")
	t.Setenv("OUROBOROS_ERB_BENCHMAX", "1")
	t.Setenv("OUROBOROS_ERB_FORCE_FTS", "1")
	prod := prodProfileFromEnv()
	if !prod.Benchmax {
		t.Fatal("test requires conflicting official+benchmax posture")
	}
	if got := interactiveFTSBudget(prod, true); got != maxLiveFTSBudget {
		t.Fatalf("forced official interactive FTS budget=%v want %v", got, maxLiveFTSBudget)
	}
	if got := boundedFTSBudget(prod, prod.LexTimeout); got != maxLiveFTSBudget {
		t.Fatalf("official residual FTS budget=%v want %v", got, maxLiveFTSBudget)
	}
}

func TestProductFTSOverrideCannotExceedLiveBound(t *testing.T) {
	setRetrievalSafetyEnv(t)
	if got := boundedFTSBudget(ProdProfile{Enabled: true}, 30*time.Second); got != maxLiveFTSBudget {
		t.Fatalf("product FTS override budget=%v want %v", got, maxLiveFTSBudget)
	}
	bench := ProdProfile{Benchmax: true, Quality: true}
	if got := boundedFTSBudget(bench, 0); got != 0 {
		t.Fatalf("non-official BENCHMAX zero budget=%v want caller-deadline-only", got)
	}
	if got := boundedFTSBudget(bench, 30*time.Second); got != 30*time.Second {
		t.Fatalf("non-official BENCHMAX explicit budget=%v want 30s", got)
	}
}

func TestProductNeonDefaultStoreFTSHasSharedDeadline(t *testing.T) {
	setRetrievalSafetyEnv(t)
	var sawDeadline atomic.Bool
	d := &retrievalSafetyDriver{query: func(ctx context.Context, _ string) (driver.Rows, error) {
		deadline, ok := ctx.Deadline()
		if ok && time.Until(deadline) > 0 && time.Until(deadline) <= maxLiveFTSBudget {
			sawDeadline.Store(true)
		}
		return emptyRetrievalRows(), nil
	}}
	c := &Client{
		db:           openRetrievalSafetyDB(t, d),
		cfg:          Config{BrainID: "brain", TopK: 4, PoolK: 12, LexicalLimit: 4},
		productOwned: true,
		qcache:       newQueryCache(time.Minute),
	}
	_, diag, err := c.RetrieveOpts(context.Background(), "Atlas recovery objective", RetrieveOptions{TopK: 4})
	if err != nil {
		t.Fatal(err)
	}
	budget, ok := diag["store_lexical_budget_ms"].(int64)
	if !sawDeadline.Load() || !ok || budget <= 0 || budget > maxLiveFTSBudget.Milliseconds() {
		t.Fatalf("product Neon store entered unbounded FTS: saw_deadline=%v diag=%#v", sawDeadline.Load(), diag)
	}
}

type lexicalVariantDeadlineStore struct {
	*MemoryChunkStore
	mu        sync.Mutex
	deadlines []time.Time
	callTimes []time.Time
}

func (s *lexicalVariantDeadlineStore) LexicalSearch(ctx context.Context, _, _ string, _ int) ([]Hit, error) {
	s.mu.Lock()
	call := len(s.deadlines)
	deadline, _ := ctx.Deadline()
	s.deadlines = append(s.deadlines, deadline)
	s.callTimes = append(s.callTimes, time.Now())
	s.mu.Unlock()
	if call == 0 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []Hit{{
		ChunkID: "later-variant#0",
		DSID:    "later-variant",
		Text:    "Atlas recovery objective is fifteen minutes.",
		Score:   1,
		Channel: "lexical",
	}}, nil
}

func TestStoreLexicalVariantsHaveChildDeadlinesWithinSharedWall(t *testing.T) {
	setRetrievalSafetyEnv(t)
	t.Setenv("OUROBOROS_ERB_LEX_TIMEOUT_MS", "120")
	t.Setenv("OUROBOROS_ERB_LEX_VARIANT_CAP", "2")
	t.Setenv("OUROBOROS_ERB_LLM_MULTIQUERY", "0")
	store := &lexicalVariantDeadlineStore{MemoryChunkStore: NewMemoryChunkStore()}
	c := &Client{
		cfg:          Config{BrainID: "brain", TopK: 4, PoolK: 12, LexicalLimit: 4},
		store:        store,
		productOwned: true,
		qcache:       newQueryCache(time.Minute),
	}
	started := time.Now()
	passages, diag, err := c.retrieveMemory(
		context.Background(),
		"Atlas recovery objective for project Zephyr",
		RetrieveOptions{TopK: 4},
		map[string]any{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("sequential lexical variants exceeded shared wall: %v", elapsed)
	}
	store.mu.Lock()
	deadlines := append([]time.Time(nil), store.deadlines...)
	callTimes := append([]time.Time(nil), store.callTimes...)
	store.mu.Unlock()
	if len(deadlines) != 2 {
		t.Fatalf("lexical calls=%d want 2", len(deadlines))
	}
	if deadlines[0].IsZero() || deadlines[1].IsZero() || !deadlines[1].After(deadlines[0]) {
		t.Fatalf("variant deadlines=%v want later variant to retain a later child deadline", deadlines)
	}
	sharedDeadlineCeiling := callTimes[0].Add(150 * time.Millisecond)
	if deadlines[0].After(sharedDeadlineCeiling) || deadlines[1].After(sharedDeadlineCeiling) {
		t.Fatalf("variant deadlines=%v escaped shared 120ms wall", deadlines)
	}
	if len(passages) == 0 || passages[0].DocumentID != "later-variant" {
		t.Fatalf("later variant did not contribute results: %#v", passages)
	}
	if diag["store_lexical_queries"] != 2 ||
		diag["store_lexical_succeeded_queries"] != 1 ||
		diag["store_lexical_failed_queries"] != 1 ||
		diag["store_lexical_timeout_queries"] != 1 ||
		diag["store_lexical_canceled_queries"] != 0 {
		t.Fatalf("variant diagnostics=%#v", diag)
	}
	variantBudget, ok := diag["store_lexical_variant_budget_ms"].(int64)
	if !ok || variantBudget <= 0 || variantBudget >= 120 {
		t.Fatalf("per-variant budget=%v diag=%#v", diag["store_lexical_variant_budget_ms"], diag)
	}
}

func TestMissingHotLexDiagnosticsDistinguishTimeoutAndEmpty(t *testing.T) {
	setRetrievalSafetyEnv(t)
	t.Run("timeout", func(t *testing.T) {
		d := &retrievalSafetyDriver{query: func(ctx context.Context, _ string) (driver.Rows, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		c := &Client{db: openRetrievalSafetyDB(t, d), cfg: Config{BrainID: "brain", LexicalLimit: 4}}
		start := time.Now()
		_, diag, err := c.retrieveInteractive(context.Background(), "Atlas recovery objective", RetrieveOptions{}, map[string]any{},
			4, 12, start, ProdProfile{Enabled: true, LexTimeout: 25 * time.Millisecond, LexTerms: 8, LexLimit: 4}, "")
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("bounded fallback took %v", elapsed)
		}
		if diag["hot_lex_state"] != "missing" || diag["neon_fts_fallback_reason"] != "missing_hot_lex" ||
			diag["neon_fts_fallback_outcome"] != "timeout" {
			t.Fatalf("timeout diagnostics not distinct: %#v", diag)
		}
	})

	t.Run("empty", func(t *testing.T) {
		d := &retrievalSafetyDriver{query: func(context.Context, string) (driver.Rows, error) {
			return emptyRetrievalRows(), nil
		}}
		c := &Client{db: openRetrievalSafetyDB(t, d), cfg: Config{BrainID: "brain", LexicalLimit: 4}}
		_, diag, err := c.retrieveInteractive(context.Background(), "Atlas recovery objective", RetrieveOptions{}, map[string]any{},
			4, 12, time.Now(), ProdProfile{Enabled: true, LexTimeout: 50 * time.Millisecond, LexTerms: 8, LexLimit: 4}, "")
		if err != nil {
			t.Fatal(err)
		}
		if diag["neon_fts_fallback_outcome"] != "empty" || diag["soft_empty"] != true {
			t.Fatalf("empty diagnostics not distinct: %#v", diag)
		}
	})
}

func TestCanceledRequestStopsForcedFanout(t *testing.T) {
	setRetrievalSafetyEnv(t)
	t.Setenv("OUROBOROS_ERB_FORCE_FTS", "1")
	d := &retrievalSafetyDriver{query: func(context.Context, string) (driver.Rows, error) {
		return lexicalRetrievalRows(), nil
	}}
	c := &Client{
		db:     openRetrievalSafetyDB(t, d),
		cfg:    Config{BrainID: "brain", TopK: 4, PoolK: 12, LexicalLimit: 4},
		qcache: newQueryCache(time.Minute),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, diag, err := c.RetrieveOpts(ctx, "Atlas recovery objective", RetrieveOptions{TopK: 4})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v want context canceled", err)
	}
	if d.queries.Load() != 0 || diag["retrieval_status"] != "canceled" || diag["retrieval_context_done_before_fanout"] != true {
		t.Fatalf("canceled request entered fanout: queries=%d diag=%#v", d.queries.Load(), diag)
	}
}

func TestMissingHotLexDoesNotAmplifyFTSAcrossRecovery(t *testing.T) {
	setRetrievalSafetyEnv(t)
	d := &retrievalSafetyDriver{query: func(_ context.Context, query string) (driver.Rows, error) {
		if strings.Contains(query, "ts_rank") {
			return lexicalRetrievalRows(), nil
		}
		return emptyRetrievalRows(), nil
	}}
	c := &Client{db: openRetrievalSafetyDB(t, d), cfg: Config{BrainID: "brain", LexicalLimit: 4}}
	_, diag, err := c.retrieveInteractive(
		context.Background(), "Atlas recovery objective for all projects", RetrieveOptions{QuestionType: "multi_doc"},
		map[string]any{}, 4, 12, time.Now(),
		ProdProfile{Enabled: true, LexTimeout: 50 * time.Millisecond, LexTerms: 8, LexLimit: 4}, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.ftsQueries.Load(); got != 1 {
		t.Fatalf("missing HotLex amplified Neon FTS to %d queries; diag=%#v", got, diag)
	}
	if diag["neon_fts_fallback_queries"] != 1 || diag["neon_fts_fallback_query_cap"] != 1 {
		t.Fatalf("single-fallback cap not diagnosed: %#v", diag)
	}
}

func TestFTSFallbackOutcomeReportsPartialFailures(t *testing.T) {
	tests := []struct {
		name                                                 string
		attempted                                            bool
		planned, hits, succeeded, failed, timedOut, canceled int
		want                                                 string
	}{
		{name: "partial hits", attempted: true, planned: 2, hits: 3, succeeded: 1, failed: 1, timedOut: 1, want: "partial_failure"},
		{name: "partial empty", attempted: true, planned: 2, succeeded: 1, failed: 1, want: "partial_failure"},
		{name: "all timeout", attempted: true, planned: 2, failed: 2, timedOut: 2, want: "timeout"},
		{name: "all canceled", attempted: true, planned: 1, failed: 1, canceled: 1, want: "canceled"},
		{name: "empty", attempted: true, planned: 1, succeeded: 1, want: "empty"},
		{name: "skipped", want: "skipped"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ftsFallbackOutcome(tt.attempted, tt.planned, tt.hits, tt.succeeded, tt.failed, tt.timedOut, tt.canceled); got != tt.want {
				t.Fatalf("outcome=%q want %q", got, tt.want)
			}
		})
	}
}

func TestBenchmaxCallerDeadlineOnlyFTSIsDiagnosed(t *testing.T) {
	setRetrievalSafetyEnv(t)
	t.Setenv("OUROBOROS_ERB_BENCHMAX", "1")
	t.Setenv("OUROBOROS_ERB_FORCE_FTS", "1")
	hot := NewHotLex("brain")
	hot.AddChunk("chunk", "doc", "Atlas recovery objective", "")
	hot.Finalize()
	d := &retrievalSafetyDriver{query: func(context.Context, string) (driver.Rows, error) {
		return emptyRetrievalRows(), nil
	}}
	c := &Client{hot: hot, db: openRetrievalSafetyDB(t, d), cfg: Config{BrainID: "brain", LexicalLimit: 4}}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_, diag, err := c.retrieveInteractive(ctx, "Atlas recovery objective", RetrieveOptions{}, map[string]any{},
		4, 12, time.Now(), prodProfileFromEnv(), "")
	if err != nil {
		t.Fatal(err)
	}
	if diag["neon_fts_fallback_caller_deadline_only"] != true || diag["neon_fts_fallback_budget_ms"] != int64(0) {
		t.Fatalf("caller-deadline-only FTS not explicit: %#v", diag)
	}
}

func TestStructureFanoutSharesCallerWallBudget(t *testing.T) {
	d := &retrievalSafetyDriver{query: func(ctx context.Context, _ string) (driver.Rows, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	db := openRetrievalSafetyDB(t, d)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	docs, diag := path2StructureExpand(ctx, db, "brain", "Atlas recovery objective", []string{"seed"}, 8)
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("parallel structure arms exceeded caller wall: %v diag=%#v", elapsed, diag)
	}
	if len(docs) != 0 || d.queries.Load() == 0 {
		t.Fatalf("expected bounded attempted fanout: docs=%v queries=%d diag=%#v", docs, d.queries.Load(), diag)
	}
	if diag["path2_entities_error"] == nil || diag["path2_relationships_error"] == nil {
		t.Fatalf("deadline did not reach both structure arms: %#v", diag)
	}
}

func TestForcedResidualRouteIsExplicitAndBounded(t *testing.T) {
	setRetrievalSafetyEnv(t)
	d := &retrievalSafetyDriver{query: func(_ context.Context, query string) (driver.Rows, error) {
		if strings.Contains(query, "ts_rank") {
			return lexicalRetrievalRows(), nil
		}
		return emptyRetrievalRows(), nil
	}}
	c := &Client{
		db:     openRetrievalSafetyDB(t, d),
		cfg:    Config{BrainID: "brain", TopK: 4, PoolK: 12, LexicalLimit: 4},
		qcache: newQueryCache(time.Minute),
	}
	_, initialDiag, err := c.RetrieveOpts(context.Background(), "Atlas recovery objective", RetrieveOptions{TopK: 4})
	if err != nil {
		t.Fatal(err)
	}
	if initialDiag["retrieve_class"] != "interactive" {
		t.Fatalf("precondition route=%#v", initialDiag)
	}
	t.Setenv("OUROBOROS_ERB_FORCE_RESIDUAL", "1")
	_, diag, err := c.RetrieveOpts(context.Background(), "Atlas recovery objective", RetrieveOptions{TopK: 4})
	if err != nil {
		t.Fatal(err)
	}
	if diag["retrieve_class"] != "residual_opt_in" || diag["retrieval_route_reason"] != "force_residual" ||
		diag["neon_fts_mode"] != "residual_multi_arm" {
		t.Fatalf("forced residual route is ambiguous: %#v", diag)
	}
	if budget, _ := diag["neon_fts_budget_ms"].(int64); budget <= 0 || budget > maxLiveFTSBudget.Milliseconds() {
		t.Fatalf("forced residual FTS budget=%v diag=%#v", diag["neon_fts_budget_ms"], diag)
	}
}

func TestHotLexAndBoundedFallbackQualityComparisonHasNoGoldSteering(t *testing.T) {
	setRetrievalSafetyEnv(t)
	query := "Atlas recovery objective"
	opts := RetrieveOptions{TopK: 4}
	if len(opts.GoldDocIDs) != 0 {
		t.Fatal("quality comparison must not pass gold IDs into retrieval")
	}

	hot := NewHotLex("brain")
	hot.AddChunk("relevant#0", "relevant", "Atlas recovery objective is fifteen minutes.", "fixture://relevant")
	hot.AddChunk("noise#0", "noise", "Cafeteria menu and picnic schedule.", "fixture://noise")
	hot.Finalize()
	hotClient := &Client{hot: hot, cfg: Config{BrainID: "brain", LexicalLimit: 4}}
	hotPassages, hotDiag, err := hotClient.retrieveInteractive(context.Background(), query, opts, map[string]any{},
		4, 12, time.Now(), ProdProfile{Enabled: true, LexTimeout: 50 * time.Millisecond, LexTerms: 8, LexLimit: 4}, "")
	if err != nil {
		t.Fatal(err)
	}

	d := &retrievalSafetyDriver{query: func(_ context.Context, query string) (driver.Rows, error) {
		if strings.Contains(query, "ts_rank") {
			return lexicalRetrievalRows(), nil
		}
		return emptyRetrievalRows(), nil
	}}
	fallbackClient := &Client{db: openRetrievalSafetyDB(t, d), cfg: Config{BrainID: "brain", LexicalLimit: 4}}
	fallbackPassages, fallbackDiag, err := fallbackClient.retrieveInteractive(context.Background(), query, opts, map[string]any{},
		4, 12, time.Now(), ProdProfile{Enabled: true, LexTimeout: 50 * time.Millisecond, LexTerms: 8, LexLimit: 4}, "")
	if err != nil {
		t.Fatal(err)
	}

	for name, passages := range map[string][]Passage{"hotlex": hotPassages, "fallback": fallbackPassages} {
		if len(passages) == 0 || passages[0].DocumentID != "relevant" {
			t.Fatalf("%s quality regression: passages=%#v", name, passages)
		}
	}
	for name, diag := range map[string]map[string]any{"hotlex": hotDiag, "fallback": fallbackDiag} {
		raw, err := json.Marshal(diag)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(string(raw)), "gold") {
			t.Fatalf("%s comparison diagnostics contain gold-derived fields: %s", name, raw)
		}
	}
	if fallbackDiag["neon_fts_fallback_outcome"] != "hits" || hotDiag["hot_lex_state"] != "available" {
		t.Fatalf("comparison routes not explicit: hot=%#v fallback=%#v", hotDiag, fallbackDiag)
	}
}
