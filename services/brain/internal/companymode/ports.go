package companymode

import (
	"context"
	"errors"
)

// ErrDenied is the single non-disclosing company-mode denial.
var ErrDenied = errors.New("companymode: not_found_or_denied")

// ErrRejected reports a typed, non-disclosing validation failure.
var ErrRejected = errors.New("companymode: rejected")

// ErrSQLiteTransfer rejects SQLite/WAL payload transfer attempts.
var ErrSQLiteTransfer = errors.New("companymode: sqlite_transfer_forbidden")

// ErrQueueFull reports bounded admission rejection under saturation.
var ErrQueueFull = errors.New("companymode: queue_full")

// ProfileID names the active deployment profile.
type ProfileID string

const (
	// ProfileLocal is the single-principal local-first profile.
	ProfileLocal ProfileID = "local"
	// ProfileCompany is the bounded two-principal company profile.
	ProfileCompany ProfileID = "company"
)

// Principal is one authenticated company principal.
type Principal struct {
	ID       string
	TenantID string
}

// Event is one tenant-scoped domain event (never a SQLite page).
type Event struct {
	TenantID  string
	EventID   string
	Kind      string
	Payload   []byte
	Sequence  uint64
	Principal string
}

// BlobRef is one immutable S3-compatible object reference.
type BlobRef struct {
	TenantID string
	Key      string
	Digest   string
	Length   int64
}

// EventStore is the PostgreSQL-shaped canonical event port.
type EventStore interface {
	Append(ctx context.Context, event Event) error
	List(ctx context.Context, tenantID string) ([]Event, error)
	Snapshot(ctx context.Context) ([]Event, error)
	Restore(ctx context.Context, events []Event) error
}

// ObjectStore is the S3-compatible ArtifactVault port. Get must return an
// error satisfying errors.Is(err, ErrDenied) only when the requested object is
// conclusively missing or access is conclusively denied. Operational failures
// such as cancellation, timeout, or provider outage must return a distinct
// error that does not wrap ErrDenied, so callers cannot mistake an inconclusive
// probe for verified absence.
type ObjectStore interface {
	Put(ctx context.Context, tenantID, key string, body []byte) (BlobRef, error)
	Get(ctx context.Context, tenantID, key string) ([]byte, error)
	Inventory(ctx context.Context) ([]BlobRef, error)
	Restore(ctx context.Context, blobs []BlobObject) error
}

// BlobObject is a full object payload for backup/restore.
type BlobObject struct {
	Ref  BlobRef
	Body []byte
}

// PolicyCheck evaluates current company authorization for a principal action.
type PolicyCheck interface {
	Allow(ctx context.Context, principal Principal, action, resource string) bool
}
