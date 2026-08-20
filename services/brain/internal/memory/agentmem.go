package memory

import (
	"fmt"
	"strings"
	"time"
)

// Agent memory tiers (MemGPT/Letta-inspired hierarchical buffer).
const (
	TierSTM = "stm" // short-term — default for new puts
	TierMTM = "mtm" // medium-term
	TierLTM = "ltm" // long-term
)

// AgentMemoryEntry is a policy-gated agent memory atom (Mem0/MemGPT-inspired MVP).
// Not full SCM continuation packets — minimal write/read for product-brain.
type AgentMemoryEntry struct {
	ID         string    `json:"id"`
	Principal  string    `json:"principal"`
	Kind       string    `json:"kind"`           // note|preference|task|fact
	Tier       string    `json:"tier,omitempty"` // stm|mtm|ltm (default stm)
	Text       string    `json:"text"`
	Tags       []string  `json:"tags,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	DocumentID string    `json:"document_id,omitempty"` // optional evidence link
}

// normalizeTier returns stm|mtm|ltm; empty/unknown → stm.
func normalizeTier(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case TierMTM:
		return TierMTM
	case TierLTM:
		return TierLTM
	default:
		return TierSTM
	}
}

// tierRank lower = preferred in search (stm first).
func tierRank(t string) int {
	switch normalizeTier(t) {
	case TierSTM:
		return 0
	case TierMTM:
		return 1
	default:
		return 2
	}
}

// PutAgentMemory stores an entry if principal is non-empty (policy gate).
// New entries default to tier stm.
func (s *Store) PutAgentMemory(principal, kind, text string, tags []string) (AgentMemoryEntry, error) {
	return s.PutAgentMemoryTier(principal, kind, text, tags, TierSTM)
}

// PutAgentMemoryTier is PutAgentMemory with an explicit tier.
func (s *Store) PutAgentMemoryTier(principal, kind, text string, tags []string, tier string) (AgentMemoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s == nil {
		return AgentMemoryEntry{}, fmt.Errorf("memory: nil store")
	}
	principal = strings.TrimSpace(principal)
	text = strings.TrimSpace(text)
	if principal == "" {
		return AgentMemoryEntry{}, fmt.Errorf("memory: agent memory requires principal (policy gate)")
	}
	if text == "" {
		return AgentMemoryEntry{}, fmt.Errorf("memory: empty text")
	}
	if kind == "" {
		kind = "note"
	}
	e := AgentMemoryEntry{
		ID: fmt.Sprintf("am-%d", s.seq.Add(1)), Principal: principal,
		Kind: kind, Tier: normalizeTier(tier), Text: text, Tags: tags, CreatedAt: time.Now().UTC(),
	}
	s.data.AgentMem = append(s.data.AgentMem, e)
	return e, s.persistLocked()
}

// PromoteAgentMemory moves an entry to a new tier (stm|mtm|ltm).
func (s *Store) PromoteAgentMemory(id, tier string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s == nil {
		return fmt.Errorf("memory: nil store")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("memory: empty agent memory id")
	}
	tier = normalizeTier(tier)
	for i := range s.data.AgentMem {
		if s.data.AgentMem[i].ID == id {
			s.data.AgentMem[i].Tier = tier
			return s.persistLocked()
		}
	}
	return fmt.Errorf("memory: agent memory %q not found", id)
}

// GetAgentMemory returns entries for principal ordered stm → mtm → ltm, then recency.
func (s *Store) GetAgentMemory(principal string, limit int) []AgentMemoryEntry {
	if s == nil || strings.TrimSpace(principal) == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	var out []AgentMemoryEntry
	for i := len(s.data.AgentMem) - 1; i >= 0; i-- {
		e := s.data.AgentMem[i]
		if e.Principal != principal {
			continue
		}
		if e.Tier == "" {
			e.Tier = TierSTM
		}
		out = append(out, e)
	}
	// Sort: tier rank asc, then CreatedAt desc (already reverse-chron from scan —
	// stable re-order by tier).
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			ri, rj := tierRank(out[i].Tier), tierRank(out[j].Tier)
			if rj < ri || (rj == ri && out[j].CreatedAt.After(out[i].CreatedAt)) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// SearchAgentMemory does simple substring match for principal; prefers stm then mtm then ltm.
func (s *Store) SearchAgentMemory(principal, q string, limit int) []AgentMemoryEntry {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return s.GetAgentMemory(principal, limit)
	}
	if limit <= 0 {
		limit = 50
	}
	// Pull a wider pool then filter + re-tier-order.
	pool := s.GetAgentMemory(principal, 500)
	var out []AgentMemoryEntry
	for _, e := range pool {
		if strings.Contains(strings.ToLower(e.Text), q) {
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}
