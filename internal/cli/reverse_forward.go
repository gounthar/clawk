package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/revfwd"
	"github.com/clawkwork/clawk/machine"
)

// reverseProxy is the host end of reverse port forwarding: it publishes the
// sandbox's reverse-forward set to the in-guest agent and bridges each
// connection the guest opens through to a host loopback service. See
// internal/revfwd for the wire protocol and why gvproxy can't do this.
//
// It is built before the VM exists (the control socket needs something to
// push reloads at) and starts listening once Start is handed a machine. Set
// may be called at any point in that lifecycle; guests connected at the time
// see the new set immediately, and one that connects later reads it from the
// first snapshot.
type reverseProxy struct {
	logger *log.Logger

	ctx      context.Context
	cancel   context.CancelFunc
	listener net.Listener
	wg       sync.WaitGroup

	mu       sync.Mutex
	forwards []config.PortForward
	// subs are the live control connections' wakeup channels. Each is
	// buffered by one and written non-blockingly, so a burst of edits
	// collapses into a single resend of the (complete) current set.
	subs map[chan struct{}]struct{}
}

func newReverseProxy(logger *log.Logger) *reverseProxy {
	return &reverseProxy{logger: logger, subs: make(map[chan struct{}]struct{})}
}

// Set replaces the published forward set and wakes every connected guest.
func (p *reverseProxy) Set(fwds []config.PortForward) {
	p.mu.Lock()
	p.forwards = slices.Clone(fwds)
	for sub := range p.subs {
		select {
		case sub <- struct{}{}:
		default: // already pending — it will read the latest set anyway
		}
	}
	p.mu.Unlock()
}

// Start begins accepting guest connections on revfwd.VSockPort. It is a
// no-op (with one log line) on a backend that can't accept guest-initiated
// vsock connections, matching startSSHAgentProxy.
func (p *reverseProxy) Start(ctx context.Context, m machine.Machine) error {
	listener, ok := m.(machine.VSockListener)
	if !ok {
		p.logger.Printf("reverse-forward: backend doesn't expose VSockListen; not starting")
		return nil
	}
	pCtx, cancel := context.WithCancel(ctx)
	l, err := listener.VSockListen(pCtx, revfwd.VSockPort)
	if err != nil {
		cancel()
		return fmt.Errorf("vsock listen port=%d: %w", revfwd.VSockPort, err)
	}
	p.serve(pCtx, cancel, l)

	p.mu.Lock()
	n := len(p.forwards)
	p.mu.Unlock()
	p.logger.Printf("reverse-forward: listening on guest vsock port %d (%d forward(s))",
		revfwd.VSockPort, n)
	return nil
}

// serve takes ownership of l and starts accepting. Split out from Start so
// the protocol can be exercised over any net.Listener — the vsock listener
// only exists inside a running VM, and everything interesting about this
// type is what it does with the connections, not where they came from.
func (p *reverseProxy) serve(ctx context.Context, cancel context.CancelFunc, l net.Listener) {
	p.ctx, p.cancel, p.listener = ctx, cancel, l
	p.wg.Add(1)
	go p.acceptLoop()
}

// Stop closes the listener and waits for in-flight forwards. Idempotent, and
// safe on a proxy that was never started.
func (p *reverseProxy) Stop() {
	if p == nil || p.listener == nil {
		return
	}
	p.cancel()
	_ = p.listener.Close()
	p.wg.Wait()
}

func (p *reverseProxy) acceptLoop() {
	defer p.wg.Done()
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || p.ctx.Err() != nil {
				return
			}
			p.logger.Printf("reverse-forward: accept: %v", err)
			return
		}
		p.wg.Add(1)
		go p.handle(conn)
	}
}

// handle reads the greeting and dispatches to the control stream or a
// one-shot connection bridge.
func (p *reverseProxy) handle(conn net.Conn) {
	defer p.wg.Done()
	defer conn.Close()

	// Shutdown has to reach connections that are simply idle — a bridged
	// websocket can sit silent for hours, and Stop waits on this goroutine.
	// Closing the conn from the context unblocks whatever it is parked in.
	defer context.AfterFunc(p.ctx, func() { _ = conn.Close() })()

	r := revfwd.NewReader(conn)
	var g revfwd.Greeting
	if err := revfwd.ReadLine(r, &g); err != nil {
		if !errors.Is(err, io.EOF) {
			p.logger.Printf("reverse-forward: reading greeting: %v", err)
		}
		return
	}
	if g.V != revfwd.ProtoVersion {
		// The guest agent is re-injected from the host's sources every cold
		// boot, so this only happens across a resumed suspend state — where
		// a down/up is exactly the fix.
		p.logger.Printf("reverse-forward: guest speaks protocol v%d, host speaks v%d — "+
			"'clawk down && clawk up' to refresh the guest agent", g.V, revfwd.ProtoVersion)
		return
	}
	switch g.Op {
	case revfwd.OpControl:
		p.serveControl(conn, r)
	case revfwd.OpConnect:
		p.serveConnect(conn, r, g.Port)
	default:
		p.logger.Printf("reverse-forward: unknown op %q", g.Op)
	}
}

