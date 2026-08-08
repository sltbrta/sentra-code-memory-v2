package contentprivacy_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/contentprivacy"
)

func TestProductionProjectionAdapterCannotPublishAroundGuard(t *testing.T) {
	now := testNow
	guard := newGuard(t, basePolicy(), nil, nil, &now)
	var published []contentprivacy.Projection
	publisher := contentprivacy.ProjectionPublisherFunc(func(_ context.Context, projection contentprivacy.Projection) error {
		published = append(published, projection)
		return nil
	})
	adapter, err := contentprivacy.NewProductionProjectionAdapter(guard, publisher)
	if err != nil {
		t.Fatal(err)
	}
	input := contentprivacy.Input{
		TenantID: "acme", ID: "production-one",
		Scope:   contentprivacy.Scope{Kind: contentprivacy.ScopeIndividual, ID: "alice"},
		Content: "contact alice@example.com today", Blind: true,
	}
	decision, err := adapter.AdmitAndPublish(context.Background(), input)
	if err != nil || decision.Status != contentprivacy.StatusRedacted || len(published) != 1 {
		t.Fatalf("production admission = %+v, published=%+v, err=%v", decision, published, err)
	}
	projection := published[0]
	if strings.Contains(projection.Content, "alice@example.com") || projection.IndexText != projection.Content || projection.CacheText != projection.Content || !projection.Blind {
		t.Fatalf("publisher received bypassable projection: %+v", projection)
	}
	receipt, err := json.Marshal(decision.Receipt)
	if err != nil || strings.Contains(string(receipt), "alice@example.com") {
		t.Fatalf("receipt retained raw content: %s, %v", receipt, err)
	}

	teamInput := contentprivacy.Input{
		TenantID: "acme", ID: "production-two",
		Scope:   contentprivacy.Scope{Kind: contentprivacy.ScopeTeam, ID: "eng"},
		Content: "token sk_1234567890abcd",
	}
	decision, err = adapter.AdmitAndPublish(context.Background(), teamInput)
	if err != nil || decision.Status != contentprivacy.StatusQuarantined || len(published) != 1 {
		t.Fatalf("quarantine reached publisher: %+v, count=%d, err=%v", decision, len(published), err)
	}
}

