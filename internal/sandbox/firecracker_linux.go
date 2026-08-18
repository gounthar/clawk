//go:build linux

// Firecracker provider (Linux). Boots an OCI image as an ext4 rootfs with
// clawk-init as PID 1 and the clawk-pty-agent on vsock — the same guest
// stack as the vz provider — and talks to it over firecracker's hybrid
// vsock. No sshd: every guest session goes through the agent, like vz.
//
// Networking mirrors vz too: the VM runs out of process under the __fcd
// daemon, which drives an in-process gvproxy (gvisor-tap-vsock) userspace
// TCP/IP stack as the guest's gateway and enforces the same per-connection
// egress allow-list + DNS-aware filtering (internal/netfilter.AllowList).
// gvproxy can't drive firecracker's TAP directly, so the daemon bridges
// the two with a frame pump (fcnet_linux.go).
//
// Each sandbox gets its own L2 bridge and its own guest MAC, so the shared
// guest IP (every gvproxy hands out 192.168.127.2) is confined to one segment
// per VM — concurrent sandboxes no longer collide.
//
// Known limitations:
//   - The worktree rides in on its own disk built at Create time rather than
//     live-mounted, so host edits don't propagate into a running guest. The
//     default (firecracker-CI) kernel has no filesystem transport at all — no
//     9p, no FUSE, no virtio-fs. clawk's own published kernel has all three
//     and boots here fine (`vm ( kernel … )`), so the missing piece is a host
//     server wired to one of them, not the kernel. virtio-fs is the exception:
//     firecracker ships no such device, whatever the guest supports.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/guestbuild"
	"github.com/clawkwork/clawk/internal/guestcfg"
	"github.com/clawkwork/clawk/internal/netfilter"
	"github.com/clawkwork/clawk/internal/vsockclient"
	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/kernel"
	"github.com/clawkwork/clawk/machine/oci"

	// Register the firecracker backend.
	_ "github.com/clawkwork/clawk/machine/firecracker"
)

const (
	// fcGuestUser is the guest user sessions run as. Firecracker boots a
	// bare-root rootfs with no `agent` user, so it's root.
	fcGuestUser = "root"
	// fcVSockCID is the guest's AF_VSOCK context ID (any value >= 3).
	fcVSockCID = 3
	// fcAgentPort is the vsock port clawk-pty-agent listens on in the guest.
	fcAgentPort = 1024
	// fcWorktreeDevice is where the worktree disk lands in the guest. Drives
	// are attached in spec order after the rootfs (machine/firecracker puts
	// rootfs first, then Spec.Disks), so vda=rootfs, vdb=guestcfg, vdc=this.
	fcWorktreeDevice = "/dev/vdc"
	// fcSwapDevice is the swap disk, attached after the worktree — so
	// vda=rootfs, vdb=guestcfg, vdc=worktree, vdd=this. Firecracker's
	// virtio-blk advertises no discard, so freed swap pages stay allocated in
	// the backing file (see sandbox.DefaultSwapSizeMiB).
	fcSwapDevice = "/dev/vdd"
	// worktreeDiskSlack is the free space added to a worktree disk beyond the
	// tree itself, for whatever the agent builds in there. Sparse, so unused
	// capacity costs nothing on the host.
	worktreeDiskSlack = 4 << 30
	// kernelResolveTimeout bounds resolveKernel's network work: an S3 listing,
	// plus a multi-megabyte download on a cold cache. Generous for that, but
	// finite — DaemonSpec runs it in a detached daemon with no terminal and no
	// supervisor, where an unreachable host would otherwise hang for good.
	kernelResolveTimeout = 10 * time.Minute
)

// FirecrackerProvider implements the Provider + agent interfaces using the
// machine/firecracker backend.
type FirecrackerProvider struct {
	store *config.Store
}

func NewFirecrackerProvider(store *config.Store) *FirecrackerProvider {
	return &FirecrackerProvider{store: store}
}

func (f *FirecrackerProvider) vmDir(sb *config.Sandbox) string { return f.store.VMDir(sb.Name) }

// fcStateDir is the subdirectory the machine library owns (firecracker's
// api/vsock sockets, pidfile, console log). Nested under vmDir so
// `clawk destroy`'s RemoveAll sweep covers it.
func (f *FirecrackerProvider) fcStateDir(sb *config.Sandbox) string {
	return filepath.Join(f.vmDir(sb), "fc")
}

