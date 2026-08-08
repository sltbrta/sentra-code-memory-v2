//go:build !faiss

package dense

// OpenFAISSNative is unavailable without -tags faiss; pure-Go HNSW is the default.
func OpenFAISSNative(dim int) (*HNSW, error) {
	return NewHNSW(dim, 16, 64), nil
}
