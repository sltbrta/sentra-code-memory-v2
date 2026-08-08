package orgscope

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
)

var (
	// ErrMissingRecoverySubstrate reports that the drill matrix did not bind
	// every substrate represented by the product retrieval path.
	ErrMissingRecoverySubstrate = errors.New("orgscope: required recovery substrate missing")
	// ErrRecoverySubstrateFailed reports an inconclusive or leaking substrate
	// restore. It never implies that a live provider was exercised.
	ErrRecoverySubstrateFailed = errors.New("orgscope: recovery substrate verification failed")
)

// RecoverySubstrateKind is one required retrieval or persistence substrate.
// The closed list is deliberately explicit: adding a product substrate must
// update requiredRecoverySubstrates or the contract tests fail closed.
type RecoverySubstrateKind string

const (
	RecoverySubstrateFilesystem RecoverySubstrateKind = "filesystem"
	RecoverySubstrateSQL        RecoverySubstrateKind = "sql"
	RecoverySubstrateVector     RecoverySubstrateKind = "vector"
	RecoverySubstrateHotLex     RecoverySubstrateKind = "hotlex"
	RecoverySubstrateGraph      RecoverySubstrateKind = "graph"
	RecoverySubstrateClaims     RecoverySubstrateKind = "claims"
	RecoverySubstrateCache      RecoverySubstrateKind = "cache"
	RecoverySubstrateObject     RecoverySubstrateKind = "object"
)

var requiredRecoverySubstrates = [...]RecoverySubstrateKind{
	RecoverySubstrateFilesystem,
	RecoverySubstrateSQL,
	RecoverySubstrateVector,
	RecoverySubstrateHotLex,
	RecoverySubstrateGraph,
	RecoverySubstrateClaims,
	RecoverySubstrateCache,
	RecoverySubstrateObject,
}

// RequiredRecoverySubstrates returns the exact, stable substrate matrix.
func RequiredRecoverySubstrates() []RecoverySubstrateKind {
	return append([]RecoverySubstrateKind(nil), requiredRecoverySubstrates[:]...)
}

// RecoveryProviderBoundary states what an adapter actually exercises.
type RecoveryProviderBoundary string

const (
	// RecoveryBoundaryHermeticFake is deterministic in-process evidence only.
	RecoveryBoundaryHermeticFake RecoveryProviderBoundary = "hermetic_fake"
	// RecoveryBoundaryProviderAdapter identifies an explicitly supplied
	// provider adapter. Its results still never set ProductionCertified.
	RecoveryBoundaryProviderAdapter RecoveryProviderBoundary = "provider_adapter"
)

// RecoverySubstrateFixture is the payload-free projection input derived from
// the recovered target. Adapters receive identifiers only, never item text.
type RecoverySubstrateFixture struct {
	TenantID            string
	GenerationID        string
	RestoreCandidateIDs []string
	RepresentativeIDs   []string
	TombstonedIDs       []string
}

// RecoverySubstrateObservation is the conclusive result returned by one
// adapter. A pass requires both representative live records and every
// expected tombstone to have been checked.
type RecoverySubstrateObservation struct {
	RepresentativeRecords int
	TombstonesChecked     int
}

// RecoverySubstrateAdapter restores and probes one disposable substrate.
// Implementations must honor context cancellation and must return an error
// for an outage or ambiguous absence; absence is never inferred from failure.
type RecoverySubstrateAdapter interface {
	Kind() RecoverySubstrateKind
	ProviderBoundary() RecoveryProviderBoundary
	Restore(context.Context, RecoverySubstrateFixture) error
	VerifyErasure(context.Context, RecoverySubstrateFixture) (RecoverySubstrateObservation, error)
}

// RecoverySubstrateMatrix binds exactly one adapter for every required kind.
type RecoverySubstrateMatrix struct {
	Adapters []RecoverySubstrateAdapter
}

