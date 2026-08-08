package factory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factory/mailbox"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factory/roster"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	_ "modernc.org/sqlite"
)

// Kernel is the deterministic Stage 05 factory kernel: admission, plan
// compilation, lease and gate facts, candidate atomicity, and the review
// reducer over the migration 005 insert-only tables. It is safe for concurrent
// use: one mutex serializes every operation, so dense sequences and
// exactly-once facts hold without cross-process locking, which the composing
// authority owner already enforces.
type Kernel struct {
	db       *sql.DB
	payloads PayloadStore
	clock    contracts.Clock
	bases    BaseResolver
	policy   contracts.PolicyCheck
	router   Router
	roster   *roster.Store
	mailbox  *mailbox.Store

	leaseTTLMillis  int64
	revocationEpoch uint64
	policyDigestHex string

	mu sync.Mutex
}

// Open attaches the factory kernel to an already-migrated authority database.
// The path must be absolute; migration 005 must already be applied (the
// composing local authority owns migrations and the process owner lock, so
// Open takes neither). WAL, full synchronous, foreign keys, and a bounded busy
// timeout mirror the authority's own durability posture.
func Open(ctx context.Context, config Config) (*Kernel, error) {
	clean := filepath.Clean(config.DatabasePath)
	if ctx == nil || !filepath.IsAbs(clean) || config.Payloads == nil || config.Clock == nil ||
		config.Bases == nil || config.Policy == nil || config.Router == nil ||
		config.LeaseTTLMillis <= 0 || !isHexDigest(config.PolicyDigestHex) {
		return nil, ErrInvalidInput
	}
	db, err := sql.Open("sqlite", clean)
	if err != nil {
		return nil, fmt.Errorf("factory: open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	rosterStore, err := roster.New(config.Clock)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	mailboxStore, err := mailbox.New(config.Clock)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	kernel := &Kernel{
		db:              db,
		payloads:        config.Payloads,
		clock:           config.Clock,
		bases:           config.Bases,
		policy:          config.Policy,
		router:          config.Router,
		roster:          rosterStore,
		mailbox:         mailboxStore,
		leaseTTLMillis:  config.LeaseTTLMillis,
		revocationEpoch: config.RevocationEpoch,
		policyDigestHex: config.PolicyDigestHex,
	}
	if err := kernel.configure(ctx); err != nil {
		return nil, errors.Join(err, kernel.Close())
	}
	if err := kernel.requireSchema(ctx); err != nil {
		return nil, errors.Join(err, kernel.Close())
	}
	return kernel, nil
}

// Close releases the database handle; the authority owner lock stays with the
// composing local authority. It is idempotent and safe to call concurrently
// with in-flight operations: those already holding the kernel mutex commit
// first, and every later call fails closed with ErrInvalidInput.
func (k *Kernel) Close() error {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return nil
	}
	err := k.db.Close()
	k.db = nil
	return err
}

func (k *Kernel) configure(ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000",
	} {
		if _, err := k.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("factory: configure database: %w", err)
		}
	}
	return nil
}

func (k *Kernel) requireSchema(ctx context.Context) error {
	var applied int
	if err := k.db.QueryRowContext(ctx,
		`SELECT count(*) FROM schema_migrations WHERE version=5`).Scan(&applied); err != nil {
		return errors.Join(ErrSchemaUnsupported, fmt.Errorf("factory: inspect migrations: %w", err))
	}
	if applied != 1 {
		return ErrSchemaUnsupported
	}
	return nil
}

// mappedIdentity converts the trusted kernel scope into the Stage 02 policy
// fact shape. The peer credentials are zero: the factory kernel is invoked
// only behind the authenticated gateway session, which already mapped them.
func mappedIdentity(identity Identity) contracts.MappedIdentityFact {
	return contracts.MappedIdentityFact{
		Principal: contracts.Identifier{Namespace: "principal", Value: identity.Principal},
		Tenant:    contracts.Identifier{Namespace: "tenant", Value: identity.Tenant},
		Session:   contracts.Identifier{Namespace: "session", Value: identity.Session},
	}
}

// authorize runs the authorization-first current-policy check; any denial,
// error, or malformed decision collapses to the static ErrNotFoundOrDenied.
func (k *Kernel) authorize(ctx context.Context, identity Identity, action string) error {
	decision, err := k.policy.Check(ctx, mappedIdentity(identity), contracts.PolicyRequest{
		Action:   action,
		Resource: contracts.Identifier{Namespace: "tenant", Value: identity.Tenant},
	})
	if err != nil || !decision.Allowed {
		return ErrNotFoundOrDenied
	}
	return nil
}

// crossCheck enforces that untrusted body identity equals the authenticated
// scope; a mismatch is indistinguishable from absence.
func crossCheck(authenticated Identity, caller CallerCrossCheck) error {
	if authenticated.Tenant != caller.Tenant || authenticated.Principal != caller.Principal ||
		authenticated.Session != caller.Session {
		return ErrNotFoundOrDenied
	}
	return nil
}

func validIdentity(identity Identity) bool {
	return identity.Tenant != "" && identity.Principal != "" && identity.Session != "" &&
		len(identity.Tenant) <= 512 && len(identity.Principal) <= 512 && len(identity.Session) <= 512
}

func validIdempotencyKey(key string) bool {
	if key == "" || len(key) > 512 {
		return false
	}
	for _, character := range key {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

// unixMillis converts one canonical millisecond instant to a UTC time.
func unixMillis(value int64) time.Time {
	return time.UnixMilli(value).UTC()
}
