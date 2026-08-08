package localauthority

import (
	"context"
	"net/http"

	"buf.build/go/protovalidate"
	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"google.golang.org/protobuf/proto"
)

// The six frozen Stage 08 connector procedures mount exactly like the Stage 03
// ingestion procedures: protobuf decode, protovalidate, and the body-identity
// cross-check run before any authority invocation, and every response is
// revalidated before it is written.

func (s *Server) handleConnectGitHubSource(writer http.ResponseWriter, request *http.Request) {
	s.handleConnector(writer, request, &contractsv1.ConnectGitHubSourceRequest{}, func(
		ctx context.Context, peer PeerContext, message proto.Message,
	) (proto.Message, error) {
		return s.config.ConnectorAuthority.ConnectGitHubSource(ctx, peer, message.(*contractsv1.ConnectGitHubSourceRequest))
	})
}

func (s *Server) handleGetConnectorStatus(writer http.ResponseWriter, request *http.Request) {
	s.handleConnector(writer, request, &contractsv1.GetConnectorStatusRequest{}, func(
		ctx context.Context, peer PeerContext, message proto.Message,
	) (proto.Message, error) {
		return s.config.ConnectorAuthority.GetConnectorStatus(ctx, peer, message.(*contractsv1.GetConnectorStatusRequest))
	})
}

func (s *Server) handleReconcileConnector(writer http.ResponseWriter, request *http.Request) {
	s.handleConnector(writer, request, &contractsv1.ReconcileConnectorRequest{}, func(
		ctx context.Context, peer PeerContext, message proto.Message,
	) (proto.Message, error) {
		return s.config.ConnectorAuthority.ReconcileConnector(ctx, peer, message.(*contractsv1.ReconcileConnectorRequest))
	})
}

func (s *Server) handleQueryConnectorEvidence(writer http.ResponseWriter, request *http.Request) {
	s.handleConnector(writer, request, &contractsv1.QueryConnectorEvidenceRequest{}, func(
		ctx context.Context, peer PeerContext, message proto.Message,
	) (proto.Message, error) {
		return s.config.ConnectorAuthority.QueryConnectorEvidence(ctx, peer, message.(*contractsv1.QueryConnectorEvidenceRequest))
	})
}

func (s *Server) handleRevokeConnector(writer http.ResponseWriter, request *http.Request) {
	s.handleConnector(writer, request, &contractsv1.RevokeConnectorRequest{}, func(
		ctx context.Context, peer PeerContext, message proto.Message,
	) (proto.Message, error) {
		return s.config.ConnectorAuthority.RevokeConnector(ctx, peer, message.(*contractsv1.RevokeConnectorRequest))
	})
}

func (s *Server) handlePurgeConnector(writer http.ResponseWriter, request *http.Request) {
	s.handleConnector(writer, request, &contractsv1.PurgeConnectorRequest{}, func(
		ctx context.Context, peer PeerContext, message proto.Message,
	) (proto.Message, error) {
		return s.config.ConnectorAuthority.PurgeConnector(ctx, peer, message.(*contractsv1.PurgeConnectorRequest))
	})
}

type connectorInvocation func(context.Context, PeerContext, proto.Message) (proto.Message, error)

func (s *Server) handleConnector(
	writer http.ResponseWriter,
	request *http.Request,
	message proto.Message,
	invoke connectorInvocation,
) {
	s.handle(writer, request, func(payload []byte, peer PeerContext) ([]byte, error) {
		if err := proto.Unmarshal(payload, message); err != nil {
			return nil, errMalformedProto
		}
		if err := protovalidate.Validate(message); err != nil {
			return nil, errMalformedProto
		}
		caller := connectorCaller(message)
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

func connectorCaller(message proto.Message) *contractsv1.UntrustedConnectorCaller {
	switch request := message.(type) {
	case *contractsv1.ConnectGitHubSourceRequest:
		return request.Caller
	case *contractsv1.GetConnectorStatusRequest:
		return request.Caller
	case *contractsv1.ReconcileConnectorRequest:
		return request.Caller
	case *contractsv1.QueryConnectorEvidenceRequest:
		return request.Caller
	case *contractsv1.RevokeConnectorRequest:
		return request.Caller
	case *contractsv1.PurgeConnectorRequest:
		return request.Caller
	default:
		return nil
	}
}
