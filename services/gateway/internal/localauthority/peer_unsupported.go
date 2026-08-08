//go:build !darwin && !linux

package localauthority

import (
	"net"
)

func systemPeerAuthenticator(uint32, PeerMapper) peerAuthenticator {
	return func(net.Conn) (PeerContext, error) { return PeerContext{}, ErrPeerDenied }
}
