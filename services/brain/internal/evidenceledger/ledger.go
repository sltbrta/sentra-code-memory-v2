// Package evidenceledger records immutable, tenant-and-brain-scoped evidence metadata.
// Artifact bytes remain in ArtifactVault; this package stores only references, anchors,
// lineage, and immediate tombstone state through a narrow repository boundary.
package evidenceledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

var (
	// ErrInvalid reports missing or malformed tenant, brain, evidence, or artifact scope.
	ErrInvalid = errors.New("evidenceledger: invalid record")
	// ErrNotFound uniformly reports absent, inaccessible, and tombstoned evidence.
	ErrNotFound = errors.New("evidenceledger: evidence not found")
	// ErrConflict reports immutable duplicate or lineage disagreement.
	ErrConflict = errors.New("evidenceledger: immutable conflict")
)

// Record identifies immutable evidence metadata and its exact ArtifactVault generation.
type Record struct {
	Tenant     contracts.Identifier
	Brain      contracts.Identifier
	Evidence   contracts.Identifier
	Artifact   contracts.Identifier
	Generation uint64
	Anchor     string
	Digest     contracts.Digest
}

// Lineage records a typed within-brain relation between two immutable evidence records.
type Lineage struct {
	Tenant   contracts.Identifier
	Brain    contracts.Identifier
	Parent   contracts.Identifier
	Child    contracts.Identifier
	Relation string
}

// Repository persists evidence metadata only. Implementations must namespace every lookup
// by tenant and brain and must not infer access from caller-supplied evidence identifiers.
type Repository interface {
	Put(context.Context, Record) (bool, error)
	Get(context.Context, contracts.Identifier, contracts.Identifier, contracts.Identifier) (Record, error)
	PutLineageIfEndpointsReadable(context.Context, Lineage) (bool, error)
	Lineage(context.Context, contracts.Identifier, contracts.Identifier, contracts.Identifier) ([]Lineage, error)
	Tombstone(context.Context, contracts.Identifier, contracts.Identifier, contracts.Identifier) error
}

// Ledger validates immutable evidence and lineage before delegating metadata persistence.
type Ledger struct {
	repository Repository
}

// New returns a ledger backed by the supplied metadata repository.
func New(repository Repository) (*Ledger, error) {
	if repository == nil {
		return nil, ErrInvalid
	}
	return &Ledger{repository: repository}, nil
}

// Admit records one immutable evidence reference. Identical retries are idempotent;
// a changed duplicate returns ErrConflict.
func (l *Ledger) Admit(ctx context.Context, record Record) (bool, error) {
	if validateRecord(record) != nil {
		return false, ErrInvalid
	}
	return l.repository.Put(ctx, record)
}

// Read returns one exact tenant-and-brain-scoped record or uniform ErrNotFound.
func (l *Ledger) Read(ctx context.Context, tenant, brain, evidence contracts.Identifier) (Record, error) {
	if validateID(tenant, "tenant") != nil || validateID(brain, "brain") != nil || validateID(evidence, "evidence") != nil {
		return Record{}, ErrInvalid
	}
	return l.repository.Get(ctx, tenant, brain, evidence)
}

// Link adds one typed lineage edge only when both endpoints exist in the same scope.
func (l *Ledger) Link(ctx context.Context, edge Lineage) (bool, error) {
	if validateID(edge.Tenant, "tenant") != nil || validateID(edge.Brain, "brain") != nil || validateID(edge.Parent, "evidence") != nil || validateID(edge.Child, "evidence") != nil || edge.Relation == "" || len(edge.Relation) > 128 || edge.Parent == edge.Child {
		return false, ErrInvalid
	}
	return l.repository.PutLineageIfEndpointsReadable(ctx, edge)
}

// Related returns copied lineage metadata for one exact scope.
func (l *Ledger) Related(ctx context.Context, tenant, brain, evidence contracts.Identifier) ([]Lineage, error) {
	if _, err := l.Read(ctx, tenant, brain, evidence); err != nil {
		return nil, err
	}
	return l.repository.Lineage(ctx, tenant, brain, evidence)
}

// Tombstone immediately makes an exact evidence record and its lineage unreadable.
func (l *Ledger) Tombstone(ctx context.Context, tenant, brain, evidence contracts.Identifier) error {
	if validateID(tenant, "tenant") != nil || validateID(brain, "brain") != nil || validateID(evidence, "evidence") != nil {
		return ErrInvalid
	}
	return l.repository.Tombstone(ctx, tenant, brain, evidence)
}

