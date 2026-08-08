//go:build cgo && faiss

package dense

// Optional real FAISS via CGo. Build with:
//
//	go build -tags faiss
//
// Requires libfaiss + headers on the host. Default residual path uses pure-Go
// HNSW (hnsw.go) so developers never need FAISS installed.
//
// Wire your CGo FAISS wrapper here and return it from OpenFAISSNative when
// production packages ship linked FAISS binaries.

// OpenFAISSNative is a hook for linked FAISS; nil means use pure-Go HNSW.
func OpenFAISSNative(dim int) (*HNSW, error) {
	// Placeholder: production FAISS packaging replaces this with CGo bindings.
	return NewHNSW(dim, 32, 128), nil
}
