package queryapi

import (
	"context"
	"errors"
	"fmt"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Handler implements the four frozen QueryService methods behind injected
// ports. It holds no state between calls and is safe for concurrent use; the
// composed engine, store, catalog, and authorizer own their own concurrency.
type Handler struct {
	engine        Engine
	conversations Conversations
	sources       SourceCatalog
	authorizer    Authorizer
	clock         Clock
	configuration shared.Digest
}

// Config binds a Handler to its ports. Every port is required, and the
// configuration digest pins receipts to the effective runtime configuration
// exactly as the Stage 03 adapter does.
type Config struct {
	Engine              Engine
	Conversations       Conversations
	Sources             SourceCatalog
	Authorizer          Authorizer
	Clock               Clock
	ConfigurationDigest shared.Digest
}

// NewHandler validates the complete configuration; a misconfigured handler
// fails at construction, never at request time. The configuration digest pins
// every receipt, so it must already be canonical lowercase-hex SHA-256.
func NewHandler(config Config) (*Handler, error) {
	if config.Engine == nil || config.Conversations == nil || config.Sources == nil ||
		config.Authorizer == nil || config.Clock == nil {
		return nil, fmt.Errorf("%w: handler requires engine, conversations, sources, authorizer, and clock", ErrInvalidConfiguration)
	}
	if config.ConfigurationDigest.Algorithm != "sha256" ||
		!isLowerHexSHA256(config.ConfigurationDigest.Hex) {
		return nil, fmt.Errorf("%w: configuration digest", ErrInvalidConfiguration)
	}
	return &Handler{
		engine:        config.Engine,
		conversations: config.Conversations,
		sources:       config.Sources,
		authorizer:    config.Authorizer,
		clock:         config.Clock,
		configuration: config.ConfigurationDigest,
	}, nil
}

// Ask answers one grounded question against one pinned generation. The funnel
// is identity, authorization, admission, engine answer, completion: current
// authorization is evaluated before the user turn and idempotency record
// commit, and exactly one assistant completion follows every admitted query.
// A cancelled transport context commits no assistant turn; the interrupted
// admission is later marked failed by restart recovery or by the replay path.
func (h *Handler) Ask(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.AskRequest,
) (*contractsv1.AskResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRequest(request); err != nil {
		return nil, err
	}
	principal, err := crossCheckPeer(peer, request.Caller)
	if err != nil {
		return nil, err
	}
	identity := peer.Identity
	freshness := mapFreshnessRequirement(request.Freshness)
	decision, authorizeErr := h.authorizer.Authorize(ctx, principal, ActionQuery, request.SourceId.Value)
	if authorizeErr != nil || !decision.Allowed {
		return h.deniedAsk(identity)
	}
	admitted, err := h.conversations.Admit(ctx, Admission{
		Principal:      principal,
		SourceID:       request.SourceId.Value,
		GenerationID:   request.GenerationId.Value,
		Text:           request.Query,
		Freshness:      freshness,
		IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		if errors.Is(err, ErrIdempotencyConflict) {
			return h.deniedAsk(identity)
		}
		if errors.Is(err, ErrRequestDenied) {
			return nil, err
		}
		return nil, errPortFailure
	}
	if admitted.Replayed {
		return h.replayAsk(ctx, identity, principal, request)
	}
	result, err := h.engine.Answer(ctx, EngineQuery{
		QueryID:        admitted.QueryID,
		Principal:      principal,
		SourceID:       request.SourceId.Value,
		GenerationID:   request.GenerationId.Value,
		Text:           request.Query,
		Freshness:      freshness,
		IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if failErr := h.failCompletion(ctx, principal, request.IdempotencyKey); failErr != nil {
			return nil, errPortFailure
		}
		return h.deniedAsk(identity)
	}
	// The public response is built and validated BEFORE the completion
	// commits, so the stored terminal state always matches a returnable
	// disposition: an answer whose freshness facts are unavailable or whose
	// mapped output violates the contract becomes a visibly failed
	// completion, never an active one no caller can receive.
	response, buildErr := h.buildAskSuccess(ctx, identity, principal, request.SourceId.Value, result)
	if buildErr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if failErr := h.failCompletion(ctx, principal, request.IdempotencyKey); failErr != nil {
			return nil, errPortFailure
		}
		if errors.Is(buildErr, errFactsUnavailable) {
			return h.deniedAsk(identity)
		}
		return nil, buildErr
	}
	// Cancellation before completion commits no assistant turn, exactly as
	// the frozen cancellation semantics require.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := h.conversations.Complete(ctx, Completion{
		Tenant:         principal.Tenant,
		PrincipalID:    principal.PrincipalID,
		IdempotencyKey: request.IdempotencyKey,
		Result:         &result,
	}); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrCompletionConflict) {
			return h.deniedAsk(identity)
		}
		return nil, errPortFailure
	}
	return response, nil
}

