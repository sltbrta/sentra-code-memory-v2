package codecrawl

import "time"

const (
	defaultWatchQueueSize = 4096
	defaultRetryInitial   = 100 * time.Millisecond
	defaultRetryMax       = 5 * time.Second
)

// refreshQueue coalesces filesystem events by path while retaining a bounded
// overflow marker. OpenOrRefresh always performs the authoritative stamp/hash
// reconciliation, so an overflow requests a full rescan instead of dropping
// work and pretending the index is current.
type refreshQueue struct {
	capacity int
	paths    map[string]struct{}
	version  uint64
	overflow bool
}

func newRefreshQueue(capacity int) *refreshQueue {
	if capacity < 1 {
		capacity = defaultWatchQueueSize
	}
	return &refreshQueue{capacity: capacity, paths: make(map[string]struct{})}
}

func (q *refreshQueue) enqueue(path string) {
	q.version++
	if path == "" {
		q.overflow = true
		return
	}
	if _, exists := q.paths[path]; exists {
		return
	}
	if len(q.paths) >= q.capacity {
		q.overflow = true
		return
	}
	q.paths[path] = struct{}{}
}

func (q *refreshQueue) pending() bool {
	return q.overflow || len(q.paths) > 0
}

func (q *refreshQueue) begin() (version uint64, depth int, fullRescan bool) {
	return q.version, len(q.paths), q.overflow
}

func (q *refreshQueue) commit(version uint64) bool {
	if q.version != version {
		return false
	}
	q.paths = make(map[string]struct{})
	q.overflow = false
	return true
}

func (q *refreshQueue) depth() int {
	return len(q.paths)
}

func retryDelay(attempt int, initial, maximum time.Duration) time.Duration {
	if initial <= 0 {
		initial = defaultRetryInitial
	}
	if maximum < initial {
		maximum = defaultRetryMax
	}
	delay := initial
	for i := 1; i < attempt; i++ {
		if delay >= maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}
