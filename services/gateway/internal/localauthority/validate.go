package localauthority

import (
	"errors"
	"math"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

var (
	errMalformedProto  = errors.New("malformed protobuf request")
	errInvalidResponse = errors.New("invalid authority response")
	actionPattern      = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,127}$`)
	resourcePattern    = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	hexPattern         = regexp.MustCompile(`^[0-9a-f]+$`)
)

func validateOpenRequest(request *contractsv1.OpenLocalSessionRequest, peer PeerContext) error {
	if request == nil || !validPrincipal(request.RequestedPrincipal, true) ||
		!validString(request.IdempotencyKey, 1, 512) || !validCursor(request.ResumeFrom) {
		return errMalformedProto
	}
	if !samePrincipal(request.RequestedPrincipal, peer.Identity) {
		return ErrPeerDenied
	}
	return nil
}

func validateExecuteRequest(request *contractsv1.ExecuteAuthorityCommandRequest, peer PeerContext) error {
	if request == nil || !validCommand(request.Command) || !validGrant(request.Grant) {
		return errMalformedProto
	}
	if !samePrincipal(request.Command.Actor, peer.Identity) || !samePrincipal(request.Grant.Initiator, peer.Identity) {
		return ErrPeerDenied
	}
	switch command := request.ArtifactCommand.(type) {
	case *contractsv1.ExecuteAuthorityCommandRequest_ArtifactAdmit:
		if command.ArtifactAdmit == nil || !validArtifact(command.ArtifactAdmit.Artifact) ||
			command.ArtifactAdmit.DeclaredLength == 0 || command.ArtifactAdmit.DeclaredLength > 1<<40 ||
			command.ArtifactAdmit.FrameCount == 0 || command.ArtifactAdmit.FrameCount > 1<<20 {
			return errMalformedProto
		}
	case *contractsv1.ExecuteAuthorityCommandRequest_ArtifactRead:
		if command.ArtifactRead == nil || !validArtifact(command.ArtifactRead.Artifact) ||
			command.ArtifactRead.Generation == 0 || command.ArtifactRead.Length == 0 ||
			command.ArtifactRead.Length > 16<<20 ||
			command.ArtifactRead.Offset > math.MaxUint64-command.ArtifactRead.Length {
			return errMalformedProto
		}
	case *contractsv1.ExecuteAuthorityCommandRequest_ArtifactDelete:
		if command.ArtifactDelete == nil || !validArtifact(command.ArtifactDelete.Artifact) ||
			command.ArtifactDelete.ExpectedGeneration == 0 {
			return errMalformedProto
		}
	default:
		return errMalformedProto
	}
	return nil
}

func validateStatusRequest(request *contractsv1.ReadStatusRequest, peer PeerContext) error {
	if request == nil || !validIdentifier(request.RequestedSession) ||
		!validCursor(request.After) {
		return errMalformedProto
	}
	if !sameIdentifier(request.RequestedSession, peer.Identity.Session) {
		return ErrPeerDenied
	}
	return nil
}

func validateOpenResponse(response *contractsv1.OpenLocalSessionResponse) error {
	if response == nil || !validReceipt(response.Receipt) ||
		(response.Session != nil && !validPrincipal(response.Session, true)) || !validPublicError(response.Error) {
		return errInvalidResponse
	}
	return nil
}

func validateExecuteResponse(response *contractsv1.ExecuteAuthorityCommandResponse) error {
	if response == nil || !validReceipt(response.Receipt) ||
		(response.Artifact != nil && !validArtifact(response.Artifact)) ||
		(response.FrameDigest != nil && !validDigest(response.FrameDigest)) ||
		!validCursor(response.NextCursor) || !validPublicError(response.Error) {
		return errInvalidResponse
	}
	if authorization := response.Authorization; authorization != nil {
		if (authorization.Receipt != nil && !validReceipt(authorization.Receipt)) ||
			(authorization.GrantId != nil && !validIdentifier(authorization.GrantId)) ||
			!validOptionalString(authorization.Action, 128) ||
			(authorization.Resource != nil && !validIdentifier(authorization.Resource)) {
			return errInvalidResponse
		}
	}
	return nil
}

func validateStatusResponse(response *contractsv1.ReadStatusResponse) error {
	if response == nil || !validReceipt(response.Receipt) ||
		(response.Session != nil && !validPrincipal(response.Session, true)) ||
		(response.ObservedAt != nil && response.ObservedAt.CheckValid() != nil) ||
		!validRender(response.Render) || !validPublicError(response.Error) {
		return errInvalidResponse
	}
	return nil
}

func validCommand(command *contractsv1.CommandEnvelope) bool {
	return command != nil && validIdentifier(command.CommandId) && validString(command.CommandType, 1, 256) &&
		validPrincipal(command.Actor, true) &&
		command.SubmittedAt != nil && command.SubmittedAt.CheckValid() == nil &&
		validString(command.IdempotencyKey, 1, 512) && validCausal(command.Causal) &&
		validDigest(command.PayloadDigest)
}

func validGrant(grant *contractsv1.CapabilityGrant) bool {
	if grant == nil || !validIdentifier(grant.GrantId) || !validPrincipal(grant.Initiator, true) ||
		!validOptionalIdentifier(grant.TaskId) ||
		!validOptionalIdentifier(grant.WorkflowId) || !validLease(grant.Lease) ||
		!validUniqueStrings(grant.Actions, 1, 64, 128, actionPattern) ||
		!validIdentifiers(grant.Resources, 1, 64) || !validOptionalString(grant.RepositoryGitOid, 128) ||
		!validPaths(grant.AllowedPaths) || len(grant.ToolGrants) > 16 || len(grant.Egress) > 16 ||
		len(grant.Limits) > 32 || !validString(grant.Nonce, 1, 128) ||
		grant.ExpiresAt == nil || grant.ExpiresAt.CheckValid() != nil || !validDigest(grant.PolicyDigest) ||
		grant.CommandFence == 0 {
		return false
	}
	for _, tool := range grant.ToolGrants {
		if tool == nil || !validDigest(tool.ToolPackageDigest) ||
			!validUniqueStrings(tool.AllowedOperations, 1, 64, 128, actionPattern) ||
			len(tool.InputArtifacts) > 64 || len(tool.Limits) > 32 {
			return false
		}
		for _, artifact := range tool.InputArtifacts {
			if !validArtifact(artifact) {
				return false
			}
		}
		for _, limit := range tool.Limits {
			if !validLimit(limit) {
				return false
			}
		}
	}
	for _, egress := range grant.Egress {
		if egress == nil || !validIdentifier(egress.ManifestId) ||
			!validString(egress.Destination, 1, 256) || !validString(egress.Classification, 1, 64) ||
			len(egress.AllowedArtifacts) < 1 || len(egress.AllowedArtifacts) > 64 ||
			!validDigest(egress.PolicyDigest) || egress.ExpiresAt == nil || egress.ExpiresAt.CheckValid() != nil {
			return false
		}
		for _, artifact := range egress.AllowedArtifacts {
			if !validArtifact(artifact) {
				return false
			}
		}
	}
	for _, limit := range grant.Limits {
		if !validLimit(limit) {
			return false
		}
	}
	return true
}

func validReceipt(receipt *contractsv1.Receipt) bool {
	if receipt == nil || !validIdentifier(receipt.ReceiptId) || receipt.Status < 1 || receipt.Status > 5 ||
		!validOptionalString(receipt.ReasonCode, 1024) || !validIdentifier(receipt.OperationId) ||
		!validCausal(receipt.Causal) || receipt.RecordedAt == nil || receipt.RecordedAt.CheckValid() != nil ||
		!validDigest(receipt.ConfigurationDigest) {
		return false
	}
	for _, evidence := range receipt.Evidence {
		if evidence == nil || !validIdentifier(evidence.EvidenceId) ||
			!validIdentifier(evidence.SourceRevisionId) ||
			(evidence.AnchorDigest != nil && !validDigest(evidence.AnchorDigest)) {
			return false
		}
	}
	return true
}

func validCausal(causal *contractsv1.CausalContext) bool {
	return causal != nil && validIdentifier(causal.CorrelationId) &&
		validIdentifier(causal.CausationId) && validIdentifier(causal.TraceId)
}

func validLease(lease *contractsv1.Lease) bool {
	return lease == nil || (validIdentifier(lease.LeaseId) && validPrincipal(lease.Holder, true) &&
		lease.Fence > 0 && lease.ExpiresAt != nil && lease.ExpiresAt.CheckValid() == nil)
}

func validLimit(limit *contractsv1.ResourceLimit) bool {
	return limit != nil && validString(limit.Name, 1, 64) && resourcePattern.MatchString(limit.Name) && limit.Maximum > 0
}

func validPrincipal(principal *contractsv1.AuthenticatedPrincipalRef, requireSession bool) bool {
	return principal != nil && validIdentifier(principal.PrincipalId) && validIdentifier(principal.TenantId) &&
		((!requireSession && principal.SessionId == nil) || validIdentifier(principal.SessionId))
}

func validArtifact(artifact *contractsv1.ArtifactRef) bool {
	return artifact != nil && validIdentifier(artifact.ArtifactId) &&
		validDigest(artifact.ContentDigest) && validIdentifier(artifact.TenantId)
}

func validDigest(digest *contractsv1.Digest) bool {
	return digest != nil && validString(digest.Algorithm, 1, 32) &&
		validString(digest.Hex, 1, 256) && hexPattern.MatchString(digest.Hex)
}

func validIdentifier(identifier *contractsv1.Identifier) bool {
	return identifier != nil && validString(identifier.Namespace, 1, 64) && validString(identifier.Value, 1, 512)
}

func validOptionalIdentifier(identifier *contractsv1.Identifier) bool {
	return identifier == nil || validIdentifier(identifier)
}

func validIdentifiers(identifiers []*contractsv1.Identifier, minimum, maximum int) bool {
	if len(identifiers) < minimum || len(identifiers) > maximum {
		return false
	}
	for _, identifier := range identifiers {
		if !validIdentifier(identifier) {
			return false
		}
	}
	return true
}

func validCursor(cursor *contractsv1.Cursor) bool {
	return cursor == nil || validOptionalString(cursor.Token, 512)
}

func validRender(render *contractsv1.RenderModel) bool {
	return render == nil || (validString(render.Title, 1, 160) && validString(render.Detail, 1, 1024) &&
		validOptionalString(render.ActionLabel, 80))
}

func validPublicError(publicError *contractsv1.PublicError) bool {
	return publicError == nil || (validString(publicError.Code, 1, 128) && validRender(publicError.Render))
}

func validPaths(paths []string) bool {
	if len(paths) > 64 {
		return false
	}
	seen := make(map[string]struct{}, len(paths))
	for _, value := range paths {
		cleaned := path.Clean(value)
		if !validString(value, 1, 512) || path.IsAbs(value) || cleaned != value ||
			cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") ||
			strings.ContainsAny(value, `\*?[`) {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validUniqueStrings(values []string, minimum, maximum, maxLength int, pattern *regexp.Regexp) bool {
	if len(values) < minimum || len(values) > maximum {
		return false
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !validString(value, 1, maxLength) || !pattern.MatchString(value) {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validString(value string, minimum, maximum int) bool {
	length := utf8.RuneCountInString(value)
	return utf8.ValidString(value) && length >= minimum && length <= maximum
}

func validOptionalString(value string, maximum int) bool {
	return validString(value, 0, maximum)
}

func samePrincipal(principal *contractsv1.AuthenticatedPrincipalRef, identity shared.MappedIdentityFact) bool {
	return sameIdentifier(principal.PrincipalId, identity.Principal) &&
		sameIdentifier(principal.TenantId, identity.Tenant) && sameIdentifier(principal.SessionId, identity.Session)
}

func sameIdentifier(identifier *contractsv1.Identifier, expected shared.Identifier) bool {
	return identifier != nil && identifier.Namespace == expected.Namespace && identifier.Value == expected.Value
}

func validMappedIdentity(identity shared.MappedIdentityFact) bool {
	return identity.Principal.Namespace != "" && identity.Principal.Value != "" &&
		identity.Tenant.Namespace != "" && identity.Tenant.Value != "" &&
		identity.Session.Namespace != "" && identity.Session.Value != ""
}