// vsockPath is firecracker's hybrid-vsock UDS; the agent client reaches
// the guest pty-agent through it with a "CONNECT <port>" handshake.
func (f *FirecrackerProvider) vsockPath(sb *config.Sandbox) string {
	return filepath.Join(f.fcStateDir(sb), "vsock.sock")
}

// GuestWorkspaceRoot: firecracker has no `agent` user and no virtio-fs, so
// the worktree is baked into the rootfs under /workspace.
func (f *FirecrackerProvider) GuestWorkspaceRoot() string { return "/workspace" }

// Create stages everything the VM boots from: the firecracker-CI kernel,
// the cross-compiled guest binaries, the OCI rootfs, the worktree disk, and
// the clawk-init manifest config disk.
func (f *FirecrackerProvider) Create(sb *config.Sandbox) error {
	if _, err := exec.LookPath("firecracker"); err != nil {
		return errors.New("firecracker binary not on PATH")
	}
	if sb.Image == "" {
		return fmt.Errorf("sandbox %q has no OCI image configured", sb.Name)
	}
	if len(sb.Phases) == 0 || sb.Phases[0].Worktree == "" {
		return fmt.Errorf("sandbox %q has no phase with a Worktree", sb.Name)
	}
	vmDir := f.vmDir(sb)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return fmt.Errorf("creating vm dir: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), kernelResolveTimeout)
	defer cancel()

	if _, err := f.resolveKernel(ctx, sb); err != nil {
		return fmt.Errorf("kernel: %w", err)
	}
	bins, err := guestbuild.Build(ctx, f.store.CacheDir(), runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("guest binaries: %w", err)
	}
	// A boot that will restore a suspend state must keep the exact disks the
	// memory image was saved against: re-materializing the rootfs (a clone
	// that truncates its destination) or rebuilding the worktree disk under a
	// restore corrupts the guest filesystem — the invariant machine.Suspendable
	// documents and restoreOrStart relies on. Everything above this line is
	// content-addressed and safe to re-run.
	rootfs := filepath.Join(vmDir, "rootfs.raw")
	if f.hasRestorableState(sb, vmDir, rootfs) {
		sb.GuestIP = guestIP
		return nil
	}
	// Materialize the OCI rootfs (build cache + clone) into a per-VM disk.
	if _, err := oci.Materialize(ctx, OCIRootFS(sb, f.store.CacheDir(), bins), rootfs); err != nil {
		return fmt.Errorf("materializing rootfs: %w", err)
	}
	if err := f.writeWorktreeDisk(sb); err != nil {
		return fmt.Errorf("worktree disk: %w", err)
	}
	if err := guestcfg.WriteDisk(f.manifest(sb), filepath.Join(vmDir, "guestcfg.img")); err != nil {
		return fmt.Errorf("guest config disk: %w", err)
	}
	if _, err := EnsureSwapDisk(vmDir, SwapDiskMiB(sb)); err != nil {
		return fmt.Errorf("swap disk: %w", err)
	}
	sb.GuestIP = guestIP
	return nil
}

// hasRestorableState reports whether this VM dir holds a suspend state that
// the next boot will restore onto the disks already present — in which case
// Create must not touch them. A state whose rootfs is gone can never restore
// (restoreOrStart discards it loudly), so it doesn't count.
//
// EVERY disk buildSpec attaches has to be there, not just the rootfs: skipping
// the build leaves whatever is on disk as the whole disk set. A state saved by
// a clawk that baked the worktree into the rootfs — no worktree.img at all —
// would otherwise survive this check, fail the suspend fingerprint (the disk
// count changed), and cold-boot against a drive that does not exist, which
// firecracker refuses outright. Rebuilding instead costs only the snapshot,
// which restoreOrStart was going to discard in that case anyway.
func (f *FirecrackerProvider) hasRestorableState(sb *config.Sandbox, vmDir, rootfs string) bool {
	if !machine.SuspendStateExists(filepath.Join(vmDir, "suspend")) {
		return false
	}
	disks := []string{rootfs, f.worktreeDiskPath(sb), filepath.Join(vmDir, "guestcfg.img")}
	if SwapDiskMiB(sb) > 0 {
		disks = append(disks, SwapDiskPath(vmDir))
	}
	for _, disk := range disks {
		if _, err := os.Stat(disk); err != nil {
			return false
		}
	}
	return true
}

// manifest is the clawk-init boot manifest for firecracker. The guest is
// configured statically with gvproxy's addresses (no DHCP client in arbitrary
// images): gateway + DNS are gvproxy, so DNS answers flow through gvproxy's
// resolver and feed the allow-list's domain matcher. Same values as the vz
// manifest (OCIGuestManifest). No virtio-fs share — the worktree arrives on
// its own disk (see writeWorktreeDisk), mounted from /dev/vdc.
func (f *FirecrackerProvider) manifest(sb *config.Sandbox) guestcfg.Manifest {
	return guestcfg.Manifest{
		Hostname: sb.Name,
		Network: &guestcfg.Network{
			Interface: "eth0",
			Address:   guestIP + "/24",
			Gateway:   gvproxyGateway,
			DNS:       []string{gvproxyGateway},
			MTU:       gvproxyMTU,
		},
		Swap: fcSwap(sb),
		Mounts: []guestcfg.Mount{{
			Path:   f.guestWorktreePath(sb),
			Block:  fcWorktreeDevice,
			FSType: "ext4",
		}},
		Services: []guestcfg.Service{{Name: "pty-agent", Path: guestcfg.AgentPath}},
	}
}

// fcSwap is the manifest's swap entry, or nil when the sandbox opted out.
// Keyed off the same SwapDiskMiB as buildSpec's disk list so the manifest
// never names a device firecracker wasn't asked to attach.
func fcSwap(sb *config.Sandbox) *guestcfg.Swap {
	if SwapDiskMiB(sb) == 0 {
		return nil
	}
	return &guestcfg.Swap{Device: fcSwapDevice, Swappiness: GuestSwappiness}
}

// guestWorktreePath is where the worktree disk is mounted in the guest —
// the same /workspace/<name> layout the bake produced, so nothing downstream
// (sessions, runners, cwd inference) sees a change.
func (f *FirecrackerProvider) guestWorktreePath(sb *config.Sandbox) string {
	return filepath.Join(f.GuestWorkspaceRoot(), filepath.Base(sb.Phases[0].Worktree))
}

// writeWorktreeDisk builds the phase worktree into its own ext4 disk, in
// userspace.
//
// Firecracker has no virtio-fs and the firecracker-CI kernel has no 9p, so
// the worktree has to be inside a block device. It used to be copied into the
// rootfs through a loop mount, which cost six privileged operations per boot
// (losetup, mount, mkdir, cp, umount, losetup -d) and required a root helper
// that mounted whatever it was told. A separate disk needs none of that: the
// same ext4 writer that builds the rootfs writes it as this user.
//
// One disk per VM, rebuilt on each Create — same lifetime and semantics as
// the bake it replaces (host edits still don't propagate into a running
// guest; a live worktree needs a 9p-capable guest kernel).
func (f *FirecrackerProvider) writeWorktreeDisk(sb *config.Sandbox) error {
	// Resolved once, here, so the walk that sizes the disk and the walk that
	// fills it see the same tree: both lstat their root, so a worktree path
	// whose last component is a symlink would size to bare slack.
	src, err := filepath.EvalSymlinks(sb.Phases[0].Worktree)
	if err != nil {
		return fmt.Errorf("resolving worktree %s: %w", sb.Phases[0].Worktree, err)
	}
	size, err := worktreeDiskSize(src)
	if err != nil {
		return err
	}
	return oci.WriteDirDisk(src, f.worktreeDiskPath(sb), size)
}

func (f *FirecrackerProvider) worktreeDiskPath(sb *config.Sandbox) string {
	return filepath.Join(f.vmDir(sb), "worktree.img")
}

// worktreeDiskSize picks the worktree disk's capacity: the tree's own size
// plus headroom for what the agent does inside it (build outputs, node_modules,
// a second checkout). Doubling covers small repos poorly, so take whichever of
// "twice the tree" and "the tree plus a flat slack" is larger. The padding is
// sparse — it costs no physical disk until the guest writes to it — so being
// generous here is close to free, and running out mid-session is not.
//
// TODO: make this configurable (clawk.mod `disk`) once anyone hits the ceiling.
func worktreeDiskSize(dir string) (int64, error) {
	var used int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A file that vanished mid-walk shouldn't fail sizing (the disk
			// build tolerates the same) — but the worktree root going missing
			// means there is nothing to boot with, and must not size to slack.
			if os.IsNotExist(err) && path != dir {
				return nil
			}
			return err
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode().IsRegular() {
			used += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("sizing worktree %s: %w", dir, err)
	}
	return max(2*used, used+worktreeDiskSlack), nil
}

// Start spawns the detached __fcd daemon — which owns gvproxy, the frame
// pump, and the firecracker VM for the VM's lifetime — and returns once the
// pty-agent answers over vsock. The VM must outlive this CLI invocation, so
// (like the vz provider) the work runs in a child process, not in-process.
func (f *FirecrackerProvider) Start(sb *config.Sandbox) error {
	if _, err := exec.LookPath("firecracker"); err != nil {
		return errors.New("firecracker binary not on PATH")
	}
	// Pre-flight /dev/kvm access. Without this, a user not in the kvm group
	// gets a detached daemon that dies on firecracker's opaque InstanceStart
	// "Permission denied (os error 13) ... /dev/kvm file's ACL" — which the
	// CLI never sees; it only observes the vsock ping timing out ("agent did
	// not become ready"). Checking here surfaces the real cause and the fix
	// in the foreground before the daemon is even spawned.
	if err := checkKVMAccess(); err != nil {
		return err
	}
	vmDir := f.vmDir(sb)
	pidPath := filepath.Join(vmDir, "fc.pid")
	if pid := readPIDFile(pidPath); pid > 0 && processAlive(pid) {
		sb.GuestIP = guestIP
		return nil // already running
	}
	// Decide the network mode once, here, and hand the decision to the daemon
	// below: if each probed independently they could disagree — the CLI
	// skipping the host devices because rootless looked available, the daemon
	// then falling back to bridge mode and finding none — and the resulting
	// error would tell the user to run the command they just ran.
	mode, why := SelectNetMode()
	// In bridge mode, create the host devices here, in the foreground, while we
	// still have the user's terminal to authenticate on. The daemon never can.
	if mode == NetModeBridge {
		if err := f.ensureBridgeHostNet(sb); err != nil {
			return fmt.Errorf("%w (%s)", err, why)
		}
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating clawk binary: %w", err)
	}
	// Setsid detaches the daemon from the CLI's controlling terminal so a
	// Ctrl+C on the foreground command doesn't take the VM down with it.
	cmd := exec.Command(self, "__fcd", sb.Name)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	// Pin the mode we just decided (and provisioned for), so the daemon can't
	// probe its way to a different answer — and pass the reason with it, or the
	// daemon reports our own handoff as if the user had pinned it, and the real
	// cause never reaches fcd.log.
	cmd.Env = append(os.Environ(), NetModeHandoff(mode, why)...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawning fcd: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("releasing fcd: %w", err)
	}
	sb.GuestIP = guestIP

	// One agent round trip proves kernel boot, clawk-init, and the pty-agent
	// are all up — the firecracker counterpart of waiting for sshd, minus sshd.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	if err := vsockclient.Ping(ctx, f.vsockPath(sb), fcAgentPort, 120*time.Second); err != nil {
		// Both logs: the console shows guest-side failures (kernel panic,
		// clawk-init), fcd.log host-side ones (spec build, gvproxy, the
		// hypervisor refusing to start). A daemon that died before the VM
		// existed leaves an empty console and the real cause in fcd.log,
		// where the CLI's own error otherwise reads as a bare vsock timeout.
		return fmt.Errorf("agent did not become ready: %w%s%s",
			err,
			LogTail(filepath.Join(vmDir, "fcd.log"), 10, "clawk daemon log"),
			ConsoleTail(filepath.Join(vmDir, "console.log"), 20))
	}
	return nil
}

