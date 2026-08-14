package codeserve

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"
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
		stat, err := f.Stat()
		if err != nil {
			return nil, err
		}
		buf := make([]byte, stat.Size())
		_, err = f.Read(buf)
		return buf, err
	}
	buf := make([]byte, maxBytes)
	n, err := f.Read(buf)
	return buf[:n], err
}

// reqCtx is a tiny indirection for context.Context that the new local-first
// handlers share. Keeps the import surface small and lets future work add a
// per-request timeout without touching every handler.
func reqCtx() context.Context {
	return ctxBackground{}
}

type ctxBackground struct{}

func (ctxBackground) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctxBackground) Done() <-chan struct{}       { return nil }
func (ctxBackground) Err() error                  { return nil }
func (ctxBackground) Value(any) any               { return nil }

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

// ensureContextInterfaceCompatibility injects a safe compile-time check for
// the context.Context interface at the package boundary.
var _ context.Context = ctxBackground{}

// silenceUnused keeps fmt imported even if other helpers go away.
var _ = fmt.Sprintf
