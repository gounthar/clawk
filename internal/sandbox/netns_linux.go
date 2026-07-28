//go:build linux

// Rootless per-sandbox networking: one unprivileged user+network namespace
// per VM, owning the bridge and the two TAPs.
//
// Creating a network device needs CAP_NET_ADMIN. Linux hands that out for
// free inside a user namespace you own — so instead of asking sudo for `ip`,
// clawk forks a child into CLONE_NEWUSER|CLONE_NEWNET, where it is root over
// its own private network, and builds the topology there with netlink. The
// result on a working host is a sandbox that boots with no privileged
// operation at all, and a host whose interface list stays empty.
//
//	host netns (the __fcd daemon):        sandbox netns (the anchor):
//	  gvproxy ── pump ── gv0 fd ────────────► gv0 ─┐
//	                     (SCM_RIGHTS)              ├── br0
//	  firecracker (via nsenter) ───────────► tap0 ─┘
//
// gvproxy stays in the host namespace on purpose: its whole job is egress
// through host sockets. Only the VM's NIC moves. A TAP fd is namespace-
// agnostic once open, so the pump reaches into the sandbox's L2 with nothing
// more than the fd the anchor sends it — while the hypervisor, which can only
// name a TAP, is launched inside the namespace (see nsenterExecPrefix).
//
// Why an anchor process rather than a named namespace: an unnamed netns lives
// exactly as long as something references it, so the anchor holding it open
// (and dying with the daemon, whose pipe it blocks on) makes teardown
// automatic — no `ip netns del`, no leaked interfaces, nothing to clean up
// after a crash. It also means the namespace is unreachable to anything that
// doesn't already have the daemon's fds.
//
// This is the same shape rootlesskit/slirp4netns give rootless podman.

package sandbox

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// netnsAnchorCmd is the hidden subcommand for the anchor, dispatched before
// cobra sees os.Args. Prefixed with "__" so it can't collide with a
// user-facing verb.
const netnsAnchorCmd = "__netns-init"

// Device names inside a sandbox's namespace. Fixed, because the namespace
// holds exactly one VM — no hashing needed (contrast tapDevice, which shares
// the host's flat namespace in bridge mode).
const (
	nsBridge   = "br0"
	nsGuestTAP = "tap0"
	nsHostTAP  = "gv0"
)

// anchorReadyByte is the handshake the anchor writes once the topology is up,
// alongside the gv0 fd.
const anchorReadyByte = 'R'

// anchorStartTimeout bounds how long we wait for that handshake. Generous:
// the work is a handful of netlink calls, so anything approaching this means
// the child is wedged, not slow.
const anchorStartTimeout = 15 * time.Second

// InitNetNSHelpers dispatches the two hidden namespace subcommands before
// cobra runs, and exits the process when it handles one. Returns normally
// when os.Args is an ordinary invocation.
//
// Must be called at the very top of main: these paths are re-execs of the
// clawk binary as a helper, not CLI commands, and must never touch the CLI's
// config store or flag parsing.
func InitNetNSHelpers() {
	if len(os.Args) < 2 {
		return
	}
	if os.Args[1] == netnsAnchorCmd {
		runNetNSAnchor()
	}
}

// runNetNSAnchor is the child side of the namespace: it builds the topology,
// hands the host-side TAP fd back over the inherited socket (fd 3), and then
// blocks on stdin so the namespace outlives this call for as long as the
// parent holds the pipe. Never returns.
func runNetNSAnchor() {
	if err := buildNetNSTopology(); err != nil {
		fmt.Fprintf(os.Stderr, "netns anchor: %v\n", err)
		os.Exit(1)
	}
	tapFD, err := openTAPFD(nsHostTAP)
	if err != nil {
		fmt.Fprintf(os.Stderr, "netns anchor: opening %s: %v\n", nsHostTAP, err)
		os.Exit(1)
	}
	if err := sendFD(handshakeFD, tapFD, anchorReadyByte); err != nil {
		fmt.Fprintf(os.Stderr, "netns anchor: handing over %s: %v\n", nsHostTAP, err)
		os.Exit(1)
	}
	// The parent owns the fd now; ours would keep the TAP attached.
	_ = unix.Close(tapFD)
	// Hold the namespace open until the parent's pipe closes (daemon exit),
	// then die and let the kernel reclaim everything in it.
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// buildNetNSTopology creates the bridge and both TAPs inside the current
// (fresh, empty) network namespace and brings everything up. All netlink, no
// iproute2: the anchor is root here, so no privilege is requested from the
// host at any point.
func buildNetNSTopology() error {
	if lo, err := netlink.LinkByName("lo"); err == nil {
		// Not fatal — nothing clawk does needs guest-side loopback on the
		// host end — but a down lo confuses anyone debugging in here.
		_ = netlink.LinkSetUp(lo)
	}

	br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: nsBridge}}
	if err := netlink.LinkAdd(br); err != nil {
		return fmt.Errorf("creating bridge %s: %w", nsBridge, err)
	}

	// tap0 is created persistent and then released: firecracker attaches to
	// it by name, and a TAP that still has an owning fd rejects a second
	// TUNSETIFF with EBUSY. Persistence keeps the device alive in the
	// namespace with no fd held — exactly what `ip tuntap add` does.
	for _, name := range []string{nsGuestTAP, nsHostTAP} {
		if err := addPersistentTAP(name); err != nil {
			return err
		}
		link, err := netlink.LinkByName(name)
		if err != nil {
			return fmt.Errorf("looking up %s: %w", name, err)
		}
		if err := netlink.LinkSetMaster(link, br); err != nil {
			return fmt.Errorf("enslaving %s to %s: %w", name, nsBridge, err)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			return fmt.Errorf("bringing up %s: %w", name, err)
		}
	}
	if err := netlink.LinkSetUp(br); err != nil {
		return fmt.Errorf("bringing up %s: %w", nsBridge, err)
	}
	return nil
}

