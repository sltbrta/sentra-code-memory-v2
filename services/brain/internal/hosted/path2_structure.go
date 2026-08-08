package hosted

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// mergePath2StructureDiag folds path2 expand diagnostics into retrieve diags.
// path2_unavailable must not clobber structureExpandPassages' pool_virtual on
// structure_mode; the path2-specific mode is always stored as path2_structure_mode.
// structure_mode is stamped path2_sql when path2 returned docs, or when path2
// reported path2_sql (tables reachable — even zero hits).
func mergePath2StructureDiag(diag, p2diag map[string]any, p2docCount int) {
	if diag == nil || p2diag == nil {
		return
	}
	if mode, ok := p2diag["structure_mode"]; ok {
		diag["path2_structure_mode"] = mode
	}
	for k, v := range p2diag {
		if k == "structure_mode" {
			if s, _ := v.(string); s == "path2_unavailable" {
				continue
			}
		}
		diag[k] = v
	}
	if p2docCount > 0 {
		diag["structure_mode"] = "path2_sql"
	}
}

// path2StructureExpand queries path2_entities / path2_facts / path2_relationships
// for document IDs related to question tokens and seed dsids.
// structure_mode is path2_sql when any arm succeeds without SQL error (even if empty);
// path2_unavailable only when every arm errors (or db is nil).
// Fail-soft: never propagates SQL errors to the caller.
func path2StructureExpand(
	ctx context.Context,
	db *sql.DB,
	brainID, question string,
	seedDSIDs []string,
	maxN int,
) (docIDs []string, diag map[string]any) {
	diag = map[string]any{
		"structure_mode":           "path2_unavailable",
		"path2_entities_hits":      0,
		"path2_facts_hits":         0,
		"path2_relationships_hits": 0,
		"path2_structure_docs":     0,
	}
	if db == nil {
		return nil, diag
	}
	if maxN <= 0 {
		maxN = 12
	}
	toks := contentTokens(question)
	if len(toks) > 6 {
		toks = toks[:6]
	}
	// Lower-case tokens for equality matches.
	lowToks := make([]string, 0, len(toks))
	for _, t := range toks {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" {
			lowToks = append(lowToks, t)
		}
	}

	// Prefer seed-dsid relationship expand FIRST (cheap when RRF/HotLex already
	// returned docs). Entity name SQL was timing out every arm under load and
	// starving multi-doc second gold (cite@90).
	total, budgetSource, nearDeadline := path2StructureBudget(ctx)
	wallCtx, wallCancel := withTimeout(ctx, total)
	defer wallCancel()
	diag["path2_structure_total_ms"] = total.Milliseconds()
	diag["path2_structure_budget_source"] = budgetSource
	diag["path2_structure_near_deadline"] = nearDeadline
	if err := ctx.Err(); err != nil {
		diag["path2_structure_context_status"] = retrievalContextStatus(err)
		return nil, diag
	}
	// Seed-rels get most of the budget; entity is optional best-effort tail.
	relBudget := total * 55 / 100
	if relBudget < 2*time.Second {
		relBudget = 2 * time.Second
	}
	if relBudget > 5*time.Second {
		relBudget = 5 * time.Second
	}
	if relBudget > total {
		relBudget = total
	}
	eBudget := total - relBudget
	if eBudget < 1500*time.Millisecond {
		eBudget = 1500 * time.Millisecond
	}
	if eBudget > 4*time.Second {
		eBudget = 4 * time.Second
	}
	if eBudget > total {
		eBudget = total
	}
	diag["path2_entity_budget_ms"] = eBudget.Milliseconds()
	diag["path2_rel_budget_ms"] = relBudget.Milliseconds()

	relSeeds := seedDSIDs
	var factDocs, relDocs, entityDocs []string
	var fErr, rErr, eErr error
	var armWG sync.WaitGroup

	// 1) Relationships from seed dsids (primary multi-doc hop).
	if len(relSeeds) > 0 {
		armWG.Add(1)
		go func() {
			defer armWG.Done()
			rctx, rcancel := withTimeout(wallCtx, relBudget)
			defer rcancel()
			relDocs, rErr = path2QueryRelationships(rctx, db, brainID, relSeeds, maxN)
		}()
	} else {
		diag["path2_relationships_skipped"] = "no_rel_seeds"
	}

	// 2) Entity + facts in parallel (secondary; fail-soft).
	armWG.Add(1)
	go func() {
		defer armWG.Done()
		ectx, ecancel := withTimeout(wallCtx, eBudget)
		defer ecancel()
		entityDocs, eErr = path2QueryEntities(ectx, db, brainID, lowToks, maxN)
		if eErr != nil || len(entityDocs) == 0 {
			return
		}
		// Only spend leftover on facts if entities returned something.
		fctx, fcancel := withTimeout(wallCtx, eBudget/2)
		defer fcancel()
		factDocs, fErr = path2QueryFacts(fctx, db, brainID, lowToks, maxN)
	}()
	armWG.Wait()
	if eErr != nil {
		diag["path2_entities_error"] = truncateErr(eErr, 160)
	}
	if fErr != nil {
		diag["path2_facts_error"] = truncateErr(fErr, 160)
	}
	if rErr != nil {
		diag["path2_relationships_error"] = truncateErr(rErr, 160)
	}
	// If entity returned seeds and relSeeds was empty, expand from entities.
	if len(relSeeds) == 0 && len(entityDocs) > 0 {
		relSeeds = entityDocs
		if len(relSeeds) > 8 {
			relSeeds = relSeeds[:8]
		}
		rctx, rcancel := withTimeout(wallCtx, relBudget)
		relDocs2, rErr2 := path2QueryRelationships(rctx, db, brainID, relSeeds, maxN)
		rcancel()
		if rErr2 == nil && len(relDocs2) > 0 {
			relDocs = relDocs2
			rErr = nil
		} else if rErr2 != nil {
			rErr = rErr2
			diag["path2_relationships_error"] = truncateErr(rErr2, 160)
		}
	}

	out, mergeDiag := path2StructureFromRows(entityDocs, factDocs, relDocs, maxN)
	for k, v := range mergeDiag {
		diag[k] = v
	}
	if len(out) > 0 {
		diag["structure_mode"] = "path2_sql"
		return out, diag
	}
	// Empty but an arm completed without error → path2 reachable (not unavailable).
	if (eErr == nil && len(lowToks) > 0) || (rErr == nil && len(seedDSIDs) > 0) || fErr == nil {
		diag["structure_mode"] = "path2_sql"
		return out, diag
	}
	// All arms errored or unattempted with nothing — unavailable.
	return nil, diag
}

