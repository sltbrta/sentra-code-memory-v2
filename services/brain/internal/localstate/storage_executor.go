package localstate

import (
	"context"
	"database/sql"
	"fmt"
)

// MetadataReader is the bounded query capability supplied only while a
// Store-owned serialized callback is active. Callers must not retain it.
type MetadataReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// MetadataWriter adds parameterized execution inside one Store-owned SQLite
// transaction. It does not expose the database handle or transaction lifecycle.
type MetadataWriter interface {
	MetadataReader
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// ReadMetadata executes one bounded callback under the authority Store's
// serialization lock. It returns ErrInvalidInput for nil/closed stores or nil callbacks.
func (s *Store) ReadMetadata(ctx context.Context, read func(MetadataReader) error) error {
	if s == nil || read == nil {
		return ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrInvalidInput
	}
	return read(s.db)
}

// WriteMetadata executes one callback in a Store-owned transaction and commits
// only when it succeeds. This serializes storage metadata with command state but
// does not make external artifact effects and later command Finalize atomic.
func (s *Store) WriteMetadata(ctx context.Context, write func(MetadataWriter) error) error {
	if s == nil || write == nil {
		return ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("localstate: begin metadata transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := write(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("localstate: commit metadata transaction: %w", err)
	}
	return nil
}
