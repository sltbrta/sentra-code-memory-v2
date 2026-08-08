package queryapi

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/proto"
)

type fakeEngine struct {
	mu                 sync.Mutex
	answerResult       EngineResult
	answerErr          error
	statusResult       EngineStatus
	statusErr          error
	answerCalls        []EngineQuery
	statusCalls        int
	blockUntilCancel   bool
	cancelDuringAnswer context.CancelFunc
}

func (e *fakeEngine) Answer(ctx context.Context, query EngineQuery) (EngineResult, error) {
	e.mu.Lock()
	e.answerCalls = append(e.answerCalls, query)
	block, cancel := e.blockUntilCancel, e.cancelDuringAnswer
	e.mu.Unlock()
	if block {
		<-ctx.Done()
		return EngineResult{}, ctx.Err()
	}
	if cancel != nil {
		cancel()
	}
	return e.answerResult, e.answerErr
}

func (e *fakeEngine) Status(_ context.Context, _ Principal, _ string) (EngineStatus, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.statusCalls++
	return e.statusResult, e.statusErr
}

func (e *fakeEngine) answers() []EngineQuery {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]EngineQuery(nil), e.answerCalls...)
}

type fakeConversations struct {
	mu            sync.Mutex
	admitResult   AdmissionResult
	admitErr      error
	admitCalls    []Admission
	completeErr   error
	completeCalls []Completion
	resolveResult Resolution
	resolveErr    error
	resolveCalls  int
	historyResult HistoryPage
	historyErr    error
	historyCalls  int
}

func (s *fakeConversations) Admit(_ context.Context, admission Admission) (AdmissionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.admitCalls = append(s.admitCalls, admission)
	return s.admitResult, s.admitErr
}

func (s *fakeConversations) Complete(_ context.Context, completion Completion) (CompletionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completeCalls = append(s.completeCalls, completion)
	return CompletionResult{AssistantTurnID: "turn-a", Sequence: 2}, s.completeErr
}

func (s *fakeConversations) Resolve(_ context.Context, _, _, _ string) (Resolution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolveCalls++
	return s.resolveResult, s.resolveErr
}

func (s *fakeConversations) History(_ context.Context, _, _, _ string, _ uint32) (HistoryPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.historyCalls++
	return s.historyResult, s.historyErr
}

func (s *fakeConversations) admissions() []Admission {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Admission(nil), s.admitCalls...)
}

func (s *fakeConversations) completions() []Completion {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Completion(nil), s.completeCalls...)
}

type fakeCatalog struct {
	listResult   SourcePage
	listErr      error
	factsResult  GenerationFacts
	factsErr     error
	reference    SourceFacts
	referenceErr error
}

func (c *fakeCatalog) List(_ context.Context, _ Principal, _ string, _ uint32) (SourcePage, error) {
	return c.listResult, c.listErr
}

func (c *fakeCatalog) Facts(_ context.Context, _ Principal, _, _ string) (GenerationFacts, error) {
	return c.factsResult, c.factsErr
}

func (c *fakeCatalog) Reference(_ context.Context, _ Principal, _ string) (SourceFacts, error) {
	return c.reference, c.referenceErr
}

type fakeAuthorizer struct {
	mu       sync.Mutex
	decision Decision
	err      error
	calls    []struct {
		action   Action
		resource string
	}
}

func (a *fakeAuthorizer) Authorize(_ context.Context, _ Principal, action Action, resource string) (Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, struct {
		action   Action
		resource string
	}{action, resource})
	return a.decision, a.err
}

func (a *fakeAuthorizer) authorizations() []struct {
	action   Action
	resource string
} {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]struct {
		action   Action
		resource string
	}(nil), a.calls...)
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

type handlerFixture struct {
	handler    *Handler
	engine     *fakeEngine
	store      *fakeConversations
	catalog    *fakeCatalog
	authorizer *fakeAuthorizer
	peer       localauthority.PeerContext
}

