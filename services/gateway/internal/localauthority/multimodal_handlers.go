package localauthority

import (
	"context"
	"net/http"

	"buf.build/go/protovalidate"
	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"google.golang.org/protobuf/proto"
)

// The five frozen Stage 11 multimodal procedures mount exactly like the Stage
// 07 meeting procedures: protobuf decode, protovalidate, and the body-identity
// cross-check run before any authority invocation, and every response is
// revalidated before it is written.

func (s *Server) handleAdmitMultimodalSource(writer http.ResponseWriter, request *http.Request) {
	s.handleMultimodal(writer, request, &contractsv1.AdmitMultimodalSourceRequest{}, func(
		ctx context.Context, peer PeerContext, message proto.Message,
	) (proto.Message, error) {
		return s.config.MultimodalAuthority.AdmitMultimodalSource(ctx, peer, message.(*contractsv1.AdmitMultimodalSourceRequest))
	})
}

func (s *Server) handleGetMultimodalStatus(writer http.ResponseWriter, request *http.Request) {
	s.handleMultimodal(writer, request, &contractsv1.GetMultimodalStatusRequest{}, func(
		ctx context.Context, peer PeerContext, message proto.Message,
	) (proto.Message, error) {
		return s.config.MultimodalAuthority.GetMultimodalStatus(ctx, peer, message.(*contractsv1.GetMultimodalStatusRequest))
	})
}

func (s *Server) handleGetMultimodalEvidence(writer http.ResponseWriter, request *http.Request) {
	s.handleMultimodal(writer, request, &contractsv1.GetMultimodalEvidenceRequest{}, func(
		ctx context.Context, peer PeerContext, message proto.Message,
	) (proto.Message, error) {
		return s.config.MultimodalAuthority.GetMultimodalEvidence(ctx, peer, message.(*contractsv1.GetMultimodalEvidenceRequest))
	})
}

func (s *Server) handleRevokeMultimodalSource(writer http.ResponseWriter, request *http.Request) {
	s.handleMultimodal(writer, request, &contractsv1.RevokeMultimodalSourceRequest{}, func(
		ctx context.Context, peer PeerContext, message proto.Message,
	) (proto.Message, error) {
		return s.config.MultimodalAuthority.RevokeMultimodalSource(ctx, peer, message.(*contractsv1.RevokeMultimodalSourceRequest))
	})
}

func (s *Server) handlePurgeMultimodalSource(writer http.ResponseWriter, request *http.Request) {
	s.handleMultimodal(writer, request, &contractsv1.PurgeMultimodalSourceRequest{}, func(
		ctx context.Context, peer PeerContext, message proto.Message,
	) (proto.Message, error) {
		return s.config.MultimodalAuthority.PurgeMultimodalSource(ctx, peer, message.(*contractsv1.PurgeMultimodalSourceRequest))
	})
}

type multimodalInvocation func(context.Context, PeerContext, proto.Message) (proto.Message, error)

func (s *Server) handleMultimodal(
	writer http.ResponseWriter,
	request *http.Request,
	message proto.Message,
	invoke multimodalInvocation,
) {
	s.handle(writer, request, func(payload []byte, peer PeerContext) ([]byte, error) {
		if err := proto.Unmarshal(payload, message); err != nil {
			return nil, errMalformedProto
		}
		if err := protovalidate.Validate(message); err != nil {
			return nil, errMalformedProto
		}
		caller := multimodalCaller(message)
		if caller == nil || !validPrincipal(caller.RequestedPrincipal, true) ||
			!samePrincipal(caller.RequestedPrincipal, peer.Identity) ||
			!sameIdentifier(caller.RequestedSession, peer.Identity.Session) {
			return nil, ErrPeerDenied
		}
		response, err := invoke(request.Context(), peer, message)
		if err != nil {
			return nil, ErrPeerDenied
		}
		return marshalValidatedResponse(response)
	})
}

func multimodalCaller(message proto.Message) *contractsv1.UntrustedMultimodalCaller {
	switch request := message.(type) {
	case *contractsv1.AdmitMultimodalSourceRequest:
		return request.Caller
	case *contractsv1.GetMultimodalStatusRequest:
		return request.Caller
	case *contractsv1.GetMultimodalEvidenceRequest:
		return request.Caller
	case *contractsv1.RevokeMultimodalSourceRequest:
		return request.Caller
	case *contractsv1.PurgeMultimodalSourceRequest:
		return request.Caller
	default:
		return nil
	}
}