// replayAsk resolves an exact idempotent retry to the original outcome. An
// active completion rebuilds the original success; a failed completion stays
// terminal and is never replayed as fact; an admitted-but-uncompleted query
// crashed mid-flight, so its assistant turn is marked failed exactly once
// before the static denial, matching restart recovery.
func (h *Handler) replayAsk(
	ctx context.Context, identity shared.MappedIdentityFact, principal Principal, request *contractsv1.AskRequest,
) (*contractsv1.AskResponse, error) {
	resolution, err := h.conversations.Resolve(ctx, principal.Tenant, principal.PrincipalID, request.IdempotencyKey)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownAdmission) {
			return h.deniedAsk(identity)
		}
		return nil, errPortFailure
	}
	if !resolution.Completed {
		if failErr := h.failCompletion(ctx, principal, request.IdempotencyKey); failErr != nil {
			return nil, errPortFailure
		}
		return h.deniedAsk(identity)
	}
	if resolution.Status != "active" || resolution.Result == nil {
		return h.deniedAsk(identity)
	}
	// An active completion was validated before it committed, so rebuilding
	// its response cannot produce a new terminal failure; a transient facts
	// outage still collapses to the static denial.
	response, buildErr := h.buildAskSuccess(ctx, identity, principal, request.SourceId.Value, *resolution.Result)
	if buildErr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(buildErr, errFactsUnavailable) {
			return h.deniedAsk(identity)
		}
		return nil, buildErr
	}
	return response, nil
}

// failCompletion appends the visibly failed assistant turn for one admitted
// query whose answer never completed. It is exactly once: a prior completion
// makes it a replay, never a second turn.
func (h *Handler) failCompletion(ctx context.Context, principal Principal, idempotencyKey string) error {
	_, err := h.conversations.Complete(ctx, Completion{
		Tenant:         principal.Tenant,
		PrincipalID:    principal.PrincipalID,
		IdempotencyKey: idempotencyKey,
		Failed:         true,
	})
	if err != nil && !errors.Is(err, ErrCompletionConflict) {
		return err
	}
	return nil
}

