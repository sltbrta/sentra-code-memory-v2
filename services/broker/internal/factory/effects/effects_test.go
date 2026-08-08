package effects_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/factory/changeset"
	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/factory/effects"
	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/factory/gitcandidate"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

const testBaseOID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

var testNow = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

// fakePolicy is a controllable current-policy port. It denies stale epochs
// exactly like the Stage 02 evaluator and allows the listed action|resource
// pairs at the current epoch.
type fakePolicy struct {
	mu      sync.Mutex
	epoch   uint64
	allowed map[string]bool
}

func (f *fakePolicy) Check(_ context.Context, _ contracts.MappedIdentityFact, request contracts.PolicyRequest) (contracts.PolicyDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	decision := contracts.PolicyDecision{
		Allowed:         false,
		RevocationEpoch: f.epoch,
		Receipt:         contracts.Receipt{Status: "rejected", ReasonCode: "not_found_or_denied"},
	}
	if request.RevocationEpoch != f.epoch {
		return decision, nil
	}
	if f.allowed[request.Action+"|"+request.Resource.Value] {
		decision.Allowed = true
		decision.Receipt.Status = "completed"
		decision.Receipt.ReasonCode = "allowed"
	}
	return decision, nil
}

// fakeFences is a controllable lease fence registry.
type fakeFences struct {
	mu     sync.Mutex
	fence  uint64
	expiry time.Time
	ok     bool
}

func (f *fakeFences) CurrentFence(context.Context, contracts.Identifier) (uint64, time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fence, f.expiry, f.ok
}

func (f *fakeFences) bump() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fence++
}

func testLeaf() effects.Leaf {
	return effects.Leaf{
		Identity: contracts.MappedIdentityFact{
			Principal: contracts.Identifier{Namespace: "principal", Value: "p1"},
			Tenant:    contracts.Identifier{Namespace: "tenant", Value: "t1"},
			Session:   contracts.Identifier{Namespace: "session", Value: "s1"},
		},
		Grant: effects.Grant{
			GrantID:   contracts.Identifier{Namespace: "grant", Value: "g1"},
			Initiator: contracts.Identifier{Namespace: "principal", Value: "p1"},
			Tenant:    contracts.Identifier{Namespace: "tenant", Value: "t1"},
			TaskID:    contracts.Identifier{Namespace: "task", Value: "task-1"},
			RunID:     contracts.Identifier{Namespace: "run", Value: "run-1"},
			Lease: effects.Lease{
				LeaseID:   contracts.Identifier{Namespace: "lease", Value: "l1"},
				Holder:    contracts.Identifier{Namespace: "principal", Value: "p1"},
				Fence:     7,
				ExpiresAt: testNow.Add(time.Hour),
			},
			Actions:          []string{effects.ActionFileRead, effects.ActionFileWrite},
			Resources:        []contracts.Identifier{{Namespace: "repository", Value: "repo-1"}},
			RepositoryGitOID: testBaseOID,
			AllowedPaths:     []string{"src/go"},
			Nonce:            "nonce-1",
			RevocationEpoch:  3,
			ExpiresAt:        testNow.Add(time.Hour),
			PolicyDigest:     changeset.DigestBytes([]byte("policy")),
			CommandFence:     1,
		},
		Scope: effects.Scope{
			OwnedPaths:     []string{"src/go/modify-00.go"},
			ForbiddenPaths: []string{"src/go/protected"},
			BaseGitOID:     testBaseOID,
		},
	}
}

func newBroker(t *testing.T) (*effects.Broker, *fakePolicy, *fakeFences) {
	t.Helper()
	policy := &fakePolicy{
		epoch: 3,
		allowed: map[string]bool{
			"file.read|repo-1":  true,
			"file.write|repo-1": true,
		},
	}
	fences := &fakeFences{fence: 7, expiry: testNow.Add(time.Hour), ok: true}
	broker, err := effects.NewBroker(policy, fences, func() time.Time { return testNow })
	if err != nil {
		t.Fatal(err)
	}
	return broker, policy, fences
}