// addPersistentTAP creates a persistent TAP device by name and drops its fd,
// leaving the device in place with nothing attached.
func addPersistentTAP(name string) error {
	fd, err := openTAPFD(name)
	if err != nil {
		return fmt.Errorf("creating tap %s: %w", name, err)
	}
	defer unix.Close(fd)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(tunSetPersist), 1); errno != 0 {
		return fmt.Errorf("TUNSETPERSIST %s: %w", name, errno)
	}
	return nil
}

// TUN/TAP ioctls and flags from <linux/if_tun.h>, spelled out locally like
// the backend's openTAP does rather than pulling in a dependency for three
// constants.
const (
	tunSetIface   = 0x400454ca // TUNSETIFF
	tunSetPersist = 0x400454cb // TUNSETPERSIST
	iffTAP        = 0x0002     // IFF_TAP: ethernet frames
	iffNoPI       = 0x1000     // IFF_NO_PI: no packet-info prefix
)

// openTAPFD creates (or attaches to) a TAP device by name and returns its raw
// fd in nonblocking mode. Nonblocking before any os.File wraps it: that is
// what registers the fd with Go's runtime poller, and machine.UserMode's
// HostTAPFile contract requires it (a blocking fd there cannot be shut down).
func openTAPFD(name string) (int, error) {
	if len(name) >= 16 { // IFNAMSIZ
		return -1, fmt.Errorf("tap name %q too long", name)
	}
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR, 0)
	if err != nil {
		return -1, fmt.Errorf("open /dev/net/tun: %w", err)
	}
	var ifr [40]byte // struct ifreq
	copy(ifr[:15], name)
	flags := uint16(iffTAP | iffNoPI)
	ifr[16], ifr[17] = byte(flags), byte(flags>>8)
	// uintptr(unsafe.Pointer(…)) written inline, in the call's own argument
	// list, and NOT behind a helper: only that syntactic form gets the
	// compiler's syscall special case, which keeps ifr alive and un-moved for
	// the duration of the call. Hidden behind a function the conversion is just
	// arithmetic on an address the compiler no longer sees as a reference, so a
	// stack move would leave the kernel writing into memory that moved — and
	// go vet's unsafeptr check goes quiet about it too.
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(tunSetIface), uintptr(unsafe.Pointer(&ifr[0]))); errno != 0 {
		unix.Close(fd)
		return -1, fmt.Errorf("TUNSETIFF %q: %w", name, errno)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("tap %q set nonblock: %w", name, err)
	}
	return fd, nil
}

// nsenterExecPrefix returns the argv prefix that runs a command inside the
// anchor's namespaces, given the anchor's pid.
//
// It joins the USER namespace as well as the network one, and that is not
// optional: setns(2) into a netns demands CAP_SYS_ADMIN both in the caller's
// own user namespace and in the one owning the target netns. We are
// unprivileged in the initial user namespace, so joining the anchor's userns
// first — where our euid, as its owner, holds every capability — is what makes
// the netns join legal. `nsenter --net` alone fails with EPERM.
//
// And it is nsenter rather than a few lines of Go, because joining a user
// namespace is forbidden to multithreaded processes, which every Go program is
// (the runtime spins up threads before main). nsenter is single-threaded C
// written for exactly this; podman, docker and kubernetes all shell out to it
// for the same reason.
func nsenterExecPrefix(nsenter string, pid int) []string {
	proc := filepath.Join("/proc", strconv.Itoa(pid), "ns")
	return []string{
		nsenter,
		"--user=" + filepath.Join(proc, "user"),
		"--net=" + filepath.Join(proc, "net"),
		// Keep our real uid/gid instead of nsenter's default setuid(0): the
		// mapping already presents us as root inside, and /dev/kvm access is
		// checked against the real (unmapped) ids, which must not change.
		"--preserve-credentials",
		"--",
	}
}