// path2StructureBudget derives the one shared wall inherited by every path2
// arm. A near caller deadline remains near; it must never be diagnosed as the
// 8s default merely because less than 50ms remains.
func path2StructureBudget(ctx context.Context) (time.Duration, string, bool) {
	if dl, ok := ctx.Deadline(); ok {
		d := time.Until(dl)
		if d < 0 {
			d = 0
		}
		return d, "caller_deadline", d <= 50*time.Millisecond
	}
	return 8 * time.Second, "default", false
}

// path2StructureArmBudgets splits total: ~45% entity, rest facts∥rels tail.
// Entity must not consume 100% or every arm reports deadline exceeded.
func path2StructureArmBudgets(total time.Duration) (entity, tail time.Duration) {
	if total <= 0 {
		total = 8 * time.Second
	}
	entity = total * 45 / 100
	if entity < 1500*time.Millisecond {
		entity = 1500 * time.Millisecond
	}
	if entity > 6*time.Second {
		entity = 6 * time.Second
	}
	if entity >= total {
		entity = total * 45 / 100
	}
	tail = total - entity
	if tail < 1500*time.Millisecond {
		tail = 1500 * time.Millisecond
	}
	return entity, tail
}

// path2StructureFromRows merges entity/fact/rel doc ID lists (testable pure helper).
func path2StructureFromRows(entityDocs, factDocs, relDocs []string, maxN int) ([]string, map[string]any) {
	diag := map[string]any{
		"path2_entities_hits":      len(entityDocs),
		"path2_facts_hits":         len(factDocs),
		"path2_relationships_hits": len(relDocs),
		"structure_mode":           "path2_sql",
	}
	seen := map[string]struct{}{}
	var out []string
	add := func(ids []string) {
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
			if maxN > 0 && len(out) >= maxN {
				return
			}
		}
	}
	add(entityDocs)
	if maxN <= 0 || len(out) < maxN {
		add(factDocs)
	}
	if maxN <= 0 || len(out) < maxN {
		add(relDocs)
	}
	if maxN > 0 && len(out) > maxN {
		out = out[:maxN]
	}
	diag["path2_structure_docs"] = len(out)
	return out, diag
}

