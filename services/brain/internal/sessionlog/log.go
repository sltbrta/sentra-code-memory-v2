package sessionlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Defaults bound the persistent event stream so it cannot grow unbounded.
const (
	// DefaultMaxEvents bounds the number of events kept in the active file.
	// When appending would exceed it, the oldest events are folded into a
	// single compaction event and the file is rewritten atomically.
	DefaultMaxEvents = 2048
	// DefaultMaxEventBytes bounds a single persisted event line.
	DefaultMaxEventBytes = 64 << 10
	// Filename is the active JSONL log stored beneath the caller's dir.
	Filename = "session-events.jsonl"
)

// Open errors are sentinel-wrapped so callers can distinguish a missing log
// (fresh start) from a malformed one (never silently repaired).
var (
	// ErrEmpty is returned by Open when dir is blank.
	ErrEmpty = errors.New("sessionlog: log directory required")
)

// Clock is injectable for deterministic write times in tests; nil uses UTC now.
type Clock func() time.Time

// utcNow is the default Clock.
func utcNow() time.Time { return time.Now().UTC() }

// Writer appends privacy-safe events to one repo-local JSONL file. It is safe
// for concurrent use within a process; callers should use one Writer per file.
// Normal operation is append-only; compaction is an explicit, recorded,
// atomic maintenance step that folds overflow into one summary event.
type Writer struct {
	mu        sync.Mutex
	path      string
	maxEvents int
	clock     Clock
	events    []Event // in-memory mirror of the persisted file (append order)
}

// Open loads (or starts) the log beneath dir. Missing logs start empty;
// malformed committed files are rejected and never overwritten. A missing
// directory is created.
func Open(dir string, opts ...Option) (*Writer, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, ErrEmpty
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("sessionlog: create dir: %w", err)
	}
	w := &Writer{
		path:      filepath.Join(dir, Filename),
		maxEvents: DefaultMaxEvents,
		clock:     utcNow,
	}
	for _, opt := range opts {
		opt(w)
	}
	raw, err := os.ReadFile(w.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return w, nil
	case err != nil:
		return nil, fmt.Errorf("sessionlog: read log: %w", err)
	}
	loaded, err := parseStrict(raw)
	if err != nil {
		return nil, fmt.Errorf("sessionlog: parse %s: %w", w.path, err)
	}
	w.events = loaded
	return w, nil
}

// Option configures a Writer.
type Option func(*Writer)

// WithMaxEvents overrides the bound on retained events (must be > 0).
func WithMaxEvents(n int) Option {
	return func(w *Writer) {
		if n > 0 {
			w.maxEvents = n
		}
	}
}

// WithClock injects a deterministic clock for tests.
func WithClock(c Clock) Option {
	return func(w *Writer) {
		if c != nil {
			w.clock = c
		}
	}
}

// parseStrict decodes newline-delimited events, rejecting blank/malformed lines
// so strict replay and open agree. Trailing newline is allowed; a final
// non-newline-terminated line is accepted only if it decodes cleanly.
func parseStrict(raw []byte) ([]Event, error) {
	var out []Event
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64<<10), DefaultMaxEventBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev Event
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&ev); err != nil {
			return nil, fmt.Errorf("sessionlog: invalid event line: %w", err)
		}
		if err := ev.Validate(); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Append validates, sequences, and atomically persists ev. The event's Seq,