// NewHermeticRecoverySubstrateMatrix returns deterministic disposable
// fixtures for all required substrate kinds. It is unit evidence, not a live
// filesystem, database, vector service, HotLex volume, graph, claims engine,
// distributed cache, or object-store integration.
func NewHermeticRecoverySubstrateMatrix() RecoverySubstrateMatrix {
	adapters := make([]RecoverySubstrateAdapter, 0, len(requiredRecoverySubstrates))
	for _, kind := range requiredRecoverySubstrates {
		adapters = append(adapters, NewHermeticRecoverySubstrate(kind))
	}
	return RecoverySubstrateMatrix{Adapters: adapters}
}

// HermeticRecoverySubstrate is a deterministic identifier-only fake. The
// type is exported so tests can build explicit matrix fixtures without
// pretending that they reached a live provider.
type HermeticRecoverySubstrate struct {
	mu      sync.Mutex
	kind    RecoverySubstrateKind
	records map[string]struct{}
}

// NewHermeticRecoverySubstrate creates one empty disposable fake adapter.
func NewHermeticRecoverySubstrate(kind RecoverySubstrateKind) *HermeticRecoverySubstrate {
	return &HermeticRecoverySubstrate{kind: kind, records: make(map[string]struct{})}
}

func (s *HermeticRecoverySubstrate) Kind() RecoverySubstrateKind { return s.kind }

func (*HermeticRecoverySubstrate) ProviderBoundary() RecoveryProviderBoundary {
	return RecoveryBoundaryHermeticFake
}

// Restore projects only recovered live identifiers and requires an empty
// target, mirroring the isolation requirement of the main drill target.
func (s *HermeticRecoverySubstrate) Restore(ctx context.Context, fixture RecoverySubstrateFixture) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || !validRecoverySubstrateKind(s.kind) || !validID(fixture.TenantID) ||
		!validID(fixture.GenerationID) || len(fixture.RepresentativeIDs) == 0 {
		return ErrRejected
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.records) != 0 {
		return ErrRejected
	}
	dead := make(map[string]struct{}, len(fixture.TombstonedIDs))
	for _, id := range fixture.TombstonedIDs {
		if !validID(id) {
			return ErrRejected
		}
		dead[id] = struct{}{}
	}
	for _, id := range fixture.RestoreCandidateIDs {
		if !validID(id) {
			return ErrRejected
		}
		if _, tombstoned := dead[id]; tombstoned {
			continue
		}
		s.records[id] = struct{}{}
	}
	return nil
}

