package query

import "context"

// expandHybridDense optionally fuses dense retrieval with lexical candidates
// via RRF, then optionally reorders via CandidateReranker. Nil ports are no-ops.
// The result is capped at MaxCandidates.
func (e *Engine) expandHybridDense(ctx context.Context, snapshot Snapshot, question string, candidates []candidate) []candidate {
	if e == nil {
		return candidates
	}
	out := candidates
	if e.dense != nil {
		if err := ctx.Err(); err != nil {
			return capCandidates(out, e.limits.MaxCandidates)
		}
		lexPaths := candidatePaths(out)
		densePaths := e.dense.Search(ctx, snapshot.GenerationID, question, e.limits.MaxCandidates)
		if len(densePaths) > 0 {
			fused := rrfFuse([][]string{lexPaths, densePaths}, defaultRRFK, e.limits.MaxCandidates)
			out = rebuildCandidatesFromPaths(out, fused)
		}
	}
	if e.reranker != nil {
		bodies := snapshotBodies(snapshot)
		if len(bodies) > 0 && len(out) > 0 {
			if err := ctx.Err(); err != nil {
				return capCandidates(out, e.limits.MaxCandidates)
			}
			paths := candidatePaths(out)
			reranked := e.reranker.Rerank(ctx, question, paths, bodies, e.limits.MaxCandidates)
			if len(reranked) > 0 {
				out = rebuildCandidatesFromPaths(out, reranked)
			}
		}
	}
	return capCandidates(out, e.limits.MaxCandidates)
}

// candidatePaths returns the ordered path list from candidates.
func candidatePaths(candidates []candidate) []string {
	if len(candidates) == 0 {
		return nil
	}
	paths := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if c.path == "" {
			continue
		}
		paths = append(paths, c.path)
	}
	return paths
}

// rebuildCandidatesFromPaths rebuilds a candidate slice in path order.
// Paths already present keep their definitions/degraded flags; dense-only or
// otherwise new paths become non-degraded path-only candidates (same shape as
// expandWithGraph neighbors).
func rebuildCandidatesFromPaths(existing []candidate, paths []string) []candidate {
	if len(paths) == 0 {
		return nil
	}
	byPath := make(map[string]candidate, len(existing))
	for _, c := range existing {
		if c.path == "" {
			continue
		}
		if _, ok := byPath[c.path]; !ok {
			byPath[c.path] = c
		}
	}
	out := make([]candidate, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		if c, ok := byPath[path]; ok {
			out = append(out, c)
			continue
		}
		out = append(out, candidate{
			path:        path,
			definitions: nil,
			degraded:    false,
		})
	}
	return out
}

// snapshotBodies maps repository-relative paths to UTF-8 projection body text.
// Empty when the projection carries no hydrated files.
func snapshotBodies(snapshot Snapshot) map[string]string {
	files := snapshot.Projection.Files
	if len(files) == 0 {
		return nil
	}
	out := make(map[string]string, len(files))
	for path, hf := range files {
		if path == "" {
			continue
		}
		out[path] = string(hf.Content)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// capCandidates truncates the candidate list to max when max > 0.
func capCandidates(candidates []candidate, max int) []candidate {
	if max <= 0 || len(candidates) <= max {
		return candidates
	}
	return candidates[:max]
}
