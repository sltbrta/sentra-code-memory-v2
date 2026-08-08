package localauthority

import (
	"context"
	"errors"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/multimodal"
)

// Public aliases over the bounded Stage 11 multimodal kernel shapes. They let
// the composing gateway command wire the production multimodal surface without
// importing brain-internal packages.
type (
	MultimodalKernel          = multimodal.Kernel
	MultimodalIdentity        = multimodal.Identity
	MultimodalAdmitCommand    = multimodal.AdmitCommand
	MultimodalStatusCommand   = multimodal.StatusCommand
	MultimodalEvidenceCommand = multimodal.EvidenceCommand
	MultimodalRevokeCommand   = multimodal.RevokeCommand
	MultimodalPurgeCommand    = multimodal.PurgeCommand
)

var (
	// ErrMultimodalNotFoundOrDenied is the single static non-disclosing kernel denial.
	ErrMultimodalNotFoundOrDenied = multimodal.ErrNotFoundOrDenied
	// ErrMultimodalInvalidInput marks malformed kernel boundary facts.
	ErrMultimodalInvalidInput = multimodal.ErrInvalidInput
	// ErrMultimodalOversized is a fail-loud pre-decode denial.
	ErrMultimodalOversized = multimodal.ErrOversized
	// ErrMultimodalMalformed is a fail-loud pre-decode denial.
	ErrMultimodalMalformed = multimodal.ErrMalformed
	// ErrMultimodalMediaTypeMismatch is a fail-loud pre-decode denial.
	ErrMultimodalMediaTypeMismatch = multimodal.ErrMediaTypeMismatch
	// ErrMultimodalEncryptedOrUnsupported is a fail-loud pre-decode denial.
	ErrMultimodalEncryptedOrUnsupported = multimodal.ErrEncryptedOrUnsupported
	// ErrMultimodalPartialPayload is a fail-loud pre-decode denial.
	ErrMultimodalPartialPayload = multimodal.ErrPartialPayload
)

// MultimodalSurface is the Stage 11 multimodal surface over one durable
// authority runtime. The kernel owns migration 007 facts and encrypted
// original/evidence payloads; this type only composes and closes.
type MultimodalSurface struct {
	runtime *Runtime
	kernel  *multimodal.Kernel
}

// OpenMultimodalSurface composes the Stage 11 multimodal kernel over one durable
// runtime. It fails closed without a durable payload vault, a database path,
// or the conversation payload port (the same encrypted vault Stage 04 uses).
func (r *Runtime) OpenMultimodalSurface(ctx context.Context) (*MultimodalSurface, error) {
	if r == nil || ctx == nil {
		return nil, ErrInvalid
	}
	if r.databasePath == "" || r.conversationPayloads == nil || r.clock == nil {
		return nil, ErrInvalid
	}
	kernel, err := multimodal.Open(ctx, multimodal.Config{
		DatabasePath: r.databasePath,
		Payloads:     r.conversationPayloads,
		Clock:        r.clock,
	})
	if err != nil {
		if errors.Is(err, multimodal.ErrSchemaUnsupported) || errors.Is(err, multimodal.ErrInvalidInput) {
			return nil, ErrInvalid
		}
		return nil, ErrUnavailable
	}
	return &MultimodalSurface{runtime: r, kernel: kernel}, nil
}

// Kernel returns the composed multimodal kernel.
func (s *MultimodalSurface) Kernel() *MultimodalKernel {
	if s == nil {
		return nil
	}
	return s.kernel
}

// Close releases the multimodal kernel handle.
func (s *MultimodalSurface) Close() error {
	if s == nil || s.kernel == nil {
		return nil
	}
	return s.kernel.Close()
}
