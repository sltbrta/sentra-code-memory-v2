package codeserve

import (
	"io"
	"os"
	"strconv"
)

// readFileBounded reads up to maxBytes from path. A maxBytes of 0 returns the
// whole file (intended for tests; production callers should pass a bound).
func readFileBounded(path string, maxBytes int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if maxBytes <= 0 {
		return io.ReadAll(f)
	}
	return io.ReadAll(io.LimitReader(f, int64(maxBytes)))
}

// fmtParseInt parses a decimal int with bounds checking. Returns 0 if the
// string is empty or unparseable so callers fall back to a default.
func fmtParseInt(s string, out *int) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	*out = n
	return n, nil
}