func newHandlerFixture(t *testing.T) *handlerFixture {
	t.Helper()
	engine := &fakeEngine{answerResult: answeredEngineResult("query-1")}
	store := &fakeConversations{admitResult: AdmissionResult{QueryID: "query-1", UserTurnID: "turn-u"}}
	catalog := &fakeCatalog{
		factsResult: validGenerationFacts(),
		reference:   SourceFacts{SourceID: "source-1", RepositoryID: "repo-1", BrainID: "brain-1", State: "ready"},
	}
	authorizer := &fakeAuthorizer{decision: Decision{Allowed: true, Epoch: 7}}
	handler, err := NewHandler(Config{
		Engine: engine, Conversations: store, Sources: catalog, Authorizer: authorizer,
		Clock:               fixedClock{now: time.UnixMilli(4242).UTC()},
		ConfigurationDigest: shared.Digest{Algorithm: "sha256", Hex: strings.Repeat("f", 64)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &handlerFixture{
		handler: handler, engine: engine, store: store, catalog: catalog,
		authorizer: authorizer, peer: testPeer(),
	}
}

func testPeer() localauthority.PeerContext {
	return localauthority.PeerContext{
		Credentials: localauthority.PeerCredentials{UID: 501, GID: 20, PID: 4242},
		Identity: shared.MappedIdentityFact{
			Principal: shared.Identifier{Namespace: "principal", Value: "p1"},
			Tenant:    shared.Identifier{Namespace: "tenant", Value: "t1"},
			Session:   shared.Identifier{Namespace: "session", Value: "sess1"},
		},
	}
}

func validCaller() *contractsv1.UntrustedQueryCaller {
	return &contractsv1.UntrustedQueryCaller{
		RequestedPrincipal: &contractsv1.AuthenticatedPrincipalRef{
			PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: "p1"},
			TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: "t1"},
			SessionId:   &contractsv1.Identifier{Namespace: "session", Value: "sess1"},
		},
		RequestedSession: &contractsv1.Identifier{Namespace: "session", Value: "sess1"},
	}
}

func validAskRequest() *contractsv1.AskRequest {
	return &contractsv1.AskRequest{
		Caller:         validCaller(),
		SourceId:       &contractsv1.Identifier{Namespace: "source", Value: "source-1"},
		GenerationId:   &contractsv1.Identifier{Namespace: "generation", Value: "gen-1"},
		Query:          "what does ConfigPath return?",
		Freshness:      contractsv1.FreshnessRequirement_FRESHNESS_REQUIREMENT_BEST_EFFORT,
		IdempotencyKey: "key-1",
	}
}

func validGenerationFacts() GenerationFacts {
	lanes := make([]LaneFacts, 0, 5)
	for _, language := range []string{"go", "typescript", "python", "rust", "java"} {
		lanes = append(lanes, LaneFacts{Language: language, Coverage: "syntax_aware"})
	}
	return GenerationFacts{
		GenerationID: "gen-1", Sequence: 1, SnapshotID: "snap-1",
		CommitOID: strings.Repeat("a", 40), TreeOID: strings.Repeat("b", 40),
		PolicyDigest: strings.Repeat("c", 64), State: "ready",
		Readiness: lanes, SourceWatermark: 1,
	}
}

func answeredEngineResult(queryID string) EngineResult {
	return EngineResult{
		Answer: EngineAnswer{
			QueryID: queryID,
			Status:  "answered",
			Prose:   "ConfigPath returns the parsed path.",
			Claims: []EngineClaim{{
				ClaimID:   "claim-0001",
				Statement: "ConfigPath returns the parsed path.",
				Citations: []EngineCitation{{
					EvidenceID: "evidence-1", SourceRevisionID: "revision-1",
					GitOID: strings.Repeat("a", 40), Path: "internal/config/config.go",
					StartLine: 12, StartColumn: 1, EndLine: 14, EndColumn: 2,
					SupportingTextDigest: strings.Repeat("d", 64),
				}},
				ConfidencePerMille: 900,
			}},
			TokenUsage: 120,
		},
		Freshness: EngineFreshness{
			GenerationID: "gen-1", Sequence: 1,
			CommitOID: strings.Repeat("a", 40), TreeOID: strings.Repeat("b", 40),
			GenerationState: "ready", State: "current", ACLEpoch: 7,
			ObservedAt: time.UnixMilli(4000).UTC(),
		},
		Coverage:   EngineCoverage{CanonicalRevisionCount: 5, IndexedRevisionCount: 5},
		Projection: "ready",
	}
}

func assertAskDenied(t *testing.T, response *contractsv1.AskResponse) {
	t.Helper()
	if response == nil {
		t.Fatal("expected a denial outcome, got nil response")
	}
	if err := protovalidate.Validate(response); err != nil {
		t.Fatalf("denial must stay contract-valid: %v", err)
	}
	failure := response.GetError()
	if failure == nil || failure.Code != deniedCode || failure.Render != nil {
		t.Fatalf("denial shape: %+v", response)
	}
	if response.GetSuccess() != nil {
		t.Fatal("denial carried a success outcome")
	}
	receipt := response.GetReceipt()
	if receipt.GetStatus() != contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED ||
		receipt.GetReasonCode() != deniedCode || len(receipt.GetEvidence()) != 0 {
		t.Fatalf("denial receipt: %+v", receipt)
	}
}

func TestAskThreadsAuthorizationAdmissionEngineAndCompletion(t *testing.T) {
	t.Parallel()
	fixture := newHandlerFixture(t)
	response, err := fixture.handler.Ask(context.Background(), fixture.peer, validAskRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := protovalidate.Validate(response); err != nil {
		t.Fatalf("success response violates the contract: %v", err)
	}
	success := response.GetSuccess()
	if success == nil {
		t.Fatalf("expected success: %+v", response)
	}
	if receipt := response.GetReceipt(); receipt.GetStatus() != contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED ||
		receipt.GetReceiptId().GetValue() != "query-1" {
		t.Fatalf("success receipt must bind the admitted query identity: %+v", receipt)
	}
	answer := success.GetAnswer()
	if answer.GetQueryId().GetValue() != "query-1" ||
		answer.GetStatus() != contractsv1.AnswerStatus_ANSWER_STATUS_ANSWERED ||
		len(answer.GetClaims()) != 1 || len(answer.GetDegradedReasons()) != 0 {
		t.Fatalf("answer mapping: %+v", answer)
	}
	claim := answer.GetClaims()[0]
	if claim.GetAuthorityClass() != contractsv1.AuthorityClass_AUTHORITY_CLASS_MODEL_PROPOSAL {
		t.Fatalf("claims must stay model proposals: %+v", claim)
	}
	citation := claim.GetCitations()[0]
	if citation.GetAnchor().GetGitOid() != strings.Repeat("a", 40) ||
		citation.GetAnchor().GetRange().GetPath() != "internal/config/config.go" ||
		citation.GetSupportingTextDigest().GetHex() != strings.Repeat("d", 64) {
		t.Fatalf("citation mapping: %+v", citation)
	}
	generation := success.GetFreshness().GetGeneration()
	if generation.GetSnapshot().GetPolicyDigest().GetHex() != strings.Repeat("c", 64) ||
		len(generation.GetLanguageReadiness()) != 5 {
		t.Fatalf("freshness must carry catalog generation facts: %+v", generation)
	}
	if success.GetFreshness().GetState() != contractsv1.FreshnessState_FRESHNESS_STATE_CURRENT ||
		success.GetFreshness().GetAclEpoch() != 7 {
		t.Fatalf("freshness state: %+v", success.GetFreshness())
	}

	authorizations := fixture.authorizer.authorizations()
	if len(authorizations) != 1 || authorizations[0].action != ActionQuery || authorizations[0].resource != "source-1" {
		t.Fatalf("authorization must precede admission: %+v", authorizations)
	}
	admissions := fixture.store.admissions()
	if len(admissions) != 1 || admissions[0].Text != "what does ConfigPath return?" ||
		admissions[0].Freshness != "best_effort" || admissions[0].IdempotencyKey != "key-1" ||
		admissions[0].Principal.PrincipalID != "p1" {
		t.Fatalf("admission: %+v", admissions)
	}
	answers := fixture.engine.answers()
	if len(answers) != 1 || answers[0].QueryID != "query-1" || answers[0].Principal.Session != "sess1" ||
		answers[0].SourceID != "source-1" || answers[0].GenerationID != "gen-1" {
		t.Fatalf("engine query must carry the admitted identity and peer principal: %+v", answers)
	}
	completions := fixture.store.completions()
	if len(completions) != 1 || completions[0].Failed || completions[0].Result == nil ||
		completions[0].IdempotencyKey != "key-1" {
		t.Fatalf("completion: %+v", completions)
	}
}

func TestAskMapsAbstainedVocabularyThrough(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"synthesis_unavailable", "retrieval_unavailable", "absent_support"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			fixture := newHandlerFixture(t)
			result := answeredEngineResult("query-1")
			result.Answer = EngineAnswer{
				QueryID: "query-1", Status: "abstained", DegradedReasons: []string{reason},
			}
			fixture.engine.answerResult = result
			response, err := fixture.handler.Ask(context.Background(), fixture.peer, validAskRequest())
			if err != nil {
				t.Fatal(err)
			}
			if err := protovalidate.Validate(response); err != nil {
				t.Fatalf("abstained response violates the contract: %v", err)
			}
			answer := response.GetSuccess().GetAnswer()
			if answer.GetStatus() != contractsv1.AnswerStatus_ANSWER_STATUS_ABSTAINED ||
				len(answer.GetDegradedReasons()) != 1 || answer.GetDegradedReasons()[0] != reason ||
				answer.GetProse() != "" || len(answer.GetClaims()) != 0 {
				t.Fatalf("abstained mapping: %+v", answer)
			}
		})
	}
}

