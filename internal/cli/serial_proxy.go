package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/serialfwd"
	"github.com/clawkwork/clawk/internal/serialport"
	"github.com/clawkwork/clawk/machine"
)

// serialProxy is the host end of serial forwarding: it publishes the
// sandbox's serial devices to the in-guest agent and, for as long as a
// guest process holds the matching PTY open, bridges bytes to and from the
// physical port. See internal/serialfwd for the wire protocol and why the
// guest is the end that dials.
//
// It is a near-twin of reverseProxy — same lifecycle, same subscribe/
// snapshot machinery, same reason for existing before the VM does. The
// bridging half is where they part company: a reverse forward hands off two
// sockets and copies until one ends, while an attachment owns a tty whose
// line configuration changes under it mid-stream.
type serialProxy struct {
	logger *log.Logger

	ctx      context.Context
	cancel   context.CancelFunc
	listener net.Listener
	wg       sync.WaitGroup

	mu      sync.Mutex
	devices []config.SerialDevice
	// subs are the live control connections' wakeup channels. Each is
	// buffered by one and written non-blockingly, so a burst of edits
	// collapses into a single resend of the (complete) current set.
	subs map[chan struct{}]struct{}
	// busy is the set of device names with an attachment in flight. A
	// serial port takes exactly one reader: a second attach has to be
	// refused rather than queued, or two guest processes would silently
	// steal each other's bytes.
	busy map[string]bool
}

func newSerialProxy(logger *log.Logger) *serialProxy {
	return &serialProxy{
		logger: logger,
		subs:   make(map[chan struct{}]struct{}),
		busy:   make(map[string]bool),
	}
}

// Set replaces the published device set and wakes every connected guest.
func (p *serialProxy) Set(devices []config.SerialDevice) {
	p.mu.Lock()
	p.devices = slices.Clone(devices)
	for sub := range p.subs {
		select {
		case sub <- struct{}{}:
		default: // already pending — it will read the latest set anyway
		}
	}
	p.mu.Unlock()
}

// Start begins accepting guest connections on serialfwd.VSockPort. It is a
// no-op (with one log line) on a backend that can't accept guest-initiated
// vsock connections, matching reverseProxy.Start.
func (p *serialProxy) Start(ctx context.Context, m machine.Machine) error {
	listener, ok := m.(machine.VSockListener)
	if !ok {
		p.logger.Printf("serial: backend doesn't expose VSockListen; not starting")
		return nil
	}
	pCtx, cancel := context.WithCancel(ctx)
	l, err := listener.VSockListen(pCtx, serialfwd.VSockPort)
	if err != nil {
		cancel()
		return fmt.Errorf("vsock listen port=%d: %w", serialfwd.VSockPort, err)
	}
	p.serve(pCtx, cancel, l)

	p.mu.Lock()
	n := len(p.devices)
	p.mu.Unlock()
	p.logger.Printf("serial: listening on guest vsock port %d (%d device(s))",
		serialfwd.VSockPort, n)
	return nil
}

// serve takes ownership of l and starts accepting. Split out from Start so
// the protocol can be exercised over any net.Listener — see reverseProxy.
func (p *serialProxy) serve(ctx context.Context, cancel context.CancelFunc, l net.Listener) {
	p.ctx, p.cancel, p.listener = ctx, cancel, l
	p.wg.Add(1)
	go p.acceptLoop()
}

// Stop closes the listener and waits for in-flight attachments. Idempotent,
// and safe on a proxy that was never started.
func (p *serialProxy) Stop() {
	if p == nil || p.listener == nil {
		return
	}
	p.cancel()
	_ = p.listener.Close()
	p.wg.Wait()
}

func (p *serialProxy) acceptLoop() {
	defer p.wg.Done()
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || p.ctx.Err() != nil {
				return
			}
			p.logger.Printf("serial: accept: %v", err)
			return
		}
		p.wg.Add(1)
		go p.handle(conn)
	}
}

