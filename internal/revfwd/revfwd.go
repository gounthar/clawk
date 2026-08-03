// Package revfwd is the wire protocol for reverse port forwarding: host
// loopback services made reachable on the guest's own loopback.
//
// `clawk forward add` is the outbound half — a guest port bound on the
// host's 127.0.0.1 by gvproxy. This is the inbound half, and it can't ride
// gvproxy: a guest process that dials 127.0.0.1 reaches the guest's own
// loopback, and no host route exists for that. So the guest binds the port
// itself and tunnels each connection to the host over AF_VSOCK.
//
// Shape, per connection (the guest always dials, the host always listens
// on VSockPort):
//
//	guest → host   one JSON Greeting, newline-terminated
//	op=control     host replies with a Snapshot line now and another on
//	               every change to the forward set, until the connection
//	               is closed. This is how the guest learns which ports to
//	               bind, so `clawk forward add-reverse` applies live.
//	op=connect     host validates Port against the current set, replies
//	               with one ConnectReply line, and on ok=true pipes raw
//	               bytes to 127.0.0.1:Port for the rest of the connection.
//
// Validation is host-side on purpose: the guest names a port, never an
// address, and a port the user didn't configure is refused. Otherwise any
// process in the sandbox could reach every service on the Mac's loopback.
//
// JSON lines rather than the binary framing of internal/vsockproto: this
// carries a handful of control messages per connection, not a byte stream
// that needs chunking, and being greppable in a log is worth more here
// than the framing overhead saved.
//
// The guest half is inlined in internal/agentembed/main.go.in (the agent
// builds standalone inside the guest and can't import this package). Any
// change here must be mirrored there — same rule as internal/vsockproto.
package revfwd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// VSockPort is the host-side AF_VSOCK port the guest dials for both
// connection kinds. Disjoint from the other fixed ports: 1024 pty-agent,
// 1025 time-sync, 1026 ssh-agent, 1027 mem-report, 1100+ 9p caches.
const VSockPort uint32 = 1028

// ProtoVersion is the current wire version. Bumped only on a breaking
// change; the guest binary is rebuilt from these sources and re-injected
// on every `clawk up`, so host and guest can't drift within a release.
const ProtoVersion = 1

// Greeting is the first line of every connection.
type Greeting struct {
	// Op is OpControl or OpConnect.
	Op string `json:"op"`

	// V is the sender's ProtoVersion.
	V int `json:"v"`

	// Port is the HOST port to dial. Set for OpConnect only.
	Port int `json:"port,omitempty"`
}

// Greeting Op values.
const (
	OpControl = "control"
	OpConnect = "connect"
)

// Snapshot is the host's reply on an OpControl connection: the complete
// set of reverse forwards as of now. Every update resends the full set —
// the guest reconciles against it rather than applying deltas, so a
// dropped-and-redialed control connection converges the same way.
type Snapshot struct {
	Forwards []Forward `json:"forwards"`
}

// Forward is one host-loopback port exposed on the guest's loopback.
type Forward struct {
	// GuestPort is bound on 127.0.0.1 inside the guest.
	GuestPort int `json:"guest"`

	// HostPort is dialed on 127.0.0.1 on the host.
	HostPort int `json:"host"`
}

// ConnectReply is the host's verdict on an OpConnect greeting. Bytes flow
// only after ok=true; on ok=false the host closes the connection.
type ConnectReply struct {
	OK bool `json:"ok"`
	// Error is a short human-readable reason when OK is false. The guest
	// logs it — it's the only place a misconfigured port surfaces.
	Error string `json:"error,omitempty"`
}

// MaxLineBytes caps one control line. Snapshots are a few dozen bytes per
// forward; the cap exists so a peer can't make the reader allocate without
// bound.
const MaxLineBytes = 64 * 1024

// ErrLineTooLong reports a control line past MaxLineBytes. The connection
// must be closed — the reader is out of sync with the framing.
var ErrLineTooLong = errors.New("revfwd: control line exceeds MaxLineBytes")

// WriteLine JSON-encodes v and writes it as one newline-terminated line.
func WriteLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("revfwd: encoding %T: %w", v, err)
	}
	if len(b)+1 > MaxLineBytes {
		return ErrLineTooLong
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ReadLine reads one newline-terminated JSON line into v.
//
// It takes a *bufio.Reader rather than an io.Reader because the same
// connection carries raw proxied bytes right after the handshake: the
// caller must keep reading through this reader, or bytes buffered past the
// newline are lost.
func ReadLine(r *bufio.Reader, v any) error {
	line, err := r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return ErrLineTooLong
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(line, v); err != nil {
		return fmt.Errorf("revfwd: decoding %T: %w", v, err)
	}
	return nil
}

// NewReader wraps c with a reader sized for MaxLineBytes, so ReadLine's
// buffer-full case really does mean "line too long" rather than "buffer
// too small".
func NewReader(r io.Reader) *bufio.Reader { return bufio.NewReaderSize(r, MaxLineBytes) }
