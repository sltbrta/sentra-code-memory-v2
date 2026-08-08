//go:build darwin || linux

package localauthority

import (
	"net"
)

// unixPeerConnection accepts only the accepted Unix connection or the
// package's own active-limit wrapper around one.
func unixPeerConnection(connection net.Conn) (*net.UnixConn, bool) {
	if unixConnection, ok := connection.(*net.UnixConn); ok && unixConnection != nil {
		return unixConnection, true
	}
	bounded, ok := connection.(*boundedConnection)
	if !ok || bounded == nil {
		return nil, false
	}
	// Authentication may see the package's active-limit wrapper, but must not
	// honor arbitrary unwrapping interfaces that could substitute a descriptor.
	unixConnection, ok := bounded.Conn.(*net.UnixConn)
	return unixConnection, ok && unixConnection != nil
}
