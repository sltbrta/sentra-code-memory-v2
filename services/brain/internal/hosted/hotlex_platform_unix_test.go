//go:build unix

package hosted

import (
	"runtime"
	"syscall"
	"testing"
)

func requireHotLexMMap(*testing.T) {}

func peakRSSBytes(t *testing.T) uint64 {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	rss := uint64(usage.Maxrss)
	if runtime.GOOS != "darwin" {
		rss *= 1024
	}
	return rss
}
