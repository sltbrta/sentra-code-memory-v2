package query

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/projections"
)

type receiptAdmissionState struct {
	mu       sync.RWMutex
	slos     []projections.ReceiptSLO
	events   []projections.PropagationEvent
	receipts []projections.PropagationReceipt
	entered  chan struct{}
	release  chan struct{}
	calls    int
}

func (s *receiptAdmissionState) admit(ctx context.Context, request EvidenceAdmissionRequest) bool {
	s.mu.Lock()
	s.calls++
	call := s.calls
	slos := append([]projections.ReceiptSLO(nil), s.slos...)
	events := append([]projections.PropagationEvent(nil), s.events...)
	receipts := append([]projections.PropagationReceipt(nil), s.receipts...)
	s.mu.Unlock()
	// The adversarial first call snapshots before it blocks. This models a
	// receipt source that admitted against the request's original timestamp
	// while a newer canonical event committed during its wait.
	if call == 1 && s.entered != nil {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return false
		}
	}
	return projections.AdmitEvidence(slos, events, receipts, projections.EvidenceAdmissionRequest{
		SourceID:     request.SourceID,
		GenerationID: request.GenerationID,
		ACLEpoch:     request.ACLEpoch,
		At:           request.At,
	}).Allowed
}

func TestAnswerEnforcesPropagationReceiptAdmissionAtEmitBoundary(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	staleID, currentID := generationIDs(t, corpus)
	event, receipts := queryReceiptFixture(currentID)
	query := fixtureQuery(
		"receipt-admission",
		currentID,
		"Which Go function in src/go/modify-00.go returns the stage marker?",
		"best_effort",
	)

	t.Run("complete current receipts emit", func(t *testing.T) {
		state := &receiptAdmissionState{
			slos: projections.DefaultReceiptSLOs(testSourceID), events: []projections.PropagationEvent{event}, receipts: receipts,
		}
		result := answerWithReceiptAdmission(t, corpus, state, query)
		if result.Answer.Status != StatusAnswered || len(result.Answer.Claims) == 0 || len(flattenCitations(result.Answer)) == 0 {
			t.Fatalf("complete current receipt set must answer with evidence: %#v", result.Answer)
		}
	})

	t.Run("superseded generation emits nothing", func(t *testing.T) {
		staleEvent, staleReceipts := queryReceiptFixture(staleID)
		staleEvent.GenerationAt = testNow.Add(-4 * time.Minute)
		for i := range staleReceipts {
			staleReceipts[i].GenerationAt = staleEvent.GenerationAt
			staleReceipts[i].ReflectedAt = staleEvent.GenerationAt.Add(time.Second)
		}
		state := &receiptAdmissionState{
			slos:     projections.DefaultReceiptSLOs(testSourceID),
			events:   []projections.PropagationEvent{staleEvent, event},
			receipts: append(staleReceipts, receipts...),
		}
		staleQuery := query
		staleQuery.QueryID = "query-receipt-admission-stale"
		staleQuery.GenerationID = staleID
		result := answerWithReceiptAdmission(t, corpus, state, staleQuery)
		assertNoEmittedEvidence(t, result)
	})

	t.Run("missing surface emits nothing", func(t *testing.T) {
		partial := append([]projections.PropagationReceipt(nil), receipts...)
		partial = partial[:len(partial)-1]
		state := &receiptAdmissionState{
			slos: projections.DefaultReceiptSLOs(testSourceID), events: []projections.PropagationEvent{event}, receipts: partial,
		}
		result := answerWithReceiptAdmission(t, corpus, state, query)
		assertReceiptAdmissionAbstention(t, result)
	})

	t.Run("canonical tombstone emits nothing", func(t *testing.T) {
		deleted := []projections.PropagationEvent{event, queryTombstoneEvent(event, testNow.Add(-time.Second))}
		state := &receiptAdmissionState{
			slos: projections.DefaultReceiptSLOs(testSourceID), events: deleted, receipts: receipts,
		}
		result := answerWithReceiptAdmission(t, corpus, state, query)
		assertReceiptAdmissionAbstention(t, result)
	})
}

