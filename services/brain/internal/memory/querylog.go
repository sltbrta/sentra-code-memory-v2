package memory

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/textbound"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/durablefile"
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
	// 0600: the query log holds user questions verbatim, so it carries the
	// same disclosure risk as the corpus D-007 tightened.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
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
		key = textbound.Bytes(key, 80)
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

// PurgeHistory removes docIDs from the query log.
//
// The log records the document ids each question was answered from, so a
// deleted document's identity survived every deletion in a file that is read
// back to build probes -- which then reference content that no longer exists.
// An entry whose citations are entirely purged is dropped rather than kept as
// a question with no evidence; the question text itself is the user's, not the
// deleted document's, but a probe with no citations is exactly what
// BuildProbesFromQueryLog deprioritises anyway.
//
// The log is rewritten through durablefile: it is append-only in normal use,
// and a purge is the one operation that must replace it. A crash mid-rewrite
// must not leave a half-purged log, since the half that survives is the part
// still naming deleted content.
func (s *Store) PurgeHistory(docIDs []string) (int, error) {
	if s == nil || len(docIDs) == 0 {
		return 0, nil
	}
	targets := idSet(docIDs)
	if len(targets) == 0 {
		return 0, nil
	}
	path := filepath.Join(s.Dir(), queryLogName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("memory: read query log: %w", err)
	}

	queryLogMu.Lock()
	defer queryLogMu.Unlock()

	removed := 0
	var kept []QueryLogEntry
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry QueryLogEntry
		if json.Unmarshal([]byte(line), &entry) != nil {
			// An unparseable line cannot be shown not to name a purged
			// document, so it is dropped rather than retained.
			removed++
			continue
		}
		remaining, dropped := withoutIDs(entry.DocIDs, targets)
		if dropped == 0 {
			kept = append(kept, entry)
			continue
		}
		removed += dropped
		if len(remaining) == 0 {
			continue
		}
		entry.DocIDs = remaining
		kept = append(kept, entry)
	}
	if removed == 0 {
		return 0, nil
	}

	return removed, durablefile.WriteFunc(path, 0o600, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		for _, entry := range kept {
			if err := enc.Encode(entry); err != nil {
				return err
			}
		}
		return nil
	})
}

// ResidualHistory returns the ids still named by the query log.
func (s *Store) ResidualHistory(docIDs []string) []string {
	if s == nil || len(docIDs) == 0 {
		return nil
	}
	targets := idSet(docIDs)
	found := map[string]struct{}{}
	for _, entry := range s.LoadQueryLog(queryLogMax) {
		for _, id := range entry.DocIDs {
			if _, ok := targets[id]; ok {
				found[id] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(found))
	for id := range found {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