// handle reads the greeting and dispatches to the control stream or a
// device attachment.
func (p *serialProxy) handle(conn net.Conn) {
	defer p.wg.Done()
	defer conn.Close()

	// Shutdown has to reach an attachment that is simply idle — a serial
	// monitor on a quiet board sends nothing for hours, and Stop waits on
	// this goroutine. Closing the conn from the context unblocks it.
	defer context.AfterFunc(p.ctx, func() { _ = conn.Close() })()

	r := serialfwd.NewReader(conn)
	var g serialfwd.Greeting
	if err := serialfwd.ReadLine(r, &g); err != nil {
		if !errors.Is(err, io.EOF) {
			p.logger.Printf("serial: reading greeting: %v", err)
		}
		return
	}
	if g.V != serialfwd.ProtoVersion {
		// The guest agent is re-injected from the host's sources every cold
		// boot, so this only happens across a resumed suspend state — where
		// a down/up is exactly the fix.
		p.logger.Printf("serial: guest speaks protocol v%d, host speaks v%d — "+
			"'clawk down && clawk up' to refresh the guest agent", g.V, serialfwd.ProtoVersion)
		return
	}
	switch g.Op {
	case serialfwd.OpControl:
		p.serveControl(conn, r)
	case serialfwd.OpAttach:
		p.serveAttach(conn, r, g)
	default:
		p.logger.Printf("serial: unknown op %q", g.Op)
	}
}

// serveControl streams the device set to one guest until it disconnects or
// the daemon shuts down. The guest treats every snapshot as the complete
// desired state, so no delta bookkeeping is needed on either side.
func (p *serialProxy) serveControl(conn net.Conn, r *bufio.Reader) {
	updates, unsubscribe := p.subscribe()
	defer unsubscribe()

	// The guest never writes again on a control connection, so a read that
	// returns is the guest going away.
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		_, _ = io.Copy(io.Discard, r)
	}()

	for {
		if err := serialfwd.WriteLine(conn, p.snapshot()); err != nil {
			if p.ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				p.logger.Printf("serial: sending snapshot: %v", err)
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

// openRetryWindow bounds how long an attach waits for a device that isn't
// in /dev yet, and openRetryInterval is how often it looks.
//
// This window is not about patience with an unplugged board — it is the
// re-enumeration gap. A board told to enter its bootloader (the 1200-baud
// touch) drops off the USB bus and comes back a moment later, and the guest
// tool reopens the port straight away. Failing that reopen would break
// every upload to a native-USB board; waiting a couple of seconds makes it
// work. Past the window the attach fails and the guest retries on its own,
// so nothing is lost by keeping it short.
const (
	openRetryWindow   = 3 * time.Second
	openRetryInterval = 100 * time.Millisecond
)

// serveAttach bridges one guest PTY to a physical serial port for as long as
// the guest holds it open.
func (p *serialProxy) serveAttach(conn net.Conn, r *bufio.Reader, g serialfwd.Greeting) {
	dev, ok := p.lookup(g.Name)
	if !ok {
		// Reachable when an edit races an attach the guest already started;
		// also the backstop if a guest ever names a device nobody
		// configured.
		p.logger.Printf("serial: refused attach to %q (not configured)", g.Name)
		p.refuse(conn, fmt.Sprintf("no serial device named %q is forwarded", g.Name))
		return
	}
	if !p.claim(dev.GuestName) {
		p.logger.Printf("serial: refused attach to %q (already attached)", dev.GuestName)
		p.refuse(conn, fmt.Sprintf("serial device %q is already in use", dev.GuestName))
		return
	}
	defer p.release(dev.GuestName)

	mode := serialfwd.DefaultMode()
	if g.Mode != nil {
		mode = *g.Mode
	}

	port, err := p.openWithRetry(dev.HostPath, mode)
	if err != nil {
		p.logger.Printf("serial: attach %s (%s): %v", dev.GuestName, dev.HostPath, err)
		p.refuse(conn, err.Error())
		return
	}
	defer port.Close()

	if err := serialfwd.WriteLine(conn, serialfwd.AttachReply{OK: true}); err != nil {
		return
	}
	p.logger.Printf("serial: %s attached to %s at %s", dev.GuestName, port.Path(), mode)
	defer p.logger.Printf("serial: %s detached from %s", dev.GuestName, port.Path())

	p.pump(conn, r, port, dev.GuestName)
}

// pump moves bytes and mode changes between one guest attachment and one
// open port until either end stops.
//
// The two directions are deliberately asymmetric. Device → guest is only
// ever data, so it gets a plain goroutine that owns the connection's write
// side outright — no lock, because nothing else writes after the handshake.
// Guest → device is a frame loop, because a mode change has to be applied
// in the right place in the byte stream rather than whenever it happens to
// arrive.
func (p *serialProxy) pump(conn net.Conn, r *bufio.Reader, port *serialport.Port, name string) {
	done := make(chan struct{}, 2)

	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 4096)
		for {
			n, err := port.Read(buf)
			if n > 0 {
				if werr := serialfwd.WriteFrame(conn, serialfwd.FrameData, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				// Any read error ends the attachment: the board was
				// unplugged, or Close raced us during teardown. Both mean
				// this port is finished, and the guest reattaches if its
				// client is still there.
				if p.ctx.Err() == nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
					p.logger.Printf("serial: %s: reading %s: %v", name, port.Path(), err)
				}
				return
			}
		}
	}()

	go func() {
		defer func() { done <- struct{}{} }()
		for {
			typ, payload, err := serialfwd.ReadFrame(r)
			if err != nil {
				if p.ctx.Err() == nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
					p.logger.Printf("serial: %s: reading frame: %v", name, err)
				}
				return
			}
			switch typ {
			case serialfwd.FrameData:
				if _, err := port.Write(payload); err != nil {
					p.logger.Printf("serial: %s: writing %s: %v", name, port.Path(), err)
					return
				}
			case serialfwd.FrameMode:
				var mode serialfwd.Mode
				if err := json.Unmarshal(payload, &mode); err != nil {
					p.logger.Printf("serial: %s: bad mode frame: %v", name, err)
					continue
				}
				if err := port.Configure(mode); err != nil {
					// Not fatal: a board that won't do 250000 baud is worth
					// a log line, but tearing the attachment down would
					// lose the session over a setting the tool may not even
					// depend on.
					p.logger.Printf("serial: %s: %v", name, err)
					continue
				}
				p.logger.Printf("serial: %s reconfigured to %s", name, mode)
			default:
				p.logger.Printf("serial: %s: ignoring %s frame", name, typ)
			}
		}
	}()

	// The first direction to finish ends the attachment. Closing the port
	// unblocks the reader goroutine (its fd is registered with the Go
	// poller); closing the conn unblocks the frame loop. Both happen in the
	// deferred cleanup of serveAttach and handle respectively, so returning
	// here is enough.
	<-done
}