func (s *HermeticRecoverySubstrate) VerifyErasure(ctx context.Context, fixture RecoverySubstrateFixture) (RecoverySubstrateObservation, error) {
	if err := ctx.Err(); err != nil {
		return RecoverySubstrateObservation{}, err
	}
	if s == nil {
		return RecoverySubstrateObservation{}, ErrRejected
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	observation := RecoverySubstrateObservation{RepresentativeRecords: len(s.records)}
	if observation.RepresentativeRecords == 0 {
		return observation, ErrRecoverySubstrateFailed
	}
	for _, id := range fixture.RepresentativeIDs {
		if _, exists := s.records[id]; !exists {
			return observation, ErrRecoverySubstrateFailed
		}
	}
	for _, id := range fixture.TombstonedIDs {
		if _, leaked := s.records[id]; leaked {
			return observation, ErrRecoverySubstrateFailed
		}
		observation.TombstonesChecked++
	}
	return observation, nil
}

func validRecoverySubstrateKind(kind RecoverySubstrateKind) bool {
	for _, required := range requiredRecoverySubstrates {
		if kind == required {
			return true
		}
	}
	return false
}

func validateRecoverySubstrateMatrix(matrix RecoverySubstrateMatrix) error {
	seen := make(map[RecoverySubstrateKind]struct{}, len(matrix.Adapters))
	for _, adapter := range matrix.Adapters {
		if recoveryAdapterNil(adapter) {
			return ErrMissingRecoverySubstrate
		}
		kind := adapter.Kind()
		boundary := adapter.ProviderBoundary()
		if !validRecoverySubstrateKind(kind) ||
			(boundary != RecoveryBoundaryHermeticFake && boundary != RecoveryBoundaryProviderAdapter) {
			return ErrRejected
		}
		if _, duplicate := seen[kind]; duplicate {
			return ErrMissingRecoverySubstrate
		}
		seen[kind] = struct{}{}
	}
	for _, required := range requiredRecoverySubstrates {
		if _, present := seen[required]; !present {
			return ErrMissingRecoverySubstrate
		}
	}
	if len(seen) != len(requiredRecoverySubstrates) {
		return ErrMissingRecoverySubstrate
	}
	return nil
}

func recoveryAdapterNil(adapter RecoverySubstrateAdapter) bool {
	if adapter == nil {
		return true
	}
	value := reflect.ValueOf(adapter)
	return value.Kind() == reflect.Pointer && value.IsNil()
}

func recoverySubstrateFixture(request RecoveryDrillRequest, target *Store) RecoverySubstrateFixture {
	target.mu.Lock()
	live := make([]string, 0, len(target.items))
	for id := range target.items {
		live = append(live, id)
	}
	target.mu.Unlock()
	sort.Strings(live)
	candidateSet := make(map[string]struct{}, len(request.Backup.Store.Items)+len(live))
	for _, item := range request.Backup.Store.Items {
		candidateSet[item.ID] = struct{}{}
	}
	for _, id := range live {
		candidateSet[id] = struct{}{}
	}
	candidates := make([]string, 0, len(candidateSet))
	for id := range candidateSet {
		candidates = append(candidates, id)
	}
	sort.Strings(candidates)
	dead := make([]string, 0, len(request.ExpectedTombstones))
	for _, tombstone := range request.ExpectedTombstones {
		dead = append(dead, tombstone.ItemID)
	}
	sort.Strings(dead)
	return RecoverySubstrateFixture{
		TenantID: request.TenantID, GenerationID: request.GenerationID,
		RestoreCandidateIDs: candidates, RepresentativeIDs: live, TombstonedIDs: dead,
	}
}

func restoreAndVerifyRecoverySubstrates(
	ctx context.Context,
	matrix RecoverySubstrateMatrix,
	fixture RecoverySubstrateFixture,
) ([]RecoverySubstrateReceipt, error) {
	adapters := append([]RecoverySubstrateAdapter(nil), matrix.Adapters...)
	sort.Slice(adapters, func(i, j int) bool { return adapters[i].Kind() < adapters[j].Kind() })
	receipts := make([]RecoverySubstrateReceipt, 0, len(adapters))
	for _, adapter := range adapters {
		receipt := RecoverySubstrateReceipt{
			Name: string(adapter.Kind()), Kind: adapter.Kind(),
			ProviderBoundary:  adapter.ProviderBoundary(),
			RestoreCandidates: len(fixture.RestoreCandidateIDs),
			TombstonesChecked: len(fixture.TombstonedIDs),
		}
		if err := adapter.Restore(ctx, fixture); err != nil {
			receipts = append(receipts, receipt)
			return receipts, errors.Join(ErrRecoverySubstrateFailed, err)
		}
		observation, err := adapter.VerifyErasure(ctx, fixture)
		receipt.TombstonesChecked = observation.TombstonesChecked
		receipt.RepresentativeRecords = observation.RepresentativeRecords
		if err != nil || observation.TombstonesChecked != len(fixture.TombstonedIDs) ||
			observation.RepresentativeRecords != len(fixture.RepresentativeIDs) {
			receipts = append(receipts, receipt)
			if err == nil {
				err = ErrRejected
			}
			return receipts, errors.Join(ErrRecoverySubstrateFailed, err)
		}
		receipt.Passed = true
		receipts = append(receipts, receipt)
	}
	return receipts, nil
}
