package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/durablefile"
)

// dataFile is the durable projection under a product brain dir.
const dataFile = "memory.json"

// diskData is the JSON shape of the memory cortex projection.
type diskData struct {
	Claims      []Claim                  `json:"claims"`
	Relations   []TemporalRelation       `json:"relations,omitempty"` // Graphiti-class bi-temporal edges
	Episodes    []Episode                `json:"episodes"`
	Utility     map[string]UtilityRecord `json:"utility,omitempty"`
	Summaries   []SummaryNode            `json:"summaries,omitempty"`
	AgentMem    []AgentMemoryEntry       `json:"agent_memory,omitempty"`
	Edges       map[string][]string      `json:"edges,omitempty"`        // doc adjacency for PPR (prior only)
	EdgeWeights map[string]float64       `json:"edge_weights,omitempty"` // "a->b" → weight
	DocTexts    map[string]string        `json:"doc_texts,omitempty"`
	Quarantine  []QuarantineEntry        `json:"quarantine,omitempty"`
	PageIndex   []PageNode               `json:"pageindex,omitempty"` // hierarchical TOC trees
	PageRank    map[string]float64       `json:"pagerank,omitempty"`  // global PR prior
}

// Store is a durable per-brain memory cortex (not a second product).
//
// Concurrency: mu guards data, disk writes, and the ContestedGroups index.
// SetDocEdges/SetDocTexts and the claim mutators (AdmitClaim, ExpireClaim,
// SupersedeClaim, ApplyResolution) lock for the full mutate+persist critical
// section; other mutators call persist() which locks only for the JSON write.
// Prefer locking in mutators when adding new concurrent writers.
type Store struct {
	dir  string
	mu   sync.Mutex
	data diskData
	seq  atomic.Int64

	// ContestedGroups index, guarded by mu. Rebuilt lazily on read and
	// invalidated by every claim/status mutation, so repeated answer-path
	// reads never rescan an unchanged claim list.
	contestedCache map[string][]Claim
	contestedValid bool
}

// Open opens or creates <brainDir>/memory/memory.json.
func Open(brainDir string) (*Store, error) {
	brainDir = filepath.Clean(brainDir)
	if brainDir == "" || brainDir == "." {
		return nil, fmt.Errorf("memory: empty brain dir")
	}
	dir := filepath.Join(brainDir, "memory")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, data: diskData{
		Utility:     map[string]UtilityRecord{},
		Edges:       map[string][]string{},
		EdgeWeights: map[string]float64{},
		DocTexts:    map[string]string{},
		PageRank:    map[string]float64{},
	}}
	path := filepath.Join(dir, dataFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, s.persist()
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, err
	}
	if s.data.Utility == nil {
		s.data.Utility = map[string]UtilityRecord{}
	}
	if s.data.Edges == nil {
		s.data.Edges = map[string][]string{}
	}
	if s.data.EdgeWeights == nil {
		s.data.EdgeWeights = map[string]float64{}
	}
	if s.data.DocTexts == nil {
		s.data.DocTexts = map[string]string{}
	}
	if s.data.PageRank == nil {
		s.data.PageRank = map[string]float64{}
	}
	if s.data.Relations == nil {
		s.data.Relations = nil // explicit empty ok
	}
	// bump seq past existing IDs roughly
	s.seq.Store(int64(len(s.data.Claims) + len(s.data.Relations) + len(s.data.Episodes) + len(s.data.AgentMem) + 10))
	return s, nil
}

func (s *Store) persist() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistLocked()
}

// persistLocked writes memory.json. Caller must hold s.mu.
// persistLocked writes the whole cortex. It used os.WriteFile, which truncates
// the live file in place: a crash, SIGKILL or ENOSPC part-way through left a
// truncated memory.json that fails json.Unmarshal at Open, taking every claim,
// temporal relation, episode, PageIndex tree, PageRank vector and agent-memory
// tier with it. There was no temp file and no backup generation, and every
// mutator takes this path -- thousands of times per gardener wave.
func (s *Store) persistLocked() error {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return durablefile.Write(filepath.Join(s.dir, dataFile), raw, 0o600)
}

// invalidateContestedLocked drops the ContestedGroups index so the next read
// rebuilds it from the claim list. Caller must hold s.mu. Called by every
// mutator that can change claim membership or status; fail-closed — a dropped
// cache only costs one rebuild, never a stale conflict view.
func (s *Store) invalidateContestedLocked() {
	s.contestedCache = nil
	s.contestedValid = false
}

// Dir returns the memory subdirectory.
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// SetDocEdges replaces the PPR adjacency (document co-occurrence graph).
// Clears EdgeWeights so WeightedEdges re-expands from the new adjacency at 1.0.
// Defensive-copies edges (and neighbor slices) before store.
func (s *Store) SetDocEdges(edges map[string][]string) error {
	if s == nil {
		return nil
	}
	cp := map[string][]string{}
	for k, nbrs := range edges {
		cp[k] = append([]string(nil), nbrs...)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Edges = cp
	s.data.EdgeWeights = map[string]float64{}
	return s.persistLocked()
}

// DocEdges returns adjacency for PPR.
func (s *Store) DocEdges() map[string][]string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]string, len(s.data.Edges))
	for k, nbrs := range s.data.Edges {
		out[k] = append([]string(nil), nbrs...)
	}
	return out
}

// SetDocTexts stores document bodies for RAPTOR/C1 probes.
// Defensive-copies the input map before merging into the store.
func (s *Store) SetDocTexts(docs map[string]string) error {
	if s == nil {
		return nil
	}
	cp := make(map[string]string, len(docs))
	for id, text := range docs {
		cp[id] = text
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.DocTexts == nil {
		s.data.DocTexts = map[string]string{}
	}
	for id, text := range cp {
		s.data.DocTexts[id] = text
	}
	return s.persistLocked()
}

// DocTexts returns stored document bodies.
func (s *Store) DocTexts() map[string]string {
	if s == nil {
		return map[string]string{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.DocTexts == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(s.data.DocTexts))
	for k, v := range s.data.DocTexts {
		out[k] = v
	}
	return out
}
