package authorityprocess

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
	gateway "github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	"google.golang.org/protobuf/proto"
)

type externalDenial struct {
	status      int
	contentType string
	body        []byte
}

type denialClock struct{ millis int64 }

func (clock denialClock) NowUnixMilli() int64 { return clock.millis }

type denialKeyResolver struct{ reference brain.KeyReference }

func (resolver denialKeyResolver) Current(ctx context.Context, tenant brain.Identifier) (brain.KeyMaterial, error) {
	if err := ctx.Err(); err != nil {
		return brain.KeyMaterial{}, err
	}
	if tenant.Namespace != "tenant" || tenant.Value != resolver.reference.Root.Value {
		return brain.KeyMaterial{}, brain.ErrUnavailable
	}
	return brain.KeyMaterial{Reference: resolver.reference, RootKey: bytes.Repeat([]byte{7}, brain.RootKeyBytes)}, nil
}

func (resolver denialKeyResolver) Resolve(
	ctx context.Context,
	tenant brain.Identifier,
	epoch uint64,
) (brain.KeyMaterial, error) {
	if epoch != resolver.reference.Epoch {
		return brain.KeyMaterial{}, brain.ErrUnavailable
	}
	return resolver.Current(ctx, tenant)
}

func TestAbsentAndRevokedResultsHaveIdenticalExternalDenialShape(t *testing.T) {
	now := time.Unix(1_000, 0)
	results := []struct {
		name   string
		result brain.Result
	}{
		{name: "absent artifact", result: actualRuntimeDenial(t, false)},
		{name: "revoked policy", result: actualRuntimeDenial(t, true)},
	}
	for _, result := range results {
		if result.result.Receipt.Status != "rejected" || result.result.Authorization.Allowed ||
			result.result.Receipt.ReasonCode != "not_found_or_denied" {
			t.Fatalf("%s runtime result = %#v", result.name, result.result)
		}
	}

	external := make([]externalDenial, 0, len(results))
	for _, source := range results {
		result, malformed := commandDenialOverHTTP(t, source.result, now)
		if result.status != http.StatusOK || result.contentType != "application/proto" {
			t.Fatalf("%s denial transport = %d %q", source.name, result.status, result.contentType)
		}
		response := &contractsv1.ExecuteAuthorityCommandResponse{}
		if err := proto.Unmarshal(result.body, response); err != nil {
			t.Fatal(err)
		}
		if response.Receipt == nil ||
			response.Receipt.Status != contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED ||
			response.Authorization == nil || response.Authorization.AclEpoch != 3 ||
			response.Error == nil || response.Error.Code != "request-denied" || response.Error.Render == nil {
			t.Fatalf("%s denial response = %#v", source.name, response)
		}
		if response.Artifact != nil || response.Generation != 0 ||
			response.FrameDigest != nil || response.NextCursor != nil {
			t.Fatalf("%s denial exposed artifact facts = %#v", source.name, response)
		}
		if malformed.status != http.StatusBadRequest || malformed.contentType != "application/json" ||
			!bytes.Equal(malformed.body, []byte(`{"code":"request-malformed"}`)) {
			t.Fatalf("malformed transport = %d %q %q", malformed.status, malformed.contentType, malformed.body)
		}
		external = append(external, result)
	}
	if external[0].status != external[1].status || external[0].contentType != external[1].contentType ||
		!bytes.Equal(external[0].body, external[1].body) {
		t.Fatalf("absent and revoked responses differ: %#v != %#v", external[0], external[1])
	}
}

func actualRuntimeDenial(t *testing.T, policyDenied bool) brain.Result {
	t.Helper()
	root := t.TempDir()
	reference := brain.KeyReference{
		Root:  brain.Identifier{Namespace: "key-root", Value: "t"},
		KeyID: brain.Identifier{Namespace: "key", Value: "denial-test-key"}, Epoch: 1,
	}
	runtime, err := brain.OpenDurable(context.Background(), brain.DurableConfig{
		DatabasePath: filepath.Join(root, "authority.db"), ObjectRoot: filepath.Join(root, "objects"),
		Tenant: brain.Identifier{Namespace: "tenant", Value: "t"}, CurrentKeyReference: reference,
		Brain: brain.Identifier{Namespace: "brain", Value: "b"}, ConfigurationDigest: testDigest("config"),
		Clock: denialClock{millis: 1_000_000}, Storage: brain.StorageOptions{FrameBytes: 4, MaxReadBytes: 1024},
	}, denialKeyResolver{reference: reference})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	_, identity := testBroker(t)
	if _, err := runtime.OpenSession(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	request := brain.ExecuteRequest{
		Identity: identity,
		Command: brain.Command{
			ID: brain.Identifier{Namespace: "command", Value: "c"}, Type: "artifact.read",
			IdempotencyKey: "key", PayloadDigest: testDigest("runtime-denial"), Fence: 7,
		},
		Artifact: brain.Artifact{
			ID:     brain.Identifier{Namespace: "artifact", Value: "a"},
			Tenant: brain.Identifier{Namespace: "tenant", Value: "t"},
			Digest: testDigest("content"), Generation: 1, KeyEpoch: 1,
		},
		Length: 7,
		Authorize: func(context.Context, brain.Identity, string, brain.Identifier) (brain.Authorization, error) {
			decision := brain.Authorization{Allowed: true, ReasonCode: "allowed", RevocationEpoch: 3}
			if policyDenied {
				decision.Allowed = false
				decision.ReasonCode = "not_found_or_denied"
				return decision, brain.ErrDenied
			}
			return decision, nil
		},
	}
	result, err := runtime.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func commandDenialOverHTTP(t *testing.T, result brain.Result, now time.Time) (externalDenial, externalDenial) {
	t.Helper()
	policy, identity := testBroker(t)
	authority := &authorityAdapter{
		runtime: &fakeRuntime{result: result, returnRaw: true},
		broker:  policy, keyEpoch: 1, now: func() time.Time { return now },
	}
	request := testExecuteRequest(t, now)
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	peer := gateway.PeerContext{Identity: identity}
	return commandAdapterHTTP(t, authority, peer, payload),
		commandAdapterHTTP(t, authority, peer, []byte{0x0a, 0xff})
}

func commandAdapterHTTP(
	t *testing.T,
	authority *authorityAdapter,
	peer gateway.PeerContext,
	payload []byte,
) externalDenial {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://local/execute", bytes.NewReader(payload))
	recorder := httptest.NewRecorder()
	message := &contractsv1.ExecuteAuthorityCommandRequest{}
	if err := proto.Unmarshal(payload, message); err != nil {
		recorder.Header().Set("Content-Type", "application/json")
		recorder.WriteHeader(http.StatusBadRequest)
		_, _ = recorder.Write([]byte(`{"code":"request-malformed"}`))
		return recordedDenial(recorder)
	}
	response, err := authority.Execute(request.Context(), peer, message)
	if err != nil {
		recorder.Header().Set("Content-Type", "application/json")
		recorder.WriteHeader(http.StatusForbidden)
		_, _ = recorder.Write([]byte(`{"code":"request-denied"}`))
		return recordedDenial(recorder)
	}
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	recorder.Header().Set("Content-Type", "application/proto")
	recorder.WriteHeader(http.StatusOK)
	_, _ = recorder.Write(body)
	return recordedDenial(recorder)
}

func recordedDenial(recorder *httptest.ResponseRecorder) externalDenial {
	return externalDenial{
		status: recorder.Code, contentType: recorder.Header().Get("Content-Type"), body: recorder.Body.Bytes(),
	}
}