func writeRequest(path string) effects.Request {
	return effects.Request{
		Action:   effects.ActionFileWrite,
		Path:     path,
		Resource: contracts.Identifier{Namespace: "repository", Value: "repo-1"},
		Now:      testNow,
	}
}

func TestAuthorizeAllowsInScopeEffect(t *testing.T) {
	broker, _, _ := newBroker(t)
	if err := broker.Authorize(context.Background(), testLeaf(), writeRequest("src/go/modify-00.go")); err != nil {
		t.Fatalf("Authorize = %v, want nil", err)
	}
}

func TestAuthorizeDeniesEscapeAttempts(t *testing.T) {
	broker, _, _ := newBroker(t)
	leaf := testLeaf()
	cases := map[string]struct {
		request effects.Request
		grant   func(*effects.Grant)
		reason  string
	}{
		"traversal beyond owned paths": {
			request: writeRequest("../outside.go"),
			reason:  effects.ReasonEscapePathShape,
		},
		"absolute path": {
			request: writeRequest("/etc/passwd"),
			reason:  effects.ReasonEscapePathShape,
		},
		"dot-dot segment inside scope": {
			request: writeRequest("src/go/../modify-00.go"),
			reason:  effects.ReasonEscapePathShape,
		},
		"outside owned paths": {
			request: writeRequest("src/typescript/modify-00.ts"),
			reason:  effects.ReasonEscapePathScope,
		},
		"forbidden path": {
			request: writeRequest("src/go/protected/secret.go"),
			reason:  effects.ReasonEscapeForbiddenPath,
		},
		"dispatch action": {
			request: effects.Request{Action: effects.ActionDispatch, Resource: writeRequest("x").Resource, Now: testNow},
			reason:  effects.ReasonEscapeDispatch,
		},
		"task-create action": {
			request: effects.Request{Action: effects.ActionTaskCreate, Resource: writeRequest("x").Resource, Now: testNow},
			reason:  effects.ReasonEscapeDispatch,
		},
		"shell effect beyond sealed surface": {
			request: effects.Request{Action: "shell.exec", Resource: writeRequest("x").Resource, Now: testNow},
			reason:  effects.ReasonEscapeEffectKind,
		},
		"network effect beyond sealed surface": {
			request: effects.Request{Action: "network.egress", Resource: writeRequest("x").Resource, Now: testNow},
			reason:  effects.ReasonEscapeEffectKind,
		},
		"model effect beyond sealed surface": {
			request: effects.Request{Action: "model.invoke", Resource: writeRequest("x").Resource, Now: testNow},
			reason:  effects.ReasonEscapeEffectKind,
		},
		"grant carrying dispatch authority is malformed": {
			request: writeRequest("src/go/modify-00.go"),
			grant: func(grant *effects.Grant) {
				grant.Actions = append(grant.Actions, effects.ActionDispatch)
			},
			reason: effects.ReasonGrantMalformed,
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			caseLeaf := leaf
			if testCase.grant != nil {
				testCase.grant(&caseLeaf.Grant)
			}
			err := broker.Authorize(context.Background(), caseLeaf, testCase.request)
			if err == nil || !errors.Is(err, effects.ErrDenied) {
				t.Fatalf("Authorize = %v, want ErrDenied", err)
			}
			reason := effects.ReasonCode(err)
			if reason != testCase.reason {
				t.Fatalf("reason = %q, want %q", reason, testCase.reason)
			}
			if !effects.IsEscape(err) && testCase.reason != effects.ReasonGrantMalformed {
				t.Fatalf("reason %q must classify as an escape attempt", reason)
			}
		})
	}
}

