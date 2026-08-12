//go:build darwin || linux

package codecrawl

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockIndexFile takes an advisory flock on the durable-index lock sidecar so
// concurrent processes coordinate Save/Load even on filesystems where rename
// atomicity alone is not enough. exclusive selects LOCK_EX (writers) versus
// LOCK_SH (readers). The returned release function unlocks and closes.
func lockIndexFile(gobPath string, exclusive bool) (release func(), err error) {
	f, err := os.OpenFile(gobPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("codecrawl: open index lock: %w", err)
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("codecrawl: acquire index lock: %w", err)
	}
	return func() {
		unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		closeErr := f.Close()
		_ = errors.Join(unlockErr, closeErr)
	}, nil
}