func TestAskRejectsMalformedRequestsBeforeAnyPort(t *testing.T) {
	t.Parallel()
	oversizedQuery := validAskRequest()
	oversizedQuery.Query = strings.Repeat("x", 8193)
	oversizedKey := validAskRequest()
	oversizedKey.IdempotencyKey = strings.Repeat("k", 513)
	emptyQuery := validAskRequest()
	emptyQuery.Query = ""
	missingFreshness := validAskRequest()
	missingFreshness.Freshness = contractsv1.FreshnessRequirement_FRESHNESS_REQUIREMENT_UNSPECIFIED
	missingSource := validAskRequest()
	missingSource.SourceId = nil
	missingGeneration := validAskRequest()
	missingGeneration.GenerationId = nil
	missingCaller := validAskRequest()
	missingCaller.Caller = nil

	cases := map[string]*contractsv1.AskRequest{
		"oversized query":           oversizedQuery,
		"oversized idempotency key": oversizedKey,
		"empty query":               emptyQuery,
		"unspecified freshness":     missingFreshness,
		"missing source":            missingSource,
		"missing generation":        missingGeneration,
		"missing caller":            missingCaller,
	}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newHandlerFixture(t)
			response, err := fixture.handler.Ask(context.Background(), fixture.peer, request)
			if !errors.Is(err, ErrInvalidRequest) || response != nil {
				t.Fatalf("err=%v response=%+v", err, response)
			}
			if len(fixture.store.admissions()) != 0 || len(fixture.engine.answers()) != 0 ||
				len(fixture.authorizer.authorizations()) != 0 {
				t.Fatal("malformed request reached a port")
			}
		})
	}
	t.Run("nil request", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		if _, err := fixture.handler.Ask(context.Background(), fixture.peer, nil); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestAskRejectsIdentityMismatchesBeforeAnyPort(t *testing.T) {
	t.Parallel()
	mutate := func(change func(*contractsv1.AskRequest)) *contractsv1.AskRequest {
		request := validAskRequest()
		change(request)
		return request
	}
	cases := map[string]*contractsv1.AskRequest{
		"principal mismatch": mutate(func(r *contractsv1.AskRequest) { r.Caller.RequestedPrincipal.PrincipalId.Value = "other" }),
		"tenant mismatch":    mutate(func(r *contractsv1.AskRequest) { r.Caller.RequestedPrincipal.TenantId.Value = "other" }),
		"principal session mismatch": mutate(func(r *contractsv1.AskRequest) {
			r.Caller.RequestedPrincipal.SessionId.Value = "other"
		}),
		"requested session mismatch": mutate(func(r *contractsv1.AskRequest) { r.Caller.RequestedSession.Value = "other" }),
		"principal namespace mismatch": mutate(func(r *contractsv1.AskRequest) {
			r.Caller.RequestedPrincipal.PrincipalId.Namespace = "other"
		}),
	}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newHandlerFixture(t)
			response, err := fixture.handler.Ask(context.Background(), fixture.peer, request)
			if !errors.Is(err, ErrRequestDenied) || response != nil {
				t.Fatalf("err=%v response=%+v", err, response)
			}
			if len(fixture.store.admissions()) != 0 || len(fixture.engine.answers()) != 0 ||
				len(fixture.authorizer.authorizations()) != 0 {
				t.Fatal("identity mismatch reached a port")
			}
		})
	}
	t.Run("unmapped peer", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		peer := fixture.peer
		peer.Identity.Session.Value = ""
		if _, err := fixture.handler.Ask(context.Background(), peer, validAskRequest()); !errors.Is(err, ErrRequestDenied) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestAskDeniesBeforeAdmissionWhenAuthorizationFails(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*fakeAuthorizer){
		"denial": func(a *fakeAuthorizer) { a.decision.Allowed = false },
		"error":  func(a *fakeAuthorizer) { a.err = errors.New("policy backend down") },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newHandlerFixture(t)
			mutate(fixture.authorizer)
			response, err := fixture.handler.Ask(context.Background(), fixture.peer, validAskRequest())
			if err != nil {
				t.Fatal(err)
			}
			assertAskDenied(t, response)
			if len(fixture.store.admissions()) != 0 || len(fixture.engine.answers()) != 0 {
				t.Fatal("denied ask was admitted or answered")
			}
		})
	}
}

