//go:build linux

package localauthority

import (
	"net"
	"syscall"
	"unsafe"
)

// ucred mirrors the Linux kernel structure returned by SO_PEERCRED.
type ucred struct {
	Pid int32
	Uid uint32
	Gid uint32
}

func systemPeerAuthenticator(expectedUID uint32, mapper PeerMapper) peerAuthenticator {
	return func(connection net.Conn) (PeerContext, error) {
		unixConnection, ok := unixPeerConnection(connection)
		if !ok {
			return PeerContext{}, ErrPeerDenied
		}
		raw, err := unixConnection.SyscallConn()
		if err != nil {
			return PeerContext{}, ErrPeerDenied
		}
		var credentials PeerCredentials
		var credentialErr error
		controlErr := raw.Control(func(fileDescriptor uintptr) {
			cred := ucred{}
			length := uint32(unsafe.Sizeof(cred))
			_, _, errno := syscall.Syscall6(
				syscall.SYS_GETSOCKOPT,
				fileDescriptor,
				syscall.SOL_SOCKET,
				syscall.SO_PEERCRED,
				uintptr(unsafe.Pointer(&cred)),
				uintptr(unsafe.Pointer(&length)),
				0,
			)
			if errno != 0 {
				credentialErr = errno
				return
			}
			if length != uint32(unsafe.Sizeof(cred)) || cred.Pid <= 0 {
				credentialErr = syscall.EINVAL
				return
			}
			credentials = PeerCredentials{UID: cred.Uid, GID: cred.Gid, PID: uint32(cred.Pid)}
		})
		if controlErr != nil || credentialErr != nil || credentials.UID != expectedUID {
			return PeerContext{}, ErrPeerDenied
		}
		identity, err := mapper.MapPeer(credentials)
		if err != nil {
			return PeerContext{}, ErrPeerDenied
		}
		peer := PeerContext{Credentials: credentials, Identity: identity}
		if !validMappedIdentity(peer.Identity) {
			return PeerContext{}, ErrPeerDenied
		}
		return peer, nil
	}
}