// path2QueryEntities tries SMF entity shapes; first success wins.
// Live full-bench-v2: brain_id, slug, display_name, source_dsids text[], tsv.
func path2QueryEntities(ctx context.Context, db *sql.DB, brainID string, tokens []string, maxN int) ([]string, error) {
	if len(tokens) == 0 {
		return nil, nil
	}
	// Prefer tokens with length ≥3 to avoid stopword slug hits (we/i/that).
	var toks []string
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if len(t) >= 3 {
			toks = append(toks, t)
		}
	}
	if len(toks) == 0 {
		toks = tokens
	}
	shapes := []func() ([]string, error){
		// SMF full-bench: display_name/slug exact + unnest source_dsids.
		func() ([]string, error) {
			q := `
SELECT DISTINCT u.dsid
FROM path2_entities e
CROSS JOIN LATERAL unnest(e.source_dsids) AS u(dsid)
WHERE e.brain_id = $1
  AND (lower(e.display_name) = ANY($2::text[]) OR lower(e.slug) = ANY($2::text[]))
  AND u.dsid <> ''
LIMIT $3`
			return scanStringCol(ctx, db, q, brainID, toks, maxN)
		},
		// SMF: plainto_tsquery on entity tsv (GIN).
		func() ([]string, error) {
			bag := strings.Join(toks, " ")
			q := `
SELECT DISTINCT u.dsid
FROM path2_entities e
CROSS JOIN LATERAL unnest(e.source_dsids) AS u(dsid)
WHERE e.brain_id = $1
  AND e.tsv @@ plainto_tsquery('english', $2)
  AND u.dsid <> ''
LIMIT $3`
			return scanStringCol(ctx, db, q, brainID, bag, maxN)
		},
		// SMF: ILIKE display_name/slug (substring entity names).
		func() ([]string, error) {
			var parts []string
			args := []any{brainID}
			for _, t := range toks {
				args = append(args, "%"+t+"%")
				n := len(args)
				parts = append(parts, fmt.Sprintf("(e.display_name ILIKE $%d OR e.slug ILIKE $%d)", n, n))
			}
			args = append(args, maxN)
			q := fmt.Sprintf(`
SELECT DISTINCT u.dsid
FROM path2_entities e
CROSS JOIN LATERAL unnest(e.source_dsids) AS u(dsid)
WHERE e.brain_id = $1 AND (%s) AND u.dsid <> ''
LIMIT $%d`, strings.Join(parts, " OR "), len(args))
			return scanStringColArgs(ctx, db, q, args...)
		},
		// Legacy: name + source_dsids.
		func() ([]string, error) {
			q := `
SELECT DISTINCT unnest(source_dsids)::text AS dsid
FROM path2_entities
WHERE brain_id = $1 AND lower(name) = ANY($2::text[])
LIMIT $3`
			return scanStringCol(ctx, db, q, brainID, toks, maxN)
		},
	}
	// First success wins. On deadline/cancel do NOT cascade into ILIKE full-scans
	// (full500: entity arm timed out then burned the rest of the budget on slower shapes).
	return path2TryShapes(ctx, shapes)
}

