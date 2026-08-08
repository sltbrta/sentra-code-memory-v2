package companymode_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/companymode"
)

func newRuntime(t *testing.T) *companymode.Runtime {
	t.Helper()
	policy, err := companymode.NewAuthzPolicy()
	if err != nil {
		t.Fatal(err)
	}
	return companymode.NewCompanyRuntime(companymode.NewFakePostgres(), companymode.NewFakeS3(), policy)
}

func TestTwoPrincipalIsolationAndProfileSwitch(t *testing.T) {
	ctx := context.Background()
	rt := newRuntime(t)
	if rt.Profile() != companymode.ProfileCompany {
		t.Fatalf("profile = %s", rt.Profile())
	}
	alice, bob := rt.Principals()

	if err := rt.Ingest(ctx, alice, "e1", "doc-a", []byte("alice-bytes")); err != nil {
		t.Fatal(err)
	}
	// Bob is viewer: query ok, ingest denied.
	if _, err := rt.Query(ctx, bob); err != nil {
		t.Fatalf("bob query: %v", err)
	}
	if err := rt.Ingest(ctx, bob, "e2", "doc-b", []byte("bob-bytes")); !errors.Is(err, companymode.ErrDenied) {
		t.Fatalf("bob ingest = %v, want denied", err)
	}
	body, err := rt.ReadBlob(ctx, bob, "doc-a")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "alice-bytes" {
		t.Fatalf("body = %q", body)
	}

	// Cross-tenant stranger never sees company data.
	stranger := companymode.Principal{ID: "eve", TenantID: "other-co"}
	if _, err := rt.Query(ctx, stranger); !errors.Is(err, companymode.ErrDenied) {
		t.Fatalf("stranger query = %v", err)
	}
	if _, err := rt.ReadBlob(ctx, stranger, "doc-a"); !errors.Is(err, companymode.ErrDenied) {
		t.Fatalf("stranger blob = %v", err)
	}

	if err := rt.SwitchProfile(companymode.ProfileLocal); err != nil {
		t.Fatal(err)
	}
	if rt.Profile() != companymode.ProfileLocal {
		t.Fatal("profile switch failed")
	}
}

func TestSQLiteTransferRejected(t *testing.T) {
	ctx := context.Background()
	events := companymode.NewFakePostgres()
	objects := companymode.NewFakeS3()
	bundle := companymode.SyncBundle{
		Events: []companymode.Event{{
			TenantID: "company-acme",
			EventID:  "bad",
			Kind:     "transfer",
			Payload:  []byte("SQLite format 3\x00"),
		}},
	}
	if err := companymode.ApplySync(ctx, events, objects, bundle); !errors.Is(err, companymode.ErrSQLiteTransfer) {
		t.Fatalf("err = %v", err)
	}
	// Legal event/blob sync works.
	ok := companymode.SyncBundle{
		Events: []companymode.Event{{
			TenantID: "company-acme", EventID: "e1", Kind: "ingest", Payload: []byte("digest"),
		}},
		Blobs: []companymode.BlobObject{{
			Ref:  companymode.BlobRef{TenantID: "company-acme", Key: "k1"},
			Body: []byte("bytes"),
		}},
	}
	if err := companymode.ApplySync(ctx, events, objects, ok); err != nil {
		t.Fatal(err)
	}
}