func TestAuthorizeDeniesStaleFenceLeaseAndEpoch(t *testing.T) {
	broker, policy, fences := newBroker(t)
	leaf := testLeaf()
	request := writeRequest("src/go/modify-00.go")

	fences.bump()
	if err := broker.Authorize(context.Background(), leaf, request); effects.ReasonCode(err) != effects.ReasonStaleFence {
		t.Fatalf("stale fence reason = %q, want %q", effects.ReasonCode(err), effects.ReasonStaleFence)
	}
	fences.fence = 7
	fences.ok = false
	if err := broker.Authorize(context.Background(), leaf, request); effects.ReasonCode(err) != effects.ReasonStaleLease {
		t.Fatalf("missing lease reason = %q, want %q", effects.ReasonCode(err), effects.ReasonStaleLease)
	}
	fences.ok = true
	fences.expiry = testNow.Add(-time.Minute)
	if err := broker.Authorize(context.Background(), leaf, request); effects.ReasonCode(err) != effects.ReasonStaleLease {
		t.Fatalf("expired lease reason = %q, want %q", effects.ReasonCode(err), effects.ReasonStaleLease)
	}
	fences.expiry = testNow.Add(time.Hour)

	policy.epoch = 4
	if err := broker.Authorize(context.Background(), leaf, request); effects.ReasonCode(err) != effects.ReasonPolicyDenied {
		t.Fatalf("revoked epoch reason = %q, want %q", effects.ReasonCode(err), effects.ReasonPolicyDenied)
	}
	policy.epoch = 3
	policy.allowed["file.write|repo-1"] = false
	if err := broker.Authorize(context.Background(), leaf, request); effects.ReasonCode(err) != effects.ReasonPolicyDenied {
		t.Fatalf("policy denial reason = %q, want %q", effects.ReasonCode(err), effects.ReasonPolicyDenied)
	}
}

func TestAuthorizeDeniesIdentityBaseAndExpiryMismatch(t *testing.T) {
	broker, _, _ := newBroker(t)
	leaf := testLeaf()
	request := writeRequest("src/go/modify-00.go")

	wrongPrincipal := leaf
	wrongPrincipal.Identity.Principal = contracts.Identifier{Namespace: "principal", Value: "p2"}
	if err := broker.Authorize(context.Background(), wrongPrincipal, request); effects.ReasonCode(err) != effects.ReasonIdentityMismatch {
		t.Fatalf("identity reason = %q, want %q", effects.ReasonCode(err), effects.ReasonIdentityMismatch)
	}

	wrongBase := leaf
	wrongBase.Scope.BaseGitOID = strings.Repeat("c", 40)
	if err := broker.Authorize(context.Background(), wrongBase, request); effects.ReasonCode(err) != effects.ReasonBaseMismatch {
		t.Fatalf("base reason = %q, want %q", effects.ReasonCode(err), effects.ReasonBaseMismatch)
	}

	expired := leaf
	expired.Grant.ExpiresAt = testNow.Add(-time.Minute)
	if err := broker.Authorize(context.Background(), expired, request); effects.ReasonCode(err) != effects.ReasonGrantMalformed {
		t.Fatalf("expired grant reason = %q, want %q", effects.ReasonCode(err), effects.ReasonGrantMalformed)
	}
}

