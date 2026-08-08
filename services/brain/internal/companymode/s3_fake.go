package companymode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// FakeS3 is an in-process S3-compatible object store. Keys are namespaced by
// tenant; cross-tenant gets deny with the same non-disclosing error.
type FakeS3 struct {
	mu    sync.Mutex
	blobs map[string]BlobObject
}

// NewFakeS3 returns an empty in-process object store.
func NewFakeS3() *FakeS3 {
	return &FakeS3{blobs: make(map[string]BlobObject)}
}

func objectKey(tenantID, key string) string {
	return tenantID + "/" + key
}

// Put stores an immutable object under a tenant namespace.
func (s *FakeS3) Put(_ context.Context, tenantID, key string, body []byte) (BlobRef, error) {
	if tenantID == "" || key == "" || body == nil {
		return BlobRef{}, ErrRejected
	}
	sum := sha256.Sum256(body)
	ref := BlobRef{
		TenantID: tenantID,
		Key:      key,
		Digest:   hex.EncodeToString(sum[:]),
		Length:   int64(len(body)),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blobs[objectKey(tenantID, key)] = BlobObject{Ref: ref, Body: append([]byte(nil), body...)}
	return ref, nil
}

// Get returns object bytes only when the tenant matches the key namespace.
func (s *FakeS3) Get(_ context.Context, tenantID, key string) ([]byte, error) {
	if tenantID == "" || key == "" {
		return nil, ErrDenied
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.blobs[objectKey(tenantID, key)]
	if !ok {
		return nil, ErrDenied
	}
	return append([]byte(nil), obj.Body...), nil
}

// Inventory lists all blob refs (operator-facing; not a user ACL surface).
func (s *FakeS3) Inventory(_ context.Context) ([]BlobRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]BlobRef, 0, len(s.blobs))
	for _, obj := range s.blobs {
		out = append(out, obj.Ref)
	}
	return out, nil
}

// Restore replaces all blobs from a backup dump.
func (s *FakeS3) Restore(_ context.Context, blobs []BlobObject) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blobs = make(map[string]BlobObject, len(blobs))
	for _, blob := range blobs {
		if blob.Ref.TenantID == "" || blob.Ref.Key == "" {
			return ErrRejected
		}
		s.blobs[objectKey(blob.Ref.TenantID, blob.Ref.Key)] = BlobObject{
			Ref:  blob.Ref,
			Body: append([]byte(nil), blob.Body...),
		}
	}
	return nil
}

// String returns a stable debug identity for tests.
func (s *FakeS3) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("FakeS3(n=%d)", len(s.blobs))
}
