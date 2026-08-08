package localauthority

import (
	"context"
	"net/http"

	"buf.build/go/protovalidate"
	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"google.golang.org/protobuf/proto"
)

// The four frozen Stage 04 query procedures mount exactly like the Stage 03
// ingestion procedures: protobuf decode, protovalidate, and the body-identity
// cross-check run before any authority invocation, and every response is
// revalidated before it is written. The trust boundary is unchanged.

func (s *Server) handleAsk(writer http.ResponseWriter, request *http.Request) {
	s.handleQuery(writer, request, &contractsv1.AskRequest{}, func(
		ctx context.Context,
		peer PeerContext,
		message proto.Message,
	) (proto.Message, error) {
		return s.config.QueryAuthority.Ask(ctx, peer, message.(*contractsv1.AskRequest))
	})
}

func (s *Server) handleListSources(writer http.ResponseWriter, request *http.Request) {
	s.handleQuery(writer, request, &contractsv1.ListSourcesRequest{}, func(
		ctx context.Context,
		peer PeerContext,
		message proto.Message,
	) (proto.Message, error) {
		return s.config.QueryAuthority.ListSources(ctx, peer, message.(*contractsv1.ListSourcesRequest))
	})
}

func (s *Server) handleGetHistory(writer http.ResponseWriter, request *http.Request) {
	s.handleQuery(writer, request, &contractsv1.GetHistoryRequest{}, func(
		ctx context.Context,
		peer PeerContext,
		message proto.Message,
	) (proto.Message, error) {
		return s.config.QueryAuthority.GetHistory(ctx, peer, message.(*contractsv1.GetHistoryRequest))
	})
}

func (s *Server) handleGetStatus(writer http.ResponseWriter, request *http.Request) {
	s.handleQuery(writer, request, &contractsv1.GetStatusRequest{}, func(
		ctx context.Context,
		peer PeerContext,
		message proto.Message,
	) (proto.Message, error) {
		return s.config.QueryAuthority.GetStatus(ctx, peer, message.(*contractsv1.GetStatusRequest))
	})
}

type queryInvocation func(context.Context, PeerContext, proto.Message) (proto.Message, error)

func (s *Server) handleQuery(
	writer http.ResponseWriter,
	request *http.Request,
	message proto.Message,
	invoke queryInvocation,
) {
	s.handle(writer, request, func(payload []byte, peer PeerContext) ([]byte, error) {
		if err := proto.Unmarshal(payload, message); err != nil {
			return nil, errMalformedProto
		}
		if err := protovalidate.Validate(message); err != nil {
			return nil, errMalformedProto
		}
		caller := queryCaller(message)
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

func queryCaller(message proto.Message) *contractsv1.UntrustedQueryCaller {
	switch request := message.(type) {
	case *contractsv1.AskRequest:
		return request.Caller
	case *contractsv1.ListSourcesRequest:
		return request.Caller
	case *contractsv1.GetHistoryRequest:
		return request.Caller
	case *contractsv1.GetStatusRequest:
		return request.Caller
	default:
		return nil
	}
}
