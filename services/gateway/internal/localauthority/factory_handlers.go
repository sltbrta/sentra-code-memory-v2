package localauthority

import (
	"context"
	"net/http"

	"buf.build/go/protovalidate"
	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"google.golang.org/protobuf/proto"
)

// The five frozen Stage 05 factory procedures mount exactly like the Stage 03
// ingestion and Stage 04 query procedures: protobuf decode, protovalidate, and
// the body-identity cross-check run before any authority invocation, and every
// response is revalidated before it is written. The trust boundary is
// unchanged.

func (s *Server) handleAdmitChangeIntent(writer http.ResponseWriter, request *http.Request) {
	s.handleFactory(writer, request, &contractsv1.AdmitChangeIntentRequest{}, func(
		ctx context.Context,
		peer PeerContext,
		message proto.Message,
	) (proto.Message, error) {
		return s.config.FactoryAuthority.AdmitChangeIntent(ctx, peer, message.(*contractsv1.AdmitChangeIntentRequest))
	})
}

func (s *Server) handleGetChangePlan(writer http.ResponseWriter, request *http.Request) {
	s.handleFactory(writer, request, &contractsv1.GetChangePlanRequest{}, func(
		ctx context.Context,
		peer PeerContext,
		message proto.Message,
	) (proto.Message, error) {
		return s.config.FactoryAuthority.GetChangePlan(ctx, peer, message.(*contractsv1.GetChangePlanRequest))
	})
}

func (s *Server) handlePreviewChangeSet(writer http.ResponseWriter, request *http.Request) {
	s.handleFactory(writer, request, &contractsv1.PreviewChangeSetRequest{}, func(
		ctx context.Context,
		peer PeerContext,
		message proto.Message,
	) (proto.Message, error) {
		return s.config.FactoryAuthority.PreviewChangeSet(ctx, peer, message.(*contractsv1.PreviewChangeSetRequest))
	})
}

func (s *Server) handleGetReviewFindings(writer http.ResponseWriter, request *http.Request) {
	s.handleFactory(writer, request, &contractsv1.GetReviewFindingsRequest{}, func(
		ctx context.Context,
		peer PeerContext,
		message proto.Message,
	) (proto.Message, error) {
		return s.config.FactoryAuthority.GetReviewFindings(ctx, peer, message.(*contractsv1.GetReviewFindingsRequest))
	})
}

func (s *Server) handleCancelChangeRun(writer http.ResponseWriter, request *http.Request) {
	s.handleFactory(writer, request, &contractsv1.CancelChangeRunRequest{}, func(
		ctx context.Context,
		peer PeerContext,
		message proto.Message,
	) (proto.Message, error) {
		return s.config.FactoryAuthority.CancelChangeRun(ctx, peer, message.(*contractsv1.CancelChangeRunRequest))
	})
}

type factoryInvocation func(context.Context, PeerContext, proto.Message) (proto.Message, error)

func (s *Server) handleFactory(
	writer http.ResponseWriter,
	request *http.Request,
	message proto.Message,
	invoke factoryInvocation,
) {
	s.handle(writer, request, func(payload []byte, peer PeerContext) ([]byte, error) {
		if err := proto.Unmarshal(payload, message); err != nil {
			return nil, errMalformedProto
		}
		if err := protovalidate.Validate(message); err != nil {
			return nil, errMalformedProto
		}
		caller := factoryCaller(message)
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

func factoryCaller(message proto.Message) *contractsv1.UntrustedFactoryCaller {
	switch request := message.(type) {
	case *contractsv1.AdmitChangeIntentRequest:
		return request.Caller
	case *contractsv1.GetChangePlanRequest:
		return request.Caller
	case *contractsv1.PreviewChangeSetRequest:
		return request.Caller
	case *contractsv1.GetReviewFindingsRequest:
		return request.Caller
	case *contractsv1.CancelChangeRunRequest:
		return request.Caller
	default:
		return nil
	}
}