func validateRecord(record Record) error {
	if record.Digest.Algorithm != "sha256" || len(record.Digest.Hex) != sha256.Size*2 {
		return ErrInvalid
	}
	decodedDigest, digestErr := hex.DecodeString(record.Digest.Hex)
	if validateID(record.Tenant, "tenant") != nil || validateID(record.Brain, "brain") != nil || validateID(record.Evidence, "evidence") != nil || validateID(record.Artifact, "artifact") != nil || record.Generation == 0 || record.Anchor == "" || len(record.Anchor) > 4096 || digestErr != nil || len(decodedDigest) != sha256.Size || hex.EncodeToString(decodedDigest) != record.Digest.Hex {
		return ErrInvalid
	}
	return nil
}

func validateID(identifier contracts.Identifier, namespace string) error {
	if identifier.Namespace != namespace || identifier.Value == "" || len(identifier.Value) > 512 {
		return ErrInvalid
	}
	return nil
}

type memoryRecord struct {
	record     Record
	tombstoned bool
}

// MemoryRepository is a concurrency-safe deterministic repository for tests and fixtures.
// Production composition supplies SQLite; this adapter is not persistent authority.
type MemoryRepository struct {
	mu      sync.RWMutex
	records map[string]memoryRecord
	edges   map[string]Lineage
}

// NewMemoryRepository returns an empty isolated evidence repository.
func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{records: make(map[string]memoryRecord), edges: make(map[string]Lineage)}
}

// Put implements immutable idempotent evidence admission.
func (m *MemoryRepository) Put(_ context.Context, record Record) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := recordKey(record.Tenant, record.Brain, record.Evidence)
	if existing, ok := m.records[key]; ok {
		if existing.record == record {
			return false, nil
		}
		return false, ErrConflict
	}
	m.records[key] = memoryRecord{record: record}
	return true, nil
}

// Get returns one exact readable record.
func (m *MemoryRepository) Get(_ context.Context, tenant, brain, evidence contracts.Identifier) (Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.records[recordKey(tenant, brain, evidence)]
	if !ok || record.tombstoned {
		return Record{}, ErrNotFound
	}
	return record.record, nil
}

// PutLineageIfEndpointsReadable atomically validates both endpoints and inserts the edge.
func (m *MemoryRepository) PutLineageIfEndpointsReadable(_ context.Context, edge Lineage) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	parent, parentOK := m.records[recordKey(edge.Tenant, edge.Brain, edge.Parent)]
	child, childOK := m.records[recordKey(edge.Tenant, edge.Brain, edge.Child)]
	if !parentOK || !childOK || parent.tombstoned || child.tombstoned {
		return false, ErrNotFound
	}
	key := edgeKey(edge)
	if existing, ok := m.edges[key]; ok {
		if existing == edge {
			return false, nil
		}
		return false, ErrConflict
	}
	m.edges[key] = edge
	return true, nil
}

// Lineage returns copied edges touching the evidence record.
func (m *MemoryRepository) Lineage(_ context.Context, tenant, brain, evidence contracts.Identifier) ([]Lineage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]Lineage, 0)
	for _, edge := range m.edges {
		if edge.Tenant == tenant && edge.Brain == brain && (edge.Parent == evidence || edge.Child == evidence) {
			result = append(result, edge)
		}
	}
	return result, nil
}

// Tombstone hides the record and removes its rebuildable lineage edges.
func (m *MemoryRepository) Tombstone(_ context.Context, tenant, brain, evidence contracts.Identifier) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := recordKey(tenant, brain, evidence)
	record, ok := m.records[key]
	if !ok {
		return ErrNotFound
	}
	record.tombstoned = true
	m.records[key] = record
	for key, edge := range m.edges {
		if edge.Tenant == tenant && edge.Brain == brain && (edge.Parent == evidence || edge.Child == evidence) {
			delete(m.edges, key)
		}
	}
	return nil
}

func recordKey(tenant, brain, evidence contracts.Identifier) string {
	return encodeComposite(tenant.Value, brain.Value, evidence.Value)
}

func edgeKey(edge Lineage) string {
	return encodeComposite(edge.Tenant.Value, edge.Brain.Value, edge.Parent.Value, edge.Child.Value, edge.Relation)
}

func encodeComposite(parts ...string) string {
	var encoded strings.Builder
	for _, part := range parts {
		encoded.WriteString(strconv.Itoa(len(part)))
		encoded.WriteByte(':')
		encoded.WriteString(part)
	}
	return encoded.String()
}
