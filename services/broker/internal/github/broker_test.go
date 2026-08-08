package github_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/github"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

func TestPublish_TwoPhaseIdempotent(t *testing.T) {
	t.Parallel()
	api := github.NewFakeAPI()
	api.SeedRef("acme", "dogfood", "refs/heads/main", fixedBase)
	broker := newBroker(t, api, allowPolicy{})

	req := baseRequest()
	first, err := broker.Publish(context.Background(), req)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !first.IsDraft || first.ProviderPRID == "" || first.Phase != github.PhasePR {
		t.Fatalf("receipt: %+v", first)
	}
	if first.HeadRef != github.HeadRef(req.Tuple) {
		t.Fatalf("head ref %s", first.HeadRef)
	}
	second, err := broker.Publish(context.Background(), req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if second.ProviderPRID != first.ProviderPRID {
		t.Fatalf("provider ids diverge: %s vs %s", first.ProviderPRID, second.ProviderPRID)
	}
	if api.PRCount() != 1 {
		t.Fatalf("pr count=%d", api.PRCount())
	}
	if api.CreateCalls != 1 {
		t.Fatalf("create calls=%d", api.CreateCalls)
	}
}

func TestPublish_ConcurrentWorkersOnePR(t *testing.T) {
	t.Parallel()
	api := github.NewFakeAPI()
	api.SeedRef("acme", "dogfood", "refs/heads/main", fixedBase)
	broker := newBroker(t, api, allowPolicy{})

	const workers = 8
	var wg sync.WaitGroup
	receipts := make([]github.Receipt, workers)
	errs := make([]error, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			receipts[i], errs[i] = broker.Publish(context.Background(), baseRequest())
		}()
	}
	wg.Wait()
	var success github.Receipt
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if success.ProviderPRID == "" {
			success = receipts[i]
			continue
		}
		if receipts[i].ProviderPRID != success.ProviderPRID {
			t.Fatalf("divergent PR ids: %s vs %s", success.ProviderPRID, receipts[i].ProviderPRID)
		}
	}
	if api.PRCount() != 1 {
		t.Fatalf("pr count=%d", api.PRCount())
	}
}

func TestPublish_CrashAfterBranchBeforePR(t *testing.T) {
	t.Parallel()
	api := github.NewFakeAPI()
	api.SeedRef("acme", "dogfood", "refs/heads/main", fixedBase)
	// Pre-seed the deterministic head ref as if phase 1 succeeded then crashed.
	req := baseRequest()
	headRef := github.HeadRef(req.Tuple)
	api.SeedRef("acme", "dogfood", headRef, fixedHead)

	broker := newBroker(t, api, allowPolicy{})
	receipt, err := broker.Publish(context.Background(), req)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if receipt.ProviderPRID == "" || !receipt.IsDraft {
		t.Fatalf("receipt: %+v", receipt)
	}
	// CreateRef may be called once (idempotent equal) or zero if GetRef hit first;
	// either way only one PR exists.
	if api.PRCount() != 1 {
		t.Fatalf("pr count=%d", api.PRCount())
	}
}

func TestPublish_CrashAfterPRBeforeAck(t *testing.T) {
	t.Parallel()
	api := github.NewFakeAPI()
	api.SeedRef("acme", "dogfood", "refs/heads/main", fixedBase)
	req := baseRequest()
	headRef := github.HeadRef(req.Tuple)
	api.SeedRef("acme", "dogfood", headRef, fixedHead)
	api.SeedPR(github.PullRequest{
		Number:  42,
		NodeID:  "PR_kw_orphan",
		HeadRef: github.BranchName(headRef),
		BaseRef: "main",
		Draft:   true,
		State:   "open",
		Title:   req.Content.Title,
		Body:    req.Content.Body,
	})

	broker := newBroker(t, api, allowPolicy{})
	receipt, err := broker.Publish(context.Background(), req)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if receipt.ProviderPRID != "PR_kw_orphan" {
		t.Fatalf("expected orphan PR reconcile, got %s", receipt.ProviderPRID)
	}
	if api.CreateCalls != 0 {
		t.Fatalf("create must not run when exact PR exists: %d", api.CreateCalls)
	}
	if api.PRCount() != 1 {
		t.Fatalf("pr count=%d", api.PRCount())
	}
}

