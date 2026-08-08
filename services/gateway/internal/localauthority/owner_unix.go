//go:build darwin || linux

package localauthority

import "syscall"

func ownerUID(info osFileInfo) uint32 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ^uint32(0)
	}
	return stat.Uid
}