func TestProductionProjectionAdapterFailsClosedWhenAbsentOrPublisherFails(t *testing.T) {
	now := testNow
	guard := newGuard(t, basePolicy(), nil, nil, &now)
	if _, err := contentprivacy.NewProductionProjectionAdapter(nil, contentprivacy.ProjectionPublisherFunc(func(context.Context, contentprivacy.Projection) error { return nil })); !errors.Is(err, contentprivacy.ErrComposition) {
		t.Fatalf("nil guard = %v", err)
	}
	if _, err := contentprivacy.NewProductionProjectionAdapter(guard, nil); !errors.Is(err, contentprivacy.ErrComposition) {
		t.Fatalf("nil publisher = %v", err)
	}
	var zero *contentprivacy.ProductionProjectionAdapter
	if _, err := zero.AdmitAndPublish(context.Background(), contentprivacy.Input{}); !errors.Is(err, contentprivacy.ErrComposition) {
		t.Fatalf("zero adapter = %v", err)
	}

	secret := "alice@example.com"
	adapter, err := contentprivacy.NewProductionProjectionAdapter(guard, contentprivacy.ProjectionPublisherFunc(func(context.Context, contentprivacy.Projection) error {
		return errors.New("backend rejected " + secret)
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.AdmitAndPublish(context.Background(), contentprivacy.Input{
		TenantID: "acme", ID: "publisher-failure",
		Scope: contentprivacy.Scope{Kind: contentprivacy.ScopeIndividual, ID: "alice"}, Content: secret,
	})
	if !errors.Is(err, contentprivacy.ErrPublish) || strings.Contains(err.Error(), secret) {
		t.Fatalf("publisher error was not collapsed: %v", err)
	}
}

func TestProductionProjectionAdapterPublisherFailureIsRetrySafe(t *testing.T) {
	for _, test := range []struct {
		name  string
		panic bool
	}{
		{name: "error"},
		{name: "panic", panic: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := testNow
			guard := newGuard(t, basePolicy(), nil, nil, &now)
			scope := contentprivacy.Scope{Kind: contentprivacy.ScopeIndividual, ID: "alice"}
			input := contentprivacy.Input{
				TenantID: "acme", ID: "retry-" + test.name, Scope: scope,
				Content: "contact alice@example.com",
			}
			attempts := 0
			var published []contentprivacy.Projection
			adapter, err := contentprivacy.NewProductionProjectionAdapter(guard, contentprivacy.ProjectionPublisherFunc(func(_ context.Context, projection contentprivacy.Projection) error {
				attempts++
				published = append(published, projection)
				if _, err := guard.Projection(input.TenantID, scope, input.ID); !errors.Is(err, contentprivacy.ErrDenied) {
					t.Fatalf("transient admission visible during publish: %v", err)
				}
				if attempts == 1 {
					if test.panic {
						panic("publisher panic with sensitive backend detail")
					}
					return errors.New("publisher error with sensitive backend detail")
				}
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}

			failed, err := adapter.AdmitAndPublish(context.Background(), input)
			if !errors.Is(err, contentprivacy.ErrPublish) || failed.Receipt.Seq != 0 {
				t.Fatalf("failed publish = %+v, %v", failed, err)
			}
			if _, err := guard.Projection(input.TenantID, scope, input.ID); !errors.Is(err, contentprivacy.ErrDenied) {
				t.Fatalf("failed publish committed admission: %v", err)
			}
			if receipts := guard.Receipts(); len(receipts) != 1 || receipts[0].Kind != contentprivacy.ReceiptPolicyInstall {
				t.Fatalf("failed publish committed receipt: %+v", receipts)
			}

			succeeded, err := adapter.AdmitAndPublish(context.Background(), input)
			if err != nil || succeeded.Receipt.Kind != contentprivacy.ReceiptContentRedact || attempts != 2 {
				t.Fatalf("publish retry = %+v, attempts=%d, err=%v", succeeded, attempts, err)
			}
			stored, err := guard.Projection(input.TenantID, scope, input.ID)
			if err != nil || succeeded.Projection == nil || !reflect.DeepEqual(stored, *succeeded.Projection) {
				t.Fatalf("committed retry projection = %+v, %v", stored, err)
			}
			if len(published) != 2 || !reflect.DeepEqual(published[0], published[1]) {
				t.Fatalf("retry changed stable projection: %+v", published)
			}
			if _, err := adapter.AdmitAndPublish(context.Background(), input); !errors.Is(err, contentprivacy.ErrConflict) || attempts != 2 {
				t.Fatalf("successful admission replay = attempts=%d, err=%v", attempts, err)
			}
		})
	}
}

func TestProductionProjectionAdapterRejectsNonRuneBoundaryFindingsBeforePublish(t *testing.T) {
	for _, test := range []struct {
		name    string
		finding contentprivacy.Finding
	}{
		{name: "start inside rune", finding: contentprivacy.Finding{Class: contentprivacy.ClassEmail, Start: 1, End: 2}},
		{name: "end inside rune", finding: contentprivacy.Finding{Class: contentprivacy.ClassEmail, Start: 0, End: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := testNow
			guard := newGuard(t, basePolicy(), fixedFindingDetector{finding: test.finding}, nil, &now)
			published := 0
			adapter, err := contentprivacy.NewProductionProjectionAdapter(guard, contentprivacy.ProjectionPublisherFunc(func(context.Context, contentprivacy.Projection) error {
				published++
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			decision, err := adapter.AdmitAndPublish(context.Background(), contentprivacy.Input{
				TenantID: "acme", ID: "rune-" + strings.ReplaceAll(test.name, " ", "-"),
				Scope: contentprivacy.Scope{Kind: contentprivacy.ScopeIndividual, ID: "alice"}, Content: "éx",
			})
			if !errors.Is(err, contentprivacy.ErrDetector) || decision.Status != contentprivacy.StatusQuarantined ||
				decision.Projection != nil || published != 0 {
				t.Fatalf("non-boundary finding = %+v, published=%d, err=%v", decision, published, err)
			}
		})
	}
}

func TestProductionProjectionAdapterDetectorFailureNeverPublishes(t *testing.T) {
	now := testNow
	guard := newGuard(t, basePolicy(), failingDetector{}, nil, &now)
	published := 0
	adapter, err := contentprivacy.NewProductionProjectionAdapter(guard, contentprivacy.ProjectionPublisherFunc(func(context.Context, contentprivacy.Projection) error {
		published++
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	decision, err := adapter.AdmitAndPublish(context.Background(), contentprivacy.Input{
		TenantID: "acme", ID: "detector-failure-production",
		Scope: contentprivacy.Scope{Kind: contentprivacy.ScopeIndividual, ID: "alice"}, Content: "opaque input",
	})
	if !errors.Is(err, contentprivacy.ErrDetector) || decision.Status != contentprivacy.StatusQuarantined || published != 0 {
		t.Fatalf("detector failure reached publisher: decision=%+v published=%d err=%v", decision, published, err)
	}
}