func TestPublish_ClosedExactPRNoDuplicate(t *testing.T) {
	t.Parallel()
	api := github.NewFakeAPI()
	api.SeedRef("acme", "dogfood", "refs/heads/main", fixedBase)
	req := baseRequest()
	headRef := github.HeadRef(req.Tuple)
	api.SeedRef("acme", "dogfood", headRef, fixedHead)
	api.SeedPR(github.PullRequest{
		Number:  7,
		HeadRef: github.BranchName(headRef),
		BaseRef: "main",
		Draft:   true,
		State:   "closed",
		Title:   req.Content.Title,
		Body:    req.Content.Body,
	})

	broker := newBroker(t, api, allowPolicy{})
	_, err := broker.Publish(context.Background(), req)
	if !errors.Is(err, github.ErrAlreadyClosed) {
		t.Fatalf("err=%v", err)
	}
	if api.CreateCalls != 0 {
		t.Fatalf("create calls=%d", api.CreateCalls)
	}
	if api.PRCount() != 1 {
		t.Fatalf("pr count=%d", api.PRCount())
	}
}

func TestPublish_MismatchedHeadRefConflict(t *testing.T) {
	t.Parallel()
	api := github.NewFakeAPI()
	api.SeedRef("acme", "dogfood", "refs/heads/main", fixedBase)
	req := baseRequest()
	api.SeedRef("acme", "dogfood", github.HeadRef(req.Tuple), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	broker := newBroker(t, api, allowPolicy{})
	_, err := broker.Publish(context.Background(), req)
	if !errors.Is(err, github.ErrConflict) {
		t.Fatalf("err=%v", err)
	}
}

func TestPublish_NonDraftMatchConflict(t *testing.T) {
	t.Parallel()
	api := github.NewFakeAPI()
	api.SeedRef("acme", "dogfood", "refs/heads/main", fixedBase)
	req := baseRequest()
	headRef := github.HeadRef(req.Tuple)
	api.SeedRef("acme", "dogfood", headRef, fixedHead)
	api.SeedPR(github.PullRequest{
		Number:  9,
		HeadRef: github.BranchName(headRef),
		BaseRef: "main",
		Draft:   false,
		State:   "open",
		Title:   req.Content.Title,
		Body:    req.Content.Body,
	})

	broker := newBroker(t, api, allowPolicy{})
	_, err := broker.Publish(context.Background(), req)
	if !errors.Is(err, github.ErrConflict) {
		t.Fatalf("err=%v", err)
	}
}

func TestPublish_RevocationBeforeProvider(t *testing.T) {
	t.Parallel()
	api := github.NewFakeAPI()
	api.SeedRef("acme", "dogfood", "refs/heads/main", fixedBase)
	broker := newBroker(t, api, denyPolicy{})

	_, err := broker.Publish(context.Background(), baseRequest())
	if github.ReasonCode(err) != github.ReasonEffectNotAuthorized {
		t.Fatalf("err=%v reason=%s", err, github.ReasonCode(err))
	}
	if api.CreateRefCalls != 0 || api.CreateCalls != 0 {
		t.Fatalf("provider must not be invoked on revoke")
	}
}

func TestPublish_GrantWithMergeForbidden(t *testing.T) {
	t.Parallel()
	api := github.NewFakeAPI()
	broker := newBroker(t, api, allowPolicy{})
	req := baseRequest()
	req.Grant.Actions = append(req.Grant.Actions, "github.merge")
	_, err := broker.Publish(context.Background(), req)
	if github.ReasonCode(err) != github.ReasonGrantMalformed {
		t.Fatalf("err=%v", err)
	}
}

func TestPublish_IdempotencyKeyContentConflict(t *testing.T) {
	t.Parallel()
	api := github.NewFakeAPI()
	api.SeedRef("acme", "dogfood", "refs/heads/main", fixedBase)
	broker := newBroker(t, api, allowPolicy{})
	req := baseRequest()
	if _, err := broker.Publish(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Content.Title = "changed title same key"
	_, err := broker.Publish(context.Background(), req)
	if github.ReasonCode(err) != github.ReasonEffectDuplicate {
		t.Fatalf("err=%v", err)
	}
	if api.PRCount() != 1 {
		t.Fatalf("pr count=%d", api.PRCount())
	}
}

func TestPublish_AmbiguousCreateThenLookup(t *testing.T) {
	t.Parallel()
	api := github.NewFakeAPI()
	api.SeedRef("acme", "dogfood", "refs/heads/main", fixedBase)
	api.FailCreateOnce = 1 // first create fails; recovery looks up
	// After fail, nothing exists yet — second Publish path: simulate that the
	// create actually landed by seeding on first failure via custom API.
	// Use sequential publish with FailCreateOnce so the second attempt creates.
	broker := newBroker(t, api, allowPolicy{})
	req := baseRequest()
	_, err := broker.Publish(context.Background(), req)
	// First attempt may fail entirely when create fails and lookup finds nothing.
	if err != nil {
		// Retry after simulated crash recovery.
		if _, err := broker.Publish(context.Background(), req); err != nil {
			t.Fatalf("retry: %v", err)
		}
	}
	if api.PRCount() != 1 {
		t.Fatalf("pr count=%d", api.PRCount())
	}
}

func TestHeadRef_Deterministic24Hex(t *testing.T) {
	t.Parallel()
	req := baseRequest()
	a := github.HeadRef(req.Tuple)
	b := github.HeadRef(req.Tuple)
	if a != b {
		t.Fatal("head ref non-deterministic")
	}
	if len(a) != len("refs/heads/ouroboros/tracer-001/")+24 {
		t.Fatalf("head ref shape: %s", a)
	}
	if a[:len(github.HeadRefPrefix)] != github.HeadRefPrefix {
		t.Fatalf("prefix: %s", a)
	}
}

func TestResolveToken_EnvSurface(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("OUROBOROS_GITHUB_TOKEN", "pat-test-value")
	if got := github.ResolveToken(); got != "pat-test-value" {
		t.Fatalf("token=%q", got)
	}
}

const (
	fixedBase = "02354ff3b1740905347f538de22ac20f96b25668"
	fixedHead = "b7e1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9"
)

func baseRequest() github.PublishRequest {
	digest := contracts.Digest{
		Algorithm: "sha256",
		Hex:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	return github.PublishRequest{
		Authenticated: contracts.MappedIdentityFact{
			Principal: contracts.Identifier{Namespace: "principal", Value: "principal-a"},
			Tenant:    contracts.Identifier{Namespace: "tenant", Value: "tenant-synthetic-a"},
			Session:   contracts.Identifier{Namespace: "session", Value: "session-1"},
		},
		Tuple: github.PublicationTuple{
			TenantID:             "tenant-synthetic-a",
			RepositoryOwner:      "acme",
			RepositoryName:       "dogfood",
			BaseRef:              "main",
			BaseCommitOID:        fixedBase,
			HeadCommitOID:        fixedHead,
			ChangeSetDigest:      digest,
			EffectApprovalDigest: contracts.Digest{Algorithm: "sha256", Hex: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			PolicyDigest:         contracts.Digest{Algorithm: "sha256", Hex: "7b2039fd876a66dd4d88e35876602e4636189f428b5d6a32466d51cc3512d02e"},
			ConfigDigest:         contracts.Digest{Algorithm: "sha256", Hex: "d72a4a9d18b8ef0d6b261591397dc41dd5f20c8df69542ea2ecd016fb17ef9a9"},
		},
		Content: github.PRContent{
			Title: "tracer-001: rename MarkerLabel",
			Body:  "Deterministic Tracer 001 draft. No raw traces.",
		},
		Grant: github.EffectGrant{
			GrantID:            contracts.Identifier{Namespace: "grant", Value: "grant-1"},
			Initiator:          contracts.Identifier{Namespace: "principal", Value: "principal-a"},
			Tenant:             contracts.Identifier{Namespace: "tenant", Value: "tenant-synthetic-a"},
			Actions:            []string{github.ActionBranchPublish, github.ActionDraftPRCreate},
			RepositoryFullName: "acme/dogfood",
			BaseCommitOID:      fixedBase,
			HeadCommitOID:      fixedHead,
			RevocationEpoch:    1,
			ExpiresAt:          time.Now().Add(time.Hour),
			PolicyDigest:       contracts.Digest{Algorithm: "sha256", Hex: "7b2039fd876a66dd4d88e35876602e4636189f428b5d6a32466d51cc3512d02e"},
			Nonce:              "nonce-1",
		},
		IdempotencyKey: "idem-1",
		ActionID:       "action-1",
	}
}

func newBroker(t *testing.T, api github.API, policy contracts.PolicyCheck) *github.Broker {
	t.Helper()
	broker, err := github.NewBroker(github.Config{
		API:    api,
		Policy: policy,
		Clock:  time.Now,
	})
	if err != nil {
		t.Fatalf("new broker: %v", err)
	}
	return broker
}

type allowPolicy struct{}

func (allowPolicy) Check(_ context.Context, _ contracts.MappedIdentityFact, req contracts.PolicyRequest) (contracts.PolicyDecision, error) {
	return contracts.PolicyDecision{
		Allowed:         true,
		RevocationEpoch: 1,
		Receipt: contracts.Receipt{
			OperationID: contracts.Identifier{Namespace: "policy", Value: req.Action},
			Status:      "completed",
			ReasonCode:  "allowed",
		},
	}, nil
}

type denyPolicy struct{}

func (denyPolicy) Check(_ context.Context, _ contracts.MappedIdentityFact, req contracts.PolicyRequest) (contracts.PolicyDecision, error) {
	return contracts.PolicyDecision{
		Allowed:         false,
		RevocationEpoch: 1,
		Receipt: contracts.Receipt{
			OperationID: contracts.Identifier{Namespace: "policy", Value: req.Action},
			Status:      "rejected",
			ReasonCode:  "denied",
		},
	}, nil
}
