package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/chunking"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
)

// ReportVersion stamps the report schema.
const ReportVersion = 1

// FixtureStats describes the golden set a report was measured on.
type FixtureStats struct {
	Documents    int `json:"documents"`
	Queries      int `json:"queries"`
	SourceTokens int `json:"source_tokens"`
}

// IntegrityViolation records one failed citation check (empty when clean).
type IntegrityViolation struct {
	ChunkID string `json:"chunk_id"`
	Reason  string `json:"reason"`
}

// StrategyResult is one strategy's full diagnostic scorecard.
type StrategyResult struct {
	Strategy   string         `json:"strategy"`
	BlindLabel string         `json:"blind_label,omitempty"`
	Policy     map[string]any `json:"policy,omitempty"`

	// Retrieval quality (diagnostic, never official).
	HitRateAtK    float64 `json:"hit_rate_at_k"`
	DocHitRateAtK float64 `json:"doc_hit_rate_at_k"`
	MRR           float64 `json:"mrr"`
	NDCGAtK       float64 `json:"ndcg_at_k"`

	// Citation integrity over every retrieved chunk observation.
	CitationIntegrity   float64              `json:"citation_integrity"`
	IntegrityViolations []IntegrityViolation `json:"integrity_violations,omitempty"`

	// Cost proxies: retrieval-only harness makes no paid calls, so cost is
	// index volume (embedding/serving footprint) rather than spend.
	ChunksIndexed   int     `json:"chunks_indexed"`
	ChunksTotal     int     `json:"chunks_total_including_parents"`
	IndexTokens     int     `json:"index_tokens"`
	MeanChunkTokens float64 `json:"mean_chunk_tokens"`
	ExpansionRatio  float64 `json:"expansion_ratio_vs_source"`
	SourceTokens    int     `json:"source_tokens"`

	// Latency, milliseconds.
	ChunkMS    float64 `json:"chunk_ms"`
	IngestMS   float64 `json:"ingest_ms"`
	QueryP50MS float64 `json:"query_p50_ms"`
	QueryP95MS float64 `json:"query_p95_ms"`
}

// Report is the chunk-eval output contract.
type Report struct {
	Tool          string           `json:"tool"`
	ReportVersion int              `json:"report_version"`
	GeneratedAt   time.Time        `json:"generated_at"`
	ScoreClass    string           `json:"score_class"`
	Official      bool             `json:"official"`
	ERBBlind      bool             `json:"erb_blind"`
	OfficialEnv   bool             `json:"official_eval_env"`
	Note          string           `json:"note"`
	TopK          int              `json:"top_k"`
	Fixtures      FixtureStats     `json:"fixtures"`
	Strategies    []StrategyResult `json:"strategies"`
}

const diagnosticNote = "Retrieval-only diagnostics from chunk-eval golden fixtures. " +
	"Official ERB scores come exclusively from the pinned EnterpriseRAG-Bench harness " +
	"with OUROBOROS_ERB_OFFICIAL_JUDGE; this tool can never emit them."

// Run benchmarks every policy against the golden fixtures. Deterministic for
// a fixed input + policy set (no LLM calls, no network).
func Run(docs []chunking.SourceDocument, queries []FixtureQuery, policies []chunking.Policy, topK int, blind bool) (*Report, error) {
	if topK <= 0 {
		topK = 8
	}
	sourceTokens := 0
	sourceByID := map[string]string{}
	for _, d := range docs {
		src := d.Source()
		sourceTokens += chunking.CountTokens(src)
		sourceByID[d.ID] = src
	}
	report := &Report{
		Tool:          "chunk-eval",
		ReportVersion: ReportVersion,
		GeneratedAt:   time.Now().UTC(),
		ScoreClass:    "diagnostic",
		Official:      false,
		ERBBlind:      blind,
		OfficialEnv:   envTruthy("OUROBOROS_ERB_OFFICIAL") || envTruthy("OUROBOROS_ERB_OFFICIAL_JUDGE"),
		Note:          diagnosticNote,
		TopK:          topK,
		Fixtures: FixtureStats{
			Documents:    len(docs),
			Queries:      len(queries),
			SourceTokens: sourceTokens,
		},
	}
	for i, p := range policies {
		res, err := runStrategy(docs, queries, sourceByID, p, topK, sourceTokens)
		if err != nil {
			return nil, fmt.Errorf("strategy %s: %w", p.Strategy, err)
		}
		if blind {
			res.BlindLabel = fmt.Sprintf("arm_%c", 'a'+i)
			res.Policy = nil // fingerprint lives only in the blind key file
		} else {
			res.Policy = p.Fingerprint()
		}
		report.Strategies = append(report.Strategies, *res)
	}
	return report, nil
}