// FCStateDir exposes the machine-library state dir to the __fcd daemon.
func (f *FirecrackerProvider) FCStateDir(sb *config.Sandbox) string { return f.fcStateDir(sb) }

// ensureBridgeHostNet provisions bridge mode's host-side network plumbing for
// sb: the IP-less bridge, the guest's TAP, and the daemon-owned gvproxy TAP.
// (Rootless mode has no host devices at all — see netmode_linux.go.)
//
// It runs in the CLI, from Start, before __fcd is spawned, because creating
// those devices needs CAP_NET_ADMIN and the CLI is the only process that can
// obtain it interactively. The daemon cannot: it is Setsid-detached, and
// sudo's timestamp records are tty-scoped, so a password the user typed a
// second earlier is invisible there and every `sudo -n` fails with "a
// password is required" — forever, no matter how recently they
// authenticated. Provisioning here means a password-sudo host prompts once,
// in front of the user, instead of failing inside a daemon whose log the
// CLI never shows. See issue #9.
//
// Idempotent and privilege-free once the devices exist (see ensureTAP).
func (f *FirecrackerProvider) ensureBridgeHostNet(sb *config.Sandbox) error {
	bridge := bridgeDevice(sb.Name)
	if err := ensureLinuxBridge(bridge); err != nil {
		return fmt.Errorf("bridge: %w", err)
	}
	if err := ensureTAP(tapDevice(sb.Name), bridge); err != nil {
		return fmt.Errorf("guest tap: %w", err)
	}
	if err := ensureTAP(gvTapDevice(sb.Name), bridge); err != nil {
		return fmt.Errorf("gvproxy tap: %w", err)
	}
	return nil
}

