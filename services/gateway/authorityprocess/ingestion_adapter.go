// Package authorityprocess maps the frozen Stage 03 ingestion RPCs into the durable local
// authority. It keeps body identity as a cross-check and emits only complete
// protobuf facts or the one static non-disclosing denial shape.
package authorityprocess

import (
	"context"
	"os"
	"os/exec"
	"strings"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
	broker "github.com/sltbrta/sentra-code-memory-v2/services/broker/localauthority"
	gateway "github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type ingestionRuntime interface {
	AddSource(context.Context, brain.AddSourceRequest) (brain.IngestionResult, error)
	GetSourceStatus(context.Context, brain.SourceStatusRequest) (brain.SourceStatus, error)
	SearchCode(context.Context, brain.SearchCodeRequest) (brain.SearchCodeResult, error)
	ReconcileSource(context.Context, brain.ReconcileSourceRequest) (brain.IngestionResult, error)
	RevokeSource(context.Context, brain.RevokeSourceRequest) (brain.IngestionResult, error)
	ConfiguredIngestionSourceID() string
}

func (adapter *authorityAdapter) AddSource(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.AddSourceRequest,
) (*contractsv1.AddSourceResponse, error) {
	runtime, contextValue, ok := adapter.ingestionContext(peer.Identity, request.GetCaller(), "source.add")
	if !ok || request == nil || request.Policy == nil || !request.Policy.UseGitignore ||
		!request.Policy.UseOuroborosignore || request.Policy.SymlinkPolicy != contractsv1.SymlinkPolicy_SYMLINK_POLICY_RECORD_WITHOUT_FOLLOW ||
		!sameDigest(request.ExpectedConfigurationDigest, adapter.configuration) {
		return adapter.deniedAdd(peer.Identity), nil
	}
	result, err := runtime.AddSource(ctx, brain.AddSourceRequest{
		IngestionContext: contextValue, ExpectedCommitOID: request.ExpectedCommitOid, IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		return adapter.deniedAdd(peer.Identity), nil
	}
	generation, err := adapter.generation(result.Status)
	if err != nil {
		return adapter.deniedAdd(peer.Identity), nil
	}
	return &contractsv1.AddSourceResponse{
		Receipt: adapter.resultReceipt(result.Receipt, peer.Identity),
		Outcome: &contractsv1.AddSourceResponse_Success{Success: &contractsv1.AddSourceSuccess{
			Source: adapter.source(result.Status, peer.Identity), Generation: generation,
		}},
	}, nil
}

func (adapter *authorityAdapter) GetSourceStatus(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.GetSourceStatusRequest,
) (*contractsv1.GetSourceStatusResponse, error) {
	runtime, contextValue, ok := adapter.ingestionContext(peer.Identity, request.GetCaller(), "source.status")
	if !ok || request == nil {
		return adapter.deniedStatus(peer.Identity), nil
	}
	status, err := runtime.GetSourceStatus(ctx, brain.SourceStatusRequest{IngestionContext: contextValue})
	if err != nil || !sameOpaque(request.SourceId, "source", status.SourceID) {
		return adapter.deniedStatus(peer.Identity), nil
	}
	generation, err := adapter.generation(status)
	if err != nil {
		return adapter.deniedStatus(peer.Identity), nil
	}
	return &contractsv1.GetSourceStatusResponse{
		Receipt: adapter.completedReceipt("source-status", peer.Identity),
		Outcome: &contractsv1.GetSourceStatusResponse_Success{Success: &contractsv1.GetSourceStatusSuccess{
			Source: adapter.source(status, peer.Identity), State: sourceState(status.State), CurrentGeneration: generation,
		}},
	}, nil
}

func (adapter *authorityAdapter) SearchCode(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.SearchCodeRequest,
) (*contractsv1.SearchCodeResponse, error) {
	runtime, contextValue, ok := adapter.ingestionContext(peer.Identity, request.GetCaller(), "source.search")
	if !ok || request == nil {
		return adapter.deniedSearch(peer.Identity), nil
	}
	_, statusContext, statusOK := adapter.ingestionContext(peer.Identity, request.GetCaller(), "source.status")
	status, err := runtime.GetSourceStatus(ctx, brain.SourceStatusRequest{IngestionContext: statusContext})
	if !statusOK || err != nil || !sameOpaque(request.SourceId, "source", status.SourceID) ||
		!sameOpaque(request.GenerationId, "generation", status.GenerationID) {
		return adapter.deniedSearch(peer.Identity), nil
	}
	result, err := runtime.SearchCode(ctx, brain.SearchCodeRequest{
		IngestionContext: contextValue, GenerationID: request.GenerationId.Value, Query: request.Query,
		Kind: searchKind(request.Kind), Limit: request.PageSize, Cursor: cursorToken(request.After),
	})
	if err != nil || result.GenerationID != status.GenerationID {
		return adapter.deniedSearch(peer.Identity), nil
	}
	generation, err := adapter.generation(status)
	if err != nil {
		return adapter.deniedSearch(peer.Identity), nil
	}
	decision, err := adapter.broker.AuthorizeSource(ctx, peer.Identity, "source.search", adapter.brain)
	if err != nil || !decision.Allowed {
		return adapter.deniedSearch(peer.Identity), nil
	}
	return &contractsv1.SearchCodeResponse{
		Receipt: adapter.completedReceipt("source-search", peer.Identity),
		Outcome: &contractsv1.SearchCodeResponse_Success{Success: &contractsv1.SearchCodeSuccess{
			Generation: generation, Occurrences: adapter.occurrences(result.Matches, decision.RevocationEpoch), NextCursor: nextCursor(result.NextCursor),
		}},
	}, nil
}

func (adapter *authorityAdapter) ReconcileSource(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.ReconcileSourceRequest,
) (*contractsv1.ReconcileSourceResponse, error) {
	runtime, contextValue, ok := adapter.ingestionContext(peer.Identity, request.GetCaller(), "source.reconcile")
	if !ok || request == nil {
		return adapter.deniedReconcile(peer.Identity), nil
	}
	if !sameOpaque(request.SourceId, "source", runtime.ConfiguredIngestionSourceID()) {
		return adapter.deniedReconcile(peer.Identity), nil
	}
	result, err := runtime.ReconcileSource(ctx, brain.ReconcileSourceRequest{
		IngestionContext: contextValue, ExpectedGenerationID: request.ExpectedGenerationId.Value,
		ExpectedCommitOID: request.ExpectedCommitOid, TargetCommitOID: request.TargetCommitOid, IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		return adapter.deniedReconcile(peer.Identity), nil
	}
	generation, err := adapter.generation(result.Status)
	if err != nil {
		return adapter.deniedReconcile(peer.Identity), nil
	}
	return &contractsv1.ReconcileSourceResponse{
		Receipt: adapter.resultReceipt(result.Receipt, peer.Identity),
		Outcome: &contractsv1.ReconcileSourceResponse_Success{Success: &contractsv1.ReconcileSourceSuccess{Generation: generation}},
	}, nil
}

func (adapter *authorityAdapter) RevokeSource(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.RevokeSourceRequest,
) (*contractsv1.RevokeSourceResponse, error) {
	runtime, contextValue, ok := adapter.ingestionContext(peer.Identity, request.GetCaller(), "source.revoke")
	if !ok || request == nil {
		return adapter.deniedRevoke(peer.Identity), nil
	}
	if !sameOpaque(request.SourceId, "source", runtime.ConfiguredIngestionSourceID()) {
		return adapter.deniedRevoke(peer.Identity), nil
	}
	epoch, err := adapter.broker.RevocationEpoch(peer.Identity.Tenant)
	if err != nil {
		return adapter.deniedRevoke(peer.Identity), nil
	}
	result, err := runtime.RevokeSource(ctx, brain.RevokeSourceRequest{
		IngestionContext: contextValue, ExpectedGenerationID: request.ExpectedGenerationId.Value,
		RevocationEpoch: epoch, IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		return adapter.deniedRevoke(peer.Identity), nil
	}
	return &contractsv1.RevokeSourceResponse{
		Receipt: adapter.resultReceipt(result.Receipt, peer.Identity),
		Outcome: &contractsv1.RevokeSourceResponse_Success{Success: &contractsv1.RevokeSourceSuccess{}},
	}, nil
}

func (adapter *authorityAdapter) ingestionContext(identity brain.Identity, caller *contractsv1.UntrustedIngestionCaller, action string) (ingestionRuntime, brain.IngestionContext, bool) {
	if adapter == nil {
		return nil, brain.IngestionContext{}, false
	}
	runtime, ok := adapter.runtime.(ingestionRuntime)
	if !ok || adapter.broker == nil || adapter.now == nil || adapter.configuration.Algorithm != "sha256" ||
		len(adapter.configuration.Hex) != 64 || !sameIngestionCaller(caller, identity) {
		return nil, brain.IngestionContext{}, false
	}
	contextValue := brain.IngestionContext{Identity: identity, ConfigurationDigest: adapter.configuration,
		Policy: brain.IngestionPolicyBothIgnoreNoFollow, Fence: 1}
	contextValue.Authorize = func(ctx context.Context, mapped brain.Identity, requestedAction string, resource brain.Identifier) (brain.Authorization, error) {
		if requestedAction != action || resource != adapter.brain {
			return brain.Authorization{ReasonCode: "not_found_or_denied"}, broker.ErrDenied
		}
		decision, err := adapter.broker.AuthorizeSource(ctx, mapped, requestedAction, broker.Identifier(resource))
		return brain.Authorization{Allowed: decision.Allowed, ReasonCode: decision.ReasonCode, RevocationEpoch: decision.RevocationEpoch}, err
	}
	return runtime, contextValue, true
}

func sameIngestionCaller(caller *contractsv1.UntrustedIngestionCaller, identity brain.Identity) bool {
	return caller != nil && samePrincipal(caller.RequestedPrincipal, identity) && sameIdentifier(caller.RequestedSession, identity.Session)
}

func (adapter *authorityAdapter) source(status brain.SourceStatus, identity brain.Identity) *contractsv1.SourceReference {
	return &contractsv1.SourceReference{SourceId: &contractsv1.Identifier{Namespace: "source", Value: status.SourceID},
		RepositoryId: &contractsv1.Identifier{Namespace: "repository", Value: adapter.repositoryID},
		BrainId:      identifierProto(adapter.brain), TenantId: identifierProto(identity.Tenant)}
}

func (adapter *authorityAdapter) generation(status brain.SourceStatus) (*contractsv1.IngestionGeneration, error) {
	if status.GenerationID == "" || status.SnapshotID == "" || status.CommitOID == "" || status.TreeOID == "" ||
		status.PolicyDigest.Algorithm != "sha256" || len(status.PolicyDigest.Hex) != 64 || status.Sequence == 0 {
		return nil, errRequestDenied
	}
	lanes := make([]*contractsv1.LanguageReadiness, 0, len(status.Readiness))
	for _, lane := range status.Readiness {
		language, coverage := codeLanguage(lane.Language), coverageState(lane.Coverage)
		if language == contractsv1.CodeLanguage_CODE_LANGUAGE_UNSPECIFIED || coverage == contractsv1.CoverageState_COVERAGE_STATE_UNSPECIFIED {
			return nil, errRequestDenied
		}
		lanes = append(lanes, &contractsv1.LanguageReadiness{Language: language, Coverage: coverage, ReasonCode: lane.ReasonCode})
	}
	return &contractsv1.IngestionGeneration{
		GenerationId: &contractsv1.Identifier{Namespace: "generation", Value: status.GenerationID}, Sequence: status.Sequence,
		Snapshot: &contractsv1.GitSnapshot{SnapshotId: &contractsv1.Identifier{Namespace: "snapshot", Value: status.SnapshotID},
			CommitOid: status.CommitOID, TreeOid: status.TreeOID, PolicyDigest: protoDigest(status.PolicyDigest)},
		State: generationState(status.State), LanguageReadiness: lanes, SourceWatermark: status.Sequence,
	}, nil
}

func (adapter *authorityAdapter) occurrences(matches []brain.CodeMatch, epoch uint64) []*contractsv1.CodeOccurrence {
	result := make([]*contractsv1.CodeOccurrence, 0, len(matches))
	for _, match := range matches {
		language, coverage := codeLanguage(match.Language), coverageState(match.Coverage)
		if language == contractsv1.CodeLanguage_CODE_LANGUAGE_UNSPECIFIED {
			continue
		}
		symbol := &contractsv1.Identifier{Namespace: "symbol", Value: match.Content}
		if match.Content == "" {
			symbol = nil
		}
		digest, ok := contentDigest(match.ContentDigest)
		if !ok {
			continue
		}
		result = append(result, &contractsv1.CodeOccurrence{
			SourceRevision: &contractsv1.SourceRevision{SourceRevisionId: &contractsv1.Identifier{Namespace: "revision", Value: match.RevisionID},
				SourceObjectId: &contractsv1.Identifier{Namespace: "object", Value: match.SourceObjectID}, UpstreamRevision: match.BlobOID,
				ContentDigest: digest, MediaType: match.MediaType,
				ByteLength: match.ByteLength, ObservedAt: timestamppb.New(adapter.now().UTC()),
				DeletionState: contractsv1.DeletionState_DELETION_STATE_ACTIVE, AclEpoch: epoch, EncryptionKeyEpoch: adapter.keyEpoch,
				ProvenanceDigest: protoDigest(adapter.configuration)},
			Anchor: &contractsv1.EvidenceAnchor_CodeAnchor{GitOid: match.BlobOID, SymbolId: symbol,
				Range: &contractsv1.SourceRange{Path: match.Path, StartLine: match.StartLine, StartColumn: match.StartColumn, EndLine: match.EndLine, EndColumn: match.EndColumn}},
			Language: language, Symbol: match.Content, Coverage: coverage,
		})
	}
	return result
}

func contentDigest(value string) (*contractsv1.Digest, bool) {
	algorithm, hexValue, found := strings.Cut(value, ":")
	if !found || algorithm != "sha256" || len(hexValue) != 64 {
		return nil, false
	}
	for _, character := range hexValue {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return nil, false
		}
	}
	return &contractsv1.Digest{Algorithm: algorithm, Hex: hexValue}, true
}

func (adapter *authorityAdapter) completedReceipt(operation string, identity brain.Identity) *contractsv1.Receipt {
	return adapter.ingestionReceipt(operation, identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, "")
}

func (adapter *authorityAdapter) resultReceipt(value shared.Receipt, identity brain.Identity) *contractsv1.Receipt {
	if value.Status == "rejected" {
		return adapter.ingestionReceipt("source", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied")
	}
	result := receipt(value, adapter.now().UTC().UnixMilli(), adapter.configuration, sessionCausal(identity))
	result.Status = contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED
	return result
}

func (adapter *authorityAdapter) ingestionReceipt(operation string, identity brain.Identity, status contractsv1.ReceiptStatus, reason string) *contractsv1.Receipt {
	return &contractsv1.Receipt{ReceiptId: &contractsv1.Identifier{Namespace: "receipt", Value: operation},
		OperationId: &contractsv1.Identifier{Namespace: "operation", Value: operation}, Status: status, ReasonCode: reason,
		Causal: sessionCausal(identity), RecordedAt: timestamppb.New(adapter.now().UTC()), ConfigurationDigest: protoDigest(adapter.configuration)}
}

func (adapter *authorityAdapter) deniedAdd(identity brain.Identity) *contractsv1.AddSourceResponse {
	return &contractsv1.AddSourceResponse{Receipt: adapter.ingestionReceipt("source-add", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"), Outcome: &contractsv1.AddSourceResponse_Error{Error: staticIngestionError()}}
}

func (adapter *authorityAdapter) deniedStatus(identity brain.Identity) *contractsv1.GetSourceStatusResponse {
	return &contractsv1.GetSourceStatusResponse{Receipt: adapter.ingestionReceipt("source-status", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"), Outcome: &contractsv1.GetSourceStatusResponse_Error{Error: staticIngestionError()}}
}

func (adapter *authorityAdapter) deniedSearch(identity brain.Identity) *contractsv1.SearchCodeResponse {
	return &contractsv1.SearchCodeResponse{Receipt: adapter.ingestionReceipt("source-search", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"), Outcome: &contractsv1.SearchCodeResponse_Error{Error: staticIngestionError()}}
}

func (adapter *authorityAdapter) deniedReconcile(identity brain.Identity) *contractsv1.ReconcileSourceResponse {
	return &contractsv1.ReconcileSourceResponse{Receipt: adapter.ingestionReceipt("source-reconcile", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"), Outcome: &contractsv1.ReconcileSourceResponse_Error{Error: staticIngestionError()}}
}

func (adapter *authorityAdapter) deniedRevoke(identity brain.Identity) *contractsv1.RevokeSourceResponse {
	return &contractsv1.RevokeSourceResponse{Receipt: adapter.ingestionReceipt("source-revoke", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"), Outcome: &contractsv1.RevokeSourceResponse_Error{Error: staticIngestionError()}}
}

func staticIngestionError() *contractsv1.PublicError {
	return &contractsv1.PublicError{Code: "not_found_or_denied"}
}

func sameDigest(value *contractsv1.Digest, expected brain.Digest) bool {
	return value != nil && value.Algorithm == expected.Algorithm && value.Hex == expected.Hex
}
func sameOpaque(value *contractsv1.Identifier, namespace, expected string) bool {
	return value != nil && value.Namespace == namespace && value.Value == expected
}
func cursorToken(cursor *contractsv1.Cursor) string {
	if cursor == nil {
		return ""
	}
	return cursor.Token
}
func nextCursor(token string) *contractsv1.Cursor {
	if token == "" {
		return nil
	}
	return &contractsv1.Cursor{Token: token}
}

func searchKind(kind contractsv1.SearchKind) brain.SearchKind {
	switch kind {
	case contractsv1.SearchKind_SEARCH_KIND_EXACT:
		return brain.SearchExact
	case contractsv1.SearchKind_SEARCH_KIND_SYMBOL:
		return brain.SearchSymbol
	case contractsv1.SearchKind_SEARCH_KIND_REFERENCE:
		return brain.SearchReference
	default:
		return ""
	}
}
func sourceState(value string) contractsv1.SourceState {
	if value == "ready" {
		return contractsv1.SourceState_SOURCE_STATE_READY
	}
	return contractsv1.SourceState_SOURCE_STATE_ADMITTED
}
func generationState(value string) contractsv1.GenerationState {
	if value == "degraded" {
		return contractsv1.GenerationState_GENERATION_STATE_DEGRADED
	}
	return contractsv1.GenerationState_GENERATION_STATE_READY
}
func coverageState(value string) contractsv1.CoverageState {
	if value == "lexical_degraded" {
		return contractsv1.CoverageState_COVERAGE_STATE_LEXICAL_DEGRADED
	}
	if value == "syntax_aware" || value == "COVERAGE_STATE_SYNTAX_AWARE" {
		return contractsv1.CoverageState_COVERAGE_STATE_SYNTAX_AWARE
	}
	if value == "COVERAGE_STATE_LEXICAL_DEGRADED" {
		return contractsv1.CoverageState_COVERAGE_STATE_LEXICAL_DEGRADED
	}
	return contractsv1.CoverageState_COVERAGE_STATE_UNSPECIFIED
}
func codeLanguage(value string) contractsv1.CodeLanguage {
	switch value {
	case "go":
		return contractsv1.CodeLanguage_CODE_LANGUAGE_GO
	case "typescript":
		return contractsv1.CodeLanguage_CODE_LANGUAGE_TYPESCRIPT
	case "python":
		return contractsv1.CodeLanguage_CODE_LANGUAGE_PYTHON
	case "rust":
		return contractsv1.CodeLanguage_CODE_LANGUAGE_RUST
	case "java":
		return contractsv1.CodeLanguage_CODE_LANGUAGE_JAVA
	default:
		return contractsv1.CodeLanguage_CODE_LANGUAGE_UNSPECIFIED
	}
}

func commandOutput(ctx context.Context, executable string, arguments []string) ([]byte, error) {
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Env = sanitizedGitEnvironment(os.Environ())
	return command.Output()
}

func sanitizedGitEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, value := range environment {
		name, _, found := strings.Cut(value, "=")
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_CEILING_DIRECTORIES":
			continue
		}
		if found {
			result = append(result, value)
		}
	}
	return result
}