// buildAskSuccess constructs the request-bound success outcome — grounded
// answer plus freshness and coverage disclosures — and validates it against
// the frozen descriptors. It runs before any completion commit so a result
// that cannot produce a valid response never becomes an active completion.
// A facts-port failure is reported as errFactsUnavailable for the caller to
// map to the static denial; a contract violation is ErrInvalidResponse.
func (h *Handler) buildAskSuccess(
	ctx context.Context, identity shared.MappedIdentityFact, principal Principal, sourceID string, result EngineResult,
) (*contractsv1.AskResponse, error) {
	facts, err := h.sources.Facts(ctx, principal, sourceID, result.Freshness.GenerationID)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errFactsUnavailable
	}
	response := &contractsv1.AskResponse{
		Receipt: h.receipt("query.ask", result.Answer.QueryID, identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.AskResponse_Success{Success: &contractsv1.AskSuccess{
			Answer:    mapAnswer(result.Answer),
			Freshness: mapFreshness(result.Freshness, facts),
			Coverage:  mapCoverage(result.Coverage),
		}},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// ListSources lists the principal's authorized non-revoked sources with
// provenance. The catalog port owns authorization and pagination; the handler
// maps and defensively filters non-servable states.
func (h *Handler) ListSources(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.ListSourcesRequest,
) (*contractsv1.ListSourcesResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRequest(request); err != nil {
		return nil, err
	}
	principal, err := crossCheckPeer(peer, request.Caller)
	if err != nil {
		return nil, err
	}
	identity := peer.Identity
	page, err := h.sources.List(ctx, principal, cursorToken(request.After), request.PageSize)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return h.deniedListSources(identity)
	}
	success := &contractsv1.ListSourcesSuccess{NextCursor: nextCursor(page.NextCursor)}
	for _, source := range page.Sources {
		state, servable := mapSourceState(source.State)
		if !servable {
			continue
		}
		summary := &contractsv1.SourceSummary{
			Source: mapSourceReference(source, identity),
			State:  state,
		}
		if source.Current != nil {
			summary.CurrentGeneration = mapGeneration(*source.Current)
		}
		success.Sources = append(success.Sources, summary)
	}
	response := &contractsv1.ListSourcesResponse{
		Receipt: h.receipt("query.sources", "query.sources", identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.ListSourcesResponse_Success{Success: success},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// GetHistory reads the authenticated principal's own private turns. History
// is scoped by the peer identity alone, so cross-principal history is
// inexpressible; hydration is reauthorized against current policy before any
// payload read.
func (h *Handler) GetHistory(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.GetHistoryRequest,
) (*contractsv1.GetHistoryResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRequest(request); err != nil {
		return nil, err
	}
	principal, err := crossCheckPeer(peer, request.Caller)
	if err != nil {
		return nil, err
	}
	identity := peer.Identity
	decision, authorizeErr := h.authorizer.Authorize(ctx, principal, ActionHydrate, historyScope)
	if authorizeErr != nil || !decision.Allowed {
		return h.deniedGetHistory(identity)
	}
	page, err := h.conversations.History(ctx, principal.Tenant, principal.PrincipalID,
		cursorToken(request.After), request.PageSize)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return h.deniedGetHistory(identity)
	}
	success := &contractsv1.GetHistorySuccess{NextCursor: nextCursor(page.NextCursor)}
	for _, turn := range page.Turns {
		success.Turns = append(success.Turns, mapTurn(turn, identity))
	}
	response := &contractsv1.GetHistoryResponse{
		Receipt: h.receipt("query.history", "query.history", identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.GetHistoryResponse_Success{Success: success},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// GetStatus reads freshness, coverage, and projection truth for one source.
// Denied, unknown, and revoked sources share the static denial because a
// status read has no abstention shape.
func (h *Handler) GetStatus(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.GetStatusRequest,
) (*contractsv1.GetStatusResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRequest(request); err != nil {
		return nil, err
	}
	principal, err := crossCheckPeer(peer, request.Caller)
	if err != nil {
		return nil, err
	}
	identity := peer.Identity
	status, err := h.engine.Status(ctx, principal, request.SourceId.Value)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return h.deniedGetStatus(identity)
	}
	reference, err := h.sources.Reference(ctx, principal, request.SourceId.Value)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return h.deniedGetStatus(identity)
	}
	facts, err := h.sources.Facts(ctx, principal, request.SourceId.Value, status.Freshness.GenerationID)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return h.deniedGetStatus(identity)
	}
	response := &contractsv1.GetStatusResponse{
		Receipt: h.receipt("query.status", "query.status", identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.GetStatusResponse_Success{Success: &contractsv1.GetStatusSuccess{
			Source:     mapSourceReference(reference, identity),
			Freshness:  mapFreshness(status.Freshness, facts),
			Coverage:   mapCoverage(status.Coverage),
			Projection: mapProjectionState(status.Projection),
		}},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// deniedAsk constructs the one static non-disclosing Ask denial. Like every
// response this package emits, it is revalidated against the frozen
// descriptors before return; a construction defect fails closed.
func (h *Handler) deniedAsk(identity shared.MappedIdentityFact) (*contractsv1.AskResponse, error) {
	response := &contractsv1.AskResponse{
		Receipt: h.receipt("query.ask", "query.ask", identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, deniedCode),
		Outcome: &contractsv1.AskResponse_Error{Error: staticPublicError()},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// deniedListSources constructs the static non-disclosing ListSources denial,
// revalidated before return like every emitted response.
func (h *Handler) deniedListSources(identity shared.MappedIdentityFact) (*contractsv1.ListSourcesResponse, error) {
	response := &contractsv1.ListSourcesResponse{
		Receipt: h.receipt("query.sources", "query.sources", identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, deniedCode),
		Outcome: &contractsv1.ListSourcesResponse_Error{Error: staticPublicError()},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// deniedGetHistory constructs the static non-disclosing GetHistory denial,
// revalidated before return like every emitted response.
func (h *Handler) deniedGetHistory(identity shared.MappedIdentityFact) (*contractsv1.GetHistoryResponse, error) {
	response := &contractsv1.GetHistoryResponse{
		Receipt: h.receipt("query.history", "query.history", identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, deniedCode),
		Outcome: &contractsv1.GetHistoryResponse_Error{Error: staticPublicError()},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// deniedGetStatus constructs the static non-disclosing GetStatus denial,
// revalidated before return like every emitted response.
func (h *Handler) deniedGetStatus(identity shared.MappedIdentityFact) (*contractsv1.GetStatusResponse, error) {
	response := &contractsv1.GetStatusResponse{
		Receipt: h.receipt("query.status", "query.status", identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, deniedCode),
		Outcome: &contractsv1.GetStatusResponse_Error{Error: staticPublicError()},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// receipt authors the request-bound receipt: completed outcomes bind the
// admitted query identity, denials use the one static shape, and every
// receipt pins the session causal context, observation time, and the
// configuration digest.
func (h *Handler) receipt(
	operation, receiptValue string, identity shared.MappedIdentityFact,
	status contractsv1.ReceiptStatus, reason string,
) *contractsv1.Receipt {
	return &contractsv1.Receipt{
		ReceiptId:   &contractsv1.Identifier{Namespace: namespaceReceipt, Value: receiptValue},
		OperationId: &contractsv1.Identifier{Namespace: namespaceOperation, Value: operation},
		Status:      status,
		ReasonCode:  reason,
		Causal:      sessionCausal(identity),
		RecordedAt:  timestamppb.New(h.clock.Now().UTC()),
		ConfigurationDigest: &contractsv1.Digest{
			Algorithm: h.configuration.Algorithm,
			Hex:       h.configuration.Hex,
		},
	}
}
