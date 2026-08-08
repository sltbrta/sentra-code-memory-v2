package roster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

var (
	// ErrInvalidInput reports missing or malformed roster facts.
	ErrInvalidInput = errors.New("factory/roster: invalid input")
	// ErrStaleFence reports a lease fence that is not the current unexpired
	// winner for its leaf node. Commits under a stale fence never become
	// canonical.
	ErrStaleFence = errors.New("factory/roster: stale fence")
	// ErrResultConflict reports a second differing canonical result for one leaf.
	ErrResultConflict = errors.New("factory/roster: leaf result conflict")
)

// Executor is the narrow database handle shared by the composing kernel: it is
// satisfied by both *sql.DB and *sql.Tx, so roster facts commit inside the
// caller's transaction.
type Executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Lease is one durable fenced lease issuance fact for one leaf node. Fence
// values advance densely from 1 per (run, node); the highest fence is the only
// possible active winner.
type Lease struct {
	// Tenant and Principal scope the owning run.
	Tenant    string
	Principal string
	// RunID and NodeID identify the leased leaf.
	RunID  string
	NodeID string
	// Fence is the dense issuance number assigned at insert time.
	Fence uint64
	// LeaseID is the deterministic opaque lease identity.
	LeaseID string
	// HolderPrincipalID names the worker principal the lease was issued to.
	HolderPrincipalID string
	// IssuedAtMs and ExpiresAtMs bound holder authority; expiry is evaluated
	// against the injected clock at authorization time.
	IssuedAtMs  int64
	ExpiresAtMs int64
}

// Result is the canonical outcome of one leaf, committed under an authorized
// fence exactly once.
type Result struct {
	// Lease identifies the run, node, and fence the commit was authorized under.
	Lease Lease
	// ArtifactID and Digest pin the result payload stored in the encrypted
	// vault by the caller before commit.
	ArtifactID string
	Digest     string
	// CommittedAtMs is the canonical commit instant from the injected clock.
	CommittedAtMs int64
	// Replayed reports that this exact result was already canonical.
	Replayed bool
}

// Store derives lease winners and commit authorization from the migration 005
// insert-only facts. It is safe for concurrent use; callers serialize writers
// through the composing kernel's mutex and single connection.
type Store struct {
	clock contracts.Clock
}

// New binds the wall clock used for issuance and staleness evaluation.
func New(clock contracts.Clock) (*Store, error) {
	if clock == nil {
		return nil, ErrInvalidInput
	}
	return &Store{clock: clock}, nil
}

