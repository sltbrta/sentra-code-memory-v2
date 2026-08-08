package localauthority

import (
	"context"
	"net/http"

	"buf.build/go/protovalidate"
	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"google.golang.org/protobuf/proto"
)

// The five frozen Stage 07 meeting procedures mount exactly like the Stage 03
// ingestion, Stage 04 query, and Stage 05 factory procedures: protobuf decode,
// protovalidate, and the body-identity cross-check run before any authority
// invocation, and every response is revalidated before it is written.

func (s *Server) handleImportTranscript(writer http.ResponseWriter, request *http.Request) {
	s.handleMeeting(writer, request, &contractsv1.ImportTranscriptRequest{}, func(
		ctx context.Context, peer PeerContext, message proto.Message,
	) (proto.Message, error) {
		return s.config.MeetingAuthority.ImportTranscript(ctx, peer, message.(*contractsv1.ImportTranscriptRequest))
	})
}

func (s *Server) handleGetMeetingStatus(writer http.ResponseWriter, request *http.Request) {
	s.handleMeeting(writer, request, &contractsv1.GetMeetingStatusRequest{}, func(
		ctx context.Context, peer PeerContext, message proto.Message,
	) (proto.Message, error) {
		return s.config.MeetingAuthority.GetMeetingStatus(ctx, peer, message.(*contractsv1.GetMeetingStatusRequest))
	})
}

func (s *Server) handleQueryMeeting(writer http.ResponseWriter, request *http.Request) {
	s.handleMeeting(writer, request, &contractsv1.QueryMeetingRequest{}, func(
		ctx context.Context, peer PeerContext, message proto.Message,
	) (proto.Message, error) {
		return s.config.MeetingAuthority.QueryMeeting(ctx, peer, message.(*contractsv1.QueryMeetingRequest))
	})
}

func (s *Server) handleRevokeMeeting(writer http.ResponseWriter, request *http.Request) {
	s.handleMeeting(writer, request, &contractsv1.RevokeMeetingRequest{}, func(
		ctx context.Context, peer PeerContext, message proto.Message,
	) (proto.Message, error) {
		return s.config.MeetingAuthority.RevokeMeeting(ctx, peer, message.(*contractsv1.RevokeMeetingRequest))
	})
}

func (s *Server) handlePurgeMeeting(writer http.ResponseWriter, request *http.Request) {
	s.handleMeeting(writer, request, &contractsv1.PurgeMeetingRequest{}, func(
		ctx context.Context, peer PeerContext, message proto.Message,
	) (proto.Message, error) {
		return s.config.MeetingAuthority.PurgeMeeting(ctx, peer, message.(*contractsv1.PurgeMeetingRequest))
	})
}

type meetingInvocation func(context.Context, PeerContext, proto.Message) (proto.Message, error)

func (s *Server) handleMeeting(
	writer http.ResponseWriter,
	request *http.Request,
	message proto.Message,
	invoke meetingInvocation,
) {
	s.handle(writer, request, func(payload []byte, peer PeerContext) ([]byte, error) {
		if err := proto.Unmarshal(payload, message); err != nil {
			return nil, errMalformedProto
		}
		if err := protovalidate.Validate(message); err != nil {
			return nil, errMalformedProto
		}
		caller := meetingCaller(message)
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

func meetingCaller(message proto.Message) *contractsv1.UntrustedMeetingCaller {
	switch request := message.(type) {
	case *contractsv1.ImportTranscriptRequest:
		return request.Caller
	case *contractsv1.GetMeetingStatusRequest:
		return request.Caller
	case *contractsv1.QueryMeetingRequest:
		return request.Caller
	case *contractsv1.RevokeMeetingRequest:
		return request.Caller
	case *contractsv1.PurgeMeetingRequest:
		return request.Caller
	default:
		return nil
	}
}
