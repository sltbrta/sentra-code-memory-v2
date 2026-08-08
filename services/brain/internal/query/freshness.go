package query

// freshnessEvaluation is the freshness decision for one pinned generation:
// the disclosed state, whether the pin is superseded, and whether the
// requirement refuses to serve it.
type freshnessEvaluation struct {
	State        FreshnessState
	Stale        bool
	AbstainStale bool
}

// evaluateFreshness pins the three frozen freshness requirements against the
// current complete generation. A superseded pin is always disclosed as stale;
// best_effort and complete_generation may serve it (v1 publications are
// atomically complete, so no wait ever applies), while abstain_if_stale
// refuses it before any retrieval.
func evaluateFreshness(requirement FreshnessRequirement, snapshot Snapshot, currentGenerationID string) freshnessEvaluation {
	if snapshot.GenerationID != currentGenerationID {
		return freshnessEvaluation{
			State:        FreshnessStaleDisclosed,
			Stale:        true,
			AbstainStale: requirement == FreshnessAbstainIfStale,
		}
	}
	if snapshot.State == GenerationDegraded {
		return freshnessEvaluation{State: FreshnessDegraded}
	}
	return freshnessEvaluation{State: FreshnessCurrent}
}

// computeCoverage counts canonical revisions in the pinned generation against
// the revision paths present in the projection. An absent or rebuilding
// projection indexes nothing; canonical facts remain, and indexed never
// exceeds canonical.
func computeCoverage(snapshot Snapshot) Coverage {
	coverage := Coverage{CanonicalRevisionCount: uint64(len(snapshot.Revisions))}
	if snapshot.Projection.State != ProjectionReady || snapshot.Projection.Index == nil {
		return coverage
	}
	indexed := make(map[string]bool, len(snapshot.Projection.Index.Files))
	for _, file := range snapshot.Projection.Index.Files {
		indexed[file.Path] = true
	}
	for _, revision := range snapshot.Revisions {
		if indexed[revision.Path] {
			coverage.IndexedRevisionCount++
		}
	}
	return coverage
}