// Issue appends the next densely fenced lease for one leaf node and returns the
// assigned lease. The schema's dense-fence trigger independently enforces
// fence == max+1, so two racing issuers cannot both win one fence; the loser
// aborts its transaction.
func (s *Store) Issue(ctx context.Context, ex Executor, lease Lease) (Lease, error) {
	if s == nil || ctx == nil || ex == nil || !validID(lease.Tenant) || !validID(lease.Principal) ||
		!validID(lease.RunID) || !validID(lease.NodeID) || !validID(lease.HolderPrincipalID) ||
		lease.ExpiresAtMs <= 0 {
		return Lease{}, ErrInvalidInput
	}
	var fence uint64
	if err := ex.QueryRowContext(ctx, `SELECT COALESCE(MAX(fence),0)+1 FROM factory_leases
		WHERE tenant_id=? AND principal_id=? AND run_id=? AND node_id=?`,
		lease.Tenant, lease.Principal, lease.RunID, lease.NodeID).Scan(&fence); err != nil {
		return Lease{}, fmt.Errorf("factory/roster: read lease fence: %w", err)
	}
	lease.Fence = fence
	lease.LeaseID = identity("ouroboros.stage05.lease.v1",
		lease.Tenant, lease.Principal, lease.RunID, lease.NodeID, strconv.FormatUint(fence, 10))
	lease.IssuedAtMs = s.clock.NowUnixMilli()
	if _, err := ex.ExecContext(ctx, `INSERT INTO factory_leases
		(tenant_id,principal_id,run_id,node_id,fence,lease_id,holder_principal_id,issued_at_ms,expires_at_ms)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		lease.Tenant, lease.Principal, lease.RunID, lease.NodeID, lease.Fence, lease.LeaseID,
		lease.HolderPrincipalID, lease.IssuedAtMs, lease.ExpiresAtMs); err != nil {
		return Lease{}, fmt.Errorf("factory/roster: issue lease: %w", err)
	}
	return lease, nil
}

// Current returns the highest-fence lease for one leaf node: the only possible
// active winner. The boolean is false when the node was never leased.
func (s *Store) Current(ctx context.Context, ex Executor, tenant, principal, runID, nodeID string) (Lease, bool, error) {
	if s == nil || ctx == nil || ex == nil || !validID(tenant) || !validID(principal) ||
		!validID(runID) || !validID(nodeID) {
		return Lease{}, false, ErrInvalidInput
	}
	lease := Lease{Tenant: tenant, Principal: principal, RunID: runID, NodeID: nodeID}
	err := ex.QueryRowContext(ctx, `SELECT fence,lease_id,holder_principal_id,issued_at_ms,expires_at_ms
		FROM factory_leases
		WHERE tenant_id=? AND principal_id=? AND run_id=? AND node_id=?
		ORDER BY fence DESC LIMIT 1`,
		tenant, principal, runID, nodeID).
		Scan(&lease.Fence, &lease.LeaseID, &lease.HolderPrincipalID, &lease.IssuedAtMs, &lease.ExpiresAtMs)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, fmt.Errorf("factory/roster: read current lease: %w", err)
	}
	return lease, true, nil
}

// Authorize returns the current lease when the presented fence is exactly the
// current fence and the lease has not expired at the injected clock. Anything
// else — a regressed fence, a fence ahead of issuance, or an expired holder —
// is ErrStaleFence.
func (s *Store) Authorize(
	ctx context.Context, ex Executor, tenant, principal, runID, nodeID string, fence uint64,
) (Lease, error) {
	if fence == 0 {
		return Lease{}, ErrInvalidInput
	}
	lease, found, err := s.Current(ctx, ex, tenant, principal, runID, nodeID)
	if err != nil {
		return Lease{}, err
	}
	if !found || lease.Fence != fence || s.clock.NowUnixMilli() >= lease.ExpiresAtMs {
		return Lease{}, ErrStaleFence
	}
	return lease, nil
}

// CommitResult records the canonical result of one leaf under an authorized
// fence, atomically inside the caller's transaction. An exact replay of an
// already-canonical result returns Replayed; a differing second result is
// ErrResultConflict; any commit under a stale fence is ErrStaleFence and never
// becomes canonical.
func (s *Store) CommitResult(ctx context.Context, ex Executor, result Result) (Result, error) {
	if s == nil || ctx == nil || ex == nil || !validID(result.ArtifactID) || !validHexDigest(result.Digest) {
		return Result{}, ErrInvalidInput
	}
	lease, err := s.Authorize(ctx, ex, result.Lease.Tenant, result.Lease.Principal,
		result.Lease.RunID, result.Lease.NodeID, result.Lease.Fence)
	if err != nil {
		return Result{}, err
	}
	result.Lease = lease
	existing, found, err := s.lookupResult(ctx, ex, lease)
	if err != nil {
		return Result{}, err
	}
	if found {
		// The canonical digest is the authoritative replay match: artifact
		// identities are vault-assigned and may differ across exact retries.
		if existing.Lease.Fence == lease.Fence && existing.Digest == result.Digest {
			existing.Replayed = true
			return existing, nil
		}
		return Result{}, ErrResultConflict
	}
	result.CommittedAtMs = s.clock.NowUnixMilli()
	if _, err := ex.ExecContext(ctx, `INSERT INTO factory_leaf_results
		(tenant_id,principal_id,run_id,node_id,fence,result_artifact_id,result_digest,committed_at_ms)
		VALUES (?,?,?,?,?,?,?,?)`,
		lease.Tenant, lease.Principal, lease.RunID, lease.NodeID, lease.Fence,
		result.ArtifactID, result.Digest, result.CommittedAtMs); err != nil {
		return Result{}, fmt.Errorf("factory/roster: commit leaf result: %w", err)
	}
	return result, nil
}

// Result returns the canonical leaf result when one exists.
func (s *Store) Result(ctx context.Context, ex Executor, tenant, principal, runID, nodeID string) (Result, bool, error) {
	if s == nil || ctx == nil || ex == nil || !validID(tenant) || !validID(principal) ||
		!validID(runID) || !validID(nodeID) {
		return Result{}, false, ErrInvalidInput
	}
	return s.lookupResult(ctx, ex, Lease{Tenant: tenant, Principal: principal, RunID: runID, NodeID: nodeID})
}

func (s *Store) lookupResult(ctx context.Context, ex Executor, lease Lease) (Result, bool, error) {
	result := Result{Lease: lease}
	err := ex.QueryRowContext(ctx, `SELECT fence,result_artifact_id,result_digest,committed_at_ms
		FROM factory_leaf_results
		WHERE tenant_id=? AND principal_id=? AND run_id=? AND node_id=?`,
		lease.Tenant, lease.Principal, lease.RunID, lease.NodeID).
		Scan(&result.Lease.Fence, &result.ArtifactID, &result.Digest, &result.CommittedAtMs)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, fmt.Errorf("factory/roster: read leaf result: %w", err)
	}
	return result, true, nil
}

func validID(value string) bool {
	if value == "" || len(value) > 512 {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