// DaemonSpec returns the machine.Spec the __fcd daemon boots, plus a cleanup
// to run when the daemon exits. It runs in the daemon process; the returned
// spec carries a UserMode net in TAP-bridge mode so the firecracker backend
// brings up gvproxy (with allow as the egress filter) bridged to the guest's
// NIC.
//
// Rootless mode builds that bridge inside a fresh user+network namespace here
// and now — it needs no privilege, so the daemon can do it despite having no
// terminal. Bridge mode instead uses host devices that ensureBridgeHostNet created
// in the CLI, and only verifies them, because creating them needs a sudo the
// daemon could never authenticate (see ensureBridgeHostNet).
func (f *FirecrackerProvider) DaemonSpec(sb *config.Sandbox, allow *netfilter.AllowList) (machine.Spec, func(), error) {
	forwards := make([]machine.PortForward, 0, len(sb.Forwards))
	for _, fwd := range sb.Forwards {
		forwards = append(forwards, machine.PortForward{
			HostPort: uint16(fwd.HostPort), GuestPort: uint16(fwd.GuestPort),
			Proto: machine.ProtoTCP,
		})
	}
	net := machine.UserMode{Filter: allow, Forwards: forwards}

	mode, why := SelectNetMode()
	var cleanup = func() {}
	var ns *sandboxNetNS // rootless mode only
	switch mode {
	case NetModeRootless:
		self, err := os.Executable()
		if err != nil {
			return machine.Spec{}, nil, fmt.Errorf("locating clawk binary: %w", err)
		}
		ns, err = startSandboxNetNS(self, os.Stderr)
		if err != nil {
			return machine.Spec{}, nil, fmt.Errorf("rootless network: %w", err)
		}
		// ns still owns the TAP fd, so cleanup closes it on any failure
		// between here and the return below. Ownership moves to the backend
		// only once the spec is really going to it (TakeHostTAP, at the end).
		cleanup = func() { _ = ns.Close() }
		net.GuestTAP = nsGuestTAP
		net.NetNSExec = ns.ExecPrefix
	default:
		fcTap, gvTap := tapDevice(sb.Name), gvTapDevice(sb.Name)
		for _, dev := range []string{bridgeDevice(sb.Name), fcTap, gvTap} {
			if !linkExists(dev) {
				return machine.Spec{}, nil, fmt.Errorf(
					"host network device %s for sandbox %q is missing (bridge mode: %s) — "+
						"it is provisioned by 'clawk up %s'; if something removed it, "+
						"run 'clawk down %s && clawk up %s'",
					dev, sb.Name, why, sb.Name, sb.Name, sb.Name)
			}
		}
		net.GuestTAP, net.HostTAP = fcTap, gvTap
	}

	// Bounded, and not with context.Background(): resolveKernel revalidates a
	// URL override on every call and can download the CI kernel, so an
	// unreachable host (allow-list refusal, captive portal, dead mirror) would
	// wedge the detached daemon here forever — holding the network namespace it
	// just created, while the CLI reports only its own vsock timeout and nothing
	// ever reaps the daemon.
	kernelCtx, cancelKernel := context.WithTimeout(context.Background(), kernelResolveTimeout)
	defer cancelKernel()
	kernelPath, err := f.resolveKernel(kernelCtx, sb)
	if err != nil {
		cleanup()
		return machine.Spec{}, nil, fmt.Errorf("kernel: %w", err)
	}
	// Ensured on every boot, not just at create, so an edited
	// `vm ( swap <size> )` takes effect on the next start — buildSpec attaches
	// whatever SwapDiskMiB says, and the device has to exist by then.
	if _, err := EnsureSwapDisk(f.vmDir(sb), SwapDiskMiB(sb)); err != nil {
		cleanup()
		return machine.Spec{}, nil, fmt.Errorf("swap disk: %w", err)
	}
	spec := f.buildSpec(sb, kernelPath)
	if ns != nil {
		// Handed over here and nowhere earlier: machine.UserMode documents that
		// HostTAPFile's ownership travels with the spec to the backend, so this
		// is the point where cleanup must stop accounting for the fd — and
		// every failure before it is a path where cleanup still has to close it.
		net.HostTAPFile = ns.TakeHostTAP()
	}
	spec.Net = []machine.Net{net}
	return spec, cleanup, nil
}