func TestAskEngineScopeFailureCommitsFailedCompletion(t *testing.T) {
	t.Parallel()
	fixture := newHandlerFixture(t)
	fixture.engine.answerErr = ErrUnknownScope
	response, err := fixture.handler.Ask(context.Background(), fixture.peer, validAskRequest())
	if err != nil {
		t.Fatal(err)
	}
	assertAskDenied(t, response)
	completions := fixture.store.completions()
	if len(completions) != 1 || !completions[0].Failed || completions[0].Result != nil {
		t.Fatalf("a failed engine outcome must commit a visibly failed turn: %+v", completions)
	}
}

func TestAskCancellationCommitsNoAssistantTurn(t *testing.T) {
	t.Parallel()
	t.Run("cancelled during engine", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		fixture.engine.blockUntilCancel = true
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()
		response, err := fixture.handler.Ask(ctx, fixture.peer, validAskRequest())
		if !errors.Is(err, context.Canceled) || response != nil {
			t.Fatalf("err=%v response=%+v", err, response)
		}
		if completions := fixture.store.completions(); len(completions) != 0 {
			t.Fatalf("cancellation committed an assistant turn: %+v", completions)
		}
		if admissions := fixture.store.admissions(); len(admissions) != 1 {
			t.Fatalf("an admitted ask keeps its user turn: %+v", admissions)
		}
	})
	t.Run("cancelled between engine and completion", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		fixture.engine.cancelDuringAnswer = cancel
		response, err := fixture.handler.Ask(ctx, fixture.peer, validAskRequest())
		if !errors.Is(err, context.Canceled) || response != nil {
			t.Fatalf("err=%v response=%+v", err, response)
		}
		if completions := fixture.store.completions(); len(completions) != 0 {
			t.Fatalf("cancellation committed an assistant turn: %+v", completions)
		}
	})
	t.Run("cancelled at entry", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := fixture.handler.Ask(ctx, fixture.peer, validAskRequest()); !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
		if len(fixture.store.admissions()) != 0 || len(fixture.engine.answers()) != 0 {
			t.Fatal("cancelled entry reached a port")
		}
	})
}

