package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/textbound"
)

// Kernel serializes Stage 08 connector lifecycle operations in process memory.
// It is safe for concurrent use. Durability across process restarts is not
// claimed by this bounded v1 surface; the process tracer owns one daemon life.
type Kernel struct {
	source    SourceAPI
	delegated *DelegatedGate
	mu        sync.Mutex
	conns     map[string]*connection
	idem      map[string]idempotencyRow // key = tenant|principal|op|key
}

// New constructs a Kernel over the given source API.
func New(config Config) (*Kernel, error) {
	if config.Source == nil {
		return nil, ErrInvalidInput
	}
	return &Kernel{
		source:    config.Source,
		delegated: config.Delegated,
		conns:     make(map[string]*connection),
		idem:      make(map[string]idempotencyRow),
	}, nil
}

// ConnectGitHubSource admits one repository scope and runs the first snapshot.
func (k *Kernel) ConnectGitHubSource(ctx context.Context, command ConnectCommand) (*contractsv1.ConnectGitHubSourceSuccess, error) {
	if k == nil || ctx == nil || command.Request == nil || !validIdentity(command.Identity) {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req := command.Request
	if req.ActionGrantRequested || req.Owner == "" || req.Repo == "" || req.SourceScope == "" || req.IdempotencyKey == "" {
		return nil, ErrInvalidInput
	}
	expectedScope := "github.com/" + req.Owner + "/" + req.Repo
	if !strings.EqualFold(req.SourceScope, expectedScope) {
		return nil, ErrInvalidInput
	}
	digest := connectDigest(req)
	idemKey := idempotencyKey(command.Identity, "connect", req.IdempotencyKey)

	k.mu.Lock()
	defer k.mu.Unlock()
	if existing, ok := k.idem[idemKey]; ok {
		if existing.requestDigest != digest {
			return nil, ErrIdempotencyConflict
		}
		conn, found := k.conns[existing.connectionID]
		if !found || conn.tenant != command.Identity.Tenant || conn.principal != command.Identity.Principal {
			return nil, ErrNotFoundOrDenied
		}
		return connectSuccess(conn), nil
	}

	page, err := k.source.Snapshot(ctx, req.Owner, req.Repo)
	if err != nil {
		return nil, err
	}
	if page.Complete && k.delegated != nil && k.delegated.OpaqueScope(req.SourceScope) && !validDelegatedSourcePage(page) {
		page.Complete = false
		page.ErrorCode = "source_freshness_unavailable"
	}
	connID := identity(
		"ouroboros.stage08.connection.v1",
		command.Identity.Tenant, command.Identity.Principal,
		req.IdempotencyKey, digest,
	)
	conn := &connection{
		id: connID, tenant: command.Identity.Tenant, principal: command.Identity.Principal,
		session: command.Identity.Session, owner: req.Owner, repo: req.Repo,
		sourceScope: req.SourceScope, aclEpoch: 1, objects: make(map[string]Object),
		requestDigest: digest, connectKey: req.IdempotencyKey,
	}
	if page.Complete {
		conn.state = stateReady
		conn.cursor = page.Cursor
		conn.sourceRevision = page.Revision
		conn.connectorDigest = page.ConnectorDigest
		conn.observedAt = page.ObservedAt
		conn.lastError = ""
		for _, obj := range page.Objects {
			if !obj.Deleted {
				conn.objects[obj.ID] = obj
			}
		}
	} else {
		conn.state = stateDegraded
		conn.cursor = "cursor-empty"
		conn.lastError = page.ErrorCode
	}
	k.conns[connID] = conn
	k.idem[idemKey] = idempotencyRow{operation: "connect", requestDigest: digest, connectionID: connID}
	return connectSuccess(conn), nil
}

// GetConnectorStatus reads readiness for one non-revoked, non-purged connection.
func (k *Kernel) GetConnectorStatus(ctx context.Context, command StatusCommand) (*contractsv1.GetConnectorStatusSuccess, error) {
	if k == nil || ctx == nil || !validIdentity(command.Identity) || !validBoundedID(command.ConnectionID) {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	conn, err := k.lookupActive(command.Identity, command.ConnectionID)
	if err != nil {
		return nil, err
	}
	return statusSuccess(conn), nil
}

// ReconcileConnector advances from the last trusted cursor after a complete page.
func (k *Kernel) ReconcileConnector(ctx context.Context, command ReconcileCommand) (*contractsv1.ReconcileConnectorSuccess, error) {
	if k == nil || ctx == nil || command.Request == nil || !validIdentity(command.Identity) {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req := command.Request
	if req.ConnectionId == nil || req.KnownCursor == "" || req.IdempotencyKey == "" || req.Reason == "" {
		return nil, ErrInvalidInput
	}
	digest := reconcileDigest(req)
	idemKey := idempotencyKey(command.Identity, "reconcile", req.IdempotencyKey)

	k.mu.Lock()
	defer k.mu.Unlock()
	if existing, ok := k.idem[idemKey]; ok {
		if existing.requestDigest != digest {
			return nil, ErrIdempotencyConflict
		}
		conn, err := k.lookupActive(command.Identity, existing.connectionID)
		if err != nil {
			return nil, err
		}
		return &contractsv1.ReconcileConnectorSuccess{
			ConnectionId:        &contractsv1.Identifier{Namespace: "connection", Value: conn.id},
			State:               lifecycleState(conn.state),
			PriorCursor:         req.KnownCursor,
			NextCursor:          conn.cursor,
			AdmittedObjectCount: 0,
			DeletedObjectCount:  0,
			PageComplete:        true,
		}, nil
	}
	conn, err := k.lookupActive(command.Identity, req.ConnectionId.Value)
	if err != nil {
		return nil, err
	}
	if conn.cursor != req.KnownCursor {
		return nil, ErrNotFoundOrDenied
	}
	prior := conn.cursor
	page, err := k.source.Delta(ctx, conn.owner, conn.repo, prior)
	if err != nil {
		return nil, err
	}
	admitted := uint32(0)
	deleted := uint32(0)
	if page.Complete && k.delegated != nil && k.delegated.OpaqueScope(conn.sourceScope) && !validDelegatedSourcePage(page) {
		page.Complete = false
		page.ErrorCode = "source_freshness_unavailable"
	}
	if page.Complete {
		conn.state = stateReady
		conn.lastError = ""
		var revised []string
		for _, obj := range page.Objects {
			if obj.Deleted {
				if _, found := conn.objects[obj.ID]; found {
					delete(conn.objects, obj.ID)
					revised = append(revised, obj.ID)
					deleted++
				}
				continue
			}
			if prior, found := conn.objects[obj.ID]; !found || prior != obj {
				revised = append(revised, obj.ID)
			}
			conn.objects[obj.ID] = obj
			admitted++
		}
		conn.cursor = page.Cursor
		conn.sourceRevision = page.Revision
		conn.connectorDigest = page.ConnectorDigest
		conn.observedAt = page.ObservedAt
		if k.delegated != nil && len(revised) > 0 {
			k.delegated.InvalidateObjects(revised)
		}
	} else {
		conn.state = stateDegraded
		conn.lastError = page.ErrorCode
		// Cursor never advances on incomplete pages; deletions never apply.
	}
	k.idem[idemKey] = idempotencyRow{
		operation: "reconcile", requestDigest: digest, connectionID: conn.id,
	}
	return &contractsv1.ReconcileConnectorSuccess{
		ConnectionId:        &contractsv1.Identifier{Namespace: "connection", Value: conn.id},
		State:               lifecycleState(conn.state),
		PriorCursor:         prior,
		NextCursor:          conn.cursor,
		AdmittedObjectCount: admitted,
		DeletedObjectCount:  deleted,
		PageComplete:        page.Complete,
	}, nil
}

// QueryConnectorEvidence answers with GitHub-native anchors from admitted objects.
func (k *Kernel) QueryConnectorEvidence(ctx context.Context, command QueryCommand) (*contractsv1.QueryConnectorEvidenceSuccess, error) {
	if k == nil || ctx == nil || command.Request == nil || !validIdentity(command.Identity) {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req := command.Request
	if req.ConnectionId == nil || req.Query == "" || req.IdempotencyKey == "" {
		return nil, ErrInvalidInput
	}
	k.mu.Lock()
	conn, err := k.lookupActive(command.Identity, req.ConnectionId.Value)
	if err != nil {
		k.mu.Unlock()
		return nil, err
	}
	query := strings.ToLower(strings.TrimSpace(req.Query))
	if k.delegated == nil || !k.delegated.OpaqueScope(conn.sourceScope) {
		var matches []Object
		for _, obj := range conn.objects {
			if objectMatchesQuery(obj, query) {
				matches = append(matches, obj)
			}
		}
		success := querySuccess(conn, buildAnswer(conn, matches, query))
		k.mu.Unlock()
		return success, nil
	}

	// Delegated-permission retrieval for ACL-opaque scopes (issue #309):
	// projection membership alone never serves opaque evidence. Missing,
	// expired, revoked, or mismatched grants and fully denied matches abstain
	// with a stable reason instead of answering.
	//
	// The live permission probes must never run under the kernel-wide mutex:
	// a slow provider would otherwise serialize every lifecycle operation
	// (RevokeConnector in particular must stay immediate). So the connection
	// facts the gate needs — identity binding already checked by lookupActive,
	// plus id, scope, and ACL epoch — are captured under the lock, the lock is
	// released for the probes, and the lookup and ACL epoch are revalidated —
	// and the captured matches re-intersected with the live object map —
	// after reacquiring before any answer is built.
	connectionID := conn.id
	sourceScope := conn.sourceScope
	aclEpoch := conn.aclEpoch
	freshness := DelegatedSourceFreshness{
		ConnectorDigest: conn.connectorDigest,
		SourceRevision:  conn.sourceRevision,
		SourceCursor:    conn.cursor,
		ObservedAt:      conn.observedAt,
	}
	k.mu.Unlock()

	// Scope authorization precedes object enumeration/fan-out. A missing,
	// expired, revoked, foreign, or unavailable grant therefore causes zero
	// object reads and zero provider calls.
	record, preauthOutcome := k.delegated.preauthorizeQuery(
		ctx, command.Identity, sourceScope, command.DelegatedGrantID, freshness,
	)
	if preauthOutcome != "" {
		k.mu.Lock()
		defer k.mu.Unlock()
		conn, err = k.lookupActive(command.Identity, connectionID)
		if err != nil || conn.aclEpoch != aclEpoch {
			return nil, ErrNotFoundOrDenied
		}
		return delegatedAbstain(conn, "delegated_"+preauthOutcome), nil
	}

	// Revalidate after the external grant-store read, then enumerate the local
	// projection. This is the first candidate fan-out in the opaque path.
	k.mu.Lock()
	conn, err = k.lookupActive(command.Identity, connectionID)
	if err != nil || conn.aclEpoch != aclEpoch {
		k.mu.Unlock()
		return nil, ErrNotFoundOrDenied
	}
	var matches []Object
	for _, obj := range conn.objects {
		if objectMatchesQuery(obj, query) {
			matches = append(matches, obj)
		}
	}
	k.mu.Unlock()

	matches, delegatedReasons, abstain := k.gateDelegatedQuery(
		ctx, command.Identity, sourceScope, command.DelegatedGrantID, query, record, matches, freshness,
	)

	// Revoke/purge race re-check, fail closed: if the connection was revoked,
	// purged, or otherwise bumped its ACL epoch while the probes ran, nothing
	// gated under the old epoch may be served. The gate has already appended
	// its audit receipt by this point; a receipt records the authorization
	// decision, never that evidence was actually served.
	k.mu.Lock()
	defer k.mu.Unlock()
	conn, err = k.lookupActive(command.Identity, connectionID)
	if err != nil {
		return nil, err
	}
	if conn.aclEpoch != aclEpoch {
		return nil, ErrNotFoundOrDenied
	}
	if abstain != "" {
		return delegatedAbstain(conn, abstain), nil
	}
	// Reconcile race re-check, fail closed: the captured matches predate the
	// unlocked probe window, and ReconcileConnector may have deleted or
	// replaced admitted objects meanwhile without bumping the ACL epoch (a
	// reconcile is not a revoke). Only objects still present in the live
	// object map may be served, and each survivor is served with its current
	// content — never the stale pre-probe capture. If every match was deleted
	// mid-probe, buildAnswer abstains with absent_support.
	current := make([]Object, 0, len(matches))
	changed := false
	for _, obj := range matches {
		if live, stillAdmitted := conn.objects[obj.ID]; stillAdmitted {
			if live != obj || !objectMatchesQuery(live, query) {
				changed = true
				continue
			}
			current = append(current, live)
		}
	}
	if changed {
		delegatedReasons = append(delegatedReasons, delegatedReasonObjectChanged)
	}
	if len(current) == 0 && changed {
		return delegatedAbstain(conn, delegatedReasonObjectChanged), nil
	}
	matches = current
	answer := buildAnswer(conn, matches, query)
	if len(delegatedReasons) > 0 {
		if answer.Status == contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_ANSWERED {
			answer.Status = contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_PARTIAL
		}
		answer.DegradedReasons = append(answer.DegradedReasons, delegatedReasons...)
	}
	return querySuccess(conn, answer), nil
}

// objectMatchesQuery is the single predicate used before and after a
// delegated probe. Query matching is never inherited by ID after a revision
// change, and deleted objects are never answerable.
func objectMatchesQuery(obj Object, query string) bool {
	if obj.Deleted {
		return false
	}
	haystack := strings.ToLower(obj.Title + " " + obj.Body + " " + obj.Path)
	return strings.Contains(haystack, query)
}

func validDelegatedSourcePage(page SnapshotPage) bool {
	return page.Complete && validDelegatedCursor(page.Cursor) &&
		validDelegatedField(page.Revision) && isLowerHexSHA256(page.ConnectorDigest) &&
		!page.ObservedAt.IsZero()
}

func querySuccess(conn *connection, answer *contractsv1.ConnectorAnswer) *contractsv1.QueryConnectorEvidenceSuccess {
	return &contractsv1.QueryConnectorEvidenceSuccess{
		Answer:       answer,
		ConnectionId: &contractsv1.Identifier{Namespace: "connection", Value: conn.id},
		State:        lifecycleState(conn.state),
	}
}

// RevokeConnector denies hydration and query immediately.
func (k *Kernel) RevokeConnector(ctx context.Context, command RevokeCommand) (*contractsv1.RevokeConnectorSuccess, error) {
	if k == nil || ctx == nil || !validIdentity(command.Identity) || !validBoundedID(command.ConnectionID) || command.IdempotencyKey == "" {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	idemKey := idempotencyKey(command.Identity, "revoke", command.IdempotencyKey)
	digest := sha256Hex([]byte(command.ConnectionID + "\x00" + command.IdempotencyKey))

	k.mu.Lock()
	defer k.mu.Unlock()
	if existing, ok := k.idem[idemKey]; ok {
		if existing.requestDigest != digest {
			return nil, ErrIdempotencyConflict
		}
		conn, found := k.conns[existing.connectionID]
		if !found || conn.tenant != command.Identity.Tenant || conn.principal != command.Identity.Principal {
			return nil, ErrNotFoundOrDenied
		}
		if conn.state != stateRevoked && conn.state != statePurged {
			return nil, ErrNotFoundOrDenied
		}
		return &contractsv1.RevokeConnectorSuccess{
			ConnectionId: &contractsv1.Identifier{Namespace: "connection", Value: conn.id},
			State:        contractsv1.ConnectorLifecycleState_CONNECTOR_LIFECYCLE_STATE_REVOKED,
		}, nil
	}
	conn, found := k.conns[command.ConnectionID]
	if !found || conn.tenant != command.Identity.Tenant || conn.principal != command.Identity.Principal {
		return nil, ErrNotFoundOrDenied
	}
	if conn.state == statePurged {
		return nil, ErrNotFoundOrDenied
	}
	conn.state = stateRevoked
	conn.aclEpoch++
	k.idem[idemKey] = idempotencyRow{operation: "revoke", requestDigest: digest, connectionID: conn.id}
	return &contractsv1.RevokeConnectorSuccess{
		ConnectionId: &contractsv1.Identifier{Namespace: "connection", Value: conn.id},
		State:        contractsv1.ConnectorLifecycleState_CONNECTOR_LIFECYCLE_STATE_REVOKED,
	}, nil
}

// PurgeConnector purges evidence objects after revoke (or from active).
func (k *Kernel) PurgeConnector(ctx context.Context, command PurgeCommand) (*contractsv1.PurgeConnectorSuccess, error) {
	if k == nil || ctx == nil || !validIdentity(command.Identity) || !validBoundedID(command.ConnectionID) || command.IdempotencyKey == "" {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	idemKey := idempotencyKey(command.Identity, "purge", command.IdempotencyKey)
	digest := sha256Hex([]byte(command.ConnectionID + "\x00purge\x00" + command.IdempotencyKey))

	k.mu.Lock()
	defer k.mu.Unlock()
	if existing, ok := k.idem[idemKey]; ok {
		if existing.requestDigest != digest {
			return nil, ErrIdempotencyConflict
		}
		conn, found := k.conns[existing.connectionID]
		if !found || conn.tenant != command.Identity.Tenant || conn.principal != command.Identity.Principal {
			return nil, ErrNotFoundOrDenied
		}
		if conn.state != statePurged {
			return nil, ErrNotFoundOrDenied
		}
		return &contractsv1.PurgeConnectorSuccess{
			ConnectionId:      &contractsv1.Identifier{Namespace: "connection", Value: conn.id},
			State:             contractsv1.ConnectorLifecycleState_CONNECTOR_LIFECYCLE_STATE_PURGED,
			PurgedObjectCount: 0,
		}, nil
	}
	conn, found := k.conns[command.ConnectionID]
	if !found || conn.tenant != command.Identity.Tenant || conn.principal != command.Identity.Principal {
		return nil, ErrNotFoundOrDenied
	}
	if conn.state == statePurged {
		return nil, ErrNotFoundOrDenied
	}
	count := uint32(len(conn.objects))
	conn.objects = make(map[string]Object)
	conn.state = statePurged
	conn.aclEpoch++
	k.idem[idemKey] = idempotencyRow{operation: "purge", requestDigest: digest, connectionID: conn.id}
	return &contractsv1.PurgeConnectorSuccess{
		ConnectionId:      &contractsv1.Identifier{Namespace: "connection", Value: conn.id},
		State:             contractsv1.ConnectorLifecycleState_CONNECTOR_LIFECYCLE_STATE_PURGED,
		PurgedObjectCount: count,
	}, nil
}

func (k *Kernel) lookupActive(identity Identity, connectionID string) (*connection, error) {
	conn, found := k.conns[connectionID]
	if !found || conn.tenant != identity.Tenant || conn.principal != identity.Principal {
		return nil, ErrNotFoundOrDenied
	}
	if conn.state == stateRevoked || conn.state == statePurged {
		return nil, ErrNotFoundOrDenied
	}
	return conn, nil
}

func connectSuccess(conn *connection) *contractsv1.ConnectGitHubSourceSuccess {
	repoCount, issueCount := countObjects(conn)
	return &contractsv1.ConnectGitHubSourceSuccess{
		ConnectionId:          &contractsv1.Identifier{Namespace: "connection", Value: conn.id},
		State:                 lifecycleState(conn.state),
		SourceScope:           conn.sourceScope,
		Provider:              "github",
		Cursor:                conn.cursor,
		RepositoryObjectCount: repoCount,
		IssueObjectCount:      issueCount,
		SnapshotComplete:      conn.state == stateReady,
	}
}

func statusSuccess(conn *connection) *contractsv1.GetConnectorStatusSuccess {
	repoCount, issueCount := countObjects(conn)
	return &contractsv1.GetConnectorStatusSuccess{
		ConnectionId:          &contractsv1.Identifier{Namespace: "connection", Value: conn.id},
		State:                 lifecycleState(conn.state),
		SourceScope:           conn.sourceScope,
		Provider:              "github",
		Cursor:                conn.cursor,
		RepositoryObjectCount: repoCount,
		IssueObjectCount:      issueCount,
		LastErrorCode:         conn.lastError,
		AclEpoch:              conn.aclEpoch,
	}
}

func countObjects(conn *connection) (repoCount, issueCount uint32) {
	for _, obj := range conn.objects {
		switch obj.Kind {
		case ObjectKindRepository, ObjectKindFile:
			repoCount++
		case ObjectKindIssue:
			issueCount++
		}
	}
	return repoCount, issueCount
}

func buildAnswer(conn *connection, matches []Object, query string) *contractsv1.ConnectorAnswer {
	if len(matches) == 0 {
		return &contractsv1.ConnectorAnswer{
			Status:             contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_ABSTAINED,
			DegradedReasons:    []string{"absent_support"},
			FactualConsistency: factualConsistencyAbstained(),
		}
	}
	obj := matches[0]
	digest := sha256Hex([]byte(obj.Body))
	anchor := &contractsv1.GitHubNativeAnchor{
		Kind:                 anchorKind(obj),
		Owner:                conn.owner,
		Repo:                 conn.repo,
		Path:                 obj.Path,
		IssueNumber:          uint32(obj.IssueNumber),
		StartLine:            uint32(obj.StartLine),
		EndLine:              uint32(obj.EndLine),
		SupportingTextDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: digest},
		Evidence: &contractsv1.EvidenceRef{
			EvidenceId:       &contractsv1.Identifier{Namespace: "evidence", Value: sha256Hex([]byte(conn.id + ":" + obj.ID))},
			SourceRevisionId: &contractsv1.Identifier{Namespace: "source-revision", Value: sha256Hex([]byte(obj.Version + ":" + obj.ID))},
		},
	}
	claim := &contractsv1.ConnectorAnswerClaim{
		ClaimId:            &contractsv1.Identifier{Namespace: "claim", Value: sha256Hex([]byte(obj.ID + ":" + query))},
		Statement:          truncate("Evidence matches query in "+obj.Title, 4096),
		Citations:          []*contractsv1.GitHubNativeAnchor{anchor},
		AuthorityClass:     contractsv1.AuthorityClass_AUTHORITY_CLASS_MODEL_PROPOSAL,
		ConfidencePerMille: 800,
	}
	status := contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_ANSWERED
	var reasons []string
	if conn.state == stateDegraded {
		status = contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_PARTIAL
		reasons = []string{"partial_coverage"}
	}
	return &contractsv1.ConnectorAnswer{
		Status:             status,
		Prose:              truncate("Found "+obj.Title+": "+obj.Body, 8192),
		Claims:             []*contractsv1.ConnectorAnswerClaim{claim},
		DegradedReasons:    reasons,
		FactualConsistency: factualConsistencyUnknown(1),
	}
}

func factualConsistencyAbstained() *contractsv1.FactualConsistencyScore {
	return &contractsv1.FactualConsistencyScore{
		Status: contractsv1.FactualConsistencyStatus_FACTUAL_CONSISTENCY_STATUS_ABSTAINED,
		Reason: contractsv1.FactualConsistencyReason_FACTUAL_CONSISTENCY_REASON_ANSWER_ABSTAINED,
	}
}

func factualConsistencyUnknown(totalClaims uint32) *contractsv1.FactualConsistencyScore {
	return &contractsv1.FactualConsistencyScore{
		Status:          contractsv1.FactualConsistencyStatus_FACTUAL_CONSISTENCY_STATUS_UNKNOWN,
		Reason:          contractsv1.FactualConsistencyReason_FACTUAL_CONSISTENCY_REASON_SCORER_UNAVAILABLE,
		TotalClaimCount: totalClaims,
	}
}

func anchorKind(obj Object) string {
	if obj.Kind == ObjectKindIssue {
		return "issue"
	}
	return "repository_file"
}

func lifecycleState(state connectionState) contractsv1.ConnectorLifecycleState {
	switch state {
	case stateReady:
		return contractsv1.ConnectorLifecycleState_CONNECTOR_LIFECYCLE_STATE_READY
	case stateDegraded:
		return contractsv1.ConnectorLifecycleState_CONNECTOR_LIFECYCLE_STATE_DEGRADED
	case stateRevoked:
		return contractsv1.ConnectorLifecycleState_CONNECTOR_LIFECYCLE_STATE_REVOKED
	case statePurged:
		return contractsv1.ConnectorLifecycleState_CONNECTOR_LIFECYCLE_STATE_PURGED
	default:
		return contractsv1.ConnectorLifecycleState_CONNECTOR_LIFECYCLE_STATE_UNSPECIFIED
	}
}

func validIdentity(identity Identity) bool {
	return identity.Tenant != "" && identity.Principal != "" && identity.Session != ""
}

func validBoundedID(value string) bool {
	return len(value) > 0 && len(value) <= 128
}

func idempotencyKey(identity Identity, op, key string) string {
	return identity.Tenant + "|" + identity.Principal + "|" + op + "|" + key
}

func connectDigest(req *contractsv1.ConnectGitHubSourceRequest) string {
	return sha256Hex([]byte(strings.Join([]string{
		req.Owner, req.Repo, req.SourceScope, fmt.Sprintf("%v", req.ActionGrantRequested), req.IdempotencyKey,
	}, "\x00")))
}

func reconcileDigest(req *contractsv1.ReconcileConnectorRequest) string {
	id := ""
	if req.ConnectionId != nil {
		id = req.ConnectionId.Value
	}
	return sha256Hex([]byte(strings.Join([]string{
		id, req.KnownCursor, req.Reason, req.IdempotencyKey,
	}, "\x00")))
}

func identity(domain, tenant, principal, key, digest string) string {
	return sha256Hex([]byte(domain + "\x00" + tenant + "\x00" + principal + "\x00" + key + "\x00" + digest))
}

func sha256Hex(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// truncate cuts on a rune boundary rather than a byte offset.
func truncate(value string, max int) string {
	return textbound.Bytes(value, max)
}
