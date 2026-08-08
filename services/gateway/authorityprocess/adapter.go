// Package authorityprocess composes the owner-only Unix gateway with the Stage 2 broker and
// brain runtime. The adapter preserves authenticated peer identity separately
// from untrusted protobuf body fields.
package authorityprocess

import (
	"context"
	"errors"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
	broker "github.com/sltbrta/sentra-code-memory-v2/services/broker/localauthority"
	gateway "github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var errRequestDenied = errors.New("local authority command: request denied")

type authorityRuntime interface {
	OpenSession(context.Context, brain.Identity) (brain.Result, error)
	Execute(context.Context, brain.ExecuteRequest) (brain.Result, error)
	ReadStatus(context.Context, brain.Identity) (brain.Status, error)
}

type authorityAdapter struct {
	runtime        authorityRuntime
	broker         *broker.Broker
	keyEpoch       uint64
	now            func() time.Time
	configuration  brain.Digest
	brain          brain.Identifier
	repositoryID   string
	approvedRoot   string
	gitExecutable  string
	commandTimeout time.Duration
}

type peerMapper struct{ broker *broker.Broker }

func (mapper peerMapper) MapPeer(credentials gateway.PeerCredentials) (shared.MappedIdentityFact, error) {
	if mapper.broker == nil {
		return shared.MappedIdentityFact{}, errRequestDenied
	}
	return mapper.broker.MapPeer(broker.PeerCredentials{
		UID: credentials.UID, GID: credentials.GID, PID: credentials.PID,
	})
}

func (adapter *authorityAdapter) OpenSession(
	ctx context.Context,
	peer gateway.PeerContext,
	request *contractsv1.OpenLocalSessionRequest,
) (*contractsv1.OpenLocalSessionResponse, error) {
	if adapter == nil || adapter.runtime == nil || !samePrincipal(request.GetRequestedPrincipal(), peer.Identity) {
		return nil, errRequestDenied
	}
	result, err := adapter.runtime.OpenSession(ctx, peer.Identity)
	if err != nil {
		return nil, errRequestDenied
	}
	return &contractsv1.OpenLocalSessionResponse{
		Session: principal(peer.Identity),
		Receipt: receipt(result.Receipt, result.RecordedAtMilli, result.ConfigurationDigest, sessionCausal(peer.Identity)),
	}, nil
}

func (adapter *authorityAdapter) Execute(
	ctx context.Context,
	peer gateway.PeerContext,
	request *contractsv1.ExecuteAuthorityCommandRequest,
) (*contractsv1.ExecuteAuthorityCommandResponse, error) {
	if adapter == nil || adapter.runtime == nil || adapter.broker == nil || request == nil ||
		!samePrincipal(request.GetCommand().GetActor(), peer.Identity) ||
		!samePrincipal(request.GetGrant().GetInitiator(), peer.Identity) {
		return nil, errRequestDenied
	}
	if adapter.keyEpoch == 0 || adapter.now == nil {
		return nil, errRequestDenied
	}
	domain, err := executeRequest(peer.Identity, request, adapter.broker, adapter.keyEpoch, adapter.now)
	if err != nil {
		return nil, errRequestDenied
	}
	result, err := adapter.runtime.Execute(ctx, domain)
	if err != nil {
		return nil, errRequestDenied
	}
	defer clear(result.Bytes)
	if result.Receipt.Status == "rejected" {
		if result.Authorization.Allowed {
			return nil, errRequestDenied
		}
		return rejectedExecuteResponse(request, result), nil
	}
	if !result.Authorization.Allowed {
		return nil, errRequestDenied
	}
	response := &contractsv1.ExecuteAuthorityCommandResponse{
		Receipt:  receipt(result.Receipt, result.RecordedAtMilli, result.ConfigurationDigest, request.Command.Causal),
		Artifact: requestArtifact(request), Generation: result.Artifact.Generation,
		NextCursor: &contractsv1.Cursor{Watermark: result.NextOffset},
	}
	if result.RangeDigest.Hex != "" {
		response.FrameDigest = protoDigest(result.RangeDigest)
	}
	response.Authorization = authorizationReceipt(request, result)
	return response, nil
}

func rejectedExecuteResponse(
	request *contractsv1.ExecuteAuthorityCommandRequest,
	result brain.Result,
) *contractsv1.ExecuteAuthorityCommandResponse {
	return &contractsv1.ExecuteAuthorityCommandResponse{
		Receipt:       receipt(result.Receipt, result.RecordedAtMilli, result.ConfigurationDigest, request.Command.Causal),
		Authorization: authorizationReceipt(request, result),
		Error: &contractsv1.PublicError{
			Code: "request-denied",
			Render: &contractsv1.RenderModel{
				Title: "Request denied", Detail: "The requested local authority operation was not permitted.",
			},
		},
	}
}

func (adapter *authorityAdapter) ReadStatus(
	ctx context.Context,
	peer gateway.PeerContext,
	request *contractsv1.ReadStatusRequest,
) (*contractsv1.ReadStatusResponse, error) {
	if adapter == nil || adapter.runtime == nil || adapter.broker == nil || request == nil ||
		!sameIdentifier(request.RequestedSession, peer.Identity.Session) {
		return nil, errRequestDenied
	}
	status, err := adapter.runtime.ReadStatus(ctx, peer.Identity)
	if err != nil {
		return nil, errRequestDenied
	}
	status.RevocationEpoch, err = adapter.broker.RevocationEpoch(peer.Identity.Tenant)
	if err != nil {
		return nil, errRequestDenied
	}
	return &contractsv1.ReadStatusResponse{
		Session: principal(status.Identity), Watermark: status.Watermark,
		RevocationEpoch: status.RevocationEpoch,
		ObservedAt:      timestamppb.New(time.UnixMilli(status.ObservedAtMilli)),
		Receipt:         receipt(status.Receipt, status.ObservedAtMilli, status.ConfigurationDigest, sessionCausal(status.Identity)),
		Render:          &contractsv1.RenderModel{Title: "Local authority ready", Detail: "Authenticated local state is available."},
	}, nil
}

func executeRequest(
	identity brain.Identity,
	request *contractsv1.ExecuteAuthorityCommandRequest,
	policy *broker.Broker,
	keyEpoch uint64,
	now func() time.Time,
) (brain.ExecuteRequest, error) {
	command := request.GetCommand()
	if command == nil || command.Causal == nil || command.Causal.Fence == 0 ||
		request.GetGrant() == nil || request.GetGrant().CommandFence != command.Causal.Fence {
		return brain.ExecuteRequest{}, errRequestDenied
	}
	artifact, offset, length, purgeNow, availableUsage, err := domainOperation(request, keyEpoch)
	if err != nil {
		return brain.ExecuteRequest{}, errRequestDenied
	}
	fingerprint, err := OperationFingerprint(identity, request)
	if err != nil || !matchesFingerprint(command.PayloadDigest, fingerprint) {
		return brain.ExecuteRequest{}, errRequestDenied
	}
	grant, usage, err := domainGrant(request.GetGrant(), identity, availableUsage)
	if err != nil {
		return brain.ExecuteRequest{}, errRequestDenied
	}
	domain := brain.ExecuteRequest{
		Identity: identity,
		Command: brain.Command{
			ID: identifier(command.CommandId), Type: command.CommandType,
			IdempotencyKey: command.IdempotencyKey, PayloadDigest: fingerprint,
			Fence: command.Causal.GetFence(),
		},
		Artifact: artifact, Offset: offset, Length: length, PurgeNow: purgeNow,
		Authorize: func(ctx context.Context, mapped brain.Identity, action string, resource brain.Identifier) (brain.Authorization, error) {
			use := broker.NewUse(action, resource, command.Causal.Fence, grant.RevocationEpoch, grant.Nonce, now())
			use.Usage = cloneUsage(usage)
			decision, err := policy.Authorize(ctx, mapped, grant, use)
			return brain.Authorization{
				Allowed: decision.Allowed, ReasonCode: decision.ReasonCode,
				RevocationEpoch: decision.RevocationEpoch,
			}, err
		},
	}
	return domain, nil
}

func domainOperation(
	request *contractsv1.ExecuteAuthorityCommandRequest,
	keyEpoch uint64,
) (brain.Artifact, uint64, uint64, bool, map[string]uint64, error) {
	if request == nil || request.Command == nil || keyEpoch == 0 {
		return brain.Artifact{}, 0, 0, false, nil, errRequestDenied
	}
	var domain brain.Artifact
	switch operation := request.ArtifactCommand.(type) {
	case *contractsv1.ExecuteAuthorityCommandRequest_ArtifactAdmit:
		if operation.ArtifactAdmit == nil || request.Command.CommandType != "artifact.admit" {
			return brain.Artifact{}, 0, 0, false, nil, errRequestDenied
		}
		domain = artifact(operation.ArtifactAdmit.Artifact)
		domain.Generation = operation.ArtifactAdmit.ExpectedGeneration + 1
		domain.ExpectedGeneration = operation.ArtifactAdmit.ExpectedGeneration
		domain.Length = operation.ArtifactAdmit.DeclaredLength
		domain.FrameCount = operation.ArtifactAdmit.FrameCount
		domain.KeyEpoch = keyEpoch
		return domain, 0, 0, false, map[string]uint64{
			"bytes":  operation.ArtifactAdmit.DeclaredLength,
			"frames": uint64(operation.ArtifactAdmit.FrameCount),
		}, nil
	case *contractsv1.ExecuteAuthorityCommandRequest_ArtifactRead:
		if operation.ArtifactRead == nil || request.Command.CommandType != "artifact.read" {
			return brain.Artifact{}, 0, 0, false, nil, errRequestDenied
		}
		domain = artifact(operation.ArtifactRead.Artifact)
		domain.Generation = operation.ArtifactRead.Generation
		domain.KeyEpoch = keyEpoch
		return domain, operation.ArtifactRead.Offset, operation.ArtifactRead.Length, false,
			map[string]uint64{"bytes": operation.ArtifactRead.Length}, nil
	case *contractsv1.ExecuteAuthorityCommandRequest_ArtifactDelete:
		if operation.ArtifactDelete == nil || request.Command.CommandType != "artifact.delete" {
			return brain.Artifact{}, 0, 0, false, nil, errRequestDenied
		}
		domain = artifact(operation.ArtifactDelete.Artifact)
		domain.Generation = operation.ArtifactDelete.ExpectedGeneration
		domain.ExpectedGeneration = operation.ArtifactDelete.ExpectedGeneration
		domain.KeyEpoch = keyEpoch
		return domain, 0, 0, operation.ArtifactDelete.PurgeAfterTombstone, map[string]uint64{}, nil
	default:
		return brain.Artifact{}, 0, 0, false, nil, errRequestDenied
	}
}

func domainGrant(
	value *contractsv1.CapabilityGrant,
	identity brain.Identity,
	availableUsage map[string]uint64,
) (broker.Grant, map[string]uint64, error) {
	if value == nil || value.ExpiresAt == nil || value.GrantId == nil || value.GrantId.Namespace != "grant" ||
		value.TaskId != nil || value.WorkflowId != nil || value.Lease != nil || value.RepositoryGitOid != "" ||
		len(value.AllowedPaths) != 0 || len(value.ToolGrants) != 0 || len(value.Egress) != 0 ||
		value.PolicyDigest == nil || value.PolicyDigest.Algorithm != "sha256" || len(value.PolicyDigest.Hex) != 64 {
		return broker.Grant{}, nil, errRequestDenied
	}
	resources := make([]broker.Identifier, 0, len(value.Resources))
	for _, resource := range value.Resources {
		resources = append(resources, identifier(resource))
	}
	limits := make(map[string]uint64, len(value.Limits))
	usage := make(map[string]uint64, len(value.Limits))
	for _, limit := range value.Limits {
		if limit == nil {
			return broker.Grant{}, nil, errRequestDenied
		}
		actual, applicable := availableUsage[limit.Name]
		if _, duplicate := limits[limit.Name]; duplicate || !applicable {
			return broker.Grant{}, nil, errRequestDenied
		}
		limits[limit.Name] = limit.Maximum
		usage[limit.Name] = actual
	}
	return broker.Grant{
		ID: value.GrantId.GetValue(), IDNamespace: value.GrantId.Namespace,
		Principal: identity.Principal, Tenant: identity.Tenant, PolicyDigest: digest(value.PolicyDigest),
		Actions: append([]string(nil), value.Actions...), Resources: resources,
		AllowedPaths: append([]string(nil), value.AllowedPaths...), Limits: limits,
		Fence: value.CommandFence, RevocationEpoch: value.RevocationEpoch,
		ExpiresAt: value.ExpiresAt.AsTime(), Nonce: value.Nonce,
	}, usage, nil
}

func cloneUsage(usage map[string]uint64) map[string]uint64 {
	if len(usage) == 0 {
		return nil
	}
	cloned := make(map[string]uint64, len(usage))
	for name, amount := range usage {
		cloned[name] = amount
	}
	return cloned
}

func artifact(value *contractsv1.ArtifactRef) brain.Artifact {
	if value == nil {
		return brain.Artifact{}
	}
	return brain.Artifact{ID: identifier(value.ArtifactId), Tenant: identifier(value.TenantId), Digest: digest(value.ContentDigest)}
}

func requestArtifact(request *contractsv1.ExecuteAuthorityCommandRequest) *contractsv1.ArtifactRef {
	switch operation := request.ArtifactCommand.(type) {
	case *contractsv1.ExecuteAuthorityCommandRequest_ArtifactAdmit:
		return operation.ArtifactAdmit.Artifact
	case *contractsv1.ExecuteAuthorityCommandRequest_ArtifactRead:
		return operation.ArtifactRead.Artifact
	case *contractsv1.ExecuteAuthorityCommandRequest_ArtifactDelete:
		return operation.ArtifactDelete.Artifact
	default:
		return nil
	}
}
