package dense

import (
	"fmt"
	"sync"
)

// MemoryStore is a thread-safe in-memory embedding store.
// Suitable for tests and single-node product path until a durable projection lands.
type MemoryStore struct {
	mu   sync.RWMutex
	dim  int // 0 until first successful Upsert
	docs map[string][]float32
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		docs: make(map[string][]float32),
	}
}

// Dim returns the fixed vector dimension after the first Upsert, or 0 if empty.
func (s *MemoryStore) Dim() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dim
}

// Len returns the number of stored embeddings.
func (s *MemoryStore) Len() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.docs)
}

// Upsert stores or replaces the embedding for id.
// The first non-empty vector fixes the store dimension; later vectors must match.
// Empty id or empty vec returns an error. A defensive copy of vec is stored.
func (s *MemoryStore) Upsert(id string, vec []float32) error {
	if s == nil {
		return fmt.Errorf("dense: nil store")
	}
	if id == "" {
		return fmt.Errorf("dense: empty document id")
	}
	if len(vec) == 0 {
		return fmt.Errorf("dense: empty vector")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.docs == nil {
		s.docs = make(map[string][]float32)
	}
	if s.dim == 0 {
		s.dim = len(vec)
	} else if len(vec) != s.dim {
		return fmt.Errorf("dense: dim mismatch: got %d want %d", len(vec), s.dim)
	}
	cp := make([]float32, len(vec))
	copy(cp, vec)
	s.docs[id] = cp
	return nil
}

// Get returns a copy of the stored vector for id, if present.
func (s *MemoryStore) Get(id string) ([]float32, bool) {
	if s == nil || id == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	vec, ok := s.docs[id]
	if !ok {
		return nil, false
	}
	cp := make([]float32, len(vec))
	copy(cp, vec)
	return cp, true
}

// Delete removes the embedding for id. It is a no-op when missing.
func (s *MemoryStore) Delete(id string) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.docs, id)
}