// openWithRetry opens the device, tolerating a short absence — see
// openRetryWindow.
func (p *serialProxy) openWithRetry(pattern string, mode serialfwd.Mode) (*serialport.Port, error) {
	deadline := time.Now().Add(openRetryWindow)
	for {
		port, err := serialport.Open(pattern, mode)
		if err == nil {
			return port, nil
		}
		// Only absence is retried. A device that exists but refuses to open
		// — held by the Arduino IDE on the Mac, or a permissions problem —
		// will refuse again in 100ms, and reporting it at once gives the
		// user something to act on.
		if !errors.Is(err, serialport.ErrNoMatch) || time.Now().After(deadline) {
			return nil, err
		}
		select {
		case <-time.After(openRetryInterval):
		case <-p.ctx.Done():
			return nil, err
		}
	}
}

// refuse sends a rejected AttachReply. Errors are dropped: the guest is
// about to see the connection close either way.
func (p *serialProxy) refuse(conn net.Conn, reason string) {
	_ = serialfwd.WriteLine(conn, serialfwd.AttachReply{OK: false, Error: reason})
}

// snapshot renders the current set in wire form. Host paths are left out —
// the guest has no use for them.
func (p *serialProxy) snapshot() serialfwd.Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap := serialfwd.Snapshot{Devices: make([]serialfwd.Device, 0, len(p.devices))}
	for _, d := range p.devices {
		snap.Devices = append(snap.Devices, serialfwd.Device{Name: d.GuestName})
	}
	return snap
}

// lookup resolves a guest-visible name to its configured device. The guest
// names a device and never a path, and an unlisted name is refused —
// otherwise any process in the sandbox could open anything in the Mac's
// /dev.
func (p *serialProxy) lookup(name string) (config.SerialDevice, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.devices {
		if d.GuestName == name {
			return d, true
		}
	}
	return config.SerialDevice{}, false
}

// claim reserves a device for one attachment, reporting false if another
// already holds it.
func (p *serialProxy) claim(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.busy[name] {
		return false
	}
	p.busy[name] = true
	return true
}

func (p *serialProxy) release(name string) {
	p.mu.Lock()
	delete(p.busy, name)
	p.mu.Unlock()
}

func (p *serialProxy) subscribe() (<-chan struct{}, func()) {
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