// NetModeForLog reports the mode the daemon will use and why it isn't
// rootless, for the daemon's startup log line.
func (f *FirecrackerProvider) NetModeForLog() (NetMode, string) { return SelectNetMode() }

// resolveKernel returns the vmlinux to direct-boot: the sandbox's override
// (clawk.mod `vm ( kernel … )`, or --kernel) when set, else the firecracker-CI
// kernel.
//
// The override used to be ignored here, which was worse than it sounds. The CI
// kernel is deliberately minimal — no 9p, no FUSE, no virtio-fs, so no
// filesystem transport of any kind — and the flag is precisely how a user says
// "boot something that has one". Silently dropping it made that impossible and
// looked like the kernel was at fault.
func (f *FirecrackerProvider) resolveKernel(ctx context.Context, sb *config.Sandbox) (string, error) {
	if sb.Kernel == "" {
		return ciEnsureKernel(ctx)
	}
	return kernel.Fetch(ctx, kernel.Options{
		CacheDir: f.store.CacheDir(),
		Arch:     runtime.GOARCH,
		Override: sb.Kernel,
	})
}

// buildSpec assembles the resource/boot/rootfs parts of the machine.Spec, given
// the kernel DaemonSpec resolved. The Net entry is filled in by DaemonSpec once
// the TAPs exist.
func (f *FirecrackerProvider) buildSpec(sb *config.Sandbox, kernelPath string) machine.Spec {
	vcpu := uint(1)
	if sb.CPU > 0 {
		vcpu = sb.CPU
	}
	memMiB := uint64(512)
	if sb.MemoryMiB > 0 {
		memMiB = sb.MemoryMiB
	}
	memMaxMiB := memMiB
	if sb.MemoryMaxMiB > memMaxMiB {
		memMaxMiB = sb.MemoryMaxMiB
	}
	disks := []machine.Disk{
		{Path: filepath.Join(f.vmDir(sb), "guestcfg.img"), ReadOnly: true},
		{Path: f.worktreeDiskPath(sb)},
	}
	if SwapDiskMiB(sb) > 0 {
		disks = append(disks, machine.Disk{Path: SwapDiskPath(f.vmDir(sb))})
	}
	return machine.Spec{
		ID:           sb.Name,
		VCPU:         vcpu,
		MemoryMiB:    memMiB,
		MemoryMaxMiB: memMaxMiB,
		Boot: machine.DirectKernel{
			Vmlinux: kernelPath,
			// Firecracker's serial is ttyS0 (no virtio-console/hvc0).
			// clawk-init reads the manifest on /dev/vdb and configures the
			// network from it, so there's no ip= cmdline.
			Cmdline: "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw " +
				"init=" + guestcfg.InitPath + " clawk.cfg=/dev/vdb",
		},
		RootFS: machine.RawDisk{Path: filepath.Join(f.vmDir(sb), "rootfs.raw")},
		// Order is the guest's device order: vdb=guestcfg, vdc=worktree
		// (fcWorktreeDevice), vdd=swap (fcSwapDevice). Appending anything here
		// shifts later devices, so keep new disks after these.
		Disks:    disks,
		VSockCID: fcVSockCID,
		Serial:   machine.Serial{LogPath: filepath.Join(f.vmDir(sb), "console.log")},
	}
}

