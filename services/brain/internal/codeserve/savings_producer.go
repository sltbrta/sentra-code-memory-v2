package codeserve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codecrawl"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/savings"
)

// The savings ledger had no producer in the product.
//
// Only the benchmark wrote to it, so `savings_summary` answered `steps: 0`
// after any amount of real use: the one surface that reports what retrieval
// saved had nothing to report unless the benchmark had been run in the same
// cache directory. The verb existed, the ledger existed, and the number was
// always zero.
//
// A served search now records a step. The baseline is the set of files the
// answer cites -- what the caller would have had to read to reach the same
// hits without retrieval -- rather than the whole indexed tree, which no
// caller reads and which would make every ratio a statement about the size of
// the repository.
//
// Recording is buffered: a durable rewrite of the ledger measures 4.2ms at
// capacity, and the retrieval path answers in well under a millisecond, so a
// synchronous write would cost several times what it measures. Nothing in the
// request path fails on a ledger error -- a metrics counter must not be able
// to fail a query.

// recordSearchSavings records one served retrieval against the ledger in the
// request's index cache. Every failure path is silent by design.
func recordSearchSavings(req Request, rootAbs string, hits []codecrawl.Hit, response Response) {
	cacheDir := savingsLedgerDir(req)
	if cacheDir == "" || len(hits) == 0 {
		return
	}
	baseline := citedFileBytes(rootAbs, hits)
	if baseline <= 0 {
		return
	}
	served, err := json.Marshal(response)
	if err != nil {
		return
	}
	bufferSavingsStep(cacheDir, savings.Step{
		Name:              string(VerbCodeSearch),
		Category:          savings.CategoryRetrieval,
		Estimator:         savings.EstimatorBytesDiv4,
		BaselineModel:     savings.BaselineGoldFiles,
		BaselineBytes:     baseline,
		ServedBytes:       int64(len(served)),
		BaselineTokensEst: estimateTokensFromBytes(baseline),
		ServedTokensEst:   estimateTokensFromBytes(int64(len(served))),
		AvoidedReads:      uint64(len(hits)),
	})
}

// savingsLedgerDir returns the directory the ledger lives in for this request,
// which is the index cache -- the same directory the benchmark records into.
func savingsLedgerDir(req Request) string {
	cache := str(req, "index_cache")
	if cache == "" {
		return ""
	}
	info, err := os.Stat(cache)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return cache
	}
	return filepath.Dir(cache)
}

// citedFileBytes sums the sizes of the distinct files a result set cites. A
// file cited twice is counted once: the caller reads it once.
func citedFileBytes(rootAbs string, hits []codecrawl.Hit) int64 {
	seen := map[string]struct{}{}
	var total int64
	for _, hit := range hits {
		if hit.Path == "" {
			continue
		}
		if _, ok := seen[hit.Path]; ok {
			continue
		}
		seen[hit.Path] = struct{}{}
		info, err := os.Stat(filepath.Join(rootAbs, filepath.FromSlash(hit.Path)))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		total += info.Size()
	}
	return total
}

// estimateTokensFromBytes is the ceil(bytes/4) yardstick named by
// savings.EstimatorBytesDiv4. It is not any model's tokenizer, which is why
// every field it feeds carries an _est suffix.
func estimateTokensFromBytes(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n + 3) / 4
}

// pendingSteps holds steps not yet written, per ledger directory.
//
// Only the buffer is process state. The ledger file itself stays the source of
// truth and is reopened for every flush and every read, because other writers
// exist -- the benchmark records through savings.Open directly, and a second
// process may share the cache. An earlier version of this cached the
// savings.Ledger itself, which made a long-lived reader answer from a snapshot
// taken before those writes; the existing savings_summary test caught it by
// reading an empty ledger, writing to it, and reading again.
var pendingSteps = struct {
	mu    sync.Mutex
	steps map[string][]savings.Step
}{steps: map[string][]savings.Step{}}

// bufferSavingsStep queues a step for dir, writing through once enough have
// accumulated to be worth a durable rewrite.
func bufferSavingsStep(dir string, step savings.Step) {
	resolved, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	pendingSteps.mu.Lock()
	pendingSteps.steps[resolved] = append(pendingSteps.steps[resolved], step)
	ready := len(pendingSteps.steps[resolved]) >= savingsFlushEvery
	pendingSteps.mu.Unlock()
	if ready {
		flushSavingsSteps(resolved)
	}
}

// savingsFlushEvery is how many served responses accumulate before the ledger
// is written.
//
// A durable rewrite of the ledger measures 4.2ms at capacity, and the
// retrieval path answers in well under a millisecond, so writing per response
// would cost several times what it measures. What is at risk between flushes
// is at most this many entries of a derived, rebuildable metrics counter that
// nothing reads to make a decision, and a summary flushes before answering.
const savingsFlushEvery = 32

// flushSavingsSteps writes dir's queued steps and clears them. Steps are
// removed from the queue only after they are durable, so a failed write
// retries on the next flush rather than losing them.
func flushSavingsSteps(dir string) {
	pendingSteps.mu.Lock()
	queued := append([]savings.Step(nil), pendingSteps.steps[dir]...)
	pendingSteps.mu.Unlock()
	if len(queued) == 0 {
		return
	}
	ledger, err := savings.Open(dir)
	if err != nil {
		return
	}
	written := 0
	for _, step := range queued {
		if err := ledger.Record(step); err != nil {
			break
		}
		written++
	}
	if written == 0 {
		return
	}
	pendingSteps.mu.Lock()
	remaining := pendingSteps.steps[dir]
	if written <= len(remaining) {
		pendingSteps.steps[dir] = append([]savings.Step(nil), remaining[written:]...)
	}
	pendingSteps.mu.Unlock()
}

// openSavingsLedgerForRead flushes dir's queued steps and returns a ledger
// read fresh from disk, so a summary never omits work already recorded and
// never answers from a stale snapshot.
func openSavingsLedgerForRead(dir string) (*savings.Ledger, error) {
	resolved, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	flushSavingsSteps(resolved)
	return savings.Open(resolved)
}

// FlushPendingSavings writes every queued step for every ledger directory.
//
// Steps are queued and written every savingsFlushEvery, which bounds what a
// crash loses to that many entries of a derived counter. A *graceful* exit
// should lose nothing at all, and without this it lost whatever had
// accumulated since the last flush -- so a short-lived process that answered
// thirty queries recorded none of them, which looks exactly like the
// no-producer bug this replaced.
//
// A hard kill still loses up to savingsFlushEvery entries. That is the
// accepted trade and is why the ledger is a metrics counter rather than an
// authority: nothing reads it to make a decision.
func FlushPendingSavings() {
	pendingSteps.mu.Lock()
	dirs := make([]string, 0, len(pendingSteps.steps))
	for dir := range pendingSteps.steps {
		dirs = append(dirs, dir)
	}
	pendingSteps.mu.Unlock()
	sort.Strings(dirs)
	for _, dir := range dirs {
		flushSavingsSteps(dir)
	}
}
