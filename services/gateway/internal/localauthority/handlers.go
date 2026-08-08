package localauthority

import (
	"errors"
	"io"
	"net/http"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"google.golang.org/protobuf/proto"
)

func (s *Server) handleOpenSession(writer http.ResponseWriter, request *http.Request) {
	s.handle(writer, request, func(payload []byte, peer PeerContext) ([]byte, error) {
		message := &contractsv1.OpenLocalSessionRequest{}
		if err := proto.Unmarshal(payload, message); err != nil {
			return nil, errMalformedProto
		}
		if err := validateOpenRequest(message, peer); err != nil {
			return nil, err
		}
		response, err := s.config.Authority.OpenSession(request.Context(), peer, message)
		if err != nil {
			return nil, ErrPeerDenied
		}
		if err := validateOpenResponse(response); err != nil {
			return nil, errInvalidResponse
		}
		return marshalResponse(response)
	})
}

func (s *Server) handleExecute(writer http.ResponseWriter, request *http.Request) {
	s.handle(writer, request, func(payload []byte, peer PeerContext) ([]byte, error) {
		message := &contractsv1.ExecuteAuthorityCommandRequest{}
		if err := proto.Unmarshal(payload, message); err != nil {
			return nil, errMalformedProto
		}
		if err := validateExecuteRequest(message, peer); err != nil {
			return nil, err
		}
		response, err := s.config.Authority.Execute(request.Context(), peer, message)
		if err != nil {
			return nil, ErrPeerDenied
		}
		if err := validateExecuteResponse(response); err != nil {
			return nil, errInvalidResponse
		}
		return marshalResponse(response)
	})
}

func (s *Server) handleStatus(writer http.ResponseWriter, request *http.Request) {
	s.handle(writer, request, func(payload []byte, peer PeerContext) ([]byte, error) {
		message := &contractsv1.ReadStatusRequest{}
		if err := proto.Unmarshal(payload, message); err != nil {
			return nil, errMalformedProto
		}
		if err := validateStatusRequest(message, peer); err != nil {
			return nil, err
		}
		response, err := s.config.Authority.ReadStatus(request.Context(), peer, message)
		if err != nil {
			return nil, ErrPeerDenied
		}
		if err := validateStatusResponse(response); err != nil {
			return nil, errInvalidResponse
		}
		return marshalResponse(response)
	})
}

func marshalResponse(response proto.Message) ([]byte, error) {
	if proto.Size(response) > maxRequestBytes {
		return nil, errInvalidResponse
	}
	return proto.Marshal(response)
}

func (s *Server) handle(
	writer http.ResponseWriter,
	request *http.Request,
	invoke func([]byte, PeerContext) ([]byte, error),
) {
	peer, ok := peerForRequest(request)
	if !ok {
		writeStaticHTTPError(writer, http.StatusForbidden, "request-denied")
		return
	}
	if request.Method != http.MethodPost {
		writeStaticHTTPError(writer, http.StatusMethodNotAllowed, "method-not-allowed")
		return
	}
	if request.Header.Get("Content-Type") != "application/proto" {
		writeStaticHTTPError(writer, http.StatusUnsupportedMediaType, "content-type-rejected")
		return
	}
	if request.ContentLength > maxRequestBytes {
		writeStaticHTTPError(writer, http.StatusRequestEntityTooLarge, "request-too-large")
		return
	}
	payload, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBytes+1))
	if err != nil || len(payload) == 0 {
		writeStaticHTTPError(writer, http.StatusBadRequest, "request-malformed")
		return
	}
	if len(payload) > maxRequestBytes {
		writeStaticHTTPError(writer, http.StatusRequestEntityTooLarge, "request-too-large")
		return
	}
	response, err := invoke(payload, peer)
	if errors.Is(err, ErrPeerDenied) {
		writeStaticHTTPError(writer, http.StatusForbidden, "request-denied")
		return
	}
	if errors.Is(err, errInvalidResponse) {
		writeStaticHTTPError(writer, http.StatusInternalServerError, "response-invalid")
		return
	}
	if err != nil {
		writeStaticHTTPError(writer, http.StatusBadRequest, "request-malformed")
		return
	}
	writer.Header().Set("Content-Type", "application/proto")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(response)
}

func writeStaticHTTPError(writer http.ResponseWriter, status int, code string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_, _ = writer.Write([]byte("{\"code\":\"" + code + "\"}"))
}
