package query

// expandWithGraph appends graph-neighbor paths that are not already among
// candidates, up to maxExtra extras. Neighbors become non-degraded path-only
// candidates (empty definitions): they rely on later definition selection /
// hydration the same way an exact path mention does. maxExtra <= 0 or an
// empty neighbor list leaves candidates unchanged. Order of graphPaths is
// preserved among the appended extras.
func expandWithGraph(candidates []candidate, graphPaths []string, maxExtra int) []candidate {
	if maxExtra <= 0 || len(graphPaths) == 0 {
		return candidates
	}
	have := make(map[string]bool, len(candidates)+len(graphPaths))
	for _, c := range candidates {
		have[c.path] = true
	}
	out := make([]candidate, len(candidates), len(candidates)+maxExtra)
	copy(out, candidates)
	added := 0
	for _, path := range graphPaths {
		if path == "" || have[path] {
			continue
		}
		out = append(out, candidate{
			path:        path,
			definitions: nil,
			degraded:    false,
		})
		have[path] = true
		added++
		if added >= maxExtra {
			break
		}
	}
	return out
}
