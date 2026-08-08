//go:build !darwin && !linux

package localauthority

func ownerUID(osFileInfo) uint32 { return ^uint32(0) }