// lookNsenter resolves nsenter, or explains its absence in the terms the
// rootless-availability check reports.
func lookNsenter() (string, error) {
	path, err := exec.LookPath("nsenter")
	if err != nil {
		return "", fmt.Errorf("nsenter is not on PATH (it ships with util-linux) — "+
			"rootless mode needs it to put the VM in its network namespace: %w", err)
	}
	return path, nil
}

// sandboxNetNS is a running namespace: the anchor process plus the host-side
// TAP fd it produced.
type sandboxNetNS struct {
	// HostTAP is the gvproxy-side TAP, already nonblocking. This struct owns
	// it until a caller takes it (TakeHostTAP); Close closes whatever is left.
	HostTAP *os.File
	// ExecPrefix re-execs a command inside the namespace.
	ExecPrefix []string

	anchor *exec.Cmd
	stdin  *os.File // closing it tells the anchor to exit
}

// TakeHostTAP hands the gvproxy-side TAP fd to the caller and forgets it, so
// Close stops accounting for it. Call it exactly where ownership really moves:
// machine.UserMode.HostTAPFile transfers the fd to the backend, which closes
// it on shutdown, and Close must not race that.
//
// Everything that does NOT take the fd — the availability probe, every failure
// between the handshake and a spec the backend accepts — relies on Close to
// close it, since nothing else will.
func (n *sandboxNetNS) TakeHostTAP() *os.File {
	f := n.HostTAP
	n.HostTAP = nil
	return f
}

// Close tears the namespace down: closing the anchor's stdin ends it, and the
// kernel reclaims the namespace (and every device in it) once the last
// reference — the anchor, plus any VM still running in it — is gone. The TAP fd
// goes too, unless a caller took ownership of it (TakeHostTAP).
func (n *sandboxNetNS) Close() error {
	if n.HostTAP != nil {
		_ = n.HostTAP.Close()
		n.HostTAP = nil
	}
	if n.stdin != nil {
		_ = n.stdin.Close()
		n.stdin = nil
	}
	if n.anchor == nil {
		return nil
	}
	cmd := n.anchor
	n.anchor = nil
	// cmd.Wait (not Process.Wait) so the goroutine copying the anchor's stderr
	// is joined and its pipe closed. The anchor exits as soon as the stdin
	// close above lands, so this returns promptly — but it must not be able to
	// hang a daemon shutdown, hence the kill after a grace period.
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(anchorExitGrace):
		_ = cmd.Process.Kill()
		<-done
	}
	return nil
}

// anchorExitGrace bounds how long Close waits for the anchor to notice its
// stdin closed. It has nothing to do but return from a read, so this only
// fires on a wedge. A var so tests can shorten it.
var anchorExitGrace = 2 * time.Second

