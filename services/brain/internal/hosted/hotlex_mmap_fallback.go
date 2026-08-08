//go:build !unix

package hosted

import "os"

// mapHotLexFile preserves snapshot support on platforms without Unix mmap by
// reading the immutable image into memory. The returned cleanup matches the
// mmap implementation's contract and is intentionally a no-op.
func mapHotLexFile(file *os.File, size int) ([]byte, func() error, error) {
	data := make([]byte, size)
	if _, err := file.ReadAt(data, 0); err != nil {
		return nil, nil, err
	}
	return data, func() error { return nil }, nil
}
