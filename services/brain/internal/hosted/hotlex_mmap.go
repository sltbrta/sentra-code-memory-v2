//go:build unix

package hosted

import (
	"os"

	"golang.org/x/sys/unix"
)

func mapHotLexFile(file *os.File, size int) ([]byte, func() error, error) {
	data, err := unix.Mmap(int(file.Fd()), 0, size, unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, nil, err
	}
	return data, func() error { return unix.Munmap(data) }, nil
}
