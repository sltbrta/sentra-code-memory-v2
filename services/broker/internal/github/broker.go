package github

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// Config binds the draft-PR broker dependencies.
type Config struct {
	// API is the GitHub provider surface (FakeAPI or RESTAPI).
	API API
	// Policy evaluates current policy immediately before each provider call.
	Policy contracts.PolicyCheck
	// Clock supplies wall-clock for grant expiry (defaults to time.Now).
	Clock func() time.Time
	// Token is the fine-grained PAT; when empty, ResolveToken reads env.
	Token string
}

// Broker executes the two-phase idempotent draft-PR publication protocol.
type Broker struct {
	api    API
	policy contracts.PolicyCheck
	clock  func() time.Time
	token  string

	mu     sync.Mutex
	outbox map[string]*outboxRow // key = publication_tuple_digest hex
}

type outboxRow struct {
	state              OutboxState
	tupleDigest        contracts.Digest
	contentDigest      contracts.Digest
	headRef            string
	providerPRID       string
	receipt            Receipt
	requestFingerprint string
}

// NewBroker returns a draft-only GitHub effect broker. API and Policy are required.
func NewBroker(cfg Config) (*Broker, error) {
	if cfg.API == nil || cfg.Policy == nil {
		return nil, fmt.Errorf("%w: api and policy required", ErrInvalidInput)
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	token := cfg.Token
	if token == "" {
		token = ResolveToken()
	}
	return &Broker{
		api:    cfg.API,
		policy: cfg.Policy,
		clock:  clock,
		token:  token,
		outbox: make(map[string]*outboxRow),
	}, nil
}

// ResolveToken reads GITHUB_TOKEN or OUROBOROS_GITHUB_TOKEN (fine-grained PAT).
func ResolveToken() string {
	for _, name := range TokenEnvNames {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

// Publish runs the two-phase protocol: branch then draft PR. Exact retries of
// the same publication tuple and content converge to one head ref, one draft
// PR, and one receipt. Crash/duplicate workers that re-enter after either
// phase also converge via lookup-before-create.
func (b *Broker) Publish(ctx context.Context, request PublishRequest) (Receipt, error) {
	if b == nil || ctx == nil {
		return Receipt{}, &Denial{Reason: ReasonPolicyDenied}
	}
	if err := validatePublishRequest(request); err != nil {
		return Receipt{}, err
	}
	if hasForbiddenAction(request.Grant.Actions) {
		return Receipt{}, &Denial{Reason: ReasonGrantMalformed}
	}
	tupleDigest := TupleDigest(request.Tuple)
	contentDigest := ContentDigest(request.Content)
	headRef := HeadRef(request.Tuple)
	fingerprint := request.IdempotencyKey + "\x00" + contentDigest.Hex

	b.mu.Lock()
	if row, ok := b.outbox[tupleDigest.Hex]; ok {
		if row.requestFingerprint != fingerprint {
			b.mu.Unlock()
			return Receipt{}, &Denial{Reason: ReasonEffectDuplicate}
		}
		if row.state == StatePRCreated {
			receipt := row.receipt
			b.mu.Unlock()
			return receipt, nil
		}
		if row.state == StateTerminalConflict {
			b.mu.Unlock()
			return Receipt{}, fmt.Errorf("%w: %s", ErrConflict, ReasonTerminalConflict)
		}
		if row.state == StatePRAlreadyClosed {
			b.mu.Unlock()
			return Receipt{}, fmt.Errorf("%w", ErrAlreadyClosed)
		}
	} else {
		b.outbox[tupleDigest.Hex] = &outboxRow{
			state:              StateBranchPending,
			tupleDigest:        tupleDigest,
			contentDigest:      contentDigest,
			headRef:            headRef,
			requestFingerprint: fingerprint,
		}
	}
	b.mu.Unlock()

	// Phase 1: publish/reconcile deterministic head ref.
	if err := b.ensureBranch(ctx, request, headRef, tupleDigest.Hex); err != nil {
		return Receipt{}, err
	}

	// Phase 2: create/reconcile exactly one draft PR.
	receipt, err := b.ensureDraftPR(ctx, request, headRef, tupleDigest, contentDigest)
	if err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

func (b *Broker) ensureBranch(ctx context.Context, request PublishRequest, headRef, tupleKey string) error {
	if err := b.reauthorize(ctx, request, ActionBranchPublish); err != nil {
		return err
	}
	b.mu.Lock()
	row := b.outbox[tupleKey]
	if row.state == StateBranchPublished || row.state == StatePRPending ||
		row.state == StatePRInFlight || row.state == StatePRCreated {
		b.mu.Unlock()
		return nil
	}
	row.state = StateBranchInFlight
	b.mu.Unlock()

	owner := request.Tuple.RepositoryOwner
	repo := request.Tuple.RepositoryName
	baseRef := normalizeRef(request.Tuple.BaseRef)

	// Stale-base check: provider base must still equal approved base OID.
	baseSHA, ok, err := b.api.GetRef(ctx, owner, repo, baseRef)
	if err != nil {
		return err
	}
	if ok && baseSHA != request.Tuple.BaseCommitOID {
		b.markConflict(tupleKey)
		return &Denial{Reason: ReasonStaleBase}
	}

	// Reconcile head ref: absent → create; equal → success; different → conflict.
	existing, present, err := b.api.GetRef(ctx, owner, repo, headRef)
	if err != nil {
		return err
	}
	if present {
		if existing != request.Tuple.HeadCommitOID {
			b.markConflict(tupleKey)
			return fmt.Errorf("%w: head ref at different oid", ErrConflict)
		}
	} else {
		if err := b.api.CreateRef(ctx, owner, repo, headRef, request.Tuple.HeadCommitOID); err != nil {
			// Ambiguous create: re-lookup before failing.
			existing, present, lookupErr := b.api.GetRef(ctx, owner, repo, headRef)
			if lookupErr != nil {
				return lookupErr
			}
			if present && existing == request.Tuple.HeadCommitOID {
				// crash recovery: ref exists at exact OID
			} else if present {
				b.markConflict(tupleKey)
				return fmt.Errorf("%w: head ref at different oid after create", ErrConflict)
			} else {
				return err
			}
		}
	}

	b.mu.Lock()
	row = b.outbox[tupleKey]
	row.state = StateBranchPublished
	b.mu.Unlock()
	return nil
}

func (b *Broker) ensureDraftPR(
	ctx context.Context,
	request PublishRequest,
	headRef string,
	tupleDigest, contentDigest contracts.Digest,
) (Receipt, error) {
	if err := b.reauthorize(ctx, request, ActionDraftPRCreate); err != nil {
		return Receipt{}, err
	}
	tupleKey := tupleDigest.Hex
	b.mu.Lock()
	row := b.outbox[tupleKey]
	if row.state == StatePRCreated {
		receipt := row.receipt
		b.mu.Unlock()
		return receipt, nil
	}
	row.state = StatePRInFlight
	b.mu.Unlock()

	owner := request.Tuple.RepositoryOwner
	repo := request.Tuple.RepositoryName
	baseBranch := BranchName(normalizeRef(request.Tuple.BaseRef))
	headBranch := BranchName(headRef)
	headFilter := owner + ":" + headBranch

	// Stale-base recheck before PR mutation.
	baseRef := normalizeRef(request.Tuple.BaseRef)
	baseSHA, ok, err := b.api.GetRef(ctx, owner, repo, baseRef)
	if err != nil {
		return Receipt{}, err
	}
	if ok && baseSHA != request.Tuple.BaseCommitOID {
		b.markConflict(tupleKey)
		return Receipt{}, &Denial{Reason: ReasonStaleBase}
	}

	// Lookup-before-create across open and closed states.
	existing, err := b.api.ListPullRequests(ctx, owner, repo, headFilter, baseBranch)
	if err != nil {
		return Receipt{}, err
	}
	var match *PullRequest
	for i := range existing {
		pr := &existing[i]
		if pr.HeadRef != headBranch || pr.BaseRef != baseBranch {
			continue
		}
		if match != nil {
			b.markConflict(tupleKey)
			return Receipt{}, fmt.Errorf("%w: multiple matching PRs", ErrConflict)
		}
		match = pr
	}
	if match != nil {
		if match.State == "closed" {
			// Exact closed PR never causes a duplicate create.
			b.mu.Lock()
			b.outbox[tupleKey].state = StatePRAlreadyClosed
			b.mu.Unlock()
			return Receipt{}, fmt.Errorf("%w", ErrAlreadyClosed)
		}
		if !match.Draft {
			b.markConflict(tupleKey)
			return Receipt{}, fmt.Errorf("%w: matching PR is not draft", ErrConflict)
		}
		if match.Title != request.Content.Title || match.Body != request.Content.Body {
			b.markConflict(tupleKey)
			return Receipt{}, fmt.Errorf("%w: content mismatch", ErrConflict)
		}
		return b.commitPRSuccess(request, headRef, tupleDigest, contentDigest, match)
	}

	// Zero matches → create draft PR.
	created, err := b.api.CreatePullRequest(ctx, owner, repo, CreatePRInput{
		Title: request.Content.Title,
		Body:  request.Content.Body,
		Head:  headBranch,
		Base:  baseBranch,
		Draft: true,
	})
	if err != nil {
		// Ambiguous failure: lookup before retry/fail (crash recovery).
		existing, lookupErr := b.api.ListPullRequests(ctx, owner, repo, headFilter, baseBranch)
		if lookupErr != nil {
			return Receipt{}, lookupErr
		}
		for i := range existing {
			pr := &existing[i]
			if pr.HeadRef == headBranch && pr.BaseRef == baseBranch && pr.State == "open" &&
				pr.Draft && pr.Title == request.Content.Title && pr.Body == request.Content.Body {
				return b.commitPRSuccess(request, headRef, tupleDigest, contentDigest, pr)
			}
		}
		return Receipt{}, err
	}
	if !created.Draft {
		b.markConflict(tupleKey)
		return Receipt{}, fmt.Errorf("%w: provider returned non-draft PR", ErrConflict)
	}
	return b.commitPRSuccess(request, headRef, tupleDigest, contentDigest, &created)
}

func (b *Broker) commitPRSuccess(
	request PublishRequest,
	headRef string,
	tupleDigest, contentDigest contracts.Digest,
	pr *PullRequest,
) (Receipt, error) {
	providerID := pr.NodeID
	if providerID == "" {
		providerID = strconv.Itoa(pr.Number)
	}
	receipt := Receipt{
		ActionID:               request.ActionID,
		Phase:                  PhasePR,
		HeadRef:                headRef,
		BaseRef:                normalizeRef(request.Tuple.BaseRef),
		BaseCommitOID:          request.Tuple.BaseCommitOID,
		HeadCommitOID:          request.Tuple.HeadCommitOID,
		RepositoryFullName:     RepositoryFullName(request.Tuple),
		ProviderPRID:           providerID,
		IsDraft:                true,
		PublicationTupleDigest: tupleDigest,
		ContentDigest:          contentDigest,
		EffectApprovalDigest:   request.Tuple.EffectApprovalDigest,
		ChangeSetDigest:        request.Tuple.ChangeSetDigest,
		OutboxState:            StatePRCreated,
		Receipt: contracts.Receipt{
			OperationID: contracts.Identifier{Namespace: "github-draft-pr", Value: request.ActionID},
			Status:      "completed",
			ReasonCode:  ReasonAllowed,
		},
	}
	b.mu.Lock()
	row := b.outbox[tupleDigest.Hex]
	row.state = StatePRCreated
	row.providerPRID = providerID
	row.receipt = receipt
	b.mu.Unlock()
	return receipt, nil
}

func (b *Broker) markConflict(tupleKey string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if row, ok := b.outbox[tupleKey]; ok {
		row.state = StateTerminalConflict
	}
}

// reauthorize rechecks identity, grant shape, expiry, sealed actions, and
// current policy immediately before each provider mutation.
func (b *Broker) reauthorize(ctx context.Context, request PublishRequest, action string) error {
	now := b.clock()
	grant := request.Grant
	if grant.GrantID.Value == "" || grant.Nonce == "" || grant.ExpiresAt.IsZero() {
		return &Denial{Reason: ReasonGrantMalformed}
	}
	if !now.Before(grant.ExpiresAt) {
		return &Denial{Reason: ReasonGrantMalformed}
	}
	if request.Authenticated.Principal != grant.Initiator ||
		request.Authenticated.Tenant != grant.Tenant {
		return &Denial{Reason: ReasonIdentityMismatch}
	}
	if hasForbiddenAction(grant.Actions) || !containsAction(grant.Actions, action) {
		return &Denial{Reason: ReasonEffectNotAuthorized}
	}
	if grant.BaseCommitOID != request.Tuple.BaseCommitOID ||
		grant.HeadCommitOID != request.Tuple.HeadCommitOID {
		return &Denial{Reason: ReasonGrantMalformed}
	}
	if grant.RepositoryFullName != RepositoryFullName(request.Tuple) {
		return &Denial{Reason: ReasonGrantMalformed}
	}
	decision, err := b.policy.Check(ctx, request.Authenticated, contracts.PolicyRequest{
		Action:          action,
		Resource:        contracts.Identifier{Namespace: "repository", Value: grant.RepositoryFullName},
		RevocationEpoch: grant.RevocationEpoch,
	})
	if err != nil || !decision.Allowed {
		return &Denial{Reason: ReasonEffectNotAuthorized}
	}
	if decision.RevocationEpoch != grant.RevocationEpoch {
		return &Denial{Reason: ReasonRevoked}
	}
	return nil
}

func validatePublishRequest(request PublishRequest) error {
	if request.ActionID == "" || request.IdempotencyKey == "" {
		return fmt.Errorf("%w: action or idempotency key missing", ErrInvalidInput)
	}
	if request.Content.Title == "" {
		return fmt.Errorf("%w: pr title missing", ErrInvalidInput)
	}
	if err := validateTuple(request.Tuple); err != nil {
		return err
	}
	if request.Authenticated.Principal.Value == "" || request.Authenticated.Tenant.Value == "" {
		return fmt.Errorf("%w: authenticated identity incomplete", ErrInvalidInput)
	}
	return nil
}

func normalizeRef(ref string) string {
	if strings.HasPrefix(ref, "refs/") {
		return ref
	}
	return "refs/heads/" + ref
}

// OutboxStateOf returns the durable outbox state for one tuple (test helper).
func (b *Broker) OutboxStateOf(tuple PublicationTuple) (OutboxState, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	row, ok := b.outbox[TupleDigest(tuple).Hex]
	if !ok {
		return "", false
	}
	return row.state, true
}
