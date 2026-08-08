package gardener

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgresQueue is a durable gardener.Queue for parallel local/team residual.
// Unlike SQLite (single-writer), Postgres supports concurrent Claim with
// FOR UPDATE SKIP LOCKED so many local workers can drain the same queue.
type PostgresQueue struct {
	db  *sql.DB
	dsn string
	// LeaseTTL is how long a claim holds a job (default 2m).
	LeaseTTL time.Duration
}

const postgresQueueSchema = `
CREATE TABLE IF NOT EXISTS gardener_jobs (
  job_id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  generation_id TEXT NOT NULL,
  document_id TEXT NOT NULL DEFAULT '',
  payload_json TEXT NOT NULL DEFAULT '{}',
  status TEXT NOT NULL DEFAULT 'pending',
  attempt INTEGER NOT NULL DEFAULT 0,
  lease_owner TEXT NOT NULL DEFAULT '',
  lease_until BIGINT NOT NULL DEFAULT 0,
  created_at BIGINT NOT NULL,
  error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS gardener_jobs_status_pg ON gardener_jobs(status, lease_until);
CREATE TABLE IF NOT EXISTS gardener_receipts (
  job_id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  generation_id TEXT NOT NULL,
  document_id TEXT NOT NULL DEFAULT '',
  ok INTEGER NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  output TEXT NOT NULL DEFAULT '',
  finished_at BIGINT NOT NULL
);
`

// OpenPostgresQueue opens a durable multi-worker queue on Postgres/Neon.
// dsn is a standard postgres URL (same shape as NEON_DATABASE_URL / DATABASE_URL).
func OpenPostgresQueue(dsn string) (*PostgresQueue, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, fmt.Errorf("gardener: empty postgres queue dsn")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("gardener: open postgres queue: %w", err)
	}
	// Parallel local workers: allow concurrent claims/completes.
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(time.Hour)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("gardener: ping postgres queue: %w", err)
	}
	if _, err := db.Exec(postgresQueueSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("gardener: postgres queue schema: %w", err)
	}
	return &PostgresQueue{db: db, dsn: dsn, LeaseTTL: 2 * time.Minute}, nil
}

// DSN returns the connection string (may contain secrets — do not log wholesale).
func (q *PostgresQueue) DSN() string {
	if q == nil {
		return ""
	}
	return q.dsn
}

// Close closes the pool.
func (q *PostgresQueue) Close() error {
	if q == nil || q.db == nil {
		return nil
	}
	return q.db.Close()
}

// Enqueue inserts jobs as pending. Duplicate job_id is ignored.
func (q *PostgresQueue) Enqueue(ctx context.Context, jobs ...Job) error {
	if q == nil || q.db == nil {
		return fmt.Errorf("gardener: nil postgres queue")
	}
	now := time.Now().UnixMilli()
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO gardener_jobs
  (job_id, kind, generation_id, document_id, payload_json, status, attempt, created_at)
VALUES ($1, $2, $3, $4, $5, 'pending', 0, $6)
ON CONFLICT (job_id) DO NOTHING`)
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

// Claim leases up to n jobs using FOR UPDATE SKIP LOCKED (parallel-safe).
func (q *PostgresQueue) Claim(ctx context.Context, workerID string, n int) ([]Job, error) {
	if q == nil || q.db == nil {
		return nil, fmt.Errorf("gardener: nil postgres queue")
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

	if _, err := tx.ExecContext(ctx, `
UPDATE gardener_jobs SET status='pending', lease_owner='', lease_until=0
WHERE status='leased' AND lease_until > 0 AND lease_until < $1`, nowMS); err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, `
SELECT job_id, kind, generation_id, document_id, payload_json, attempt
FROM gardener_jobs
WHERE status='pending'
ORDER BY created_at ASC
LIMIT $1
FOR UPDATE SKIP LOCKED`, n)
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
SET status='leased', lease_owner=$1, lease_until=$2, attempt=attempt+1
WHERE job_id=$3 AND status='pending'`, workerID, until, r.id); err != nil {
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

// Complete records a receipt and marks the job done/failed.
func (q *PostgresQueue) Complete(ctx context.Context, receipt Receipt) error {
	if q == nil || q.db == nil {
		return fmt.Errorf("gardener: nil postgres queue")
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
UPDATE gardener_jobs SET status=$1, lease_owner='', lease_until=0, error=$2
WHERE job_id=$3`, status, receipt.Error, receipt.JobID); err != nil {
		return err
	}
	ok := 0
	if receipt.OK {
		ok = 1
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO gardener_receipts
  (job_id, kind, generation_id, document_id, ok, error, output, finished_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (job_id) DO UPDATE SET
  kind=EXCLUDED.kind, generation_id=EXCLUDED.generation_id,
  document_id=EXCLUDED.document_id, ok=EXCLUDED.ok, error=EXCLUDED.error,
  output=EXCLUDED.output, finished_at=EXCLUDED.finished_at`,
		receipt.JobID, string(receipt.Kind), receipt.GenerationID, receipt.DocumentID,
		ok, receipt.Error, receipt.Output, finished); err != nil {
		return err
	}
	return tx.Commit()
}

var _ Queue = (*PostgresQueue)(nil)
