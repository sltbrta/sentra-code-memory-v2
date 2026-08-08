package dense

import "testing"

func TestSearchNearestNeighbor(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	s.Upsert("a", []float32{1, 0, 0})
	s.Upsert("b", []float32{0, 1, 0})
	s.Upsert("c", []float32{0.9, 0.1, 0})
	hits := s.Search([]float32{1, 0, 0}, 2)
	if len(hits) < 1 || hits[0].DocumentID != "a" {
		t.Fatalf("hits=%+v", hits)
	}
}
