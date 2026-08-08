package codecrawl

import (
	"testing"
	"time"
)

func TestRefreshQueueCoalescesAndRetainsOverflow(t *testing.T) {
	q := newRefreshQueue(2)
	q.enqueue("a.go")
	q.enqueue("a.go")
	q.enqueue("b.go")
	q.enqueue("c.go")
	if !q.pending() || q.depth() != 2 {
		t.Fatalf("queue pending=%v depth=%d", q.pending(), q.depth())
	}
	version, _, full := q.begin()
	if !full {
		t.Fatal("expected bounded queue overflow to request a full rescan")
	}
	if q.commit(version) != true || q.pending() {
		t.Fatal("matching successful refresh should clear the queue")
	}
}

func TestRefreshQueueKeepsEventsArrivingDuringRefresh(t *testing.T) {
	q := newRefreshQueue(4)
	q.enqueue("before.go")
	version, _, _ := q.begin()
	q.enqueue("during.go")
	if q.commit(version) {
		t.Fatal("refresh must not clear events arriving after its snapshot")
	}
	if !q.pending() || q.depth() != 2 {
		t.Fatalf("pending=%v depth=%d", q.pending(), q.depth())
	}
}

func TestRetryDelayIsBoundedExponential(t *testing.T) {
	initial := 25 * time.Millisecond
	maximum := 100 * time.Millisecond
	if got := retryDelay(1, initial, maximum); got != 25*time.Millisecond {
		t.Fatalf("attempt 1 delay=%s", got)
	}
	if got := retryDelay(3, initial, maximum); got != 100*time.Millisecond {
		t.Fatalf("attempt 3 delay=%s", got)
	}
	if got := retryDelay(20, initial, maximum); got != maximum {
		t.Fatalf("bounded delay=%s", got)
	}
}
