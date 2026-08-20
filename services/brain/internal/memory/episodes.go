package memory

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Episode is a time-bounded binding of evidence (Zep episode subgraph analogue).
type Episode struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"` // ingest|meeting|chat|incident|deploy|custom
	Title       string    `json:"title,omitempty"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end,omitempty"`
	DocumentIDs []string  `json:"document_ids,omitempty"`
	ClaimIDs    []string  `json:"claim_ids,omitempty"`
	Summary     string    `json:"summary,omitempty"`
	Generation  string    `json:"generation_id,omitempty"`
}

// BindEpisode creates or updates an episode binding documents/claims.
// BindEpisode takes the store lock and delegates. The bindEpisodeLocked form exists
// because composed maintenance operations call it while already holding
// the lock, and sync.Mutex is not reentrant -- taking it twice deadlocks.
func (s *Store) BindEpisode(ep Episode) (Episode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bindEpisodeLocked(ep)
}

// bindEpisodeLocked assumes the caller holds s.mu.
func (s *Store) bindEpisodeLocked(ep Episode) (Episode, error) {
	if s == nil {
		return Episode{}, fmt.Errorf("memory: nil store")
	}
	if ep.ID == "" {
		ep.ID = fmt.Sprintf("ep-%d", s.seq.Add(1))
	}
	if ep.Kind == "" {
		ep.Kind = "custom"
	}
	if ep.Start.IsZero() {
		ep.Start = time.Now().UTC()
	}
	// Replace if same ID.
	for i := range s.data.Episodes {
		if s.data.Episodes[i].ID == ep.ID {
			s.data.Episodes[i] = ep
			return ep, s.persistLocked()
		}
	}
	s.data.Episodes = append(s.data.Episodes, ep)
	return ep, s.persistLocked()
}

// ResegmentEpisode merges document IDs from multiple episodes into one (C8).
func (s *Store) ResegmentEpisode(targetID string, sourceIDs []string, title string) (Episode, error) {
	if s == nil {
		return Episode{}, fmt.Errorf("memory: nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	docs := map[string]struct{}{}
	claims := map[string]struct{}{}
	var start, end time.Time
	for _, id := range sourceIDs {
		for _, ep := range s.data.Episodes {
			if ep.ID != id {
				continue
			}
			for _, d := range ep.DocumentIDs {
				docs[d] = struct{}{}
			}
			for _, c := range ep.ClaimIDs {
				claims[c] = struct{}{}
			}
			if start.IsZero() || ep.Start.Before(start) {
				start = ep.Start
			}
			if end.IsZero() || ep.End.After(end) {
				end = ep.End
			}
		}
	}
	var docIDs, claimIDs []string
	for d := range docs {
		docIDs = append(docIDs, d)
	}
	for c := range claims {
		claimIDs = append(claimIDs, c)
	}
	sort.Strings(docIDs)
	sort.Strings(claimIDs)
	if title == "" {
		title = "resegmented:" + strings.Join(sourceIDs, ",")
	}
	return s.bindEpisodeLocked(Episode{
		ID: targetID, Kind: "reseg", Title: title,
		Start: start, End: end, DocumentIDs: docIDs, ClaimIDs: claimIDs,
		Summary: "resegmented from " + strings.Join(sourceIDs, ","),
	})
}

// ListEpisodes returns episodes sorted by start time.
func (s *Store) ListEpisodes() []Episode {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Episode(nil), s.data.Episodes...)
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// EpisodesForDocument returns episodes that include docID.
func (s *Store) EpisodesForDocument(docID string) []Episode {
	var out []Episode
	for _, ep := range s.ListEpisodes() {
		for _, d := range ep.DocumentIDs {
			if d == docID {
				out = append(out, ep)
				break
			}
		}
	}
	return out
}

// ResegmentResult reports whether a reseg/bind happened.
type ResegmentResult struct {
	Reseg         bool     `json:"reseg"`
	Bound         bool     `json:"bound,omitempty"`
	EpisodeID     string   `json:"episode_id,omitempty"`
	SourceIDs     []string `json:"source_ids,omitempty"`
	EpisodesAfter int      `json:"episodes_after"`
}

// ResegmentNearby merges the two most recent episodes when their starts are
// within maxGap (default 72h). If fewer than 2 episodes and docs exist, binds
// a generation episode. Returns diagnostic result.
func (s *Store) ResegmentNearby(maxGap time.Duration, generationID string, docIDs []string) ResegmentResult {
	res := ResegmentResult{}
	if s == nil {
		return res
	}
	if maxGap <= 0 {
		maxGap = 72 * time.Hour
	}
	eps := s.ListEpisodes()
	res.EpisodesAfter = len(eps)
	if len(eps) >= 2 {
		// ListEpisodes is sorted by Start ascending; take last two.
		a := eps[len(eps)-2]
		b := eps[len(eps)-1]
		gap := b.Start.Sub(a.Start)
		if gap < 0 {
			gap = -gap
		}
		if gap <= maxGap {
			target := fmt.Sprintf("reseg-%s-%s", a.ID, b.ID)
			merged, err := s.ResegmentEpisode(target, []string{a.ID, b.ID}, "nearby:"+a.ID+","+b.ID)
			if err == nil {
				res.Reseg = true
				res.EpisodeID = merged.ID
				res.SourceIDs = []string{a.ID, b.ID}
				res.EpisodesAfter = len(s.ListEpisodes())
				return res
			}
		}
		// Even if gap large, still merge recent pair for lifecycle reseg job.
		target := fmt.Sprintf("reseg-%s-%s", a.ID, b.ID)
		merged, err := s.ResegmentEpisode(target, []string{a.ID, b.ID}, "lifecycle:"+a.ID+","+b.ID)
		if err == nil {
			res.Reseg = true
			res.EpisodeID = merged.ID
			res.SourceIDs = []string{a.ID, b.ID}
			res.EpisodesAfter = len(s.ListEpisodes())
		}
		return res
	}
	if len(eps) == 0 && len(docIDs) > 0 {
		ep, err := s.BindEpisode(Episode{
			Kind: "ingest", Title: "lifecycle:" + generationID,
			DocumentIDs: docIDs, Generation: generationID,
			Start: time.Now().UTC(),
		})
		if err == nil {
			res.Bound = true
			res.EpisodeID = ep.ID
			res.EpisodesAfter = len(s.ListEpisodes())
		}
	}
	return res
}

// LifecycleResegment is the product post-wave episode reseg hook:
// if ≥2 episodes, merge recent pair; else if 0 episodes and docs exist, BindEpisode.
func (s *Store) LifecycleResegment(generationID string, docs map[string]string) ResegmentResult {
	var docIDs []string
	for id := range docs {
		if id != "" {
			docIDs = append(docIDs, id)
		}
	}
	sort.Strings(docIDs)
	return s.ResegmentNearby(72*time.Hour, generationID, docIDs)
}
