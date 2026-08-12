package contextpack

import (
	"errors"
	"sync"
	"time"
)

// ErrLimitExceeded is returned by fail-safe Governor checks when a configured
// limit is exceeded. Governors deny; they never panic.
var ErrLimitExceeded = errors.New("contextpack: resource limit exceeded")

// Limits bounds local resource use. A zero (or negative) field means
// unlimited for that dimension.
type Limits struct {
	// MaxWorkers caps concurrently held worker slots.
	MaxWorkers int `json:"max_workers,omitempty"`
	// MaxCandidates caps the number of candidate sources admitted.
	MaxCandidates int `json:"max_candidates,omitempty"`
	// MaxOutputBytes caps total emitted output bytes.
	MaxOutputBytes int `json:"max_output_bytes,omitempty"`
	// MaxWallTime caps elapsed wall time since governor creation.
	MaxWallTime time.Duration `json:"max_wall_time,omitempty"`
}

func (l Limits) normalized() Limits {
	if l.MaxWorkers < 0 {
		l.MaxWorkers = 0
	}
	if l.MaxCandidates < 0 {
		l.MaxCandidates = 0
	}
	if l.MaxOutputBytes < 0 {
		l.MaxOutputBytes = 0
	}
	if l.MaxWallTime < 0 {
		l.MaxWallTime = 0
	}
	return l
}

// Governor enforces Limits fail-safe: every check denies on overflow instead
// of erroring open. It is safe for concurrent use.
type Governor struct {
	limits Limits
	now    func() time.Time
	start  time.Time

	mu          sync.Mutex
	workers     int
	candidates  int
	outputBytes int
}

// NewGovernor starts a governor against limits. now is injectable for
// deterministic wall-time tests; nil uses time.Now.
func NewGovernor(l Limits, now func() time.Time) *Governor {
	if now == nil {
		now = time.Now
	}
	return &Governor{limits: l.normalized(), now: now, start: now()}
}

// AcquireWorker takes a worker slot. Returns false when MaxWorkers is set and
// all slots are held. Callers must pair success with ReleaseWorker.
func (g *Governor) AcquireWorker() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.limits.MaxWorkers > 0 && g.workers >= g.limits.MaxWorkers {
		return false
	}
	g.workers++
	return true
}

// ReleaseWorker returns a worker slot. Releasing with none held is a no-op
// (fail-safe: never goes negative).
func (g *Governor) ReleaseWorker() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.workers > 0 {
		g.workers--
	}
}

// AllowCandidate admits one candidate source. Returns false when
// MaxCandidates is set and already reached.
func (g *Governor) AllowCandidate() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.limits.MaxCandidates > 0 && g.candidates >= g.limits.MaxCandidates {
		return false
	}
	g.candidates++
	return true
}

// ReserveOutputBytes accounts n emitted bytes against MaxOutputBytes.
// Returns false (and reserves nothing) when the reservation would overflow.
func (g *Governor) ReserveOutputBytes(n int) bool {
	if n < 0 {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.limits.MaxOutputBytes > 0 && g.outputBytes+n > g.limits.MaxOutputBytes {
		return false
	}
	g.outputBytes += n
	return true
}

// CheckTime returns ErrLimitExceeded when MaxWallTime is set and elapsed.
func (g *Governor) CheckTime() error {
	if g.limits.MaxWallTime <= 0 {
		return nil
	}
	if g.now().Sub(g.start) > g.limits.MaxWallTime {
		return ErrLimitExceeded
	}
	return nil
}

// Report is a visible snapshot of configured limits and current usage.
type Report struct {
	Limits          Limits `json:"limits"`
	ActiveWorkers   int    `json:"active_workers"`
	Candidates      int    `json:"candidates"`
	OutputBytes     int    `json:"output_bytes"`
	ElapsedMillis   int64  `json:"elapsed_ms"`
	WallTimeLimited bool   `json:"wall_time_limited"`
}

// Report returns the current usage snapshot. Limits stay visible even when
// nothing has been consumed.
func (g *Governor) Report() Report {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Report{
		Limits:          g.limits,
		ActiveWorkers:   g.workers,
		Candidates:      g.candidates,
		OutputBytes:     g.outputBytes,
		ElapsedMillis:   g.now().Sub(g.start).Milliseconds(),
		WallTimeLimited: g.limits.MaxWallTime > 0 && g.now().Sub(g.start) > g.limits.MaxWallTime,
	}
}
