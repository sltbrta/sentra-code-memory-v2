package localauthority

import (
	"context"
	"net/http"

	"buf.build/go/protovalidate"
	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"google.golang.org/protobuf/proto"
)

func (s *Server) handleAddSource(writer http.ResponseWriter, request *http.Request) {
	s.handleIngestion(writer, request, &contractsv1.AddSourceRequest{}, func(
		ctx context.Context,
		peer PeerContext,
		message proto.Message,
	) (proto.Message, error) {
		return s.config.IngestionAuthority.AddSource(ctx, peer, message.(*contractsv1.AddSourceRequest))
	})
}

func (s *Server) handleGetSourceStatus(writer http.ResponseWriter, request *http.Request) {
	s.handleIngestion(writer, request, &contractsv1.GetSourceStatusRequest{}, func(
		ctx context.Context,
		peer PeerContext,
		message proto.Message,
	) (proto.Message, error) {
		return s.config.IngestionAuthority.GetSourceStatus(ctx, peer, message.(*contractsv1.GetSourceStatusRequest))
	})
}

func (s *Server) handleSearchCode(writer http.ResponseWriter, request *http.Request) {
	s.handleIngestion(writer, request, &contractsv1.SearchCodeRequest{}, func(
		ctx context.Context,
		peer PeerContext,
		message proto.Message,
	) (proto.Message, error) {
		return s.config.IngestionAuthority.SearchCode(ctx, peer, message.(*contractsv1.SearchCodeRequest))
	})
}

func (s *Server) handleReconcileSource(writer http.ResponseWriter, request *http.Request) {
	s.handleIngestion(writer, request, &contractsv1.ReconcileSourceRequest{}, func(
		ctx context.Context,
		peer PeerContext,
		message proto.Message,
	) (proto.Message, error) {
		return s.config.IngestionAuthority.ReconcileSource(ctx, peer, message.(*contractsv1.ReconcileSourceRequest))
	})
}

func (s *Server) handleRevokeSource(writer http.ResponseWriter, request *http.Request) {
	s.handleIngestion(writer, request, &contractsv1.RevokeSourceRequest{}, func(
		ctx context.Context,
		peer PeerContext,
		message proto.Message,
	) (proto.Message, error) {
		return s.config.IngestionAuthority.RevokeSource(ctx, peer, message.(*contractsv1.RevokeSourceRequest))
	})
}

type ingestionInvocation func(context.Context, PeerContext, proto.Message) (proto.Message, error)

func (s *Server) handleIngestion(
	writer http.ResponseWriter,
	request *http.Request,
	message proto.Message,
	invoke ingestionInvocation,
) {
	s.handle(writer, request, func(payload []byte, peer PeerContext) ([]byte, error) {
		if err := proto.Unmarshal(payload, message); err != nil {
			return nil, errMalformedProto
		}
		if err := protovalidate.Validate(message); err != nil {
			return nil, errMalformedProto
		}
		caller := ingestionCaller(message)
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

func ingestionCaller(message proto.Message) *contractsv1.UntrustedIngestionCaller {
	switch request := message.(type) {
	case *contractsv1.AddSourceRequest:
		return request.Caller
	case *contractsv1.GetSourceStatusRequest:
		return request.Caller
	case *contractsv1.SearchCodeRequest:
		return request.Caller
	case *contractsv1.ReconcileSourceRequest:
		return request.Caller
	case *contractsv1.RevokeSourceRequest:
		return request.Caller
	default:
		return nil
	}
}

func marshalValidatedResponse(response proto.Message) ([]byte, error) {
	if response == nil || !response.ProtoReflect().IsValid() {
		return nil, errInvalidResponse
	}
	if err := protovalidate.Validate(response); err != nil {
		return nil, errInvalidResponse
	}
	return marshalResponse(response)
}