// path2TryShapes runs SQL shape fallbacks; stops on deadline so slow tails never run.
func path2TryShapes(ctx context.Context, shapes []func() ([]string, error)) ([]string, error) {
	var lastErr error
	for i, shape := range shapes {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		ids, err := shape()
		if err == nil {
			return ids, nil
		}
		lastErr = err
		if ctx.Err() != nil || isCtxDeadline(err) {
			return nil, lastErr
		}
		// Skip heavy ILIKE/legacy shapes when little wall left (<2s).
		if rem, ok := ctxRemaining(ctx); ok && rem < 2*time.Second && i >= 1 {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

func isCtxDeadline(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		strings.Contains(err.Error(), "deadline exceeded") || strings.Contains(err.Error(), "context canceled")
}

func ctxRemaining(ctx context.Context) (time.Duration, bool) {
	dl, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	return time.Until(dl), true
}

// path2QueryFacts matches question tokens against fact rows and resolves document
// IDs via subject_entity → path2_entities.slug → source_dsids (live SMF shape).
func path2QueryFacts(ctx context.Context, db *sql.DB, brainID string, tokens []string, maxN int) ([]string, error) {
	if len(tokens) == 0 {
		return nil, nil
	}
	var toks []string
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if len(t) >= 3 {
			toks = append(toks, t)
		}
	}
	if len(toks) == 0 {
		toks = tokens
	}
	shapes := []func() ([]string, error){
		// Live SMF: exact subject_entity slug match (indexed via join path; no full-scan ILIKE).
		func() ([]string, error) {
			q := `
SELECT DISTINCT u.dsid
FROM path2_facts f
JOIN path2_entities e ON e.brain_id = f.brain_id AND e.slug = f.subject_entity
CROSS JOIN LATERAL unnest(e.source_dsids) AS u(dsid)
WHERE f.brain_id = $1
  AND f.subject_entity = ANY($2::text[])
  AND u.dsid <> ''
LIMIT $3`
			return scanStringCol(ctx, db, q, brainID, toks, maxN)
		},
		// Live SMF: attribute_name exact (e.g. "requires").
		func() ([]string, error) {
			q := `
SELECT DISTINCT u.dsid
FROM path2_facts f
JOIN path2_entities e ON e.brain_id = f.brain_id AND e.slug = f.subject_entity
CROSS JOIN LATERAL unnest(e.source_dsids) AS u(dsid)
WHERE f.brain_id = $1
  AND lower(f.attribute_name) = ANY($2::text[])
  AND u.dsid <> ''
LIMIT $3`
			return scanStringCol(ctx, db, q, brainID, toks, maxN)
		},
		// Legacy document_id column (older product shapes).
		func() ([]string, error) {
			var parts []string
			args := []any{brainID}
			for _, t := range toks {
				args = append(args, "%"+t+"%")
				n := len(args)
				parts = append(parts, fmt.Sprintf(
					"(subject ILIKE $%d OR predicate ILIKE $%d OR object ILIKE $%d OR fact_text ILIKE $%d)",
					n, n, n, n))
			}
			args = append(args, maxN)
			q := fmt.Sprintf(`
SELECT DISTINCT document_id::text
FROM path2_facts
WHERE brain_id = $1 AND (%s)
LIMIT $%d`, strings.Join(parts, " OR "), len(args))
			return scanStringColArgs(ctx, db, q, args...)
		},
	}
	return path2TryShapes(ctx, shapes)
}

// path2QueryRelationships expands seed dsids via undirected edges.
// Live SMF: source_slug, target_slug, relation, source_dsid (document evidence).
func path2QueryRelationships(ctx context.Context, db *sql.DB, brainID string, seeds []string, maxN int) ([]string, error) {
	if len(seeds) == 0 {
		return nil, nil
	}
	if len(seeds) > 12 {
		seeds = seeds[:12]
	}
	shapes := []func() ([]string, error){
		// Fast path: one-hop peers via source_dsid equality on edges (indexed).
		// Avoids heavy slug CTE that timed out under burst.
		func() ([]string, error) {
			q := `
SELECT DISTINCT peer.dsid
FROM (
  SELECT target_slug AS slug FROM path2_relationships
  WHERE brain_id = $1 AND source_dsid = ANY($2::text[]) AND source_dsid <> ''
  UNION
  SELECT source_slug FROM path2_relationships
  WHERE brain_id = $1 AND source_dsid = ANY($2::text[]) AND source_dsid <> ''
) s
JOIN LATERAL (
  SELECT r.source_dsid AS dsid FROM path2_relationships r
  WHERE r.brain_id = $1 AND r.source_dsid <> '' AND r.source_dsid <> ALL($2::text[])
    AND (r.source_slug = s.slug OR r.target_slug = s.slug)
  LIMIT 8
) peer ON true
LIMIT $3`
			return scanStringCol(ctx, db, q, brainID, seeds, maxN)
		},
		// Live SMF: seed dsids → related source_dsid peers (slug neighborhood CTE).
		func() ([]string, error) {
			q := `
WITH seed_slugs AS (
  SELECT DISTINCT source_slug AS slug FROM path2_relationships
  WHERE brain_id = $1 AND source_dsid = ANY($2::text[]) AND source_dsid <> ''
  UNION
  SELECT DISTINCT target_slug FROM path2_relationships
  WHERE brain_id = $1 AND source_dsid = ANY($2::text[]) AND source_dsid <> ''
)
SELECT DISTINCT r.source_dsid::text
FROM path2_relationships r
WHERE r.brain_id = $1
  AND r.source_dsid <> ''
  AND (r.source_slug IN (SELECT slug FROM seed_slugs) OR r.target_slug IN (SELECT slug FROM seed_slugs))
LIMIT $3`
			return scanStringCol(ctx, db, q, brainID, seeds, maxN)
		},
		// Live SMF: seeds treated as slugs (entity names from prior hop).
		func() ([]string, error) {
			q := `
SELECT DISTINCT source_dsid::text FROM path2_relationships
WHERE brain_id = $1 AND source_dsid <> ''
  AND (lower(source_slug) = ANY($2::text[]) OR lower(target_slug) = ANY($2::text[]))
LIMIT $3`
			low := make([]string, len(seeds))
			for i, s := range seeds {
				low[i] = strings.ToLower(s)
			}
			return scanStringCol(ctx, db, q, brainID, low, maxN)
		},
		// Legacy src/dst columns.
		func() ([]string, error) {
			q := `
SELECT DISTINCT dst::text FROM path2_relationships
WHERE brain_id = $1 AND src = ANY($2::text[])
UNION
SELECT DISTINCT src::text FROM path2_relationships
WHERE brain_id = $1 AND dst = ANY($2::text[])
LIMIT $3`
			return scanStringCol(ctx, db, q, brainID, seeds, maxN)
		},
	}
	ids, err := path2TryShapes(ctx, shapes)
	if err != nil {
		return nil, err
	}
	seedSet := map[string]struct{}{}
	for _, s := range seeds {
		seedSet[s] = struct{}{}
	}
	var out []string
	for _, id := range ids {
		if _, ok := seedSet[id]; ok {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// hydratePath2StructureDocs loads chunk passages for path2 structure-promoted dsids.
func hydratePath2StructureDocs(ctx context.Context, db *sql.DB, cfg Config, docs []string, chunksPerDoc int) []Passage {
	if db == nil || len(docs) == 0 {
		return nil
	}
	if chunksPerDoc < 1 {
		chunksPerDoc = 2
	}
	var out []Passage
	for _, dsid := range docs {
		hits, err := siblingChunks(ctx, db, cfg, dsid, chunksPerDoc)
		if err != nil || len(hits) == 0 {
			continue
		}
		for _, h := range hits {
			text := clipPassageText(h.Text, storagePassageChars(cfg.MaxPassageChars))
			out = append(out, Passage{
				DocumentID: dsid,
				Text:       text,
				Score:      0.35,
				ChunkID:    h.ChunkID,
				SourceURI:  h.SourceURI,
				Channel:    "path2_structure",
			})
		}
	}
	return out
}

func scanStringCol(ctx context.Context, db *sql.DB, query string, args ...any) ([]string, error) {
	return scanStringColArgs(ctx, db, query, args...)
}

func scanStringColArgs(ctx context.Context, db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		if s.Valid && strings.TrimSpace(s.String) != "" {
			out = append(out, strings.TrimSpace(s.String))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func truncateErr(err error, n int) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if n > 0 && len(s) > n {
		return s[:n]
	}
	return s
}
