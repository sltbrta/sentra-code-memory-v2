package projections

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/dense"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ontology"
)

var errInjectedDenseSchemaIteration = errors.New("injected dense schema iteration failure")

type denseSchemaErrorConnector struct{}

func (denseSchemaErrorConnector) Connect(context.Context) (driver.Conn, error) {
	return denseSchemaErrorConn{}, nil
}

func (denseSchemaErrorConnector) Driver() driver.Driver { return denseSchemaErrorDriver{} }

type denseSchemaErrorDriver struct{}

func (denseSchemaErrorDriver) Open(string) (driver.Conn, error) { return denseSchemaErrorConn{}, nil }

type denseSchemaErrorConn struct{}

func (denseSchemaErrorConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (denseSchemaErrorConn) Close() error              { return nil }
func (denseSchemaErrorConn) Begin() (driver.Tx, error) { return nil, errors.New("unexpected begin") }
func (denseSchemaErrorConn) QueryContext(
	context.Context, string, []driver.NamedValue,
) (driver.Rows, error) {
	return denseSchemaErrorRows{}, nil
}

type denseSchemaErrorRows struct{}

func (denseSchemaErrorRows) Columns() []string {
	return []string{"cid", "name", "type", "notnull", "dflt_value", "pk"}
}
func (denseSchemaErrorRows) Close() error              { return nil }
func (denseSchemaErrorRows) Next([]driver.Value) error { return errInjectedDenseSchemaIteration }

func openTemp(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "projections.sqlite3")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenCreatesSchema(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	var n int
	if err := db.SQL.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN
		 ('ontology_edges','dense_vectors','projection_meta')`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("tables = %d want 3", n)
	}
}

func TestOpenMigratesLegacyDenseModelIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite3")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE dense_vectors (
		generation_id TEXT NOT NULL,
		document_id TEXT NOT NULL,
		dim INTEGER NOT NULL,
		embedding BLOB NOT NULL,
		PRIMARY KEY (generation_id, document_id)
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO dense_vectors VALUES ('g','d',2,?)`, packFloat32LE([]float32{1, 0})); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var model string
	if err := db.SQL.QueryRow(`SELECT model_id FROM dense_vectors WHERE document_id='d'`).Scan(&model); err != nil {
		t.Fatal(err)
	}
	if model != "legacy" {
		t.Fatalf("migrated model=%q want legacy", model)
	}
}

func TestEnsureDenseModelColumnReportsIterationError(t *testing.T) {
	db := sql.OpenDB(denseSchemaErrorConnector{})
	defer db.Close()

	err := ensureDenseModelColumn(db)
	if !errors.Is(err, errInjectedDenseSchemaIteration) ||
		!strings.Contains(err.Error(), "iterate dense schema") {
		t.Fatalf("ensureDenseModelColumn error = %v, want iteration failure", err)
	}
}

