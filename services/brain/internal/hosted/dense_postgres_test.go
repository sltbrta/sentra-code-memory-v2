package hosted

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/projections"
)

type postgresDenseTestDriver struct {
	fallbackErr error
	vectorRows  [][]driver.Value
}

type postgresDenseTestConn struct {
	driver *postgresDenseTestDriver
}

func (c *postgresDenseTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, driver.ErrSkip
}

func (c *postgresDenseTestConn) Close() error { return nil }

func (c *postgresDenseTestConn) Begin() (driver.Tx, error) { return nil, driver.ErrSkip }

func (c *postgresDenseTestConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "residual_dense_vectors_v") {
		return &postgresDenseTestRows{
			columns: []string{"document_id", "dsid", "chunk_id", "source_uri", "score"},
			values:  c.driver.vectorRows,
		}, nil
	}
	if strings.Contains(query, "residual_dense_vectors") {
		return nil, c.driver.fallbackErr
	}
	return nil, fmt.Errorf("unexpected postgres dense query: %s", query)
}

type postgresDenseTestRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *postgresDenseTestRows) Columns() []string { return r.columns }

func (r *postgresDenseTestRows) Close() error { return nil }

func (r *postgresDenseTestRows) Next(dest []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

var postgresDenseTestDriverID atomic.Int64

func openPostgresDenseTestDB(t *testing.T, d *postgresDenseTestDriver) *sql.DB {
	t.Helper()
	name := "hosted_postgres_dense_test_" + strconv.FormatInt(postgresDenseTestDriverID.Add(1), 10)
	sql.Register(name, postgresDenseTestDriverFunc(func() (driver.Conn, error) {
		return &postgresDenseTestConn{driver: d}, nil
	}))
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type postgresDenseTestDriverFunc func() (driver.Conn, error)

func (f postgresDenseTestDriverFunc) Open(string) (driver.Conn, error) { return f() }

type postgresDenseTableRow struct {
	dsid, chunkID, sourceURI string
	dim                      int64
	embedding                []byte
	score                    float64
}

type postgresDenseTableDriver struct {
	vectorRows   map[string]postgresDenseTableRow
	fallbackRows map[string]postgresDenseTableRow
	calls        []string

	fallbackDeleteErr error
	vectorUpsertErr   error
	vectorDeleteErr   error
	fallbackUpsertErr error
}

type postgresDenseTableConn struct {
	driver            *postgresDenseTableDriver
	txSnapshot        *postgresDenseTableSnapshot
	savepointSnapshot *postgresDenseTableSnapshot
}

type postgresDenseTableSnapshot struct {
	vectorRows   map[string]postgresDenseTableRow
	fallbackRows map[string]postgresDenseTableRow
}

func (c *postgresDenseTableConn) Prepare(string) (driver.Stmt, error) {
	return nil, driver.ErrSkip
}

func (c *postgresDenseTableConn) Close() error { return nil }

func (c *postgresDenseTableConn) Begin() (driver.Tx, error) {
	if c.txSnapshot != nil {
		return nil, fmt.Errorf("postgres dense test transaction already active")
	}
	c.driver.calls = append(c.driver.calls, "begin")
	c.txSnapshot = c.snapshot()
	return &postgresDenseTableTx{conn: c}, nil
}

func (c *postgresDenseTableConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	query = strings.Join(strings.Fields(query), " ")
	switch query {
	case "SAVEPOINT postgres_dense_vector_write":
		c.driver.calls = append(c.driver.calls, "savepoint_vector")
		c.savepointSnapshot = c.snapshot()
		return driver.RowsAffected(0), nil
	case "ROLLBACK TO SAVEPOINT postgres_dense_vector_write":
		c.driver.calls = append(c.driver.calls, "rollback_to_vector")
		if c.savepointSnapshot == nil {
			return nil, fmt.Errorf("postgres dense test savepoint is not active")
		}
		c.restore(c.savepointSnapshot)
		return driver.RowsAffected(0), nil
	}
	id := args[1].Value.(string)
	switch {
	case strings.HasPrefix(query, "DELETE FROM residual_dense_vectors_v"):
		c.driver.calls = append(c.driver.calls, "delete_vector")
		if c.driver.vectorDeleteErr != nil {
			return nil, c.driver.vectorDeleteErr
		}
		delete(c.driver.vectorRows, id)
	case strings.HasPrefix(query, "DELETE FROM residual_dense_vectors"):
		c.driver.calls = append(c.driver.calls, "delete_fallback")
		if c.driver.fallbackDeleteErr != nil {
			return nil, c.driver.fallbackDeleteErr
		}
		delete(c.driver.fallbackRows, id)
	case strings.HasPrefix(query, "INSERT INTO residual_dense_vectors_v"):
		c.driver.calls = append(c.driver.calls, "upsert_vector")
		if c.driver.vectorUpsertErr != nil {
			return nil, c.driver.vectorUpsertErr
		}
		c.driver.vectorRows[id] = postgresDenseTableRow{
			dim:       args[2].Value.(int64),
			chunkID:   args[4].Value.(string),
			dsid:      args[5].Value.(string),
			sourceURI: args[6].Value.(string),
			score:     1,
		}
	case strings.HasPrefix(query, "INSERT INTO residual_dense_vectors"):
		c.driver.calls = append(c.driver.calls, "upsert_fallback")
		if c.driver.fallbackUpsertErr != nil {
			return nil, c.driver.fallbackUpsertErr
		}
		c.driver.fallbackRows[id] = postgresDenseTableRow{
			dim:       args[2].Value.(int64),
			embedding: append([]byte(nil), args[3].Value.([]byte)...),
			chunkID:   args[4].Value.(string),
			dsid:      args[5].Value.(string),
			sourceURI: args[6].Value.(string),
		}
	default:
		return nil, fmt.Errorf("unexpected postgres dense exec: %s", query)
	}
	return driver.RowsAffected(1), nil
}

type postgresDenseTableTx struct {
	conn *postgresDenseTableConn
}

func (tx *postgresDenseTableTx) Commit() error {
	tx.conn.driver.calls = append(tx.conn.driver.calls, "commit")
	tx.conn.txSnapshot = nil
	tx.conn.savepointSnapshot = nil
	return nil
}

func (tx *postgresDenseTableTx) Rollback() error {
	tx.conn.driver.calls = append(tx.conn.driver.calls, "rollback")
	if tx.conn.txSnapshot == nil {
		return sql.ErrTxDone
	}
	tx.conn.restore(tx.conn.txSnapshot)
	tx.conn.txSnapshot = nil
	tx.conn.savepointSnapshot = nil
	return nil
}

func (c *postgresDenseTableConn) snapshot() *postgresDenseTableSnapshot {
	return &postgresDenseTableSnapshot{
		vectorRows:   clonePostgresDenseTableRows(c.driver.vectorRows),
		fallbackRows: clonePostgresDenseTableRows(c.driver.fallbackRows),
	}
}

func (c *postgresDenseTableConn) restore(snapshot *postgresDenseTableSnapshot) {
	c.driver.vectorRows = clonePostgresDenseTableRows(snapshot.vectorRows)
	c.driver.fallbackRows = clonePostgresDenseTableRows(snapshot.fallbackRows)
}

func clonePostgresDenseTableRows(rows map[string]postgresDenseTableRow) map[string]postgresDenseTableRow {
	cloned := make(map[string]postgresDenseTableRow, len(rows))
	for id, row := range rows {
		row.embedding = append([]byte(nil), row.embedding...)
		cloned[id] = row
	}
	return cloned
}

func (c *postgresDenseTableConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	query = strings.Join(strings.Fields(query), " ")
	if strings.Contains(query, "residual_dense_vectors_v") {
		ids := sortedPostgresDenseTableIDs(c.driver.vectorRows)
		values := make([][]driver.Value, 0, len(ids))
		for _, id := range ids {
			row := c.driver.vectorRows[id]
			values = append(values, []driver.Value{id, row.dsid, row.chunkID, row.sourceURI, row.score})
		}
		return &postgresDenseTestRows{
			columns: []string{"document_id", "dsid", "chunk_id", "source_uri", "score"},
			values:  values,
		}, nil
	}
	if strings.Contains(query, "residual_dense_vectors") {
		ids := sortedPostgresDenseTableIDs(c.driver.fallbackRows)
		values := make([][]driver.Value, 0, len(ids))
		for _, id := range ids {
			row := c.driver.fallbackRows[id]
			values = append(values, []driver.Value{id, row.dsid, row.chunkID, row.sourceURI, row.dim, row.embedding})
		}
		return &postgresDenseTestRows{
			columns: []string{"document_id", "dsid", "chunk_id", "source_uri", "dim", "embedding"},
			values:  values,
		}, nil
	}
	return nil, fmt.Errorf("unexpected postgres dense query: %s", query)
}

func sortedPostgresDenseTableIDs(rows map[string]postgresDenseTableRow) []string {
	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func openPostgresDenseTableDB(t *testing.T, d *postgresDenseTableDriver) *sql.DB {
	t.Helper()
	if d.vectorRows == nil {
		d.vectorRows = make(map[string]postgresDenseTableRow)
	}
	if d.fallbackRows == nil {
		d.fallbackRows = make(map[string]postgresDenseTableRow)
	}
	name := "hosted_postgres_dense_table_test_" + strconv.FormatInt(postgresDenseTestDriverID.Add(1), 10)
	sql.Register(name, postgresDenseTestDriverFunc(func() (driver.Conn, error) {
		return &postgresDenseTableConn{driver: d}, nil
	}))
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestPostgresDenseFailedVectorUpsertReplacesStaleVector(t *testing.T) {
	driver := &postgresDenseTableDriver{
		vectorRows: map[string]postgresDenseTableRow{
			"point": {dsid: "stale-vector", chunkID: "stale#0", score: 0.99},
		},
		fallbackRows: map[string]postgresDenseTableRow{
			"point": {dsid: "older-fallback", chunkID: "older#0", dim: 2, embedding: packF32([]float32{0, 1})},
		},
		vectorUpsertErr: fmt.Errorf("pgvector cast failed"),
	}
	d := &postgresDense{db: openPostgresDenseTableDB(t, driver), gen: "brain", pgvector: true}

	err := d.Upsert([]DensePoint{{
		ID:     "point",
		Vector: []float32{1, 0},
		Payload: map[string]any{
			"document_id": "latest-document",
			"chunk_id":    "latest#0",
			"source_uri":  "postgres://latest",
		},
	}})
	if err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	if want := []string{"begin", "delete_fallback", "savepoint_vector", "upsert_vector", "rollback_to_vector", "delete_vector", "upsert_fallback", "commit"}; !reflect.DeepEqual(driver.calls, want) {
		t.Fatalf("Upsert() calls = %v, want %v", driver.calls, want)
	}
	if _, ok := driver.vectorRows["point"]; ok {
		t.Fatal("failed vector upsert left stale pgvector row")
	}
	if got := driver.fallbackRows["point"].dsid; got != "latest-document" {
		t.Fatalf("fallback dsid = %q, want latest-document", got)
	}

	got, err := d.Search(denseQuery{Vector: []float32{1, 0}}, 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	wantHits := []Hit{{
		ChunkID: "latest#0", DSID: "latest-document", SourceURI: "postgres://latest",
		Score: 1, Channel: "dense_postgres",
	}}
	if !reflect.DeepEqual(got.Hits, wantHits) {
		t.Fatalf("Search() hits = %+v, want %+v", got.Hits, wantHits)
	}
	if got.Diagnostics.Route != "pgvector_bytea_merge" {
		t.Fatalf("Search() route = %q, want pgvector_bytea_merge", got.Diagnostics.Route)
	}
}

func TestPostgresDenseFailedVectorUpsertCannotLeakStaleVectorOnFallbackOverflow(t *testing.T) {
	fallbackRows := make(map[string]postgresDenseTableRow, projections.ExactDenseFallbackLimit)
	for i := 0; i < projections.ExactDenseFallbackLimit; i++ {
		id := fmt.Sprintf("fallback-%03d", i)
		fallbackRows[id] = postgresDenseTableRow{
			dsid: id, chunkID: id + "#0", dim: 2, embedding: packF32([]float32{1, 0}),
		}
	}
	driver := &postgresDenseTableDriver{
		vectorRows: map[string]postgresDenseTableRow{
			"point": {dsid: "stale-vector", chunkID: "stale#0", score: 0.99},
			"live":  {dsid: "live-document", chunkID: "live#0", score: 0.80},
		},
		fallbackRows:    fallbackRows,
		vectorUpsertErr: fmt.Errorf("pgvector cast failed"),
	}
	d := &postgresDense{db: openPostgresDenseTableDB(t, driver), gen: "brain", pgvector: true}

	if err := d.Upsert([]DensePoint{{ID: "point", Vector: []float32{1, 0}}}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	if _, ok := driver.vectorRows["point"]; ok {
		t.Fatal("failed vector upsert left stale pgvector row")
	}
	if got := len(driver.fallbackRows); got != projections.ExactDenseFallbackLimit+1 {
		t.Fatalf("fallback rows = %d, want %d", got, projections.ExactDenseFallbackLimit+1)
	}

	got, err := d.Search(denseQuery{Vector: []float32{1, 0}}, 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	wantHits := []Hit{{ChunkID: "live#0", DSID: "live-document", Score: 0.80, Channel: "dense_pgvector"}}
	if !reflect.DeepEqual(got.Hits, wantHits) {
		t.Fatalf("Search() hits = %+v, want %+v", got.Hits, wantHits)
	}
	if got.Diagnostics.Route != "pgvector_bytea_overflow" {
		t.Fatalf("Search() route = %q, want pgvector_bytea_overflow", got.Diagnostics.Route)
	}
}

func TestPostgresDenseUpsertCleanupFailuresStopTransition(t *testing.T) {
	t.Run("fallback delete fails before vector write", func(t *testing.T) {
		driver := &postgresDenseTableDriver{
			vectorRows: map[string]postgresDenseTableRow{
				"point": {dsid: "existing-vector"},
			},
			fallbackRows: map[string]postgresDenseTableRow{
				"point": {dsid: "existing-fallback"},
			},
			fallbackDeleteErr: fmt.Errorf("delete fallback failed"),
		}
		d := &postgresDense{db: openPostgresDenseTableDB(t, driver), gen: "brain", pgvector: true}
		err := d.Upsert([]DensePoint{{ID: "point", Vector: []float32{1, 0}}})
		if err == nil || !strings.Contains(err.Error(), "clear postgres dense fallback before vector upsert") {
			t.Fatalf("Upsert() error = %v, want fallback cleanup failure", err)
		}
		if want := []string{"begin", "delete_fallback", "rollback"}; !reflect.DeepEqual(driver.calls, want) {
			t.Fatalf("Upsert() calls = %v, want %v", driver.calls, want)
		}
		if driver.vectorRows["point"].dsid != "existing-vector" || driver.fallbackRows["point"].dsid != "existing-fallback" {
			t.Fatal("failed pre-delete mutated existing state")
		}
	})

	t.Run("vector delete fails before fallback write", func(t *testing.T) {
		existingVector := postgresDenseTableRow{dsid: "existing-vector", chunkID: "existing-vector#0", score: 0.75}
		existingFallback := postgresDenseTableRow{
			dsid: "existing-fallback", chunkID: "existing-fallback#0", sourceURI: "postgres://existing",
			dim: 2, embedding: packF32([]float32{0, 1}),
		}
		driver := &postgresDenseTableDriver{
			vectorRows: map[string]postgresDenseTableRow{
				"point": existingVector,
			},
			fallbackRows: map[string]postgresDenseTableRow{
				"point": existingFallback,
			},
			vectorUpsertErr: fmt.Errorf("pgvector cast failed"),
			vectorDeleteErr: fmt.Errorf("delete vector failed"),
		}
		d := &postgresDense{db: openPostgresDenseTableDB(t, driver), gen: "brain", pgvector: true}
		err := d.Upsert([]DensePoint{{ID: "point", Vector: []float32{1, 0}}})
		if err == nil || !strings.Contains(err.Error(), "clear stale postgres dense vector after upsert failure") {
			t.Fatalf("Upsert() error = %v, want vector cleanup failure", err)
		}
		want := []string{"begin", "delete_fallback", "savepoint_vector", "upsert_vector", "rollback_to_vector", "delete_vector", "rollback"}
		if !reflect.DeepEqual(driver.calls, want) {
			t.Fatalf("Upsert() calls = %v, want %v", driver.calls, want)
		}
		if got := driver.fallbackRows["point"]; !reflect.DeepEqual(got, existingFallback) {
			t.Fatalf("fallback after rollback = %+v, want %+v", got, existingFallback)
		}
		if got := driver.vectorRows["point"]; !reflect.DeepEqual(got, existingVector) {
			t.Fatalf("vector after rollback = %+v, want %+v", got, existingVector)
		}
	})

	t.Run("fallback write failure restores both old representations", func(t *testing.T) {
		existingVector := postgresDenseTableRow{dsid: "existing-vector", chunkID: "existing-vector#0", score: 0.75}
		existingFallback := postgresDenseTableRow{
			dsid: "existing-fallback", chunkID: "existing-fallback#0", sourceURI: "postgres://existing",
			dim: 2, embedding: packF32([]float32{0, 1}),
		}
		driver := &postgresDenseTableDriver{
			vectorRows: map[string]postgresDenseTableRow{
				"point": existingVector,
			},
			fallbackRows: map[string]postgresDenseTableRow{
				"point": existingFallback,
			},
			vectorUpsertErr:   fmt.Errorf("pgvector cast failed"),
			fallbackUpsertErr: fmt.Errorf("write fallback failed"),
		}
		d := &postgresDense{db: openPostgresDenseTableDB(t, driver), gen: "brain", pgvector: true}
		err := d.Upsert([]DensePoint{{ID: "point", Vector: []float32{1, 0}}})
		if err == nil || !strings.Contains(err.Error(), "write fallback failed") {
			t.Fatalf("Upsert() error = %v, want fallback write failure", err)
		}
		want := []string{"begin", "delete_fallback", "savepoint_vector", "upsert_vector", "rollback_to_vector", "delete_vector", "upsert_fallback", "rollback"}
		if !reflect.DeepEqual(driver.calls, want) {
			t.Fatalf("Upsert() calls = %v, want %v", driver.calls, want)
		}
		if got := driver.fallbackRows["point"]; !reflect.DeepEqual(got, existingFallback) {
			t.Fatalf("fallback after rollback = %+v, want %+v", got, existingFallback)
		}
		if got := driver.vectorRows["point"]; !reflect.DeepEqual(got, existingVector) {
			t.Fatalf("vector after rollback = %+v, want %+v", got, existingVector)
		}
	})
}

func TestPostgresDenseSuccessfulVectorUpsertClearsFallbackFirst(t *testing.T) {
	driver := &postgresDenseTableDriver{
		fallbackRows: map[string]postgresDenseTableRow{
			"point": {dsid: "old-fallback"},
		},
	}
	d := &postgresDense{db: openPostgresDenseTableDB(t, driver), gen: "brain", pgvector: true}

	if err := d.Upsert([]DensePoint{{ID: "point", Vector: []float32{1, 0}}}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	want := []string{"begin", "delete_fallback", "savepoint_vector", "upsert_vector", "commit"}
	if !reflect.DeepEqual(driver.calls, want) {
		t.Fatalf("Upsert() calls = %v, want %v", driver.calls, want)
	}
	if _, ok := driver.fallbackRows["point"]; ok {
		t.Fatal("successful vector upsert left old fallback row")
	}
	if _, ok := driver.vectorRows["point"]; !ok {
		t.Fatal("successful vector upsert did not persist vector row")
	}
}

func TestPostgresDenseSearchServesPgvectorOnFallbackOverflow(t *testing.T) {
	tests := []struct {
		name          string
		topK          int
		fallbackLimit int
		fallbackErr   func(int) error
		vectorRows    [][]driver.Value
		wantHits      []Hit
	}{
		{
			name:          "sorts tied provider hits by vector id and truncates",
			topK:          2,
			fallbackLimit: 17,
			fallbackErr: func(limit int) error {
				return &projections.ExactFallbackLimitError{Scope: "brain-overflow", Limit: limit}
			},
			vectorRows: [][]driver.Value{
				{"vector-z", "doc-z", "chunk-z", "postgres://z", 0.80},
				{"vector-b", "doc-b", "chunk-b", "postgres://b", 0.90},
				{"vector-a", "doc-a", "chunk-a", "postgres://a", 0.90},
			},
			wantHits: []Hit{
				{ChunkID: "chunk-a", DSID: "doc-a", SourceURI: "postgres://a", Score: 0.90, Channel: "dense_pgvector"},
				{ChunkID: "chunk-b", DSID: "doc-b", SourceURI: "postgres://b", Score: 0.90, Channel: "dense_pgvector"},
			},
		},
		{
			name:          "preserves wrapped overflow limit while serving provider",
			topK:          1,
			fallbackLimit: 29,
			fallbackErr: func(limit int) error {
				return fmt.Errorf("bounded BYTEA probe: %w", &projections.ExactFallbackLimitError{
					Scope: "brain-overflow", Limit: limit,
				})
			},
			vectorRows: [][]driver.Value{
				{"vector-low", "doc-low", "chunk-low", "postgres://low", 0.70},
				{"vector-high", "doc-high", "chunk-high", "postgres://high", 0.95},
			},
			wantHits: []Hit{
				{ChunkID: "chunk-high", DSID: "doc-high", SourceURI: "postgres://high", Score: 0.95, Channel: "dense_pgvector"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &postgresDense{
				db: openPostgresDenseTestDB(t, &postgresDenseTestDriver{
					fallbackErr: tt.fallbackErr(tt.fallbackLimit),
					vectorRows:  tt.vectorRows,
				}),
				gen:      "brain-overflow",
				pgvector: true,
			}

			got, err := d.Search(denseQuery{Vector: []float32{1, 0}}, tt.topK)
			if err != nil {
				t.Fatalf("Search() error = %v", err)
			}
			if !reflect.DeepEqual(got.Hits, tt.wantHits) {
				t.Fatalf("Search() hits = %+v, want %+v", got.Hits, tt.wantHits)
			}
			if got.Diagnostics.Route != "pgvector_bytea_overflow" {
				t.Fatalf("Search() route = %q, want pgvector_bytea_overflow", got.Diagnostics.Route)
			}
			if got.Diagnostics.IndexState != "provider_managed" {
				t.Fatalf("Search() index state = %q, want provider_managed", got.Diagnostics.IndexState)
			}
			if got.Diagnostics.ExactFallbackLimit != tt.fallbackLimit {
				t.Fatalf("Search() exact fallback limit = %d, want %d", got.Diagnostics.ExactFallbackLimit, tt.fallbackLimit)
			}
		})
	}
}

func TestMergePostgresDenseHits(t *testing.T) {
	hit := func(vectorID, dsid string, score float64, channel string) postgresDenseHit {
		return postgresDenseHit{
			vectorID: vectorID,
			hit: Hit{
				ChunkID: vectorID + "#0",
				DSID:    dsid,
				Score:   score,
				Channel: channel,
			},
		}
	}

	tests := []struct {
		name             string
		vector           []postgresDenseHit
		fallback         []postgresDenseHit
		topK             int
		fallbackOverflow bool
		want             []Hit
	}{
		{
			name: "fallback wins duplicate vector id",
			vector: []postgresDenseHit{
				hit("duplicate", "stale-provider", 0.99, "dense_pgvector"),
				hit("provider-only", "provider-only", 0.80, "dense_pgvector"),
			},
			fallback: []postgresDenseHit{
				hit("duplicate", "latest-fallback", 0.70, "dense_postgres"),
			},
			topK: 3,
			want: []Hit{
				hit("provider-only", "provider-only", 0.80, "dense_pgvector").hit,
				hit("duplicate", "latest-fallback", 0.70, "dense_postgres").hit,
			},
		},
		{
			name: "orders ties by vector id before topK truncation",
			vector: []postgresDenseHit{
				hit("vector-c", "doc-c", 0.90, "dense_pgvector"),
				hit("vector-a", "doc-a", 0.90, "dense_pgvector"),
				hit("vector-b", "doc-b", 0.90, "dense_pgvector"),
			},
			topK: 2,
			want: []Hit{
				hit("vector-a", "doc-a", 0.90, "dense_pgvector").hit,
				hit("vector-b", "doc-b", 0.90, "dense_pgvector").hit,
			},
		},
		{
			name: "overflow excludes incomplete fallback set",
			vector: []postgresDenseHit{
				hit("provider", "provider", 0.80, "dense_pgvector"),
			},
			fallback: []postgresDenseHit{
				hit("partial-fallback", "partial-fallback", 1.0, "dense_postgres"),
			},
			topK:             2,
			fallbackOverflow: true,
			want: []Hit{
				hit("provider", "provider", 0.80, "dense_pgvector").hit,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergePostgresDenseHits(tt.vector, tt.fallback, tt.topK, tt.fallbackOverflow)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("mergePostgresDenseHits() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