func TestExecuteIsIdempotentAndReauthorizesReplay(t *testing.T) {
	broker, _, fences := newBroker(t)
	leaf := testLeaf()
	runs := 0
	mutate := func(context.Context) error { runs++; return nil }
	request := writeRequest("src/go/modify-00.go")
	request.IdempotencyKey = "effect-1"

	receipt, err := broker.Execute(context.Background(), leaf, request, mutate)
	if err != nil || receipt.Status != "completed" {
		t.Fatalf("first execute = %#v, %v", receipt, err)
	}
	replayed, err := broker.Execute(context.Background(), leaf, request, mutate)
	if err != nil {
		t.Fatalf("exact replay = %v, want original receipt", err)
	}
	if replayed != receipt || runs != 1 {
		t.Fatalf("replay = %#v with %d runs, want identical receipt and exactly one execution", replayed, runs)
	}

	conflicting := request
	conflicting.Path = "src/go/modify-00.go"
	conflicting.Action = effects.ActionFileRead
	if _, err := broker.Execute(context.Background(), leaf, conflicting, mutate); effects.ReasonCode(err) != effects.ReasonIdempotencyConflict {
		t.Fatalf("conflicting reuse = %q, want %q", effects.ReasonCode(err), effects.ReasonIdempotencyConflict)
	}

	// Revocation wins over replay: a fence bump makes the exact retry deny
	// under current policy instead of replaying the original outcome.
	fences.bump()
	if _, err := broker.Execute(context.Background(), leaf, request, mutate); effects.ReasonCode(err) != effects.ReasonStaleFence {
		t.Fatalf("post-revocation replay = %q, want %q", effects.ReasonCode(err), effects.ReasonStaleFence)
	}
	if runs != 1 {
		t.Fatalf("mutation ran %d times, want exactly one", runs)
	}
}

func TestAuthorizeMutationChecksRenamePreimageScope(t *testing.T) {
	broker, _, _ := newBroker(t)
	leaf := testLeaf()
	bound := broker.Bind(leaf)
	edit := changeset.Edit{
		Path: "src/go/modify-00.go", OldPath: "src/typescript/rename-00.ts", Op: changeset.OpRename,
		Lang:         changeset.LanguageGo,
		BeforeDigest: changeset.DigestBytes([]byte("old\n")),
		AfterDigest:  changeset.DigestBytes([]byte("new\n")),
		NewContent:   []byte("new\n"),
	}
	err := bound.AuthorizeMutation(context.Background(), gitcandidate.Mutation{Index: 0, Edit: edit})
	if effects.ReasonCode(err) != effects.ReasonEscapePathScope {
		t.Fatalf("rename pre-image escape = %q, want %q", effects.ReasonCode(err), effects.ReasonEscapePathScope)
	}
	inScope := changeset.Edit{
		Path: "src/go/modify-00.go", Op: changeset.OpModify, Lang: changeset.LanguageGo,
		BeforeDigest: changeset.DigestBytes([]byte("old\n")),
		AfterDigest:  changeset.DigestBytes([]byte("new\n")),
		NewContent:   []byte("new\n"),
	}
	if err := bound.AuthorizeMutation(context.Background(), gitcandidate.Mutation{Index: 0, Edit: inScope}); err != nil {
		t.Fatalf("in-scope mutation = %v, want nil", err)
	}
}

func TestValidateGrantRejectsMalformedBaseOID(t *testing.T) {
	grant := testLeaf().Grant
	for name, oid := range map[string]string{
		"uppercase hex": strings.ToUpper(testBaseOID),
		"short":         "abcd1234",
		"non-hex":       strings.Repeat("g", 40),
		"empty":         "",
	} {
		mutated := grant
		mutated.RepositoryGitOID = oid
		if err := effects.ValidateGrant(mutated, testNow); effects.ReasonCode(err) != effects.ReasonGrantMalformed {
			t.Fatalf("%s: reason = %q, want %q", name, effects.ReasonCode(err), effects.ReasonGrantMalformed)
		}
	}
	if err := effects.ValidateGrant(grant, testNow); err != nil {
		t.Fatalf("valid grant = %v, want nil", err)
	}
}

