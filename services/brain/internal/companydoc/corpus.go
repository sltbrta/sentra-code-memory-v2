package companydoc

import (
	"context"
	"fmt"
	"sync"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/dense"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/gardener"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ontology"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/rerank"
)

// LiveCorpus is an in-process product company-doc generation.
type LiveCorpus struct {
	mu           sync.RWMutex
	SourceID     string
	GenerationID string
	Docs         map[string]string
	Titles       map[string]string
	Store        *ontology.GenerationStore
	Dense        *dense.MemoryStore
	Queue        *gardener.MemoryQueue
	dim          int
}

// OpenLive builds a LiveCorpus from a validated Batch and runs OnPublished.
func OpenLive(ctx context.Context, batch Batch) (*LiveCorpus, error) {
	if err := ValidateBatch(batch); err != nil {
		return nil, err
	}
	docs := TextMap(batch.Documents)
	titles := make(map[string]string, len(batch.Documents))
	for _, d := range batch.Documents {
		titles[d.ID] = d.Title
	}
	store := ontology.NewGenerationStore()
	hopper := ontology.StoreHopper{Store: store}
	q := &gardener.MemoryQueue{}
	enr := &gardener.GenerationEnricher{
		Queue:     q,
		Budget:    gardener.DefaultBudget(),
		GraphSink: hopper,
	}
	if _, err := enr.OnPublished(ctx, batch.GenerationID, docs); err != nil {
		return nil, err
	}
	sched := &gardener.Scheduler{
		Queue:   q,
		Workers: gardener.DefaultWorkers(),
		Budget:  gardener.DefaultBudget(),
	}
	_, _ = sched.RunWave(ctx, "companydoc-open")

	ds := dense.NewMemoryStore()
	dim := 64
	for id, text := range docs {
		_ = ds.Upsert(id, BagOfWords(text, dim))
	}
	return &LiveCorpus{
		SourceID:     batch.SourceID,
		GenerationID: batch.GenerationID,
		Docs:         docs,
		Titles:       titles,
		Store:        store,
		Dense:        ds,
		Queue:        q,
		dim:          dim,
	}, nil
}

// Hopper returns ontology graph hopper.
func (c *LiveCorpus) Hopper() ontology.StoreHopper {
	if c == nil {
		return ontology.StoreHopper{}
	}
	return ontology.StoreHopper{Store: c.Store}
}

// Retrieve returns ranked document ids (lexical + dense RRF + graph + CE).
func (c *LiveCorpus) Retrieve(ctx context.Context, question string, topK int) ([]string, map[string]any) {
	diag := map[string]any{"source": "companydoc_live"}
	if c == nil || len(c.Docs) == 0 {
		return nil, diag
	}
	if topK <= 0 {
		topK = 8
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	type scored struct {
		id string
		v  float64
	}
	var lex []scored
	qtok := bagTokens(question)
	for id, text := range c.Docs {
		lex = append(lex, scored{id, tokenOverlap(qtok, bagTokens(text))})
	}
	for i := 0; i < len(lex); i++ {
		for j := i + 1; j < len(lex); j++ {
			if lex[j].v > lex[i].v {
				lex[i], lex[j] = lex[j], lex[i]
			}
		}
	}
	lexIDs := make([]string, 0, topK*2)
	for _, s := range lex {
		if s.v <= 0 {
			continue
		}
		lexIDs = append(lexIDs, s.id)
		if len(lexIDs) >= topK*2 {
			break
		}
	}
	diag["lexical"] = lexIDs

	var denseIDs []string
	if c.Dense != nil {
		hits := c.Dense.Search(BagOfWords(question, c.dim), topK*2)
		for _, h := range hits {
			denseIDs = append(denseIDs, h.DocumentID)
		}
	}
	diag["dense"] = denseIDs

	fused := rrfFuseLists(lexIDs, denseIDs, 60, topK*2)
	seeds := fused
	if len(seeds) > 5 {
		seeds = seeds[:5]
	}
	neighbors := c.Hopper().Expand(c.GenerationID, seeds, 12)
	diag["graph"] = neighbors
	for _, n := range neighbors {
		if !containsStr(fused, n) {
			fused = append(fused, n)
		}
	}

	bodies := make([]string, len(fused))
	for i, id := range fused {
		bodies[i] = c.Docs[id]
	}
	var lr rerank.LexicalReranker
	ranked, _ := lr.Rerank(ctx, question, bodies, topK)
	out := make([]string, 0, topK)
	if len(ranked) == 0 {
		if len(fused) > topK {
			return fused[:topK], diag
		}
		return fused, diag
	}
	for _, r := range ranked {
		if r.Index >= 0 && r.Index < len(fused) {
			out = append(out, fused[r.Index])
		}
		if len(out) >= topK {
			break
		}
	}
	diag["final"] = out
	return out, diag
}

// Answer synthesizes an extractive answer from retrieved docs.
func (c *LiveCorpus) Answer(ctx context.Context, question string, topK int) (string, []string, map[string]any) {
	cited, diag := c.Retrieve(ctx, question, topK)
	if len(cited) == 0 {
		return "No supporting documents found for the question.", nil, diag
	}
	var parts []string
	for _, id := range cited {
		snip := c.Docs[id]
		if len(snip) > 400 {
			snip = snip[:400]
		}
		if title := c.Titles[id]; title != "" {
			parts = append(parts, fmt.Sprintf("[%s] %s — %s", id, title, snip))
		} else {
			parts = append(parts, fmt.Sprintf("[%s] %s", id, snip))
		}
	}
	ans := "Based on product brain evidence:\n- " + parts[0]
	for i := 1; i < len(parts); i++ {
		ans += "\n- " + parts[i]
	}
	return ans, cited, diag
}

func rrfFuseLists(a, b []string, k, topN int) []string {
	if k <= 0 {
		k = 60
	}
	scores := map[string]float64{}
	add := func(list []string) {
		for i, id := range list {
			scores[id] += 1.0 / float64(k+i+1)
		}
	}
	add(a)
	add(b)
	type p struct {
		id string
		sc float64
	}
	var arr []p
	for id, sc := range scores {
		arr = append(arr, p{id, sc})
	}
	for i := 0; i < len(arr); i++ {
		for j := i + 1; j < len(arr); j++ {
			if arr[j].sc > arr[i].sc {
				arr[i], arr[j] = arr[j], arr[i]
			}
		}
	}
	out := make([]string, 0, topN)
	for _, x := range arr {
		out = append(out, x.id)
		if len(out) >= topN {
			break
		}
	}
	return out
}

func containsStr(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
