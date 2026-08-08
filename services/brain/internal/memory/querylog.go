package memory

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// queryLogMu serializes appends so concurrent ask paths don't interleave writes.
var queryLogMu sync.Mutex

// QueryLogEntry is one ask/search probe candidate for C1 (GAP-MEM-C1-QUERY-LOG).
type QueryLogEntry struct {
	Question  string    `json:"question"`
	DocIDs    []string  `json:"doc_ids,omitempty"` // cited / top docs when known
	At        time.Time `json:"at"`
	SessionID string    `json:"session_id,omitempty"`
}

const queryLogName = "query_log.jsonl"
const queryLogMax = 500

// AppendQueryLog records a user question for later C1 probes.
func (s *Store) AppendQueryLog(e QueryLogEntry) error {
	if s == nil || strings.TrimSpace(e.Question) == "" {
		return nil
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	path := filepath.Join(s.Dir(), queryLogName)
	if err := os.MkdirAll(s.Dir(), 0o755); err != nil {
		return err
	}
	queryLogMu.Lock()
	defer queryLogMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	// Single-line JSON: Encode is one Write for typical sizes; lock prevents interleave.
	raw, err := json.Marshal(e)
	if err != nil {
		_ = f.Close()
		return err
	}
	_, err = f.Write(append(raw, '\n'))
	cerr := f.Close()
	if err != nil {
		return err
	}
	return cerr
}

// LoadQueryLog returns recent query log entries (newest last), capped.
func (s *Store) LoadQueryLog(limit int) []QueryLogEntry {
	if s == nil {
		return nil
	}
	if limit <= 0 {
		limit = 50
	}
	path := filepath.Join(s.Dir(), queryLogName)
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var all []QueryLogEntry
	sc := bufio.NewScanner(f)
	// Raise token size for long questions (default 64K can truncate).
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e QueryLogEntry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		all = append(all, e)
	}
	_ = sc.Err() // partial success preferred over hard fail on one bad line
	if len(all) > queryLogMax {
		all = all[len(all)-queryLogMax:]
	}
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all
}

// BuildProbesFromQueryLog builds C1 probes from real user questions.
// Prefers entries with cited DocIDs (stronger probes) and dedupes near-identical questions.
func BuildProbesFromQueryLog(entries []QueryLogEntry, maxProbes int) []Probe {
	if maxProbes <= 0 {
		maxProbes = 5
	}
	// First pass: entries with gold DocIDs (newest first).
	var withGold, without []QueryLogEntry
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if strings.TrimSpace(e.Question) == "" {
			continue
		}
		if len(e.DocIDs) > 0 {
			withGold = append(withGold, e)
		} else {
			without = append(without, e)
		}
	}
	var out []Probe
	seenQ := map[string]struct{}{}
	add := func(e QueryLogEntry) {
		q := strings.TrimSpace(e.Question)
		key := strings.ToLower(q)
		if len(key) > 80 {
			key = key[:80]
		}
		if _, ok := seenQ[key]; ok {
			return
		}
		seenQ[key] = struct{}{}
		// Drop agent: / summary: cite noise from expected set.
		var docs []string
		for _, id := range e.DocIDs {
			id = strings.TrimSpace(id)
			if id == "" || strings.HasPrefix(id, "agent:") || strings.HasPrefix(id, "summary:") {
				continue
			}
			docs = append(docs, id)
		}
		// Prefer gold; skip empty-expected entirely when gold exists later.
		out = append(out, Probe{Question: q, ExpectedDocIDs: docs})
	}
	for _, e := range withGold {
		if len(out) >= maxProbes {
			break
		}
		add(e)
	}
	// Only fill without-gold when we still need probes and have no gold at all.
	if len(out) == 0 {
		for _, e := range without {
			if len(out) >= maxProbes {
				break
			}
			if len(strings.TrimSpace(e.Question)) < 8 {
				continue
			}
			add(e)
		}
	}
	// Drop probes that still have empty expected after filtering noise.
	var strong []Probe
	for _, p := range out {
		if len(p.ExpectedDocIDs) > 0 {
			strong = append(strong, p)
		}
	}
	if len(strong) > 0 {
		return strong
	}
	return out
}