// Stop signals the __fcd daemon, which tears down the VM, the frame pump, and
// gvproxy. A missing/stale pidfile is not an error.
//
// The timeout MUST exceed the daemon's own graceful-stop budget (gracefulStop
// gives m.Stop 15s: CtrlAltDel wait 10s + SIGTERM-firecracker wait 5s). The
// minimal OCI guest doesn't power off on CtrlAltDel, so the daemon always
// spends ~10s there before falling through to SIGTERM the firecracker child.
// If we SIGKILL the daemon before that completes, firecracker is orphaned —
// it keeps the guest TAP open, and the next boot fails with "Open tap device
// failed: Resource busy". 25s leaves margin over the 15s budget plus the
// daemon's post-stop cleanup.
func (f *FirecrackerProvider) Stop(sb *config.Sandbox) error {
	return stopByPIDFile(filepath.Join(f.vmDir(sb), "fc.pid"), 25*time.Second)
}

// Destroy stops the VM and removes its host devices and state.
//
// Device teardown is best-effort and deliberately non-interactive
// (runSudoQuiet): a destroy that can't reach sudo should still delete the VM
// dir, not stop to ask for a password on the way out. It is also driven by
// what actually exists rather than by the current mode — a rootless sandbox
// has no host devices to remove (its namespace took them with it), while a
// sandbox created back when the host used bridge mode still does, and those
// must not be leaked just because rootless works now.
func (f *FirecrackerProvider) Destroy(sb *config.Sandbox) error {
	_ = f.Stop(sb)
	// TAPs first, then the bridge they were enslaved to — it belongs to this
	// sandbox alone, so leaving it behind would leak one interface per
	// destroyed sandbox.
	for _, dev := range []string{tapDevice(sb.Name), gvTapDevice(sb.Name), bridgeDevice(sb.Name)} {
		if linkExists(dev) {
			_ = runSudoQuiet("ip", "link", "del", dev)
		}
	}
	return os.RemoveAll(f.vmDir(sb))
}

