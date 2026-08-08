package multimodal

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate/schema"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

const (
	testTenant    = "tenant-a"
	testPrincipal = "principal-a"
	testSession   = "session-a"
)

type fakePayloads struct {
	mu      sync.Mutex
	objects map[string][]byte
	purged  map[string]bool
}

func newFakePayloads() *fakePayloads {
	return &fakePayloads{objects: make(map[string][]byte), purged: make(map[string]bool)}
}

func (f *fakePayloads) Put(_ context.Context, _ string, payload []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "artifact-" + digestBytes(payload)
	f.objects[id] = append([]byte(nil), payload...)
	return id, nil
}

func (f *fakePayloads) Get(_ context.Context, _, artifactID string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.purged[artifactID] {
		return nil, errors.New("purged")
	}
	payload, found := f.objects[artifactID]
	if !found {
		return nil, errors.New("missing")
	}
	return append([]byte(nil), payload...), nil
}

func (f *fakePayloads) Purge(_ context.Context, _, artifactID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purged[artifactID] = true
	delete(f.objects, artifactID)
	return nil
}

type testClock struct{ now int64 }

func (c *testClock) NowUnixMilli() int64 { return c.now }

func testIdentity() Identity {
	return Identity{Tenant: testTenant, Principal: testPrincipal, Session: testSession}
}

func openTestKernel(t *testing.T) (*Kernel, *fakePayloads) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "authority.db")
	authority, err := localstate.OpenWithMigrations(ctx, path, schema.Migrations(), localstate.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.OpenSession(ctx, contracts.MappedIdentityFact{
		Principal:   contracts.Identifier{Namespace: "principal", Value: testPrincipal},
		Tenant:      contracts.Identifier{Namespace: "tenant", Value: testTenant},
		Session:     contracts.Identifier{Namespace: "session", Value: testSession},
		Credentials: contracts.PeerCredentials{UID: 501, PID: 4242},
	}); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	payloads := newFakePayloads()
	kernel, err := Open(ctx, Config{
		DatabasePath: path,
		Payloads:     payloads,
		Clock:        &testClock{now: 1_700_000_000_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernel.Close() })
	return kernel, payloads
}

func textPayload() []byte {
	return []byte("# Sprint notes\n\nShip the billing service next week.\n")
}

func validTextAdmit(key string, payload []byte) (*contractsv1.AdmitMultimodalSourceRequest, []byte) {
	digest := digestBytes(payload)
	return &contractsv1.AdmitMultimodalSourceRequest{
		Caller: &contractsv1.UntrustedMultimodalCaller{
			RequestedPrincipal: &contractsv1.AuthenticatedPrincipalRef{
				PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: testPrincipal},
				TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: testTenant},
				SessionId:   &contractsv1.Identifier{Namespace: "session", Value: testSession},
			},
			RequestedSession: &contractsv1.Identifier{Namespace: "session", Value: testSession},
		},
		Envelope: &contractsv1.MultimodalSourceEnvelope{
			SourceObjectId: &contractsv1.Identifier{Namespace: "fixture", Value: "text-valid"},
			Kind:           contractsv1.MultimodalSourceKind_MULTIMODAL_SOURCE_KIND_TEXT_MARKDOWN,
			MediaType:      "text/markdown",
			ByteLength:     uint64(len(payload)),
			ContentDigest:  &contractsv1.Digest{Algorithm: "sha256", Hex: digest},
			EncryptedOriginal: &contractsv1.ArtifactRef{
				ArtifactId:    &contractsv1.Identifier{Namespace: "artifact", Value: "pending"},
				ContentDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: digest},
				TenantId:      &contractsv1.Identifier{Namespace: "tenant", Value: testTenant},
			},
			ExtractorIdentity: &contractsv1.Digest{Algorithm: "sha256", Hex: extractorPinHex},
			BrainId:           &contractsv1.Identifier{Namespace: "brain", Value: "brain-a"},
			TenantId:          &contractsv1.Identifier{Namespace: "tenant", Value: testTenant},
			Modality: &contractsv1.MultimodalSourceEnvelope_Text{
				Text: &contractsv1.TextMarkdownBounds{Utf8ByteLength: uint64(len(payload))},
			},
		},
		IdempotencyKey: key,
	}, payload
}