// runStrategy executes chunk -> ingest -> index -> retrieve -> score for one policy.
func runStrategy(docs []chunking.SourceDocument, queries []FixtureQuery, sourceByID map[string]string,
	p chunking.Policy, topK, sourceTokens int) (*StrategyResult, error) {
	ctx := context.Background()

	t0 := time.Now()
	receipts, err := chunking.Chunk(docs, p)
	if err != nil {
		return nil, err
	}
	chunkMS := float64(time.Since(t0).Microseconds()) / 1000.0

	receiptByID := map[string]chunking.Receipt{}
	var writes []hosted.ChunkWrite
	indexTokens := 0
	for _, r := range receipts {
		receiptByID[r.ChunkID] = r
		if r.Role != chunking.RoleChunk {
			continue // parents stay in the receipt ledger, not the index
		}
		writes = append(writes, hosted.ChunkWrite{
			DocumentID: r.DocumentID,
			ChunkID:    r.ChunkID,
			Text:       r.Text,
		})
		indexTokens += r.Tokens
	}
	if len(writes) == 0 {
		return nil, fmt.Errorf("no indexable chunks")
	}

	// Existing ingestion contract: ChunkStore.UpsertChunks.
	t1 := time.Now()
	store := hosted.NewMemoryChunkStore()
	if err := store.EnsureSchema(ctx); err != nil {
		return nil, err
	}
	brainID := "chunk-eval-" + string(p.Strategy)
	if err := store.UpsertChunks(ctx, brainID, writes); err != nil {
		return nil, err
	}
	ingestMS := float64(time.Since(t1).Microseconds()) / 1000.0

	// Existing retrieval contract: HotLex BM25 projection over the same writes.
	hot := hosted.ProjectChunks(brainID, writes)

	var firstRanks []int
	hitCount, docHitCount := 0, 0
	ndcgSum := 0.0
	latencies := make([]float64, 0, len(queries))
	seenChunks := map[string]hosted.Hit{}

	for _, q := range queries {
		t2 := time.Now()
		hits := hot.Search(q.Question, topK)
		latencies = append(latencies, float64(time.Since(t2).Microseconds())/1000.0)

		goldDocs := map[string][]string{}
		for _, g := range q.Gold {
			goldDocs[g.DocumentID] = append(goldDocs[g.DocumentID], g.Needle)
		}
		isGold := func(h hosted.Hit) bool {
			for _, needle := range goldDocs[h.DSID] {
				if strings.Contains(h.Text, needle) {
					return true
				}
			}
			return false
		}
		// Relevant chunk count over the whole index: overlap can legitimately
		// duplicate a needle across adjacent chunks, and NDCG's ideal must
		// account for that rather than assume one relevant chunk per gold.
		relevantCount := 0
		for _, w := range writes {
			for _, needle := range goldDocs[w.DocumentID] {
				if strings.Contains(w.Text, needle) {
					relevantCount++
					break
				}
			}
		}
		rel := make([]int, 0, len(hits))
		firstRank := 0
		docHit := false
		for rank, h := range hits {
			seenChunks[h.ChunkID] = h
			gold := isGold(h)
			if gold {
				rel = append(rel, 1)
				if firstRank == 0 {
					firstRank = rank + 1
				}
			} else {
				rel = append(rel, 0)
			}
			if _, ok := goldDocs[h.DSID]; ok {
				docHit = true
			}
		}
		firstRanks = append(firstRanks, firstRank)
		if hitAtK(firstRanks[len(firstRanks)-1:], topK) {
			hitCount++
		}
		if docHit {
			docHitCount++
		}
		ndcgSum += ndcgAtK(rel, relevantCount, topK)
	}

	integrity, violations := verifyCitationIntegrity(seenChunks, receiptByID, sourceByID)

	meanChunkTokens := 0.0
	if len(writes) > 0 {
		meanChunkTokens = float64(indexTokens) / float64(len(writes))
	}
	expansion := 0.0
	if sourceTokens > 0 {
		expansion = float64(indexTokens) / float64(sourceTokens)
	}
	return &StrategyResult{
		Strategy:            string(p.Strategy),
		HitRateAtK:          float64(hitCount) / float64(len(queries)),
		DocHitRateAtK:       float64(docHitCount) / float64(len(queries)),
		MRR:                 meanReciprocalRank(firstRanks),
		NDCGAtK:             ndcgSum / float64(len(queries)),
		CitationIntegrity:   integrity,
		IntegrityViolations: violations,
		ChunksIndexed:       len(writes),
		ChunksTotal:         len(receipts),
		IndexTokens:         indexTokens,
		MeanChunkTokens:     meanChunkTokens,
		ExpansionRatio:      expansion,
		SourceTokens:        sourceTokens,
		ChunkMS:             chunkMS,
		IngestMS:            ingestMS,
		QueryP50MS:          percentile(latencies, 50),
		QueryP95MS:          percentile(latencies, 95),
	}, nil
}

