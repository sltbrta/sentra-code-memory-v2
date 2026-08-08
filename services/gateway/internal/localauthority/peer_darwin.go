//go:build darwin

package localauthority

import (
	"net"
	"syscall"
	"unsafe"
)

const (
	solLocal      = 0
	localPeerCred = 1
	localPeerPID  = 2
)

type xucred struct {
	Version uint32
	UID     uint32
	GroupsN int16
	Groups  [16]uint32
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
			cred := xucred{}
			length := uint32(unsafe.Sizeof(cred))
			_, _, errno := syscall.Syscall6(
				syscall.SYS_GETSOCKOPT,
				fileDescriptor,
				solLocal,
				localPeerCred,
				uintptr(unsafe.Pointer(&cred)),
				uintptr(unsafe.Pointer(&length)),
				0,
			)
			if errno != 0 {
				credentialErr = errno
				return
			}
			if length != uint32(unsafe.Sizeof(cred)) || cred.GroupsN < 1 {
				credentialErr = syscall.EINVAL
				return
			}
			pid, err := syscall.GetsockoptInt(int(fileDescriptor), solLocal, localPeerPID)
			if err != nil {
				credentialErr = err
				return
			}
			credentials = PeerCredentials{UID: cred.UID, GID: cred.Groups[0], PID: uint32(pid)}
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