// Time, Schema, and ID are assigned here; caller-supplied values for those
// fields are ignored so the stream is always internally consistent.
func (w *Writer) Append(ev Event) (Event, error) {
	if w == nil || w.path == "" {
		return Event{}, errors.New("sessionlog: nil writer")
	}
	if !ev.Kind.Valid() {
		return Event{}, fmt.Errorf("sessionlog: unknown kind %q", ev.Kind)
	}
	// Sanitize first so validation and provenance checks see capped pointers and
	// free text; this also keeps the persisted stream privacy-safe by construction.
	ev = ev.Sanitize()
	if err := ev.validateFields(); err != nil {
		return Event{}, err
	}
	if err := ValidateProvenance(ev); err != nil {
		return Event{}, err
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	seq := uint64(len(w.events) + 1)
	now := w.clock().UTC().Format(time.RFC3339Nano)
	sealed := Event{
		Schema:           Schema,
		ID:               EventID(seq, ev.Session, string(ev.Kind), now),
		Seq:              seq,
		Session:          ev.Session,
		Kind:             ev.Kind,
		Time:             now,
		Verb:             ev.Verb,
		Provenance:       ev.Provenance,
		Freshness:        ev.Freshness,
		PredictedDigest:  ev.PredictedDigest,
		ObservedDigest:   ev.ObservedDigest,
		Counts:           ev.Counts,
		CoverageWarnings: ev.CoverageWarnings,
		FreeText:         ev.FreeText,
		Folded:           ev.Folded,
	}
	next := append(append([]Event(nil), w.events...), sealed)
	if len(next) > w.maxEvents {
		// Fold the oldest overflow into one compaction event so the result is
		// exactly maxEvents: overflow events collapse to one summary.
		overflow := len(next) - w.maxEvents + 1
		if overflow < 1 {
			overflow = 1
		}
		folded := next[:overflow]
		fold := compactionFrom(folded, now, ev.Session)
		kept := append([]Event{fold}, next[overflow:]...)
		next = kept
		// Renumber so the stream stays contiguous and monotonic; replay order
		// is preserved and compaction stays the authoritative summary.
		for i := range next {
			next[i].Seq = uint64(i + 1)
			next[i].ID = EventID(next[i].Seq, next[i].Session, string(next[i].Kind), next[i].Time)
		}
		sealed = next[len(next)-1]
	}
	if err := w.writeAtomic(next); err != nil {
		return Event{}, err
	}
	w.events = next
	return sealed, nil
}

// compactionFrom folds a contiguous run of events into one compaction summary.
func compactionFrom(src []Event, now, session string) Event {
	counts := map[string]int64{}
	var foldedBytes int64
	earliest, latest := "", ""
	for _, e := range src {
		counts[string(e.Kind)+"_events"]++
		for k, v := range e.Counts {
			counts[k] += v
		}
		if earliest == "" || (e.Time != "" && e.Time < earliest) {
			earliest = e.Time
		}
		if e.Time > latest {
			latest = e.Time
		}
		body, _ := json.Marshal(e)
		foldedBytes += int64(len(body))
	}
	counts["folded_events"] = int64(len(src))
	counts["folded_bytes"] = foldedBytes
	return Event{
		Schema:    Schema,
		Kind:      KindCompaction,
		Session:   session,
		Time:      now,
		Freshness: FreshnessAsOf,
		Provenance: Provenance{
			Tree:       "compaction",
			Confidence: 1,
		},
		Counts: counts,
		FreeText: fmt.Sprintf(
			"compacted %d events (%s..%s)", len(src), earliest, latest),
		Folded: len(src),
	}
}

// writeAtomic persists all events as JSONL via a temp file + sync + rename so
// readers never observe a torn log.
func (w *Writer) writeAtomic(events []Event) error {
	dir := filepath.Dir(w.path)
	tmp, err := os.CreateTemp(dir, ".session-events-*.tmp")
	if err != nil {
		return fmt.Errorf("sessionlog: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("sessionlog: secure temp: %w", err)
	}
	enc := json.NewEncoder(tmp)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			return fmt.Errorf("sessionlog: encode event: %w", err)
		}
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sessionlog: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("sessionlog: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, w.path); err != nil {
		return fmt.Errorf("sessionlog: commit log: %w", err)
	}
	committed = true
	return nil
}

// Events returns a copy of all persisted events in append order.
func (w *Writer) Events() []Event {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Event(nil), w.events...)
}

// Len returns the number of persisted events.
func (w *Writer) Len() int {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.events)
}

// Path returns the on-disk log path.
func (w *Writer) Path() string {
	if w == nil {
		return ""
	}
	return w.path
}

// Read reads and strictly validates a JSONL event stream from r.
func Read(r io.Reader) ([]Event, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("sessionlog: read stream: %w", err)
	}
	return parseStrict(raw)
}

// Applier is the deterministic event-to-projection interface used by Replay
// (Phase 4, issue #31). Implementations accumulate state from the stream; the
// same stream must always rebuild the same state.
type Applier interface {
	Apply(Event) error
}

// ApplierFunc is a function adapter for Applier.
type ApplierFunc func(Event) error

// Apply calls f(ev).
func (f ApplierFunc) Apply(ev Event) error { return f(ev) }

// Replay applies events in append order to a. Strict replay rejects a
// malformed event line and stops; the projection is left partially applied
// so callers can observe where divergence began.
func Replay(events []Event, a Applier) error {
	for i, ev := range events {
		if err := ev.Validate(); err != nil {
			return fmt.Errorf("sessionlog: invalid event at %d: %w", i, err)
		}
		if err := a.Apply(ev); err != nil {
			return fmt.Errorf("sessionlog: apply event %d: %w", i, err)
		}
	}
	return nil
}