func TestAskIdempotentReplayReturnsOriginalOutcome(t *testing.T) {
	t.Parallel()
	fixture := newHandlerFixture(t)
	fixture.store.admitResult = AdmissionResult{QueryID: "query-1", UserTurnID: "turn-u", Replayed: true}
	stored := answeredEngineResult("query-1")
	stored.Answer.Prose = "the original answer text"
	fixture.store.resolveResult = Resolution{
		QueryID: "query-1", UserTurnID: "turn-u", SessionID: "sess1",
		Completed: true, Status: "active", Result: &stored, AssistantTurnID: "turn-a",
	}
	response, err := fixture.handler.Ask(context.Background(), fixture.peer, validAskRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := protovalidate.Validate(response); err != nil {
		t.Fatalf("replay response violates the contract: %v", err)
	}
	answer := response.GetSuccess().GetAnswer()
	if answer.GetProse() != "the original answer text" || answer.GetQueryId().GetValue() != "query-1" {
		t.Fatalf("replay must return the original outcome: %+v", answer)
	}
	if len(fixture.engine.answers()) != 0 {
		t.Fatal("replay re-ran the engine")
	}
	if len(fixture.store.completions()) != 0 {
		t.Fatal("replay committed a second completion")
	}
}

func TestAskReplayOfFailedOrInterruptedQueryDenies(t *testing.T) {
	t.Parallel()
	t.Run("failed completion stays terminal", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		fixture.store.admitResult = AdmissionResult{QueryID: "query-1", UserTurnID: "turn-u", Replayed: true}
		fixture.store.resolveResult = Resolution{
			QueryID: "query-1", Completed: true, Status: "failed", AssistantTurnID: "turn-a",
		}
		response, err := fixture.handler.Ask(context.Background(), fixture.peer, validAskRequest())
		if err != nil {
			t.Fatal(err)
		}
		assertAskDenied(t, response)
		if len(fixture.engine.answers()) != 0 {
			t.Fatal("a failed completion was replayed as fact")
		}
	})
	t.Run("crash mid-query marks failed exactly once", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		fixture.store.admitResult = AdmissionResult{QueryID: "query-1", UserTurnID: "turn-u", Replayed: true}
		fixture.store.resolveResult = Resolution{QueryID: "query-1", UserTurnID: "turn-u", SessionID: "sess1"}
		response, err := fixture.handler.Ask(context.Background(), fixture.peer, validAskRequest())
		if err != nil {
			t.Fatal(err)
		}
		assertAskDenied(t, response)
		completions := fixture.store.completions()
		if len(completions) != 1 || !completions[0].Failed {
			t.Fatalf("interrupted admission must be marked failed: %+v", completions)
		}
		if len(fixture.engine.answers()) != 0 {
			t.Fatal("an interrupted query was replayed as fact")
		}
	})
}

func TestAskConflictingIdempotencyDeniesWithoutMutation(t *testing.T) {
	t.Parallel()
	fixture := newHandlerFixture(t)
	fixture.store.admitErr = ErrIdempotencyConflict
	response, err := fixture.handler.Ask(context.Background(), fixture.peer, validAskRequest())
	if err != nil {
		t.Fatal(err)
	}
	assertAskDenied(t, response)
	if len(fixture.engine.answers()) != 0 || len(fixture.store.completions()) != 0 {
		t.Fatal("conflicting key mutated state")
	}
}

func TestAskStoreAndCompletionFailuresFailClosed(t *testing.T) {
	t.Parallel()
	t.Run("admission failure", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		fixture.store.admitErr = errors.New("sqlite: database is locked")
		response, err := fixture.handler.Ask(context.Background(), fixture.peer, validAskRequest())
		if err == nil || response != nil {
			t.Fatalf("err=%v response=%+v", err, response)
		}
	})
	t.Run("completion failure", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		fixture.store.completeErr = errors.New("sqlite: disk full")
		response, err := fixture.handler.Ask(context.Background(), fixture.peer, validAskRequest())
		if err == nil || response != nil {
			t.Fatalf("err=%v response=%+v", err, response)
		}
	})
	t.Run("completion conflict", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		fixture.store.completeErr = ErrCompletionConflict
		response, err := fixture.handler.Ask(context.Background(), fixture.peer, validAskRequest())
		if err != nil {
			t.Fatal(err)
		}
		assertAskDenied(t, response)
	})
	t.Run("facts failure commits failed completion", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		fixture.catalog.factsErr = errors.New("catalog unavailable")
		response, err := fixture.handler.Ask(context.Background(), fixture.peer, validAskRequest())
		if err != nil {
			t.Fatal(err)
		}
		assertAskDenied(t, response)
		if completions := fixture.store.completions(); len(completions) != 1 ||
			!completions[0].Failed || completions[0].Result != nil {
			t.Fatalf("an unreturnable outcome must commit failed, never active: %+v", completions)
		}
	})
}