// verifyCitationIntegrity checks every retrieved chunk against its receipt:
// offsets slice the exact source text, the hash matches, the store round-trip
// preserved the text, and parent links resolve and contain the child.
func verifyCitationIntegrity(seen map[string]hosted.Hit, receipts map[string]chunking.Receipt,
	sourceByID map[string]string) (float64, []IntegrityViolation) {
	if len(seen) == 0 {
		return 1, nil
	}
	var violations []IntegrityViolation
	passed := 0
	for chunkID, hit := range seen {
		reason := verifyOne(chunkID, hit, receipts, sourceByID)
		if reason == "" {
			passed++
			continue
		}
		if len(violations) < 50 {
			violations = append(violations, IntegrityViolation{ChunkID: chunkID, Reason: reason})
		}
	}
	return float64(passed) / float64(len(seen)), violations
}

func verifyOne(chunkID string, hit hosted.Hit, receipts map[string]chunking.Receipt, sourceByID map[string]string) string {
	r, ok := receipts[chunkID]
	if !ok {
		return "chunk_id does not resolve to a receipt"
	}
	src, ok := sourceByID[r.DocumentID]
	if !ok {
		return "document_id does not resolve to a source"
	}
	if r.Start < 0 || r.End > len(src) || r.Start >= r.End {
		return "offsets outside source bounds"
	} else if src[r.Start:r.End] != r.Text {
		return "offsets do not slice the receipt text"
	}
	sum := sha256.Sum256([]byte(r.Text))
	if hex.EncodeToString(sum[:]) != r.SHA256 {
		return "sha256 mismatch"
	}
	if hit.Text != r.Text {
		return "store round-trip changed the text"
	}
	if r.ParentID != "" {
		parent, ok := receipts[r.ParentID]
		if !ok {
			return "parent_id does not resolve"
		}
		if parent.DocumentID != r.DocumentID {
			return "parent belongs to another document"
		}
		if r.Start < parent.Start || r.End > parent.End {
			return "child escapes parent bounds"
		}
	}
	return ""
}

func envTruthy(k string) bool {
	v := strings.TrimSpace(os.Getenv(k))
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
