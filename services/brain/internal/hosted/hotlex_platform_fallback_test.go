//go:build !unix

package hosted

import "testing"

func requireHotLexMMap(t *testing.T) {
	t.Helper()
	t.Skip("mmap memory and RSS evidence is Unix-only")
}

// peakRSSBytes keeps the evidence helper buildable on non-Unix targets. The
// calling tests are skipped by requireHotLexMMap before reaching it.
func peakRSSBytes(*testing.T) uint64 { return 0 }