// serveControl streams the forward set to one guest until it disconnects or
// the daemon shuts down. The guest treats every snapshot as the complete
// desired state, so no delta bookkeeping is needed on either side.
func (p *reverseProxy) serveControl(conn net.Conn, r *bufio.Reader) {
	updates, unsubscribe := p.subscribe()
	defer unsubscribe()

	// The guest never writes again on a control connection, so a read that
	// returns is the guest going away. Without this the daemon would keep
	// a dead subscriber until the next edit tried to write to it.
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		_, _ = io.Copy(io.Discard, r)
	}()

	for {
		if err := revfwd.WriteLine(conn, p.snapshot()); err != nil {
			if p.ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				p.logger.Printf("reverse-forward: sending snapshot: %v", err)
			}
			return
		}
		select {
		case <-updates:
		case <-gone:
			return
		case <-p.ctx.Done():
			return
		}
	}
}

// serveConnect bridges one guest-initiated connection to a host loopback
// service, after checking the requested port is actually configured.
func (p *reverseProxy) serveConnect(conn net.Conn, r *bufio.Reader, hostPort int) {
	if !p.allowed(hostPort) {
		// Reachable when an edit races a connection the guest already
		// accepted; also the backstop if a guest ever asks for a port
		// nobody configured.
		p.logger.Printf("reverse-forward: refused connect to host port %d (not configured)", hostPort)
		_ = revfwd.WriteLine(conn, revfwd.ConnectReply{
			OK:    false,
			Error: fmt.Sprintf("host port %d is not reverse-forwarded", hostPort),
		})
		return
	}
	host, err := dialHostLoopback(hostPort)
	if err != nil {
		p.logger.Printf("reverse-forward: dial host port %d: %v", hostPort, err)
		_ = revfwd.WriteLine(conn, revfwd.ConnectReply{
			OK: false, Error: fmt.Sprintf("dial host port %d: %v", hostPort, err),
		})
		return
	}
	defer host.Close()
	if err := revfwd.WriteLine(conn, revfwd.ConnectReply{OK: true}); err != nil {
		return
	}

	// Guest → host reads through r, not conn: the handshake reader may hold
	// bytes the guest pipelined behind its greeting, and reading the raw
	// conn would drop them.
	//
	// The first direction to end tears down both, rather than half-closing
	// and waiting for the other. Half-close isn't available: vz hands us a
	// *VirtioSocketConnection, which implements net.Conn but no CloseWrite,
	// so a shutdown on this side would never reach the guest and the other
	// copy would block until something timed out. Same shape as the
	// ssh-agent forwarder on either end of the same transport.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(host, r); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, host); done <- struct{}{} }()
	<-done
}

// hostLoopbackAddrs are the addresses a reverse forward's host port is
// tried on, in order.
//
// Both families, because "localhost" is not one address. On macOS it
// resolves to ::1 first, and a server told to bind the *name* usually ends
// up on the IPv6 loopback alone — `python3 -m http.server --bind localhost`
// takes the first getaddrinfo result and binds [::1] only; much Node
// tooling lands the same way. Dialling 127.0.0.1 alone makes every one of
// those servers look like nothing is listening, which surfaces in the guest
// as a connection that is accepted and immediately reset.
//
// IPv4 first because it's still where most things bind, and a refused
// connect on loopback costs microseconds.
var hostLoopbackAddrs = []string{"127.0.0.1", "::1"}

// hostLoopbackDialTimeout bounds one attempt. Loopback either answers or
// refuses at once; the timeout only matters so a filtered address can't
// strand the connection before the other family is tried.
const hostLoopbackDialTimeout = 5 * time.Second

// dialHostLoopback connects to port on the host's loopback, trying each
// address family. Returns the last failure when none answer — for the
// common "nothing is listening" case every attempt reports the same
// connection-refused, so the last one is as good as a joined list and
// reads better in the guest's log.
func dialHostLoopback(port int) (net.Conn, error) {
	d := net.Dialer{Timeout: hostLoopbackDialTimeout}
	var lastErr error
	for _, host := range hostLoopbackAddrs {
		conn, err := d.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// snapshot renders the current set in wire form.
func (p *reverseProxy) snapshot() revfwd.Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap := revfwd.Snapshot{Forwards: make([]revfwd.Forward, 0, len(p.forwards))}
	for _, f := range p.forwards {
		snap.Forwards = append(snap.Forwards, revfwd.Forward{
			GuestPort: f.GuestPort, HostPort: f.HostPort,
		})
	}
	return snap
}

// allowed reports whether hostPort is in the current set. The guest names a
// port and never an address, and an unlisted one is refused — otherwise any
// process in the sandbox could reach every service on the Mac's loopback.
func (p *reverseProxy) allowed(hostPort int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.forwards {
		if f.HostPort == hostPort {
			return true
		}
	}
	return false
}

func (p *reverseProxy) subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	p.mu.Lock()
	p.subs[ch] = struct{}{}
	p.mu.Unlock()
	return ch, func() {
		p.mu.Lock()
		delete(p.subs, ch)
		p.mu.Unlock()
	}
}
