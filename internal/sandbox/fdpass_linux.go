//go:build linux

// SCM_RIGHTS fd passing between the daemon and its namespace anchor.
//
// A file descriptor sent this way is the same open file in the receiver, with
// none of the sender's namespace attached to it — which is the whole trick
// behind rootless mode: the anchor creates the TAP where it has
// CAP_NET_ADMIN, and the daemon reads and writes frames on it from the host
// network namespace, where gvproxy lives.

package sandbox

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// handshakeFD is where the anchor finds its end of the socketpair: exec'd
// children see cmd.ExtraFiles[0] as fd 3.
const handshakeFD = 3

// handshakePair returns the two ends of a unix stream socket: the parent's as
// a *net.UnixConn (so it has deadlines and ReadMsgUnix), the child's as an
// *os.File to hand to exec.Cmd.ExtraFiles.
func handshakePair() (*net.UnixConn, *os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("socketpair: %w", err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "netns-handshake")
	// FileConn dups the fd and registers the copy with the runtime poller,
	// which is what makes read deadlines work; our original is then surplus.
	conn, err := net.FileConn(parentFile)
	_ = parentFile.Close()
	if err != nil {
		_ = unix.Close(fds[1])
		return nil, nil, fmt.Errorf("wrapping handshake socket: %w", err)
	}
	uconn, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		_ = unix.Close(fds[1])
		return nil, nil, fmt.Errorf("handshake socket is %T, want *net.UnixConn", conn)
	}
	return uconn, os.NewFile(uintptr(fds[1]), "netns-handshake-child"), nil
}

// sendFD sends one file descriptor plus a one-byte status over the raw socket
// fd. The byte matters: SCM_RIGHTS needs at least one byte of ordinary
// payload to ride along, and it doubles as the anchor's "topology is up"
// signal.
func sendFD(sockFD, fd int, status byte) error {
	if err := unix.Sendmsg(sockFD, []byte{status}, unix.UnixRights(fd), nil, 0); err != nil {
		return fmt.Errorf("sendmsg: %w", err)
	}
	return nil
}

// recvFD waits up to timeout for one descriptor sent by sendFD.
//
// The timeout is what turns "the anchor wedged before sending anything" into a
// prompt error instead of a hang; an anchor that died outright closes the
// socket, which surfaces as EOF.
func recvFD(conn *net.UnixConn, timeout time.Duration) (int, error) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return -1, fmt.Errorf("setting handshake deadline: %w", err)
	}
	buf := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4)) // exactly one fd
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return -1, fmt.Errorf("reading the namespace handshake: %w", err)
	}
	if n == 0 && oobn == 0 {
		return -1, errors.New("the namespace helper exited without handing over its tap")
	}

	scms, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return -1, fmt.Errorf("parsing control message: %w", err)
	}
	for _, scm := range scms {
		fds, err := unix.ParseUnixRights(&scm)
		if err != nil {
			continue
		}
		if len(fds) > 0 {
			// Close any extras rather than leak them; we asked for one.
			for _, extra := range fds[1:] {
				_ = unix.Close(extra)
			}
			return fds[0], nil
		}
	}
	return -1, errors.New("the namespace handshake carried no file descriptor")
}
