package companymode

import (
	"context"
	"sync"
)

// FakePostgres is an in-process PostgreSQL-shaped event ledger with tenant
// isolation. Rows from one tenant are never visible to another. It is not a
// substitute for real PostgreSQL RLS, but proves the product isolation port.
type FakePostgres struct {
	mu     sync.Mutex
	byTen  map[string][]Event
	seq    map[string]uint64
	closed bool
}

// NewFakePostgres returns an empty in-process event ledger.
func NewFakePostgres() *FakePostgres {
	return &FakePostgres{
		byTen: make(map[string][]Event),
		seq:   make(map[string]uint64),
	}
}

// Append inserts one event under its tenant sequence.
func (s *FakePostgres) Append(_ context.Context, event Event) error {
	if event.TenantID == "" || event.EventID == "" || event.Kind == "" {
		return ErrRejected
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrRejected
	}
	s.seq[event.TenantID]++
	event.Sequence = s.seq[event.TenantID]
	s.byTen[event.TenantID] = append(s.byTen[event.TenantID], event)
	return nil
}

// List returns events for exactly one tenant.
func (s *FakePostgres) List(_ context.Context, tenantID string) ([]Event, error) {
	if tenantID == "" {
		return nil, ErrRejected
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.byTen[tenantID]
	out := make([]Event, len(src))
	copy(out, src)
	return out, nil
}

// Snapshot returns a full ordered dump for backup.
func (s *FakePostgres) Snapshot(_ context.Context) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Event
	for _, events := range s.byTen {
		out = append(out, events...)
	}
	return out, nil
}

// Restore replaces all events from a backup dump.
func (s *FakePostgres) Restore(_ context.Context, events []Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byTen = make(map[string][]Event)
	s.seq = make(map[string]uint64)
	for _, event := range events {
		if event.TenantID == "" {
			return ErrRejected
		}
		s.byTen[event.TenantID] = append(s.byTen[event.TenantID], event)
		if event.Sequence > s.seq[event.TenantID] {
			s.seq[event.TenantID] = event.Sequence
		}
	}
	return nil
}