func TestAnswerFailsClosedWhenTombstoneLandsDuringReceiptAdmission(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	event, receipts := queryReceiptFixture(currentID)
	state := &receiptAdmissionState{
		slos: projections.DefaultReceiptSLOs(testSourceID), events: []projections.PropagationEvent{event}, receipts: receipts,
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	query := fixtureQuery(
		"receipt-tombstone-race",
		currentID,
		"Which Go function in src/go/modify-00.go returns the stage marker?",
		"best_effort",
	)

	type answerResult struct {
		result Result
		err    error
	}
	clock := &sequenceClock{times: []time.Time{
		testNow.Add(-time.Minute),    // result observation
		testNow,                      // first receipt admission
		testNow.Add(2 * time.Second), // post-admission receipt recheck
	}}
	engine := newReceiptAdmissionEngineWithClock(t, corpus, state, clock)
	done := make(chan answerResult, 1)
	go func() {
		result, err := engine.Answer(context.Background(), query)
		done <- answerResult{result: result, err: err}
	}()
	<-state.entered
	state.mu.Lock()
	state.events = append(state.events, queryTombstoneEvent(event, testNow.Add(time.Second)))
	state.mu.Unlock()
	close(state.release)

	got := <-done
	if got.err != nil {
		t.Fatalf("Answer: %v", got.err)
	}
	assertReceiptAdmissionAbstention(t, got.result)
	state.mu.RLock()
	calls := state.calls
	state.mu.RUnlock()
	if calls != 2 {
		t.Fatalf("receipt admission calls = %d, want initial admission plus final recheck", calls)
	}
}

type sequenceClock struct {
	mu    sync.Mutex
	times []time.Time
	next  int
}

func (c *sequenceClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.next >= len(c.times) {
		return c.times[len(c.times)-1]
	}
	now := c.times[c.next]
	c.next++
	return now
}

func queryReceiptFixture(generationID string) (projections.PropagationEvent, []projections.PropagationReceipt) {
	generationAt := testNow.Add(-2 * time.Minute)
	event := projections.PropagationEvent{
		EventID:      "query-index-" + generationID,
		SourceID:     testSourceID,
		Operation:    projections.ReceiptIndex,
		GenerationID: generationID,
		GenerationAt: generationAt,
	}
	receipts := make([]projections.PropagationReceipt, 0, len(projections.ReceiptSurfaces()))
	for _, surface := range projections.ReceiptSurfaces() {
		receipts = append(receipts, projections.PropagationReceipt{
			ReceiptID:           event.EventID + "-" + string(surface),
			EventID:             event.EventID,
			SourceID:            event.SourceID,
			Surface:             surface,
			Operation:           event.Operation,
			GenerationID:        generationID,
			CurrentGenerationID: generationID,
			GenerationAt:        generationAt,
			ReflectedAt:         generationAt.Add(time.Second),
			Attempt:             1,
			Succeeded:           true,
		})
	}
	return event, receipts
}

func queryTombstoneEvent(generation projections.PropagationEvent, tombstoneAt time.Time) projections.PropagationEvent {
	return projections.PropagationEvent{
		EventID:      "query-delete-" + generation.GenerationID,
		SourceID:     generation.SourceID,
		Operation:    projections.ReceiptDelete,
		GenerationID: generation.GenerationID,
		GenerationAt: generation.GenerationAt,
		TombstoneAt:  tombstoneAt,
	}
}

func newReceiptAdmissionEngine(t *testing.T, corpus Corpus, state *receiptAdmissionState) *Engine {
	return newReceiptAdmissionEngineWithClock(t, corpus, state, stubClock{now: testNow})
}

func newReceiptAdmissionEngineWithClock(t *testing.T, corpus Corpus, state *receiptAdmissionState, clock Clock) *Engine {
	t.Helper()
	engine, err := NewEngine(Config{
		Corpus:           corpus,
		Authorizer:       &stubAuthorizer{epoch: 7},
		Synthesizer:      NewDeterministicSynthesizer(),
		Clock:            clock,
		Limits:           DefaultLimits(),
		EvidenceAdmitter: EvidenceAdmitterFunc(state.admit),
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func answerWithReceiptAdmission(t *testing.T, corpus Corpus, state *receiptAdmissionState, query Query) Result {
	t.Helper()
	result, err := newReceiptAdmissionEngine(t, corpus, state).Answer(context.Background(), query)
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	return result
}

func assertReceiptAdmissionAbstention(t *testing.T, result Result) {
	t.Helper()
	assertNoEmittedEvidence(t, result)
	if len(result.Answer.DegradedReasons) != 1 || result.Answer.DegradedReasons[0] != ReasonAbsentSupport {
		t.Fatalf("receipt admission denial must emit no evidence: %#v", result.Answer)
	}
}

func assertNoEmittedEvidence(t *testing.T, result Result) {
	t.Helper()
	if result.Answer.Status != StatusAbstained || result.Answer.Prose != "" || len(result.Answer.Claims) != 0 {
		t.Fatalf("receipt admission denial must emit no evidence: %#v", result.Answer)
	}
}
