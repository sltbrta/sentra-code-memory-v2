package projections

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// schemaSQL creates projection tables when missing. Projections are rebuildable;
// there is no migration ladder — Open applies CREATE IF NOT EXISTS only.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS ontology_edges (
  generation_id TEXT NOT NULL,
  src TEXT NOT NULL,
  dst TEXT NOT NULL,
  rel TEXT NOT NULL,
  weight REAL NOT NULL DEFAULT 1,
  provenance TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (generation_id, src, dst, rel)
);
CREATE INDEX IF NOT EXISTS ontology_edges_gen ON ontology_edges(generation_id);
CREATE TABLE IF NOT EXISTS dense_vectors (
  generation_id TEXT NOT NULL,
  document_id TEXT NOT NULL,
  model_id TEXT NOT NULL DEFAULT 'legacy',
  dim INTEGER NOT NULL,
  embedding BLOB NOT NULL,
	chunk_id TEXT NOT NULL DEFAULT '',
	dsid TEXT NOT NULL DEFAULT '',
	source_uri TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (generation_id, document_id)
);
CREATE TABLE IF NOT EXISTS projection_meta (
  generation_id TEXT NOT NULL,
  key TEXT NOT NULL,
  value TEXT NOT NULL,
  PRIMARY KEY (generation_id, key)
);
`

// DB wraps a projection SQLite handle opened by Open.
type DB struct {
	SQL *sql.DB
}

// Open opens (or creates) a projection database at path and applies schema.
// Path may be absolute or relative; empty path is rejected.
func Open(path string) (*DB, error) {
	clean := strings.TrimSpace(path)
	if clean == "" {
		return nil, fmt.Errorf("projections: empty database path")
	}
	clean = filepath.Clean(clean)
	if err := os.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
		return nil, fmt.Errorf("projections: create parent dir: %w", err)
	}
	db, err := sql.Open("sqlite", clean)
	if err != nil {
		return nil, fmt.Errorf("projections: open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("projections: configure database: %w", err)
		}
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("projections: apply schema: %w", err)
	}
	// dense_vectors predates model-pinned ANN serving. Projection databases are
	// rebuildable, but adding the column in place preserves the bounded exact
	// fallback for small legacy corpora.
	if err := ensureDenseModelColumn(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	for _, column := range []struct{ name, definition string }{
		{"chunk_id", "TEXT NOT NULL DEFAULT ''"},
		{"dsid", "TEXT NOT NULL DEFAULT ''"},
		{"source_uri", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := ensureDenseColumn(db, column.name, column.definition); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS dense_vectors_identity
		ON dense_vectors(generation_id, model_id, dim, document_id)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("projections: index dense identity: %w", err)
	}
	return &DB{SQL: db}, nil
}

func ensureDenseModelColumn(db *sql.DB) error {
	return ensureDenseColumn(db, "model_id", "TEXT NOT NULL DEFAULT 'legacy'")
}

func ensureDenseColumn(db *sql.DB, column, definition string) error {
	rows, err := db.Query(`PRAGMA table_info(dense_vectors)`)
	if err != nil {
		return fmt.Errorf("projections: inspect dense schema: %w", err)
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return fmt.Errorf("projections: scan dense schema: %w", err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("projections: iterate dense schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("projections: close dense schema rows: %w", err)
	}
	if found {
		return nil
	}
	// column and definition are closed constants supplied by Open, never input.
	if _, err := db.Exec(`ALTER TABLE dense_vectors ADD COLUMN ` + column + ` ` + definition); err != nil {
		return fmt.Errorf("projections: add dense %s: %w", column, err)
	}
	return nil
}

// Close releases the database handle. It is idempotent.
func (d *DB) Close() error {
	if d == nil || d.SQL == nil {
		return nil
	}
	err := d.SQL.Close()
	d.SQL = nil
	return err
}