func TestBackupRestoreSmoke(t *testing.T) {
	ctx := context.Background()
	rt := newRuntime(t)
	alice, _ := rt.Principals()
	if err := rt.Ingest(ctx, alice, "e1", "k1", []byte("payload")); err != nil {
		t.Fatal(err)
	}
	// Reach into ports via backup of the runtime stores by re-creating with shared fakes.
	events := companymode.NewFakePostgres()
	objects := companymode.NewFakeS3()
	policy, err := companymode.NewAuthzPolicy()
	if err != nil {
		t.Fatal(err)
	}
	rt2 := companymode.NewCompanyRuntime(events, objects, policy)
	if err := rt2.Ingest(ctx, alice, "e1", "k1", []byte("payload")); err != nil {
		t.Fatal(err)
	}
	backup, err := companymode.CreateBackup(ctx, companymode.ProfileCompany, "company-acme", events, objects)
	if err != nil {
		t.Fatal(err)
	}
	if backup.Manifest.EventCount != 1 || backup.Manifest.BlobCount != 1 {
		t.Fatalf("manifest = %+v", backup.Manifest)
	}
	targetE := companymode.NewFakePostgres()
	targetO := companymode.NewFakeS3()
	if err := companymode.RestoreBackup(ctx, targetE, targetO, backup); err != nil {
		t.Fatal(err)
	}
	listed, err := targetE.List(ctx, "company-acme")
	if err != nil || len(listed) != 1 {
		t.Fatalf("restored events = %v %v", listed, err)
	}
	body, err := targetO.Get(ctx, "company-acme", "k1")
	if err != nil || string(body) != "payload" {
		t.Fatalf("restored blob = %q %v", body, err)
	}
}

func TestTwentyUserMixedLoadNoACLBypass(t *testing.T) {
	ctx := context.Background()
	rt := newRuntime(t)
	result, err := companymode.RunMixedLoadSmoke(ctx, rt)
	if err != nil {
		t.Fatalf("load smoke: %v result=%+v", err, result)
	}
	if result.Status != "passed" {
		t.Fatalf("status = %s", result.Status)
	}
	if result.Users != 20 {
		t.Fatalf("users = %d", result.Users)
	}
	if result.ACLBypass != 0 {
		t.Fatalf("acl bypass = %d", result.ACLBypass)
	}
	if result.QueryOK < 1 || result.IngestOK < 1 {
		t.Fatalf("throughput query_ok=%d ingest_ok=%d", result.QueryOK, result.IngestOK)
	}
}

func TestOperatorStatusAndAdmissionBounds(t *testing.T) {
	ctx := context.Background()
	rt := newRuntime(t)
	alice, bob := rt.Principals()
	receipt, err := rt.OperatorStatus(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.OpenFGAMode != "in_process_evaluator" {
		t.Fatalf("openfga mode = %s", receipt.OpenFGAMode)
	}
	if receipt.OpenFGANote == "" {
		t.Fatal("expected DEF-015 residual note")
	}
	if _, err := rt.OperatorStatus(ctx, bob); !errors.Is(err, companymode.ErrDenied) {
		t.Fatalf("bob operator = %v", err)
	}

	// Saturate interactive query queue.
	adm := companymode.NewAdmission()
	for i := 0; i < 12; i++ {
		if d, err := adm.Decide(ctx, companymode.ClassInteractiveQuery); err != nil || d != companymode.Admit {
			t.Fatalf("admit %d: %v %s", i, err, d)
		}
	}
	if d, err := adm.Decide(ctx, companymode.ClassInteractiveQuery); !errors.Is(err, companymode.ErrQueueFull) || d != companymode.Reject {
		t.Fatalf("expect full: %v %s", err, d)
	}
}

func TestTenantRowIsolationInFakePostgres(t *testing.T) {
	ctx := context.Background()
	pg := companymode.NewFakePostgres()
	if err := pg.Append(ctx, companymode.Event{TenantID: "a", EventID: "1", Kind: "x", Payload: []byte("a")}); err != nil {
		t.Fatal(err)
	}
	if err := pg.Append(ctx, companymode.Event{TenantID: "b", EventID: "2", Kind: "x", Payload: []byte("b")}); err != nil {
		t.Fatal(err)
	}
	a, err := pg.List(ctx, "a")
	if err != nil || len(a) != 1 || string(a[0].Payload) != "a" {
		t.Fatalf("tenant a = %+v %v", a, err)
	}
	b, err := pg.List(ctx, "b")
	if err != nil || len(b) != 1 || string(b[0].Payload) != "b" {
		t.Fatalf("tenant b = %+v %v", b, err)
	}
}
