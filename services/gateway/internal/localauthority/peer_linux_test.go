//go:build linux

package localauthority

import (
	"errors"
	"net"
	"path/filepath"
	"testing"

	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

func TestSystemPeerAuthenticatorHandlesBoundedLinuxUnixPeer(t *testing.T) {
	t.Parallel()
	bounded := newLinuxBoundedListener(t)
	expected := testCredentials()
	connection, client := acceptLinuxTestPeer(t, bounded)
	defer client.Close()
	peerConnection, err := authenticateConnection(
		connection,
		systemPeerAuthenticator(expected.UID, exactLinuxPeerMapper(expected)),
	)
	if err != nil {
		t.Fatalf("authenticate bounded Unix peer: %v", err)
	}
	authenticated, ok := peerConnection.(*authenticatedConnection)
	if !ok || authenticated.peer.Credentials != expected || authenticated.peer.Identity != mappedIdentity(expected) {
		t.Fatalf("authenticated peer = %#v", peerConnection)
	}
	if err := peerConnection.Close(); err != nil {
		t.Fatal(err)
	}
	if len(bounded.active) != 0 {
		t.Fatal("closing authenticated connection did not release active bound")
	}
}

func TestSystemPeerAuthenticatorRejectsWrongLinuxIdentity(t *testing.T) {
	t.Parallel()
	bounded := newLinuxBoundedListener(t)
	expected := testCredentials()
	connection, client := acceptLinuxTestPeer(t, bounded)
	defer client.Close()
	wrongUID := expected.UID ^ 1
	if _, err := authenticateConnection(
		connection,
		systemPeerAuthenticator(wrongUID, exactLinuxPeerMapper(expected)),
	); !errors.Is(err, ErrPeerDenied) {
		t.Fatalf("wrong-UID authentication error = %v", err)
	}
	if len(bounded.active) != 0 {
		t.Fatal("denied connection did not release active bound")
	}
}

func TestSystemPeerAuthenticatorRejectsArbitraryConnectionWrapper(t *testing.T) {
	t.Parallel()
	bounded := newLinuxBoundedListener(t)
	expected := testCredentials()
	connection, client := acceptLinuxTestPeer(t, bounded)
	defer client.Close()
	if _, err := authenticateConnection(
		&countingConnection{Conn: connection},
		systemPeerAuthenticator(expected.UID, exactLinuxPeerMapper(expected)),
	); !errors.Is(err, ErrPeerDenied) {
		t.Fatalf("arbitrarily wrapped connection error = %v", err)
	}
	if len(bounded.active) != 0 {
		t.Fatal("rejected wrapper did not release active bound")
	}
}

func newLinuxBoundedListener(t *testing.T) *boundedListener {
	t.Helper()
	listener, err := net.Listen("unix", filepath.Join(secureSocketDirectory(t), "peer.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close Linux peer listener: %v", err)
		}
	})
	return newBoundedListener(listener, 1).(*boundedListener)
}

func exactLinuxPeerMapper(expected PeerCredentials) PeerMapper {
	return PeerMapperFunc(func(credentials PeerCredentials) (shared.MappedIdentityFact, error) {
		if credentials != expected {
			return shared.MappedIdentityFact{}, ErrPeerDenied
		}
		return mappedIdentity(credentials), nil
	})
}

func acceptLinuxTestPeer(t *testing.T, listener net.Listener) (net.Conn, net.Conn) {
	t.Helper()
	client, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	connection, err := listener.Accept()
	if err != nil {
		_ = client.Close()
		t.Fatal(err)
	}
	return connection, client
}
