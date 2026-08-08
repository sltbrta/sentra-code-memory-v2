package federation

import (
	"sort"
	"strings"
	"time"
)

// BrainCard is a non-sensitive projection for federation selection (FED-001).
type BrainCard struct {
	BrainID    string   `json:"brain_id"`
	TenantID   string   `json:"tenant_id,omitempty"`
	Path       string   `json:"path"` // local path for MVP
	Region     string   `json:"region,omitempty"`
	Topics     []string `json:"topics,omitempty"`
	DocCount   int      `json:"doc_count"`
	FreshMS    int64    `json:"fresh_ms,omitempty"`
	CostHint   float64  `json:"cost_hint"`
	AllowedFor []string `json:"allowed_for,omitempty"` // principals
}

// Capability is an attenuated short-lived grant (FED-004).
type Capability struct {
	Principal string    `json:"principal"`
	BrainID   string    `json:"brain_id"`
	Action    string    `json:"action"`
	Expires   time.Time `json:"expires"`
}

// Valid reports whether capability is live for principal/brain/action.
func (c Capability) Valid(principal, brainID, action string, now time.Time) bool {
	if c.Principal != principal || c.BrainID != brainID {
		return false
	}
	if c.Action != "" && c.Action != action {
		return false
	}
	return now.Before(c.Expires)
}

// MintCapability issues a short-lived capability.
func MintCapability(principal, brainID, action string, ttl time.Duration, now time.Time) Capability {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if now.IsZero() {
		now = time.Now()
	}
	return Capability{Principal: principal, BrainID: brainID, Action: action, Expires: now.Add(ttl)}
}

// FilterCards keeps cards the principal may see (FED-002 authorize-before-fanout).
func FilterCards(cards []BrainCard, principal, tenant, region string) []BrainCard {
	var out []BrainCard
	for _, c := range cards {
		if tenant != "" && c.TenantID != "" && c.TenantID != tenant {
			continue
		}
		if region != "" && c.Region != "" && c.Region != region {
			continue
		}
		if len(c.AllowedFor) > 0 && !contains(c.AllowedFor, principal) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// RankCards sorts by topic overlap then lower cost (FED-003).
func RankCards(cards []BrainCard, query string, k int) []BrainCard {
	qtoks := tokens(query)
	type scored struct {
		c BrainCard
		s float64
	}
	var ss []scored
	for _, c := range cards {
		sc := 0.0
		for _, t := range c.Topics {
			for _, q := range qtoks {
				if strings.EqualFold(t, q) {
					sc += 1
				}
			}
		}
		sc += float64(c.DocCount) * 0.001
		sc -= c.CostHint
		ss = append(ss, scored{c, sc})
	}
	sort.Slice(ss, func(i, j int) bool {
		if ss[i].s == ss[j].s {
			return ss[i].c.BrainID < ss[j].c.BrainID
		}
		return ss[i].s > ss[j].s
	})
	if k <= 0 {
		k = 3
	}
	if k > len(ss) {
		k = len(ss)
	}
	out := make([]BrainCard, 0, k)
	for i := 0; i < k; i++ {
		out = append(out, ss[i].c)
	}
	return out
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func tokens(q string) []string {
	fields := strings.Fields(strings.ToLower(q))
	var out []string
	for _, f := range fields {
		f = strings.Trim(f, ".,?!")
		if len(f) >= 3 {
			out = append(out, f)
		}
	}
	return out
}