// TestExecuteConcurrentSameKeyMutatesOnce proves the in-flight placeholder:
// while one execution holds the key, a concurrent same-key call is rejected
// and never mutates.
func TestExecuteConcurrentSameKeyMutatesOnce(t *testing.T) {
	broker, _, _ := newBroker(t)
	leaf := testLeaf()
	request := writeRequest("src/go/modify-00.go")
	request.IdempotencyKey = "effect-concurrent"
	started := make(chan struct{})
	release := make(chan struct{})
	runs := 0
	firstDone := make(chan error, 1)
	go func() {
		_, err := broker.Execute(context.Background(), leaf, request, func(context.Context) error {
			runs++
			close(started)
			<-release
			return nil
		})
		firstDone <- err
	}()
	<-started
	receipt, secondErr := broker.Execute(context.Background(), leaf, request, func(context.Context) error {
		runs++
		return nil
	})
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first execute = %v, want completed", err)
	}
	if effects.ReasonCode(secondErr) != effects.ReasonIdempotencyConflict {
		t.Fatalf("concurrent second = %q, want %q", effects.ReasonCode(secondErr), effects.ReasonIdempotencyConflict)
	}
	if receipt.Status != "rejected" {
		t.Fatalf("concurrent receipt = %q, want rejected", receipt.Status)
	}
	if runs != 1 {
		t.Fatalf("mutation ran %d times under a concurrent same-key call, want exactly one", runs)
	}
	// After the first execution completes, the exact replay returns its receipt.
	replayed, err := broker.Execute(context.Background(), leaf, request, func(context.Context) error {
		runs++
		return nil
	})
	if err != nil || replayed.Status != "completed" || runs != 1 {
		t.Fatalf("post-completion replay = %#v, %v, runs = %d; want the original receipt", replayed, err, runs)
	}
}

// TestCallerSuppliedTimeNeverAuthorizes proves the broker evaluates only its
// own clock: an expired grant denies even when the caller supplies an earlier
// instant at which the grant was still valid.
func TestCallerSuppliedTimeNeverAuthorizes(t *testing.T) {
	broker, _, _ := newBroker(t)
	leaf := testLeaf()
	leaf.Grant.ExpiresAt = testNow.Add(-time.Minute)
	request := writeRequest("src/go/modify-00.go")
	request.Now = testNow.Add(-2 * time.Minute)
	if err := broker.Authorize(context.Background(), leaf, request); effects.ReasonCode(err) != effects.ReasonGrantMalformed {
		t.Fatalf("expired grant with earlier supplied Now = %q, want %q",
			effects.ReasonCode(err), effects.ReasonGrantMalformed)
	}
	runs := 0
	request.IdempotencyKey = "effect-time"
	if _, err := broker.Execute(context.Background(), leaf, request, func(context.Context) error {
		runs++
		return nil
	}); effects.ReasonCode(err) != effects.ReasonGrantMalformed || runs != 0 {
		t.Fatalf("expired grant execute = %q with %d runs, want denial and zero runs",
			effects.ReasonCode(err), runs)
	}
}

// TestEffectKeysAreNamespacedByGrant proves two grants may reuse one
// idempotency key: each grant's scope executes once and replays within its
// own scope only.
func TestEffectKeysAreNamespacedByGrant(t *testing.T) {
	broker, _, _ := newBroker(t)
	first := testLeaf()
	second := testLeaf()
	second.Grant.GrantID = contracts.Identifier{Namespace: "grant", Value: "g2"}
	second.Grant.Nonce = "nonce-2"
	runs := 0
	mutate := func(context.Context) error { runs++; return nil }
	request := writeRequest("src/go/modify-00.go")
	request.IdempotencyKey = "shared-effect"

	receipt1, err := broker.Execute(context.Background(), first, request, mutate)
	if err != nil || receipt1.Status != "completed" {
		t.Fatalf("first grant execute = %#v, %v", receipt1, err)
	}
	receipt2, err := broker.Execute(context.Background(), second, request, mutate)
	if err != nil || receipt2.Status != "completed" {
		t.Fatalf("second grant same-key execute = %#v, %v, want its own execution", receipt2, err)
	}
	if runs != 2 {
		t.Fatalf("runs = %d, want exactly one execution per grant scope", runs)
	}
	replayed, err := broker.Execute(context.Background(), first, request, mutate)
	if err != nil || replayed != receipt1 || runs != 2 {
		t.Fatalf("first grant replay = %#v, %v with %d runs, want the original receipt", replayed, err, runs)
	}
}