func TestGraphRepositoryPutGetReplace(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	repo := &GraphRepository{DB: db.SQL}

	g := ontology.Graph{
		GenerationID: "gen-1",
		Edges: []ontology.Edge{
			{DocumentSrc: "a", DocumentDst: "b", Rel: ontology.RelCoProject, Weight: 1.5, Provenance: "deterministic"},
			{DocumentSrc: "b", DocumentDst: "c", Rel: ontology.RelCites, Weight: 1, Provenance: "gardener_llm"},
		},
	}
	if err := repo.PutGraph(g); err != nil {
		t.Fatalf("PutGraph: %v", err)
	}
	got, ok, err := repo.GetGraph("gen-1")
	if err != nil || !ok {
		t.Fatalf("GetGraph: ok=%v err=%v", ok, err)
	}
	if len(got.Edges) != 2 {
		t.Fatalf("edges = %d want 2", len(got.Edges))
	}
	if got.Edges[0].DocumentSrc != "a" || got.Edges[0].Weight != 1.5 {
		t.Fatalf("edge0 = %+v", got.Edges[0])
	}

	// Replace: only the new edge set remains.
	if err := repo.PutGraph(ontology.Graph{
		GenerationID: "gen-1",
		Edges: []ontology.Edge{
			{DocumentSrc: "x", DocumentDst: "y", Rel: ontology.RelMentions, Weight: 2},
		},
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, ok, err = repo.GetGraph("gen-1")
	if err != nil || !ok || len(got.Edges) != 1 {
		t.Fatalf("after replace: edges=%d ok=%v err=%v", len(got.Edges), ok, err)
	}
	if got.Edges[0].DocumentSrc != "x" {
		t.Fatalf("got %+v", got.Edges[0])
	}

	// Empty graph still present.
	if err := repo.PutGraph(ontology.Graph{GenerationID: "gen-empty"}); err != nil {
		t.Fatalf("empty put: %v", err)
	}
	empty, ok, err := repo.GetGraph("gen-empty")
	if err != nil || !ok {
		t.Fatalf("empty get: ok=%v err=%v", ok, err)
	}
	if len(empty.Edges) != 0 {
		t.Fatalf("empty edges = %d", len(empty.Edges))
	}

	_, ok, err = repo.GetGraph("missing")
	if err != nil || ok {
		t.Fatalf("missing: ok=%v err=%v", ok, err)
	}
}

func TestRepoHopperExpand(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	repo := &GraphRepository{DB: db.SQL}
	if err := repo.PutGraph(ontology.Graph{
		GenerationID: "g",
		Edges: []ontology.Edge{
			{DocumentSrc: "seed", DocumentDst: "nbr", Rel: ontology.RelCoProject, Weight: 1},
			{DocumentSrc: "nbr", DocumentDst: "far", Rel: ontology.RelCites, Weight: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	h := RepoHopper{Repo: repo}
	out := h.Expand("g", []string{"seed"}, 5)
	if len(out) == 0 {
		t.Fatal("expected expansion neighbors")
	}
	found := false
	for _, id := range out {
		if id == "nbr" || id == "far" {
			found = true
		}
		if id == "seed" {
			t.Fatalf("seed leaked into expansion: %v", out)
		}
	}
	if !found {
		t.Fatalf("expansion = %v, want nbr or far", out)
	}
}

func TestSQLDenseStoreUpsertSearch(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	store := &SQLDenseStore{DB: db.SQL}

	if err := store.Upsert("gen", "x", []float32{1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert("gen", "y", []float32{0, 1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert("gen", "z", []float32{0, 0, 1}); err != nil {
		t.Fatal(err)
	}
	// Other generation must not leak into search.
	if err := store.Upsert("other", "noise", []float32{1, 0, 0}); err != nil {
		t.Fatal(err)
	}

	hits, err := store.Search("gen", []float32{0, 1, 0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].DocumentID != "y" {
		t.Fatalf("hits = %+v want y", hits)
	}
	if math.Abs(hits[0].Score-1) > 1e-5 {
		t.Fatalf("score = %v want ~1", hits[0].Score)
	}

	// Upsert replaces.
	if err := store.Upsert("gen", "x", []float32{0, 1, 0}); err != nil {
		t.Fatal(err)
	}
	hits, err = store.Search("gen", []float32{0, 1, 0}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("hits = %+v", hits)
	}
	// x and y both perfect; stable order by DocumentID.
	if hits[0].DocumentID != "x" && hits[0].DocumentID != "y" {
		t.Fatalf("top = %s", hits[0].DocumentID)
	}

	vec, ok, err := store.Get("gen", "y")
	if err != nil || !ok || len(vec) != 3 || vec[1] != 1 {
		t.Fatalf("Get y: vec=%v ok=%v err=%v", vec, ok, err)
	}
}

func TestSQLDenseStoreScopedIdentityAndBoundedExactFallback(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	store := &SQLDenseStore{DB: db.SQL}
	a := dense.IndexIdentity{Scope: "brain-a", Model: "embed:v1", Dimensions: 2}
	b := dense.IndexIdentity{Scope: "brain-b", Model: "embed:v1", Dimensions: 2}
	if err := store.UpsertScoped(a, "a-only", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertScoped(b, "b-only", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	hits, diag, err := store.SearchExactBounded(a, []float32{1, 0}, 4, ExactDenseFallbackLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].DocumentID != "a-only" || diag.DistanceCalculations != 1 {
		t.Fatalf("scoped hits=%+v diag=%+v", hits, diag)
	}
	for i := 0; i < ExactDenseFallbackLimit; i++ {
		if err := store.UpsertScoped(a, fmt.Sprintf("overflow-%04d", i), []float32{0, 1}); err != nil {
			t.Fatal(err)
		}
	}
	_, overflowDiag, err := store.SearchExactBounded(a, []float32{1, 0}, 4, ExactDenseFallbackLimit)
	var limitErr *ExactFallbackLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("error=%v, want ExactFallbackLimitError", err)
	}
	if overflowDiag.CorpusVectors != ExactDenseFallbackLimit+1 || overflowDiag.ExactFallbackLimit != ExactDenseFallbackLimit {
		t.Fatalf("overflow diagnostics=%+v", overflowDiag)
	}
}

func TestSQLDenseStoreUpsertScopedRequiresScope(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	store := &SQLDenseStore{DB: db.SQL}

	err := store.UpsertScoped(dense.IndexIdentity{Model: "embed:v1", Dimensions: 2}, "doc", []float32{1, 0})
	if err == nil || err.Error() != "projections: empty scope" {
		t.Fatalf("UpsertScoped error = %v, want empty scope", err)
	}
}

func TestPackUnpackFloat32LE(t *testing.T) {
	t.Parallel()
	in := []float32{1, -2.5, 0, float32(math.Pi)}
	blob := packFloat32LE(in)
	if len(blob) != 16 {
		t.Fatalf("blob len = %d", len(blob))
	}
	out, err := unpackFloat32LE(blob, 4)
	if err != nil {
		t.Fatal(err)
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("i=%d got %v want %v", i, out[i], in[i])
		}
	}
	if _, err := unpackFloat32LE(blob[:6], 0); err == nil {
		t.Fatal("expected bad length error")
	}
}

func TestOpenEmptyPath(t *testing.T) {
	t.Parallel()
	if _, err := Open(""); err == nil {
		t.Fatal("expected error")
	}
}
