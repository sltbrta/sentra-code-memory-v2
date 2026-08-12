//go:build !darwin && !linux

package codecrawl

// lockIndexFile is a no-op on platforms without flock support; Save still
// provides tmp+rename atomicity, fsync, parent-directory sync, and the
// in-process mutex, so single-host durability guarantees are unchanged.
func lockIndexFile(gobPath string, exclusive bool) (release func(), err error) {
	return func() {}, nil
}
