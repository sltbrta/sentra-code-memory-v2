package gardener

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const sqliteQueueSchema = `
CREATE TABLE IF NOT EXISTS gardener_jobs (
  job_id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  generation_id TEXT NOT NULL,
  document_id TEXT NOT NULL DEFAULT '',
  payload_json TEXT NOT NULL DEFAULT '{}',
  status TEXT NOT NULL DEFAULT 'pending',
  attempt INTEGER NOT NULL DEFAULT 0,
  lease_owner TEXT NOT NULL DEFAULT '',
  lease_until INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS gardener_jobs_status ON gardener_jobs(status, lease_until);
CREATE TABLE IF NOT EXISTS gardener_receipts (
  job_id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  generation_id TEXT NOT NULL,
  document_id TEXT NOT NULL DEFAULT '',
  ok INTEGER NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  output TEXT NOT NULL DEFAULT '',
  finished_at INTEGER NOT NULL
);
`

// SQLiteQueue is a durable gardener.Queue for product async enrichment.
// Jobs survive process restarts; Claim uses a lease so a crashed worker
// redelivers after lease_until.
type SQLiteQueue struct {
	db   *sql.DB
	path string
	// LeaseTTL is how long a claim holds a job (default 2m).
	LeaseTTL time.Duration
}

// OpenSQLiteQueue opens or creates a durable queue at path.
func OpenSQLiteQueue(path string) (*SQLiteQueue, error) {
	clean := strings.TrimSpace(path)
	if clean == "" {
		return nil, fmt.Errorf("gardener: empty sqlite queue path")
	}
	clean = filepath.Clean(clean)
	if err := os.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
		return nil, fmt.Errorf("gardener: mkdir queue parent: %w", err)
	}
	db, err := sql.Open("sqlite", clean)
	if err != nil {
		return nil, fmt.Errorf("gardener: open sqlite queue: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("gardener: pragma: %w", err)
		}
	}
	if _, err := db.Exec(sqliteQueueSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("gardener: schema: %w", err)
	}
	return &SQLiteQueue{db: db, path: clean, LeaseTTL: 2 * time.Minute}, nil
}

// Path returns the database file path.
func (q *SQLiteQueue) Path() string {
	if q == nil {
		return ""
	}
	return q.path
}

// Close closes the underlying database.
func (q *SQLiteQueue) Close() error {
	if q == nil || q.db == nil {
		return nil
	}
	return q.db.Close()
}

// Enqueue inserts jobs as pending. Duplicate job_id is ignored (idempotent).
func (q *SQLiteQueue) Enqueue(ctx context.Context, jobs ...Job) error {
	if q == nil || q.db == nil {
		return fmt.Errorf("gardener: nil sqlite queue")
	}
	now := time.Now().UnixMilli()
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
INSERT OR IGNORE INTO gardener_jobs
  (job_id, kind, generation_id, document_id, payload_json, status, attempt, created_at)
VALUES (?, ?, ?, ?, ?, 'pending', 0, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, j := range jobs {
		if strings.TrimSpace(j.ID) == "" {
			continue
		}
		payload, _ := json.Marshal(j.Payload)
		if payload == nil {
			payload = []byte("{}")
		}
		if _, err := stmt.ExecContext(ctx, j.ID, string(j.Kind), j.GenerationID, j.DocumentID, string(payload), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Claim leases up to n pending (or expired-lease) jobs for workerID.
func (q *SQLiteQueue) Claim(ctx context.Context, workerID string, n int) ([]Job, error) {
	if q == nil || q.db == nil {
		return nil, fmt.Errorf("gardener: nil sqlite queue")
	}
	if n <= 0 {
		return nil, nil
	}
	ttl := q.LeaseTTL
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	now := time.Now()
	until := now.Add(ttl).UnixMilli()
	nowMS := now.UnixMilli()

	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Reclaim expired leases (lease_until is Unix milliseconds).
	if _, err := tx.ExecContext(ctx, `
UPDATE gardener_jobs SET status='pending', lease_owner='', lease_until=0
WHERE status='leased' AND lease_until > 0 AND lease_until < ?`, nowMS); err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, `
SELECT job_id, kind, generation_id, document_id, payload_json, attempt
FROM gardener_jobs
WHERE status='pending'
ORDER BY created_at ASC
LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	type row struct {
		id, kind, gen, doc, payload string
		attempt                     int
	}
	var picked []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.kind, &r.gen, &r.doc, &r.payload, &r.attempt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		picked = append(picked, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]Job, 0, len(picked))
	for _, r := range picked {
		if _, err := tx.ExecContext(ctx, `
UPDATE gardener_jobs
SET status='leased', lease_owner=?, lease_until=?, attempt=attempt+1
WHERE job_id=? AND status='pending'`, workerID, until, r.id); err != nil {
			return nil, err
		}
		var payload map[string]string
		_ = json.Unmarshal([]byte(r.payload), &payload)
		if payload == nil {
			payload = map[string]string{}
		}
		out = append(out, Job{
			ID:           r.id,
			Kind:         JobKind(r.kind),
			GenerationID: r.gen,
			DocumentID:   r.doc,
			Payload:      payload,
			Attempt:      r.attempt + 1,
			CreatedAt:    now,
		})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// Complete records a receipt and marks the job done (or failed).
func (q *SQLiteQueue) Complete(ctx context.Context, receipt Receipt) error {
	if q == nil || q.db == nil {
		return fmt.Errorf("gardener: nil sqlite queue")
	}
	status := "done"
	if !receipt.OK {
		status = "failed"
	}
	finished := receipt.FinishedAt.UnixMilli()
	if finished == 0 {
		finished = time.Now().UnixMilli()
	}
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
UPDATE gardener_jobs SET status=?, lease_owner='', lease_until=0, error=?
WHERE job_id=?`, status, receipt.Error, receipt.JobID); err != nil {
		return err
	}
	ok := 0
	if receipt.OK {
		ok = 1
	}
	if _, err := tx.ExecContext(ctx, `
INSERT OR REPLACE INTO gardener_receipts
  (job_id, kind, generation_id, document_id, ok, error, output, finished_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		receipt.JobID, string(receipt.Kind), receipt.GenerationID, receipt.DocumentID,
		ok, receipt.Error, receipt.Output, finished); err != nil {
		return err
	}
	return tx.Commit()
}

// PendingCount returns jobs still pending or leased.
func (q *SQLiteQueue) PendingCount(ctx context.Context) (int, error) {
	if q == nil || q.db == nil {
		return 0, fmt.Errorf("gardener: nil sqlite queue")
	}
	var n int
	err := q.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM gardener_jobs WHERE status IN ('pending','leased')`).Scan(&n)
	return n, err
}

// var _ Queue = (*SQLiteQueue)(nil) compile-time check
var _ Queue = (*SQLiteQueue)(nil)
