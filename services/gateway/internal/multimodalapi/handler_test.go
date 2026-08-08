package multimodalapi

import (
	"context"
	"testing"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

type fakeKernel struct {
	admitResult *contractsv1.AdmitMultimodalSourceSuccess
	admitErr    error
}

func (f *fakeKernel) Admit(context.Context, AdmitCommand) (*contractsv1.AdmitMultimodalSourceSuccess, error) {
	return f.admitResult, f.admitErr
}
func (f *fakeKernel) Status(context.Context, StatusCommand) (*contractsv1.GetMultimodalStatusSuccess, error) {
	return nil, ErrUnknownSource
}
func (f *fakeKernel) Evidence(context.Context, EvidenceCommand) (*contractsv1.GetMultimodalEvidenceSuccess, error) {
	return nil, ErrUnknownSource
}
func (f *fakeKernel) Revoke(context.Context, RevokeCommand) (*contractsv1.RevokeMultimodalSourceSuccess, error) {
	return nil, ErrUnknownSource
}
func (f *fakeKernel) Purge(context.Context, PurgeCommand) (*contractsv1.PurgeMultimodalSourceSuccess, error) {
	return nil, ErrUnknownSource
}

type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

func testPeer() localauthority.PeerContext {
	return localauthority.PeerContext{
		Identity: shared.MappedIdentityFact{
			Principal: shared.Identifier{Namespace: "principal", Value: "principal-a"},
			Tenant:    shared.Identifier{Namespace: "tenant", Value: "tenant-a"},
			Session:   shared.Identifier{Namespace: "session", Value: "session-a"},
		},
	}
}

func validCaller() *contractsv1.UntrustedMultimodalCaller {
	return &contractsv1.UntrustedMultimodalCaller{
		RequestedPrincipal: &contractsv1.AuthenticatedPrincipalRef{
			PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: "principal-a"},
			TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: "tenant-a"},
			SessionId:   &contractsv1.Identifier{Namespace: "session", Value: "session-a"},
		},
		RequestedSession: &contractsv1.Identifier{Namespace: "session", Value: "session-a"},
	}
}

func TestHandlerAdmitSuccessAndDenial(t *testing.T) {
	t.Parallel()
	handler, err := NewHandler(Config{
		Kernel: &fakeKernel{admitResult: &contractsv1.AdmitMultimodalSourceSuccess{
			SourceId:         &contractsv1.Identifier{Namespace: "multimodal-source", Value: "s1"},
			SourceRevisionId: &contractsv1.Identifier{Namespace: "source-revision", Value: "r1"},
			State:            contractsv1.MultimodalSourceState_MULTIMODAL_SOURCE_STATE_ADMITTED,
		}},
		Clock:               fixedClock{at: time.Unix(1_700_000_000, 0).UTC()},
		ConfigurationDigest: shared.Digest{Algorithm: "sha256", Hex: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	request := &contractsv1.AdmitMultimodalSourceRequest{
		Caller: validCaller(),
		Envelope: &contractsv1.MultimodalSourceEnvelope{
			SourceObjectId: &contractsv1.Identifier{Namespace: "fixture", Value: "text"},
			Kind:           contractsv1.MultimodalSourceKind_MULTIMODAL_SOURCE_KIND_TEXT_MARKDOWN,
			MediaType:      "text/plain",
			ByteLength:     4,
			ContentDigest:  &contractsv1.Digest{Algorithm: "sha256", Hex: digest},
			EncryptedOriginal: &contractsv1.ArtifactRef{
				ArtifactId:    &contractsv1.Identifier{Namespace: "artifact", Value: "pending"},
				ContentDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: digest},
				TenantId:      &contractsv1.Identifier{Namespace: "tenant", Value: "tenant-a"},
			},
			ExtractorIdentity: &contractsv1.Digest{Algorithm: "sha256", Hex: digest},
			BrainId:           &contractsv1.Identifier{Namespace: "brain", Value: "brain-a"},
			TenantId:          &contractsv1.Identifier{Namespace: "tenant", Value: "tenant-a"},
			Modality: &contractsv1.MultimodalSourceEnvelope_Text{
				Text: &contractsv1.TextMarkdownBounds{Utf8ByteLength: 4},
			},
		},
		IdempotencyKey: "k1",
	}
	response, err := handler.AdmitMultimodalSource(context.Background(), testPeer(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetSuccess() == nil || response.GetSuccess().SourceId.Value != "s1" {
		t.Fatalf("response = %#v", response)
	}

	denied, err := NewHandler(Config{
		Kernel:              &fakeKernel{admitErr: ErrOversized},
		Clock:               fixedClock{at: time.Unix(1_700_000_000, 0).UTC()},
		ConfigurationDigest: shared.Digest{Algorithm: "sha256", Hex: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := denied.AdmitMultimodalSource(context.Background(), testPeer(), request)
	if err != nil {
		t.Fatal(err)
	}
	if out.GetError() == nil || out.GetError().Code != "oversized" {
		t.Fatalf("denied = %#v", out)
	}
}