func (f *FirecrackerProvider) Status(sb *config.Sandbox) (string, error) {
	pid := readPIDFile(filepath.Join(f.vmDir(sb), "fc.pid"))
	if pid > 0 && processAlive(pid) {
		return "running", nil
	}
	return "stopped", nil
}

// --- agent (vsock) sessions ---

// Shell opens an interactive login shell in the guest over the vsock agent.
func (f *FirecrackerProvider) Shell(sb *config.Sandbox, workdir string) error {
	code, err := vsockclient.Run(context.Background(), vsockclient.Config{
		SocketPath:  f.vsockPath(sb),
		ConnectPort: fcAgentPort,
		Cwd:         workdir,
		User:        fcGuestUser,
		// A login shell is line-oriented: no screen clear, keep scrollback.
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

// Exec runs a command in the guest over the vsock agent. Used for the
// coding-agent attach (claude), so it's interactive.
func (f *FirecrackerProvider) Exec(sb *config.Sandbox, command ...string) error {
	if len(command) == 0 {
		return fmt.Errorf("firecracker exec: empty command")
	}
	code, err := vsockclient.Run(context.Background(), vsockclient.Config{
		SocketPath:  f.vsockPath(sb),
		ConnectPort: fcAgentPort,
		Cmd:         command[0],
		Args:        command[1:],
		User:        fcGuestUser,
		// Exec carries the coding-agent (claude) attach — a full-screen
		// TUI, so clear to a clean canvas.
		ClearScreen: true,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("command exited with status %d", code)
	}
	return nil
}

// ExecCapture runs a command non-interactively and returns its combined
// output over the agent's frame protocol.
func (f *FirecrackerProvider) ExecCapture(sb *config.Sandbox, command ...string) (string, error) {
	if len(command) == 0 {
		return "", fmt.Errorf("firecracker execcapture: empty command")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	out, code, err := vsockclient.Output(ctx, f.vsockPath(sb), fcAgentPort,
		fcGuestUser, command[0], command[1:]...)
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, fmt.Errorf("command exited with status %d", code)
	}
	return out, nil
}