func TestAdmitStatusEvidenceRevokePurgeMatrix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernel, payloads := openTestKernel(t)
	payload := textPayload()
	request, body := validTextAdmit("admit-1", payload)

	admitted, err := kernel.Admit(ctx, AdmitCommand{
		Identity: testIdentity(), Request: request, Payload: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if admitted.State != contractsv1.MultimodalSourceState_MULTIMODAL_SOURCE_STATE_ADMITTED {
		t.Fatalf("state = %v", admitted.State)
	}
	sourceID := admitted.SourceId.Value

	// Exact duplicate admit replays.
	replay, err := kernel.Admit(ctx, AdmitCommand{
		Identity: testIdentity(), Request: request, Payload: body,
	})
	if err != nil || replay.SourceId.Value != sourceID {
		t.Fatalf("replay = %#v err=%v", replay, err)
	}
	// Conflicting reuse is non-disclosing denial.
	conflict, conflictBody := validTextAdmit("admit-1", []byte("different payload content here"))
	if _, err := kernel.Admit(ctx, AdmitCommand{
		Identity: testIdentity(), Request: conflict, Payload: conflictBody,
	}); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("conflict err = %v", err)
	}

	status, err := kernel.Status(ctx, StatusCommand{Identity: testIdentity(), SourceID: sourceID})
	if err != nil {
		t.Fatal(err)
	}
	if status.Status.State != contractsv1.MultimodalSourceState_MULTIMODAL_SOURCE_STATE_READY {
		t.Fatalf("status state = %v", status.Status.State)
	}
	if len(status.Status.Lanes) < 2 {
		t.Fatalf("lanes = %#v", status.Status.Lanes)
	}

	evidence, err := kernel.Evidence(ctx, EvidenceCommand{
		Identity: testIdentity(), SourceID: sourceID, PageSize: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Items) == 0 {
		t.Fatal("expected evidence items")
	}
	if evidence.Items[0].Anchor.GetText() == nil && evidence.Items[0].Anchor.GetBytes() == nil {
		t.Fatalf("expected text/bytes anchor: %#v", evidence.Items[0])
	}

	// Cross-principal denial.
	other := testIdentity()
	other.Principal = "principal-b"
	if _, err := kernel.Status(ctx, StatusCommand{Identity: other, SourceID: sourceID}); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("cross-principal err = %v", err)
	}

	if _, err := kernel.Revoke(ctx, RevokeCommand{
		Identity: testIdentity(), SourceID: sourceID, IdempotencyKey: "revoke-1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := kernel.Status(ctx, StatusCommand{Identity: testIdentity(), SourceID: sourceID}); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("revoked status err = %v", err)
	}
	if _, err := kernel.Revoke(ctx, RevokeCommand{
		Identity: testIdentity(), SourceID: sourceID, IdempotencyKey: "revoke-1",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := kernel.Purge(ctx, PurgeCommand{
		Identity: testIdentity(), SourceID: sourceID, IdempotencyKey: "purge-1",
	}); err != nil {
		t.Fatal(err)
	}
	if len(payloads.objects) != 0 {
		t.Fatalf("payloads remained after purge: %#v", payloads.objects)
	}
	if _, err := kernel.Purge(ctx, PurgeCommand{
		Identity: testIdentity(), SourceID: sourceID, IdempotencyKey: "purge-1",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPreDecodeDenialsAndExtractors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernel, _ := openTestKernel(t)

	// Oversized text declaration.
	payload := []byte("hi")
	request, _ := validTextAdmit("oversize", payload)
	request.Envelope.ByteLength = maxTextBytes + 1
	request.Envelope.Modality = &contractsv1.MultimodalSourceEnvelope_Text{
		Text: &contractsv1.TextMarkdownBounds{Utf8ByteLength: maxTextBytes + 1},
	}
	if _, err := kernel.Admit(ctx, AdmitCommand{
		Identity: testIdentity(), Request: request, Payload: payload,
	}); !errors.Is(err, ErrOversized) && !errors.Is(err, ErrPartialPayload) && !errors.Is(err, ErrMalformed) {
		t.Fatalf("oversized err = %v", err)
	}

	// Media type mismatch (JPEG as PNG kind).
	pngPayload := minimalPNG()
	digest := digestBytes(pngPayload)
	baseCaller, _ := validTextAdmit("x", payload)
	jpegReq := &contractsv1.AdmitMultimodalSourceRequest{
		Caller: baseCaller.Caller,
		Envelope: &contractsv1.MultimodalSourceEnvelope{
			SourceObjectId: &contractsv1.Identifier{Namespace: "fixture", Value: "png-bad"},
			Kind:           contractsv1.MultimodalSourceKind_MULTIMODAL_SOURCE_KIND_PNG,
			MediaType:      "image/jpeg",
			ByteLength:     uint64(len(pngPayload)),
			ContentDigest:  &contractsv1.Digest{Algorithm: "sha256", Hex: digest},
			EncryptedOriginal: &contractsv1.ArtifactRef{
				ArtifactId:    &contractsv1.Identifier{Namespace: "artifact", Value: "pending"},
				ContentDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: digest},
				TenantId:      &contractsv1.Identifier{Namespace: "tenant", Value: testTenant},
			},
			ExtractorIdentity: &contractsv1.Digest{Algorithm: "sha256", Hex: extractorPinHex},
			BrainId:           &contractsv1.Identifier{Namespace: "brain", Value: "brain-a"},
			TenantId:          &contractsv1.Identifier{Namespace: "tenant", Value: testTenant},
			Modality: &contractsv1.MultimodalSourceEnvelope_Png{
				Png: &contractsv1.PngBounds{ByteLength: uint64(len(pngPayload)), WidthPx: 1, HeightPx: 1},
			},
		},
		IdempotencyKey: "png-jpeg",
	}
	if _, err := kernel.Admit(ctx, AdmitCommand{
		Identity: testIdentity(), Request: jpegReq, Payload: pngPayload,
	}); !errors.Is(err, ErrMediaTypeMismatch) {
		t.Fatalf("jpeg mismatch err = %v", err)
	}

	// Valid PNG.
	pngReq := jpegReq
	pngReq.Envelope.MediaType = "image/png"
	pngReq.IdempotencyKey = "png-ok"
	if _, err := kernel.Admit(ctx, AdmitCommand{
		Identity: testIdentity(), Request: pngReq, Payload: pngPayload,
	}); err != nil {
		t.Fatal(err)
	}

	// Valid minimal PDF.
	pdfPayload := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Page >>\nendobj\n%%EOF\n")
	pdfDigest := digestBytes(pdfPayload)
	pdfReq := &contractsv1.AdmitMultimodalSourceRequest{
		Caller: jpegReq.Caller,
		Envelope: &contractsv1.MultimodalSourceEnvelope{
			SourceObjectId: &contractsv1.Identifier{Namespace: "fixture", Value: "pdf-ok"},
			Kind:           contractsv1.MultimodalSourceKind_MULTIMODAL_SOURCE_KIND_PDF,
			MediaType:      "application/pdf",
			ByteLength:     uint64(len(pdfPayload)),
			ContentDigest:  &contractsv1.Digest{Algorithm: "sha256", Hex: pdfDigest},
			EncryptedOriginal: &contractsv1.ArtifactRef{
				ArtifactId:    &contractsv1.Identifier{Namespace: "artifact", Value: "pending"},
				ContentDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: pdfDigest},
				TenantId:      &contractsv1.Identifier{Namespace: "tenant", Value: testTenant},
			},
			ExtractorIdentity: &contractsv1.Digest{Algorithm: "sha256", Hex: extractorPinHex},
			BrainId:           &contractsv1.Identifier{Namespace: "brain", Value: "brain-a"},
			TenantId:          &contractsv1.Identifier{Namespace: "tenant", Value: testTenant},
			Modality: &contractsv1.MultimodalSourceEnvelope_Pdf{
				Pdf: &contractsv1.PdfBounds{ByteLength: uint64(len(pdfPayload)), PageCount: 1},
			},
		},
		IdempotencyKey: "pdf-ok",
	}
	if _, err := kernel.Admit(ctx, AdmitCommand{
		Identity: testIdentity(), Request: pdfReq, Payload: pdfPayload,
	}); err != nil {
		t.Fatal(err)
	}

	// Valid minimal PCM WAV.
	wavPayload := minimalWAV()
	wavDigest := digestBytes(wavPayload)
	wavReq := &contractsv1.AdmitMultimodalSourceRequest{
		Caller: jpegReq.Caller,
		Envelope: &contractsv1.MultimodalSourceEnvelope{
			SourceObjectId: &contractsv1.Identifier{Namespace: "fixture", Value: "wav-ok"},
			Kind:           contractsv1.MultimodalSourceKind_MULTIMODAL_SOURCE_KIND_PCM_WAV,
			MediaType:      "audio/wav",
			ByteLength:     uint64(len(wavPayload)),
			ContentDigest:  &contractsv1.Digest{Algorithm: "sha256", Hex: wavDigest},
			EncryptedOriginal: &contractsv1.ArtifactRef{
				ArtifactId:    &contractsv1.Identifier{Namespace: "artifact", Value: "pending"},
				ContentDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: wavDigest},
				TenantId:      &contractsv1.Identifier{Namespace: "tenant", Value: testTenant},
			},
			ExtractorIdentity: &contractsv1.Digest{Algorithm: "sha256", Hex: extractorPinHex},
			BrainId:           &contractsv1.Identifier{Namespace: "brain", Value: "brain-a"},
			TenantId:          &contractsv1.Identifier{Namespace: "tenant", Value: testTenant},
			Modality: &contractsv1.MultimodalSourceEnvelope_PcmWav{
				PcmWav: &contractsv1.PcmWavBounds{
					ByteLength: uint64(len(wavPayload)), DurationMillis: 1,
					SampleRateHz: 8000, Channels: 1,
				},
			},
		},
		IdempotencyKey: "wav-ok",
	}
	admitted, err := kernel.Admit(ctx, AdmitCommand{
		Identity: testIdentity(), Request: wavReq, Payload: wavPayload,
	})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := kernel.Evidence(ctx, EvidenceCommand{
		Identity: testIdentity(), SourceID: admitted.SourceId.Value, PageSize: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Items) == 0 || ev.Items[0].Anchor.GetAudio() == nil {
		t.Fatalf("expected audio anchor: %#v", ev.Items)
	}
	// No speaker identity on audio anchors.
	if ev.Items[0].Anchor.GetAudio().SpeakerEntityId != nil {
		t.Fatal("audio must not carry speaker identity")
	}
}

func TestPartialReady(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernel, _ := openTestKernel(t)
	payload := textPayload()
	request, body := validTextAdmit("partial-1", payload)
	admitted, err := kernel.Admit(ctx, AdmitCommand{
		Identity: testIdentity(), Request: request, Payload: body, ForcePartial: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := kernel.Status(ctx, StatusCommand{
		Identity: testIdentity(), SourceID: admitted.SourceId.Value,
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.Status.State != contractsv1.MultimodalSourceState_MULTIMODAL_SOURCE_STATE_PARTIAL_READY {
		t.Fatalf("state = %v", status.Status.State)
	}
}

// minimalPNG is a valid 1x1 transparent PNG.
func minimalPNG() []byte {
	return []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
		0x42, 0x60, 0x82,
	}
}

// minimalWAV is a tiny mono PCM WAV (8 kHz, 16-bit, 8 samples).
func minimalWAV() []byte {
	// 8 samples * 2 bytes = 16 data bytes.
	dataSize := uint32(16)
	riffSize := uint32(36 + dataSize)
	out := make([]byte, 44+dataSize)
	copy(out[0:4], "RIFF")
	out[4] = byte(riffSize)
	out[5] = byte(riffSize >> 8)
	out[6] = byte(riffSize >> 16)
	out[7] = byte(riffSize >> 24)
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	out[16] = 16 // fmt chunk size
	out[20] = 1  // PCM
	out[22] = 1  // mono
	// sample rate 8000
	out[24] = 0x40
	out[25] = 0x1f
	// byte rate 16000
	out[28] = 0x80
	out[29] = 0x3e
	out[32] = 2  // block align
	out[34] = 16 // bits
	copy(out[36:40], "data")
	out[40] = byte(dataSize)
	out[41] = byte(dataSize >> 8)
	out[42] = byte(dataSize >> 16)
	out[43] = byte(dataSize >> 24)
	return out
}
