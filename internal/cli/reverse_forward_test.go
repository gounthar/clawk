package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/revfwd"
	"github.com/stretchr/testify/require"
)

// startTestProxy runs a reverseProxy over a loopback TCP listener instead of
// vsock (which only exists inside a running VM) and returns its address. The
// protocol is transport-agnostic, so everything below exercises the real
// accept/handshake/bridge path.
func startTestProxy(t *testing.T, forwards ...config.PortForward) (*reverseProxy, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	p := newReverseProxy(log.New(io.Discard, "", 0))
	p.Set(forwards)
	ctx, cancel := context.WithCancel(context.Background())
	p.serve(ctx, cancel, ln)
	t.Cleanup(p.Stop)
	return p, ln.Addr().String()
}

// dialProxy opens a connection and sends one greeting, returning the
// connection plus the reader that must be used for everything after it.
func dialProxy(t *testing.T, addr string, g revfwd.Greeting) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, revfwd.WriteLine(conn, g))
	return conn, revfwd.NewReader(conn)
}

// echoServer stands in for a service bound to the host's loopback.
func echoServer(t *testing.T) int {
	t.Helper()
	return echoServerOn(t, "127.0.0.1:0")
}

func echoServerOn(t *testing.T, addr string) int {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// A control connection gets the current set immediately, then a fresh full
// set on every edit — that push is what makes `forward add-reverse` apply
// without a reboot.
func TestReverseProxyControlStreamPushesUpdates(t *testing.T) {
	p, addr := startTestProxy(t, config.PortForward{HostPort: 5432, GuestPort: 15432})
	_, r := dialProxy(t, addr, revfwd.Greeting{Op: revfwd.OpControl, V: revfwd.ProtoVersion})

	var snap revfwd.Snapshot
	require.NoError(t, revfwd.ReadLine(r, &snap))
	require.Equal(t, []revfwd.Forward{{GuestPort: 15432, HostPort: 5432}}, snap.Forwards)

	p.Set([]config.PortForward{
		{HostPort: 5432, GuestPort: 15432},
		{HostPort: 63342, GuestPort: 63342},
	})
	require.NoError(t, revfwd.ReadLine(r, &snap))
	require.Equal(t, []revfwd.Forward{
		{GuestPort: 15432, HostPort: 5432},
		{GuestPort: 63342, HostPort: 63342},
	}, snap.Forwards)

	// Removals travel the same way: the set is absolute, never a delta.
	p.Set(nil)
	require.NoError(t, revfwd.ReadLine(r, &snap))
	require.Empty(t, snap.Forwards)
}

func TestReverseProxyBridgesConfiguredPort(t *testing.T) {
	port := echoServer(t)
	_, addr := startTestProxy(t, config.PortForward{HostPort: port, GuestPort: 9999})

	conn, r := dialProxy(t, addr,
		revfwd.Greeting{Op: revfwd.OpConnect, V: revfwd.ProtoVersion, Port: port})
	var reply revfwd.ConnectReply
	require.NoError(t, revfwd.ReadLine(r, &reply))
	require.True(t, reply.OK, "reply: %+v", reply)

	_, err := conn.Write([]byte("ping\n"))
	require.NoError(t, err)
	got, err := r.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ping\n", got)
}

// A host service on the IPv6 loopback alone must still be reachable.
// "localhost" resolves to ::1 first on macOS, so binding the name — which
// is what `python3 -m http.server --bind localhost` and much Node tooling
// do — often means [::1] and nothing on 127.0.0.1. Dialling only IPv4 made
// those look dead: the guest accepted the connection, the host dial was
// refused, and curl reported `(56) Recv failure: Connection reset by peer`.
func TestReverseProxyBridgesIPv6OnlyHostService(t *testing.T) {
	port := echoServerOn(t, "[::1]:0")
	// Precondition for the regression: the service really is invisible over
	// IPv4, so this test fails if the fix is reverted.
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err == nil {
		c.Close()
		t.Skip("something else holds the IPv4 loopback on this port; can't isolate the case")
	}

	_, addr := startTestProxy(t, config.PortForward{HostPort: port, GuestPort: 9999})
	conn, r := dialProxy(t, addr,
		revfwd.Greeting{Op: revfwd.OpConnect, V: revfwd.ProtoVersion, Port: port})
	var reply revfwd.ConnectReply
	require.NoError(t, revfwd.ReadLine(r, &reply))
	require.True(t, reply.OK, "IPv6-only host service unreachable: %+v", reply)

	_, err = conn.Write([]byte("ping\n"))
	require.NoError(t, err)
	got, err := r.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ping\n", got)
}

// The guest names a port and nothing else, and one that isn't configured is
// refused — otherwise anything in the sandbox could reach every service on
// the host's loopback.
func TestReverseProxyRefusesUnconfiguredPort(t *testing.T) {
	port := echoServer(t)
	_, addr := startTestProxy(t) // nothing configured

	_, r := dialProxy(t, addr,
		revfwd.Greeting{Op: revfwd.OpConnect, V: revfwd.ProtoVersion, Port: port})
	var reply revfwd.ConnectReply
	require.NoError(t, revfwd.ReadLine(r, &reply))
	require.False(t, reply.OK)
	require.Contains(t, reply.Error, "not reverse-forwarded")
}

// Only the HOST port authorises a connection; a guest port that happens to
// match some other mapping's host port must not open a hole.
func TestReverseProxyRefusesGuestPortAsHostPort(t *testing.T) {
	port := echoServer(t)
	_, addr := startTestProxy(t, config.PortForward{HostPort: 1, GuestPort: port})

	_, r := dialProxy(t, addr,
		revfwd.Greeting{Op: revfwd.OpConnect, V: revfwd.ProtoVersion, Port: port})
	var reply revfwd.ConnectReply
	require.NoError(t, revfwd.ReadLine(r, &reply))
	require.False(t, reply.OK)
}

// A configured port whose service isn't up must fail the connection rather
// than hang the caller.
func TestReverseProxyReportsDialFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	dead := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	_, addr := startTestProxy(t, config.PortForward{HostPort: dead, GuestPort: dead})
	_, r := dialProxy(t, addr,
		revfwd.Greeting{Op: revfwd.OpConnect, V: revfwd.ProtoVersion, Port: dead})
	var reply revfwd.ConnectReply
	require.NoError(t, revfwd.ReadLine(r, &reply))
	require.False(t, reply.OK)
	require.Contains(t, reply.Error, fmt.Sprintf("dial host port %d", dead))
	require.Contains(t, reply.Error, "connection refused")
}

// A guest agent from a different protocol generation is hung up on, not
// guessed at.
func TestReverseProxyRejectsVersionMismatch(t *testing.T) {
	_, addr := startTestProxy(t, config.PortForward{HostPort: 5432, GuestPort: 5432})
	_, r := dialProxy(t, addr,
		revfwd.Greeting{Op: revfwd.OpControl, V: revfwd.ProtoVersion + 1})
	var snap revfwd.Snapshot
	require.ErrorIs(t, revfwd.ReadLine(r, &snap), io.EOF)
}

// Stop must return even with a control connection parked on an idle stream —
// the daemon's shutdown waits on it.
func TestReverseProxyStopClosesIdleConnections(t *testing.T) {
	p, addr := startTestProxy(t, config.PortForward{HostPort: 5432, GuestPort: 5432})
	_, r := dialProxy(t, addr, revfwd.Greeting{Op: revfwd.OpControl, V: revfwd.ProtoVersion})
	var snap revfwd.Snapshot
	require.NoError(t, revfwd.ReadLine(r, &snap))

	done := make(chan struct{})
	go func() { defer close(done); p.Stop() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop blocked on an idle control connection")
	}
}
