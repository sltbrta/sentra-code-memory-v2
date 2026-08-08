package localauthority

import (
	"errors"
	"io"
	"net/http"
)

// Stage 06 Tracer 001 mounts as a JSON composition facade on the same
// authenticated owner-only socket as the product protobuf procedures. The TUI
// client posts application/json; product Stages 02–05 remain application/proto.
// Peer authentication and connection budgets are unchanged.

func (s *Server) handleTracerSession(writer http.ResponseWriter, request *http.Request) {
	s.handleTracer(writer, request, "Session")
}

func (s *Server) handleTracerIngest(writer http.ResponseWriter, request *http.Request) {
	s.handleTracer(writer, request, "Ingest")
}

func (s *Server) handleTracerAsk(writer http.ResponseWriter, request *http.Request) {
	s.handleTracer(writer, request, "Ask")
}

func (s *Server) handleTracerIntent(writer http.ResponseWriter, request *http.Request) {
	s.handleTracer(writer, request, "Intent")
}

func (s *Server) handleTracerPlan(writer http.ResponseWriter, request *http.Request) {
	s.handleTracer(writer, request, "Plan")
}

func (s *Server) handleTracerReview(writer http.ResponseWriter, request *http.Request) {
	s.handleTracer(writer, request, "Review")
}

func (s *Server) handleTracerDraftPR(writer http.ResponseWriter, request *http.Request) {
	s.handleTracer(writer, request, "DraftPr")
}

func (s *Server) handleTracerOutcome(writer http.ResponseWriter, request *http.Request) {
	s.handleTracer(writer, request, "Outcome")
}

func (s *Server) handleTracer(writer http.ResponseWriter, request *http.Request, step string) {
	peer, ok := peerForRequest(request)
	if !ok {
		writeStaticHTTPError(writer, http.StatusForbidden, "request-denied")
		return
	}
	if request.Method != http.MethodPost {
		writeStaticHTTPError(writer, http.StatusMethodNotAllowed, "method-not-allowed")
		return
	}
	if mediaType(request.Header.Get("Content-Type")) != "application/json" {
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
	if s.config.TracerAuthority == nil {
		writeStaticHTTPError(writer, http.StatusForbidden, "request-denied")
		return
	}
	response, err := s.config.TracerAuthority.Advance(request.Context(), peer, step, payload)
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
	if len(response) == 0 || len(response) > maxRequestBytes {
		writeStaticHTTPError(writer, http.StatusInternalServerError, "response-invalid")
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(response)
}

func mediaType(contentType string) string {
	if contentType == "" {
		return ""
	}
	for i := 0; i < len(contentType); i++ {
		if contentType[i] == ';' {
			return trimASCIISpace(contentType[:i])
		}
	}
	return trimASCIISpace(contentType)
}

func trimASCIISpace(value string) string {
	start, end := 0, len(value)
	for start < end && (value[start] == ' ' || value[start] == '\t') {
		start++
	}
	for end > start && (value[end-1] == ' ' || value[end-1] == '\t') {
		end--
	}
	return value[start:end]
}
