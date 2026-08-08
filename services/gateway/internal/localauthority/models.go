// Package localauthority exposes the authenticated local authority gateway.
// It authenticates and validates transport input while preserving the complete
// frozen protobuf messages passed to and returned by canonical authority.
package localauthority

import (
	"context"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

const (
	openSessionProcedure       = "/ouroboros.contracts.v1.LocalAuthorityService/OpenLocalSession"
	executeCommandProcedure    = "/ouroboros.contracts.v1.LocalAuthorityService/ExecuteAuthorityCommand"
	readStatusProcedure        = "/ouroboros.contracts.v1.LocalAuthorityService/ReadStatus"
	addSourceProcedure         = "/ouroboros.contracts.v1.IngestionService/AddSource"
	getSourceStatusProcedure   = "/ouroboros.contracts.v1.IngestionService/GetSourceStatus"
	searchCodeProcedure        = "/ouroboros.contracts.v1.IngestionService/SearchCode"
	reconcileSourceProcedure   = "/ouroboros.contracts.v1.IngestionService/ReconcileSource"
	revokeSourceProcedure      = "/ouroboros.contracts.v1.IngestionService/RevokeSource"
	askProcedure               = "/ouroboros.contracts.v1.QueryService/Ask"
	listSourcesProcedure       = "/ouroboros.contracts.v1.QueryService/ListSources"
	getHistoryProcedure        = "/ouroboros.contracts.v1.QueryService/GetHistory"
	getStatusProcedure         = "/ouroboros.contracts.v1.QueryService/GetStatus"
	admitChangeIntentProcedure = "/ouroboros.contracts.v1.FactoryService/AdmitChangeIntent"
	getChangePlanProcedure     = "/ouroboros.contracts.v1.FactoryService/GetChangePlan"
	previewChangeSetProcedure  = "/ouroboros.contracts.v1.FactoryService/PreviewChangeSet"
	getReviewFindingsProcedure = "/ouroboros.contracts.v1.FactoryService/GetReviewFindings"
	cancelChangeRunProcedure   = "/ouroboros.contracts.v1.FactoryService/CancelChangeRun"
	// Stage 06 Tracer 001 composition facade (JSON wire, not product protobuf RPCs).
	tracerSessionProcedure = "/ouroboros.contracts.v1.Tracer001Service/Session"
	tracerIngestProcedure  = "/ouroboros.contracts.v1.Tracer001Service/Ingest"
	tracerAskProcedure     = "/ouroboros.contracts.v1.Tracer001Service/Ask"
	tracerIntentProcedure  = "/ouroboros.contracts.v1.Tracer001Service/Intent"
	tracerPlanProcedure    = "/ouroboros.contracts.v1.Tracer001Service/Plan"
	tracerReviewProcedure  = "/ouroboros.contracts.v1.Tracer001Service/Review"
	tracerDraftPRProcedure = "/ouroboros.contracts.v1.Tracer001Service/DraftPr"
	tracerOutcomeProcedure = "/ouroboros.contracts.v1.Tracer001Service/Outcome"
	// Stage 07 meeting-transcript procedures (protobuf RPCs).
	importTranscriptProcedure = "/ouroboros.contracts.v1.MeetingService/ImportTranscript"
	getMeetingStatusProcedure = "/ouroboros.contracts.v1.MeetingService/GetMeetingStatus"
	queryMeetingProcedure     = "/ouroboros.contracts.v1.MeetingService/QueryMeeting"
	revokeMeetingProcedure    = "/ouroboros.contracts.v1.MeetingService/RevokeMeeting"
	purgeMeetingProcedure     = "/ouroboros.contracts.v1.MeetingService/PurgeMeeting"
	// Stage 08 connector procedures (protobuf RPCs).
	connectGitHubProcedure      = "/ouroboros.contracts.v1.ConnectorService/ConnectGitHubSource"
	getConnectorStatusProcedure = "/ouroboros.contracts.v1.ConnectorService/GetConnectorStatus"
	reconcileConnectorProcedure = "/ouroboros.contracts.v1.ConnectorService/ReconcileConnector"
	queryConnectorProcedure     = "/ouroboros.contracts.v1.ConnectorService/QueryConnectorEvidence"
	revokeConnectorProcedure    = "/ouroboros.contracts.v1.ConnectorService/RevokeConnector"
	purgeConnectorProcedure     = "/ouroboros.contracts.v1.ConnectorService/PurgeConnector"
	// Stage 11 multimodal procedures (protobuf RPCs).
	admitMultimodalProcedure       = "/ouroboros.contracts.v1.MultimodalService/AdmitMultimodalSource"
	getMultimodalStatusProcedure   = "/ouroboros.contracts.v1.MultimodalService/GetMultimodalStatus"
	getMultimodalEvidenceProcedure = "/ouroboros.contracts.v1.MultimodalService/GetMultimodalEvidence"
	revokeMultimodalProcedure      = "/ouroboros.contracts.v1.MultimodalService/RevokeMultimodalSource"
	purgeMultimodalProcedure       = "/ouroboros.contracts.v1.MultimodalService/PurgeMultimodalSource"
	maxRequestBytes                = 64 * 1024
)

// Authority receives the authenticated peer separately from the complete body.
// Implementations own canonical idempotency, authorization, persistence, and
// response construction; the gateway never fabricates or collapses those facts.
type Authority interface {
	OpenSession(context.Context, PeerContext, *contractsv1.OpenLocalSessionRequest) (*contractsv1.OpenLocalSessionResponse, error)
	Execute(context.Context, PeerContext, *contractsv1.ExecuteAuthorityCommandRequest) (*contractsv1.ExecuteAuthorityCommandResponse, error)
	ReadStatus(context.Context, PeerContext, *contractsv1.ReadStatusRequest) (*contractsv1.ReadStatusResponse, error)
}

// IngestionAuthority receives authenticated ingestion requests after schema
// validation and body-identity cross-checks. Implementations own authorization,
// source existence, canonical state transitions, and response construction.
type IngestionAuthority interface {
	AddSource(context.Context, PeerContext, *contractsv1.AddSourceRequest) (*contractsv1.AddSourceResponse, error)
	GetSourceStatus(context.Context, PeerContext, *contractsv1.GetSourceStatusRequest) (*contractsv1.GetSourceStatusResponse, error)
	SearchCode(context.Context, PeerContext, *contractsv1.SearchCodeRequest) (*contractsv1.SearchCodeResponse, error)
	ReconcileSource(context.Context, PeerContext, *contractsv1.ReconcileSourceRequest) (*contractsv1.ReconcileSourceResponse, error)
	RevokeSource(context.Context, PeerContext, *contractsv1.RevokeSourceRequest) (*contractsv1.RevokeSourceResponse, error)
}

// QueryAuthority receives authenticated Stage 04 query requests after schema
// validation and body-identity cross-checks. Implementations own authorization,
// admission, grounded answering, conversation history, and response
// construction; the transport widens only the served surface, never the trust
// boundary. Any returned error maps to the static request-denied shape.
type QueryAuthority interface {
	Ask(context.Context, PeerContext, *contractsv1.AskRequest) (*contractsv1.AskResponse, error)
	ListSources(context.Context, PeerContext, *contractsv1.ListSourcesRequest) (*contractsv1.ListSourcesResponse, error)
	GetHistory(context.Context, PeerContext, *contractsv1.GetHistoryRequest) (*contractsv1.GetHistoryResponse, error)
	GetStatus(context.Context, PeerContext, *contractsv1.GetStatusRequest) (*contractsv1.GetStatusResponse, error)
}

// FactoryAuthority receives authenticated Stage 05 factory requests after
// schema validation and body-identity cross-checks. Implementations own
// admission revalidation, run authority, candidate atomicity, review facts,
// and response construction; the transport widens only the served surface,
// never the trust boundary. Any returned error maps to the static
// request-denied shape.
type FactoryAuthority interface {
	AdmitChangeIntent(context.Context, PeerContext, *contractsv1.AdmitChangeIntentRequest) (*contractsv1.AdmitChangeIntentResponse, error)
	GetChangePlan(context.Context, PeerContext, *contractsv1.GetChangePlanRequest) (*contractsv1.GetChangePlanResponse, error)
	PreviewChangeSet(context.Context, PeerContext, *contractsv1.PreviewChangeSetRequest) (*contractsv1.PreviewChangeSetResponse, error)
	GetReviewFindings(context.Context, PeerContext, *contractsv1.GetReviewFindingsRequest) (*contractsv1.GetReviewFindingsResponse, error)
	CancelChangeRun(context.Context, PeerContext, *contractsv1.CancelChangeRunRequest) (*contractsv1.CancelChangeRunResponse, error)
}

// TracerAuthority receives authenticated Stage 06 Tracer 001 JSON path steps
// after peer authentication. Request and response bodies are application/json
// composition-facade payloads (not product protobuf RPCs). Implementations own
// composition into Stages 03–05 + draft-PR/outcome; any returned error maps to
// the static request-denied shape unless it is a malformed-request sentinel.
type TracerAuthority interface {
	// Advance executes one public path step. step is the public procedure leaf
	// name: Session, Ingest, Ask, Intent, Plan, Review, DraftPr, Outcome.
	// body is the raw JSON request; the response is raw JSON bytes.
	Advance(ctx context.Context, peer PeerContext, step string, body []byte) ([]byte, error)
}

// MeetingAuthority receives authenticated Stage 07 meeting requests after
// schema validation and body-identity cross-checks. Implementations own
// transcript import, temporal query, revoke, purge, and response construction;
// the transport widens only the served surface, never the trust boundary.
// Any returned error maps to the static request-denied shape.
type MeetingAuthority interface {
	ImportTranscript(context.Context, PeerContext, *contractsv1.ImportTranscriptRequest) (*contractsv1.ImportTranscriptResponse, error)
	GetMeetingStatus(context.Context, PeerContext, *contractsv1.GetMeetingStatusRequest) (*contractsv1.GetMeetingStatusResponse, error)
	QueryMeeting(context.Context, PeerContext, *contractsv1.QueryMeetingRequest) (*contractsv1.QueryMeetingResponse, error)
	RevokeMeeting(context.Context, PeerContext, *contractsv1.RevokeMeetingRequest) (*contractsv1.RevokeMeetingResponse, error)
	PurgeMeeting(context.Context, PeerContext, *contractsv1.PurgeMeetingRequest) (*contractsv1.PurgeMeetingResponse, error)
}

// ConnectorAuthority receives authenticated Stage 08 connector requests after
// schema validation and body-identity cross-checks. Implementations own
// GitHub source connection lifecycle, cursor/reconcile, cited query, revoke,
// purge, and response construction; the transport widens only the served
// surface, never the trust boundary. Any returned error maps to the static
// request-denied shape.
type ConnectorAuthority interface {
	ConnectGitHubSource(context.Context, PeerContext, *contractsv1.ConnectGitHubSourceRequest) (*contractsv1.ConnectGitHubSourceResponse, error)
	GetConnectorStatus(context.Context, PeerContext, *contractsv1.GetConnectorStatusRequest) (*contractsv1.GetConnectorStatusResponse, error)
	ReconcileConnector(context.Context, PeerContext, *contractsv1.ReconcileConnectorRequest) (*contractsv1.ReconcileConnectorResponse, error)
	QueryConnectorEvidence(context.Context, PeerContext, *contractsv1.QueryConnectorEvidenceRequest) (*contractsv1.QueryConnectorEvidenceResponse, error)
	RevokeConnector(context.Context, PeerContext, *contractsv1.RevokeConnectorRequest) (*contractsv1.RevokeConnectorResponse, error)
	PurgeConnector(context.Context, PeerContext, *contractsv1.PurgeConnectorRequest) (*contractsv1.PurgeConnectorResponse, error)
}

// MultimodalAuthority receives authenticated Stage 11 multimodal requests after
// schema validation and body-identity cross-checks. Implementations own source
// admission, readiness, evidence, revoke, purge, and response construction;
// the transport widens only the served surface, never the trust boundary.
// Any returned error maps to the static request-denied shape.
type MultimodalAuthority interface {
	AdmitMultimodalSource(context.Context, PeerContext, *contractsv1.AdmitMultimodalSourceRequest) (*contractsv1.AdmitMultimodalSourceResponse, error)
	GetMultimodalStatus(context.Context, PeerContext, *contractsv1.GetMultimodalStatusRequest) (*contractsv1.GetMultimodalStatusResponse, error)
	GetMultimodalEvidence(context.Context, PeerContext, *contractsv1.GetMultimodalEvidenceRequest) (*contractsv1.GetMultimodalEvidenceResponse, error)
	RevokeMultimodalSource(context.Context, PeerContext, *contractsv1.RevokeMultimodalSourceRequest) (*contractsv1.RevokeMultimodalSourceResponse, error)
	PurgeMultimodalSource(context.Context, PeerContext, *contractsv1.PurgeMultimodalSourceRequest) (*contractsv1.PurgeMultimodalSourceResponse, error)
}

// PeerMapper maps authenticated operating-system facts into authority identity.
// A mapping failure denies the connection before any request bytes are read.
type PeerMapper interface {
	MapPeer(PeerCredentials) (shared.MappedIdentityFact, error)
}

// PeerMapperFunc adapts a function into the narrow composition interface.
type PeerMapperFunc func(PeerCredentials) (shared.MappedIdentityFact, error)

// MapPeer invokes the wrapped peer mapping function.
func (mapper PeerMapperFunc) MapPeer(credentials PeerCredentials) (shared.MappedIdentityFact, error) {
	return mapper(credentials)
}

// PeerCredentials are operating-system facts captured before request decode.
type PeerCredentials struct {
	// UID is the authenticated operating-system user identifier.
	UID uint32
	// GID is the authenticated primary peer group identifier.
	GID uint32
	// PID is the authenticated peer process identifier.
	PID uint32
}

// PeerContext binds authenticated operating-system facts to authority identity.
type PeerContext struct {
	// Credentials are captured from the accepted Unix socket.
	Credentials PeerCredentials
	// Identity is the trusted configured mapping for those credentials.
	Identity shared.MappedIdentityFact
}