func TestAskRejectsContractViolatingEngineOutput(t *testing.T) {
	t.Parallel()
	mutate := func(change func(*EngineResult)) EngineResult {
		result := answeredEngineResult("query-1")
		change(&result)
		return result
	}
	cases := map[string]EngineResult{
		"unsafe citation path": mutate(func(r *EngineResult) { r.Answer.Claims[0].Citations[0].Path = "../outside/repo" }),
		"out of vocabulary reason": mutate(func(r *EngineResult) {
			r.Answer.Status = "partial"
			r.Answer.DegradedReasons = []string{"denied_support"}
		}),
		"claims without citations": mutate(func(r *EngineResult) { r.Answer.Claims[0].Citations = nil }),
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newHandlerFixture(t)
			fixture.engine.answerResult = result
			response, err := fixture.handler.Ask(context.Background(), fixture.peer, validAskRequest())
			if !errors.Is(err, ErrInvalidResponse) || response != nil {
				t.Fatalf("err=%v response=%+v", err, response)
			}
			completions := fixture.store.completions()
			if len(completions) != 1 || !completions[0].Failed || completions[0].Result != nil {
				t.Fatalf("invalid output must commit a failed completion, never an active one: %+v", completions)
			}
		})
	}
	t.Run("retry after invalid output replays the failed terminal state", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		fixture.store.admitResult = AdmissionResult{QueryID: "query-1", UserTurnID: "turn-u", Replayed: true}
		fixture.store.resolveResult = Resolution{
			QueryID: "query-1", Completed: true, Status: "failed", AssistantTurnID: "turn-a",
		}
		response, err := fixture.handler.Ask(context.Background(), fixture.peer, validAskRequest())
		if err != nil {
			t.Fatal(err)
		}
		assertAskDenied(t, response)
		if len(fixture.engine.answers()) != 0 || len(fixture.store.completions()) != 0 {
			t.Fatal("a terminally failed key re-ran or re-completed")
		}
	})
}

func TestListSourcesMapsAuthorizedPageAndFiltersUnservable(t *testing.T) {
	t.Parallel()
	fixture := newHandlerFixture(t)
	current := validGenerationFacts()
	fixture.catalog.listResult = SourcePage{
		Sources: []SourceFacts{
			{SourceID: "source-1", RepositoryID: "repo-1", BrainID: "brain-1", State: "ready", Current: &current},
			{SourceID: "source-2", RepositoryID: "repo-1", BrainID: "brain-1", State: "admitted"},
			{SourceID: "source-3", RepositoryID: "repo-1", BrainID: "brain-1", State: "revoked"},
			{SourceID: "source-4", RepositoryID: "repo-1", BrainID: "brain-1", State: "bogus"},
		},
		NextCursor: "cursor-2",
	}
	request := &contractsv1.ListSourcesRequest{Caller: validCaller(), PageSize: 25}
	response, err := fixture.handler.ListSources(context.Background(), fixture.peer, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := protovalidate.Validate(response); err != nil {
		t.Fatalf("list response violates the contract: %v", err)
	}
	sources := response.GetSuccess().GetSources()
	if len(sources) != 2 {
		t.Fatalf("revoked and unknown states must never list: %+v", sources)
	}
	if sources[0].GetSource().GetSourceId().GetValue() != "source-1" ||
		sources[0].GetState() != contractsv1.SourceState_SOURCE_STATE_READY ||
		sources[0].GetCurrentGeneration() == nil ||
		sources[1].GetState() != contractsv1.SourceState_SOURCE_STATE_ADMITTED ||
		sources[1].GetCurrentGeneration() != nil {
		t.Fatalf("source summary mapping: %+v", sources)
	}
	if response.GetSuccess().GetNextCursor().GetToken() != "cursor-2" {
		t.Fatalf("cursor mapping: %+v", response.GetSuccess())
	}
}

func TestListSourcesBoundaryFailures(t *testing.T) {
	t.Parallel()
	t.Run("catalog failure", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		fixture.catalog.listErr = ErrUnknownScope
		response, err := fixture.handler.ListSources(context.Background(), fixture.peer,
			&contractsv1.ListSourcesRequest{Caller: validCaller(), PageSize: 25})
		if err != nil {
			t.Fatal(err)
		}
		if err := protovalidate.Validate(response); err != nil {
			t.Fatalf("denial must stay contract-valid: %v", err)
		}
		if response.GetError().GetCode() != deniedCode {
			t.Fatalf("response: %+v", response)
		}
	})
	t.Run("identity mismatch", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		request := &contractsv1.ListSourcesRequest{Caller: validCaller(), PageSize: 25}
		request.Caller.RequestedSession.Value = "other"
		if _, err := fixture.handler.ListSources(context.Background(), fixture.peer, request); !errors.Is(err, ErrRequestDenied) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("page bound validation", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		for _, size := range []uint32{0, 101} {
			request := &contractsv1.ListSourcesRequest{Caller: validCaller(), PageSize: size}
			if _, err := fixture.handler.ListSources(context.Background(), fixture.peer, request); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("size=%d err=%v", size, err)
			}
		}
	})
}

