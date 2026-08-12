package contextpack

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// Key identifies one source range for dedup and handle stability.
type Key struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// Fingerprint is a deterministic content digest for one source range.
func Fingerprint(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// HandleKind names the progressive-disclosure expansion lane.
type HandleKind string

const (
	// HandleExpandRange expands one emitted or omitted source range.
	HandleExpandRange HandleKind = "expand_range"
)

// Handle is a stable progressive-disclosure pointer. ID is a pure function
// of (path, range, content fingerprint): identical source re-packs to the
// identical handle, and content drift yields a new ID so old handles are
// detectably stale.
type Handle struct {
	ID          string     `json:"id"`
	Kind        HandleKind `json:"kind"`
	Path        string     `json:"path"`
	StartLine   int        `json:"start_line"`
	EndLine     int        `json:"end_line"`
	Fingerprint string     `json:"fingerprint"`
}

// NewHandle builds the deterministic handle for a source range.
func NewHandle(k Key, content string) Handle {
	fp := Fingerprint(content)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%s", k.Path, k.StartLine, k.EndLine, fp)))
	return Handle{
		ID:          "h_" + hex.EncodeToString(sum[:8]),
		Kind:        HandleExpandRange,
		Path:        k.Path,
		StartLine:   k.StartLine,
		EndLine:     k.EndLine,
		Fingerprint: fp,
	}
}

// HandleState is the resolution outcome for a handle ID.
type HandleState string

const (
	// HandleOK: handle is known and its fingerprint matches the latest
	// tracked content for its range.
	HandleOK HandleState = "ok"
	// HandleStale: handle is known but newer content was tracked for its
	// range; the caller must re-fetch.
	HandleStale HandleState = "stale"
	// HandleUnknown: handle ID was never issued by this registry.
	HandleUnknown HandleState = "unknown"
)

// Registry is the session-scoped dedup and handle ledger. It tracks the
// latest content fingerprint per range and hands out back-pointers for
// repeated unchanged source. Safe for concurrent use; not persisted.
type Registry struct {
	mu     sync.Mutex
	seq    int
	byKey  map[Key]int       // range -> ordinal of first emission
	latest map[Key]string    // range -> latest fingerprint
	issued map[string]Handle // handle ID -> issued handle
}

// NewRegistry returns an empty session registry.
func NewRegistry() *Registry {
	return &Registry{
		byKey:  make(map[Key]int),
		latest: make(map[Key]string),
		issued: make(map[string]Handle),
	}
}

// Track records one emission of (k, fingerprint). When the fingerprint
// matches the latest tracked content for k, dedup is true and ref is a
// back-pointer ("d<ordinal>") to the first emission of that range; callers
// must then omit the content bytes. Otherwise the range is (re-)registered
// and ref points at the current emission.
func (r *Registry) Track(k Key, fingerprint string) (dedup bool, ref string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.latest[k]; ok && cur == fingerprint {
		return true, fmt.Sprintf("d%d", r.byKey[k])
	}
	r.seq++
	if _, ok := r.byKey[k]; !ok {
		r.byKey[k] = r.seq
	}
	r.latest[k] = fingerprint
	return false, fmt.Sprintf("d%d", r.byKey[k])
}

// Issue records a handle so later Resolve calls can classify it.
func (r *Registry) Issue(h Handle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.issued[h.ID] = h
}

// Resolve classifies a handle ID: unknown handles and stale handles fail
// clearly instead of silently expanding the wrong bytes.
func (r *Registry) Resolve(id string) (Handle, HandleState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.issued[id]
	if !ok {
		return Handle{}, HandleUnknown
	}
	k := Key{Path: h.Path, StartLine: h.StartLine, EndLine: h.EndLine}
	if cur, ok := r.latest[k]; !ok || cur != h.Fingerprint {
		return h, HandleStale
	}
	return h, HandleOK
}

// DedupRef is the back-pointer attached to a deduplicated item, pointing at
// the ordinal of the first emission of the same unchanged range.
type DedupRef = string
