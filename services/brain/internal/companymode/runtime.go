package companymode

import (
	"context"
	"fmt"
	"sync"
)

// Runtime is the bounded company profile with two principals and isolation.
type Runtime struct {
	mu        sync.Mutex
	profile   ProfileID
	tenantID  string
	alice     Principal
	bob       Principal
	events    EventStore
	objects   ObjectStore
	policy    PolicyCheck
	admission *Admission
	status    []string
}

// NewCompanyRuntime wires the two-principal company profile over the given ports.
func NewCompanyRuntime(events EventStore, objects ObjectStore, policy PolicyCheck) *Runtime {
	tenant := "company-acme"
	return &Runtime{
		profile:   ProfileCompany,
		tenantID:  tenant,
		alice:     Principal{ID: "alice", TenantID: tenant},
		bob:       Principal{ID: "bob", TenantID: tenant},
		events:    events,
		objects:   objects,
		policy:    policy,
		admission: NewAdmission(),
	}
}

// Profile returns the active deployment profile.
func (r *Runtime) Profile() ProfileID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.profile
}

// SwitchProfile toggles local/company. Company retains tenant isolation.
func (r *Runtime) SwitchProfile(profile ProfileID) error {
	if profile != ProfileLocal && profile != ProfileCompany {
		return ErrRejected
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.profile = profile
	r.status = append(r.status, fmt.Sprintf("profile=%s", profile))
	return nil
}

// Principals returns the two company principals.
func (r *Runtime) Principals() (Principal, Principal) {
	return r.alice, r.bob
}

// TenantID returns the company tenant.
func (r *Runtime) TenantID() string {
	return r.tenantID
}

// Ingest records one authorized ingestion event and blob for a principal.
func (r *Runtime) Ingest(ctx context.Context, principal Principal, eventID, blobKey string, body []byte) error {
	if principal.TenantID != r.tenantID {
		return ErrDenied
	}
	if !r.policy.Allow(ctx, principal, "source.add", "brain:company") {
		return ErrDenied
	}
	decision, err := r.admission.Decide(ctx, ClassIngestion)
	if err != nil {
		return err
	}
	if decision != Admit {
		return ErrQueueFull
	}
	defer r.admission.Release(ClassIngestion)

	ref, err := r.objects.Put(ctx, principal.TenantID, blobKey, body)
	if err != nil {
		return err
	}
	return r.events.Append(ctx, Event{
		TenantID:  principal.TenantID,
		EventID:   eventID,
		Kind:      "ingest",
		Payload:   []byte(ref.Digest),
		Principal: principal.ID,
	})
}

// Query lists events visible to a principal under current authorization.
func (r *Runtime) Query(ctx context.Context, principal Principal) ([]Event, error) {
	if principal.TenantID != r.tenantID {
		return nil, ErrDenied
	}
	if !r.policy.Allow(ctx, principal, "query", "brain:company") {
		return nil, ErrDenied
	}
	decision, err := r.admission.Decide(ctx, ClassInteractiveQuery)
	if err != nil {
		return nil, err
	}
	if decision != Admit {
		return nil, ErrQueueFull
	}
	defer r.admission.Release(ClassInteractiveQuery)
	return r.events.List(ctx, principal.TenantID)
}

// ReadBlob returns a blob only for an authorized same-tenant principal.
func (r *Runtime) ReadBlob(ctx context.Context, principal Principal, key string) ([]byte, error) {
	if principal.TenantID != r.tenantID {
		return nil, ErrDenied
	}
	if !r.policy.Allow(ctx, principal, "artifact.read", "brain:company") {
		return nil, ErrDenied
	}
	return r.objects.Get(ctx, principal.TenantID, key)
}

// OperatorStatus returns a permission-aware operator receipt.
func (r *Runtime) OperatorStatus(ctx context.Context, principal Principal) (StatusReceipt, error) {
	if !r.policy.Allow(ctx, principal, "operator.status", "tenant:"+r.tenantID) {
		return StatusReceipt{}, ErrDenied
	}
	decision, err := r.admission.Decide(ctx, ClassOperator)
	if err != nil {
		return StatusReceipt{}, err
	}
	if decision != Admit {
		return StatusReceipt{}, ErrQueueFull
	}
	defer r.admission.Release(ClassOperator)

	events, err := r.events.List(ctx, r.tenantID)
	if err != nil {
		return StatusReceipt{}, err
	}
	blobs, err := r.objects.Inventory(ctx)
	if err != nil {
		return StatusReceipt{}, err
	}
	tenantBlobs := 0
	for _, b := range blobs {
		if b.TenantID == r.tenantID {
			tenantBlobs++
		}
	}
	return StatusReceipt{
		Profile:     string(r.Profile()),
		TenantID:    r.tenantID,
		EventCount:  len(events),
		BlobCount:   tenantBlobs,
		Admission:   r.admission.Snapshot(),
		Principals:  []string{r.alice.ID, r.bob.ID},
		OpenFGAMode: "in_process_evaluator",
		OpenFGANote: "DEF-015 residual: HTTP client + hermetic dual-run landed; default path remains in-process; live store conformance open (#72 partial)",
		BackupReady: true,
		NonClaims:   []string{"k8s", "1000_users", "aws_eks", "multi_region"},
	}, nil
}

// StatusReceipt is the operator-facing company status surface.
type StatusReceipt struct {
	Profile     string                 `json:"profile"`
	TenantID    string                 `json:"tenant_id"`
	EventCount  int                    `json:"event_count"`
	BlobCount   int                    `json:"blob_count"`
	Admission   map[OperationClass]int `json:"admission"`
	Principals  []string               `json:"principals"`
	OpenFGAMode string                 `json:"openfga_mode"`
	OpenFGANote string                 `json:"openfga_note"`
	BackupReady bool                   `json:"backup_ready"`
	NonClaims   []string               `json:"non_claims"`
}