// startSandboxNetNS forks the anchor into a fresh user+network namespace,
// waits for its topology, and returns a handle to it.
//
// The single-uid mapping (our uid ↔ 0 inside) is what makes this unprivileged
// and is also why it needs no /etc/subuid entry, unlike container runtimes:
// clawk only wants capabilities over its own network, never over other users'
// files.
// diagOut is where the anchor's stderr is echoed; nil discards it (used when
// merely probing, where the failure reason is returned rather than logged).
func startSandboxNetNS(self string, diagOut io.Writer) (_ *sandboxNetNS, err error) {
	// Resolved before the fork: a namespace we cannot put the VM into is worse
	// than no namespace, and this is the cheapest of the two failures.
	nsenter, err := lookNsenter()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errNetNSUnavailable, err)
	}
	parentSock, childSock, err := handshakePair()
	if err != nil {
		return nil, err
	}
	defer parentSock.Close()

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		childSock.Close()
		return nil, fmt.Errorf("netns lifetime pipe: %w", err)
	}
	defer stdinR.Close()

	// The anchor's stderr is where its failures are described; keep a copy for
	// the error we return (the fallback path logs it) while still letting it
	// flow to the caller's log, if it wants one.
	diag := &teeBuffer{out: diagOut, max: 4 << 10}

	cmd := exec.Command(self, netnsAnchorCmd)
	cmd.Stdin = stdinR
	cmd.Stdout = nil
	cmd.Stderr = diag
	cmd.ExtraFiles = []*os.File{childSock} // becomes fd 3 in the child
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
		// setgroups must stay denied for an unprivileged mapping; the kernel
		// refuses to write gid_map otherwise.
		GidMappingsEnableSetgroups: false,
	}
	startErr := cmd.Start()
	// Our copy of the child's socket must go NOW, whether or not the start
	// worked: while we hold it, the socket never reaches EOF, so an anchor that
	// died on its first syscall would cost us the full handshake timeout
	// instead of failing instantly.
	childSock.Close()
	if startErr != nil {
		stdinW.Close()
		return nil, fmt.Errorf("%w: %w", errNetNSUnavailable, startErr)
	}
	// One teardown, called from at most one place: the recvFD path below has to
	// reap before it can quote the anchor's stderr, and the deferred guard has
	// to cover every other failure. Two copies of the same three calls made the
	// second Wait return "Wait was already called" and the second Kill ESRCH —
	// swallowed, but it left no readable rule about who owns the child.
	var teardownOnce sync.Once
	teardown := func() {
		teardownOnce.Do(func() {
			stdinW.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait() // reap, and flush the stderr copier
		})
	}
	defer func() {
		if err != nil {
			teardown()
		}
	}()

	tapFD, err := recvFD(parentSock, anchorStartTimeout)
	if err != nil {
		// Reap first so the anchor's own diagnosis (which is far more specific
		// than "handshake failed") is in the buffer before we quote it.
		teardown()
		if d := diag.String(); d != "" {
			return nil, fmt.Errorf("%w: %s", errNetNSUnavailable, d)
		}
		return nil, fmt.Errorf("%w: %w", errNetNSUnavailable, err)
	}
	// Nonblocking is set by the anchor before the fd travels, but SCM_RIGHTS
	// carries the file, not the fd flags of the sender's descriptor — so set
	// it here too, on the raw fd, before os.NewFile decides about the poller.
	if err := unix.SetNonblock(tapFD, true); err != nil {
		unix.Close(tapFD)
		return nil, fmt.Errorf("received tap fd set nonblock: %w", err)
	}

	return &sandboxNetNS{
		HostTAP:    os.NewFile(uintptr(tapFD), "netns:"+nsHostTAP),
		ExecPrefix: nsenterExecPrefix(nsenter, cmd.Process.Pid),
		anchor:     cmd,
		stdin:      stdinW,
	}, nil
}

// teeBuffer forwards writes to out while retaining the first max bytes, so a
// child's stderr can both reach the log and be quoted in an error.
type teeBuffer struct {
	out io.Writer
	max int

	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *teeBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	if room := t.max - t.buf.Len(); room > 0 {
		t.buf.Write(p[:min(len(p), room)])
	}
	t.mu.Unlock()
	if t.out != nil {
		return t.out.Write(p)
	}
	return len(p), nil
}

func (t *teeBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(t.buf.String())
}

// errNetNSUnavailable marks a host that cannot do rootless networking, as
// opposed to one where it broke. Callers fall back to bridge mode.
var errNetNSUnavailable = errors.New("rootless networking unavailable")

// netNSHint turns a namespace-creation failure into one actionable sentence,
// naming the setting to change rather than echoing errno. The text lands in
// `clawk doctor` and the daemon log, so it stays short: the raw error is
// already in the log line beside it.
func netNSHint(err error) string {
	// Strip our own framing so the hint doesn't repeat itself.
	msg := strings.TrimPrefix(err.Error(), errNetNSUnavailable.Error()+": ")
	msg = strings.TrimPrefix(msg, "netns anchor: ")

	permission := strings.Contains(msg, "operation not permitted") ||
		strings.Contains(msg, "permission denied")
	switch {
	case strings.Contains(msg, "/dev/net/tun"):
		return "/dev/net/tun is not usable by this user (rootless mode needs it; " +
			"most distros ship it world-accessible)"
	case permission && sysctlIs("/proc/sys/kernel/apparmor_restrict_unprivileged_userns", "1"):
		// Ubuntu 24.04+ denies unprivileged user namespaces this way.
		return "unprivileged user namespaces are blocked by AppArmor " +
			"(kernel.apparmor_restrict_unprivileged_userns=1); allow clawk with an " +
			"AppArmor profile granting 'userns,', or set that sysctl to 0"
	case permission && sysctlIs("/proc/sys/user/max_user_namespaces", "0"):
		return "unprivileged user namespaces are disabled " +
			"(user.max_user_namespaces=0); raise it to enable rootless mode"
	case permission:
		return "the kernel refused an unprivileged user namespace: " + msg
	default:
		return msg
	}
}

// sysctlIs reports whether a /proc sysctl file holds exactly want.
func sysctlIs(path, want string) bool {
	v, err := os.ReadFile(path)
	return err == nil && strings.TrimSpace(string(v)) == want
}
