package memory

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// GraphRAGLLMEnabled is true when OUROBOROS_BRAIN_GRAPHRAG_LLM=1.
func GraphRAGLLMEnabled() bool {
	v := strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_GRAPHRAG_LLM"))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

// GraphRAGMapReduce builds community-level map answers then a reduce summary
// (GAP-IR-GRAPHRAG-FULL). Deterministic extractive by default — no network.
func GraphRAGMapReduce(docs map[string]string, edges map[string][]string, question string, maxCommunities int) []SummaryNode {
	return GraphRAGMapReduceOpts(docs, edges, question, maxCommunities, nil)
}

// GraphRAGMapReduceOpts optionally applies an abstractive reduce via llm when
// GraphRAGLLMEnabled (GAP-IR-GRAPHRAG-FULL quality).
func GraphRAGMapReduceOpts(
	docs map[string]string,
	edges map[string][]string,
	question string,
	maxCommunities int,
	llm LLMExtractFunc,
) []SummaryNode {
	if maxCommunities <= 0 {
		maxCommunities = 6
	}
	comm := BuildCommunitySummaries(docs, edges, maxCommunities)
	if len(comm) == 0 {
		return nil
	}
	qToks := pageIndexTokens(question)
	type scored struct {
		n  SummaryNode
		sc float64
	}
	var maps []scored
	for _, n := range comm {
		blob := strings.ToLower(n.Text)
		var sc float64
		for _, t := range qToks {
			if strings.Contains(blob, t) {
				sc++
			}
		}
		// Prefer communities that mention more distinct query tokens (quality).
		if sc == 0 {
			sc = 0.1
		} else {
			sc = sc + 0.05*float64(len(n.Text)/200)
		}
		n.Kind = "graphrag_map"
		n.ID = "graphrag-map-" + n.ID
		maps = append(maps, scored{n: n, sc: sc})
	}
	sort.Slice(maps, func(i, j int) bool { return maps[i].sc > maps[j].sc })
	if len(maps) > maxCommunities {
		maps = maps[:maxCommunities]
	}
	var out []SummaryNode
	var reduceParts []string
	for _, m := range maps {
		out = append(out, m.n)
		snip := m.n.Text
		if len(snip) > 240 {
			snip = snip[:240]
		}
		reduceParts = append(reduceParts, snip)
	}
	if len(reduceParts) == 0 {
		return out
	}
	reduceText := fmt.Sprintf("reduce over %d communities: %s", len(reduceParts), strings.Join(reduceParts, " | "))
	// Optional abstractive reduce (fail-closed to extractive).
	if GraphRAGLLMEnabled() && llm != nil {
		sys := `Summarize the community map notes into one tight multi-hop answer draft.
Keep concrete names, numbers, and IDs. Max 6 sentences. No markdown.`
		user := "Question: " + question + "\n\nMap notes:\n" + strings.Join(reduceParts, "\n---\n")
		if raw, err := llm(context.Background(), sys, user); err == nil {
			raw = strings.TrimSpace(raw)
			if len(raw) > 40 {
				reduceText = raw
				if len(reduceText) > 2000 {
					reduceText = reduceText[:2000]
				}
			}
		}
	}
	out = append(out, SummaryNode{
		ID: "graphrag-reduce", Kind: "graphrag_reduce", Text: reduceText,
	})
	return out
}

// SeedPhrasePassageEdgesFromClaims builds HippoRAG-style phrase↔doc bipartite
// edges from admitted claims (GAP-IR-HIPPORAG-OPENIE).
func (s *Store) SeedPhrasePassageEdgesFromClaims() int {
	if s == nil {
		return 0
	}
	claims := s.CurrentClaims(time.Time{}, true)
	if len(claims) == 0 {
		return 0
	}
	// phrase node "phrase:token" — must match BuildBipartitePhraseEdges / PhraseSeedScores.
	weights := s.WeightedEdges()
	if weights == nil {
		weights = map[string]float64{}
	}
	added := 0
	for _, c := range claims {
		// Align with bipartite builder: subject + object (+ span tokens). Skip raw predicate.
		phrases := []string{
			strings.ToLower(c.Subject),
			strings.ToLower(c.Object),
		}
		// Also seed content tokens from evidence span (density quality).
		if st := strings.ToLower(c.SpanText); st != "" {
			for _, t := range pageIndexTokens(st) {
				if len(t) >= 4 {
					phrases = append(phrases, t)
				}
			}
		}
		for _, ph := range phrases {
			ph = strings.TrimSpace(ph)
			if len(ph) < 3 {
				continue
			}
			// Same prefix as BuildBipartitePhraseEdges so PhraseSeedScores/PPR match.
			node := "phrase:" + strings.ReplaceAll(ph, " ", "_")
			if len(node) > 48 {
				node = node[:48]
			}
			for _, doc := range c.DocumentIDs {
				if doc == "" {
					continue
				}
				k1 := edgeKey(node, doc)
				k2 := edgeKey(doc, node)
				if _, ok := weights[k1]; !ok {
					weights[k1] = 1.2
					added++
				}
				if _, ok := weights[k2]; !ok {
					weights[k2] = 1.2
				}
			}
		}
	}
	if added == 0 {
		return 0
	}
	_ = s.SetWeightedEdges(weights)
	return added
}