func TestGetHistoryMapsPrivatePageAfterReauthorization(t *testing.T) {
	t.Parallel()
	fixture := newHandlerFixture(t)
	answer := answeredEngineResult("query-1").Answer
	fixture.store.historyResult = HistoryPage{
		Turns: []HistoryTurn{
			{TurnID: "turn-1", SessionID: "sess1", Sequence: 1, Role: "user", Status: "active",
				OccurredAtMs: 1001, Text: "what does ConfigPath return?"},
			{TurnID: "turn-2", SessionID: "sess1", Sequence: 2, Role: "assistant", Status: "active",
				OccurredAtMs: 1002, Answer: &answer},
			{TurnID: "turn-3", SessionID: "sess1", Sequence: 3, Role: "assistant", Status: "failed",
				OccurredAtMs: 1003},
		},
		NextCursor: "cursor-2",
	}
	request := &contractsv1.GetHistoryRequest{Caller: validCaller(), PageSize: 50}
	response, err := fixture.handler.GetHistory(context.Background(), fixture.peer, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := protovalidate.Validate(response); err != nil {
		t.Fatalf("history response violates the contract: %v", err)
	}
	authorizations := fixture.authorizer.authorizations()
	if len(authorizations) != 1 || authorizations[0].action != ActionHydrate || authorizations[0].resource != historyScope {
		t.Fatalf("history hydration must reauthorize: %+v", authorizations)
	}
	turns := response.GetSuccess().GetTurns()
	if len(turns) != 3 {
		t.Fatalf("turns: %+v", turns)
	}
	if turns[0].GetRole() != contractsv1.ConversationRole_CONVERSATION_ROLE_USER ||
		turns[0].GetText() == "" || turns[0].GetAnswer() != nil {
		t.Fatalf("user turn shape: %+v", turns[0])
	}
	if turns[1].GetRole() != contractsv1.ConversationRole_CONVERSATION_ROLE_ASSISTANT ||
		turns[1].GetAnswer() == nil || turns[1].GetText() != "" ||
		turns[1].GetStatus() != contractsv1.ConversationTurnStatus_CONVERSATION_TURN_STATUS_ACTIVE {
		t.Fatalf("active assistant turn shape: %+v", turns[1])
	}
	if turns[2].GetStatus() != contractsv1.ConversationTurnStatus_CONVERSATION_TURN_STATUS_FAILED ||
		turns[2].GetAnswer() != nil {
		t.Fatalf("failed turn must carry no answer: %+v", turns[2])
	}
	if response.GetSuccess().GetNextCursor().GetToken() != "cursor-2" {
		t.Fatalf("cursor mapping: %+v", response.GetSuccess())
	}
}

func TestGetHistoryBoundaryFailures(t *testing.T) {
	t.Parallel()
	t.Run("authorization denial", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		fixture.authorizer.decision.Allowed = false
		response, err := fixture.handler.GetHistory(context.Background(), fixture.peer,
			&contractsv1.GetHistoryRequest{Caller: validCaller(), PageSize: 50})
		if err != nil {
			t.Fatal(err)
		}
		if response.GetError().GetCode() != deniedCode || fixture.store.historyCalls != 0 {
			t.Fatalf("denial must precede hydration: %+v", response)
		}
	})
	t.Run("store failure covers bad cursors", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		fixture.store.historyErr = errors.New("conversation: invalid input")
		response, err := fixture.handler.GetHistory(context.Background(), fixture.peer,
			&contractsv1.GetHistoryRequest{
				Caller:   validCaller(),
				PageSize: 50,
				After:    &contractsv1.Cursor{Token: "forged"},
			})
		if err != nil {
			t.Fatal(err)
		}
		if response.GetError().GetCode() != deniedCode {
			t.Fatalf("response: %+v", response)
		}
	})
	t.Run("identity mismatch", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		request := &contractsv1.GetHistoryRequest{Caller: validCaller(), PageSize: 50}
		request.Caller.RequestedPrincipal.TenantId.Value = "other"
		if _, err := fixture.handler.GetHistory(context.Background(), fixture.peer, request); !errors.Is(err, ErrRequestDenied) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("page bound validation", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		request := &contractsv1.GetHistoryRequest{Caller: validCaller(), PageSize: 0}
		if _, err := fixture.handler.GetHistory(context.Background(), fixture.peer, request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestGetStatusMapsAuthorizedView(t *testing.T) {
	t.Parallel()
	fixture := newHandlerFixture(t)
	fixture.engine.statusResult = EngineStatus{
		SourceID: "source-1",
		Freshness: EngineFreshness{
			GenerationID: "gen-1", Sequence: 1,
			CommitOID: strings.Repeat("a", 40), TreeOID: strings.Repeat("b", 40),
			GenerationState: "ready", State: "current", ACLEpoch: 7,
			ObservedAt: time.UnixMilli(4000).UTC(),
		},
		Coverage:   EngineCoverage{CanonicalRevisionCount: 5, IndexedRevisionCount: 4},
		Projection: "rebuilding",
	}
	request := &contractsv1.GetStatusRequest{
		Caller:   validCaller(),
		SourceId: &contractsv1.Identifier{Namespace: "source", Value: "source-1"},
	}
	response, err := fixture.handler.GetStatus(context.Background(), fixture.peer, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := protovalidate.Validate(response); err != nil {
		t.Fatalf("status response violates the contract: %v", err)
	}
	success := response.GetSuccess()
	if success.GetSource().GetBrainId().GetValue() != "brain-1" ||
		success.GetProjection() != contractsv1.ProjectionState_PROJECTION_STATE_REBUILDING ||
		success.GetCoverage().GetIndexedRevisionCount() != 4 ||
		success.GetFreshness().GetGeneration().GetGenerationId().GetValue() != "gen-1" {
		t.Fatalf("status mapping: %+v", success)
	}
}

func TestGetStatusBoundaryFailures(t *testing.T) {
	t.Parallel()
	t.Run("unknown revoked or denied scope", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		fixture.engine.statusErr = ErrUnknownScope
		response, err := fixture.handler.GetStatus(context.Background(), fixture.peer,
			&contractsv1.GetStatusRequest{
				Caller:   validCaller(),
				SourceId: &contractsv1.Identifier{Namespace: "source", Value: "source-1"},
			})
		if err != nil {
			t.Fatal(err)
		}
		if response.GetError().GetCode() != deniedCode {
			t.Fatalf("response: %+v", response)
		}
	})
	t.Run("catalog failure", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		fixture.catalog.referenceErr = ErrUnknownScope
		response, err := fixture.handler.GetStatus(context.Background(), fixture.peer,
			&contractsv1.GetStatusRequest{
				Caller:   validCaller(),
				SourceId: &contractsv1.Identifier{Namespace: "source", Value: "source-1"},
			})
		if err != nil {
			t.Fatal(err)
		}
		if response.GetError().GetCode() != deniedCode {
			t.Fatalf("response: %+v", response)
		}
	})
	t.Run("identity mismatch", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		request := &contractsv1.GetStatusRequest{
			Caller:   validCaller(),
			SourceId: &contractsv1.Identifier{Namespace: "source", Value: "source-1"},
		}
		request.Caller.RequestedPrincipal.PrincipalId.Value = "other"
		if _, err := fixture.handler.GetStatus(context.Background(), fixture.peer, request); !errors.Is(err, ErrRequestDenied) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("missing source", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		request := &contractsv1.GetStatusRequest{Caller: validCaller()}
		if _, err := fixture.handler.GetStatus(context.Background(), fixture.peer, request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestHandlerRequiresCompleteConfiguration(t *testing.T) {
	t.Parallel()
	valid := Config{
		Engine: &fakeEngine{}, Conversations: &fakeConversations{}, Sources: &fakeCatalog{},
		Authorizer: &fakeAuthorizer{}, Clock: fixedClock{},
		ConfigurationDigest: shared.Digest{Algorithm: "sha256", Hex: strings.Repeat("f", 64)},
	}
	if _, err := NewHandler(valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"missing engine":         func(c *Config) { c.Engine = nil },
		"missing conversations":  func(c *Config) { c.Conversations = nil },
		"missing sources":        func(c *Config) { c.Sources = nil },
		"missing authorizer":     func(c *Config) { c.Authorizer = nil },
		"missing clock":          func(c *Config) { c.Clock = nil },
		"bad digest":             func(c *Config) { c.ConfigurationDigest.Hex = "zz" },
		"non-hex 64-char digest": func(c *Config) { c.ConfigurationDigest.Hex = strings.Repeat("g", 64) },
		"uppercase digest":       func(c *Config) { c.ConfigurationDigest.Hex = strings.Repeat("F", 64) },
		"short digest":           func(c *Config) { c.ConfigurationDigest.Hex = strings.Repeat("f", 63) },
	} {
		t.Run(name, func(t *testing.T) {
			config := valid
			mutate(&config)
			if _, err := NewHandler(config); !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestResponsesAreConstructedFreshPerRequest(t *testing.T) {
	t.Parallel()
	fixture := newHandlerFixture(t)
	first, err := fixture.handler.Ask(context.Background(), fixture.peer, validAskRequest())
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.handler.Ask(context.Background(), fixture.peer, validAskRequest())
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first.GetSuccess().GetAnswer() == second.GetSuccess().GetAnswer() {
		t.Fatal("responses share mutable state across requests")
	}
	if !proto.Equal(first, second) {
		t.Fatal("identical admitted requests produced divergent outcomes")
	}
	first.GetSuccess().GetAnswer().Prose = "mutated"
	if second.GetSuccess().GetAnswer().GetProse() == "mutated" {
		t.Fatal("mutating one response corrupted another")
	}
}
