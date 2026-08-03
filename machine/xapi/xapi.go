// Package xapi is a machine.Backend for XCP-ng / XenServer pools driven
// through the XAPI management API.
//
// Import this package for its side effect of registering itself with the
// machine registry:
//
//	import _ "github.com/clawkwork/clawk/machine/xapi"
//
// Unlike firecracker and vz, the hypervisor is not a local process: the
// backend is an API client and the VM runs on a remote host. Three
// consequences shape everything below.
//
// No vsock. Xen has no virtio-vsock, so Machine.VSock returns
// ErrVSockUnsupported and the control path is TCP over a management VIF
// on a private network. See ControlDialer.
//
// No virtio-fs. Spec.Shares is rejected. The 9P server in internal/ninep
// is served over the same management VIF instead, which is the substitute
// firecracker already establishes precedent for (it reports VirtioFS false
// too).
//
// No direct kernel load. XAPI can boot a kernel from dom0 (PV_kernel) but
// putting per-image vmlinux files in dom0 is a non-starter for a supported
// pool. Instead a DirectKernel Spec is honoured by synthesising a small
// read-only *boot VDI* (ESP + GRUB + vmlinux + initrd) attached as the
// first device, with the OCI rootfs as the second and root= pointed at it.
// One boot VDI per (kernel, cmdline) pair, cached in the SR.
package xapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/clawkwork/clawk/machine"
)

// Name is the registered backend name.
const Name = "xapi"

// Config addresses a pool and the resources the backend is allowed to use
// inside it.
//
// OPEN QUESTION for upstream: machine.Spec has no extension point, and the
// package doc says backend-specific settings "live in backend-specific
// option structs, not here". For a local hypervisor there is nothing to
// configure; for a remote one there is a pool address, credentials, an SR
// and two networks. This package takes them via Configure() before the
// first New(), which works but is process-global. A cleaner shape would be
// a Backend constructor alongside the registry — worth raising as an issue
// rather than deciding unilaterally.
type Config struct {
	// URL is the pool master, e.g. "https://xcp-ng-01.lab.example".
	URL      string
	Username string
	Password string

	// InsecureTLS skips verification of the pool's certificate. XCP-ng
	// installs generate a self-signed cert, so a stock lab pool is
	// unreachable without either this or a CA the daemon trusts. Off by
	// default: the session token authenticating every subsequent call
	// crosses this connection, so an unverified one is a real exposure,
	// not a formality.
	InsecureTLS bool

	// SRUUID is the storage repository that holds rootfs VDIs. A
	// file-based SR (ext, NFS, XOSTOR) gives fast VDI.clone, which is what
	// makes per-sandbox disks cheap the way clawk's copy-on-write clones
	// are on APFS/FICLONE. An LVM SR will full-copy and is a bad fit.
	SRUUID string

	// SandboxNetworkUUID is the network the sandbox VIF joins. It should
	// be a private network with no PIF: the guest's only route out is the
	// gateway VM running gvproxy, which is where the egress allow-list is
	// enforced. If empty, the backend creates one per machine and destroys
	// it with the machine.
	SandboxNetworkUUID string

	// MgmtNetworkUUID is the network carrying the control channel (guest
	// agent, 9P shares, ssh-agent proxy) between the clawk daemon and the
	// guest. It must be reachable from wherever the daemon runs and must
	// NOT be reachable from anything the agent could be tricked into
	// talking to.
	MgmtNetworkUUID string

	// TemplateUUID is the VM template to clone ("Other install media" is
	// the usual choice). Empty means the backend builds a VM record from
	// scratch.
	TemplateUUID string

	// NestedVirt allows the backend to honour Spec.NestedVirt by setting
	// platform:exp-nested-hvm. Off by default: nested virt on XCP-ng is
	// experimental and callers should opt in knowingly.
	NestedVirt bool
}

var (
	cfgMu sync.RWMutex
	cfg   Config
	// dialClient is swappable so tests can inject a fake pool.
	dialClient = NewClient
)

// Configure sets the pool this backend talks to. Must be called before the
// first New(). Safe to call again to point at a different pool; existing
// Machines keep the client they were created with.
func Configure(c Config) { cfgMu.Lock(); cfg = c; cfgMu.Unlock() }

func currentConfig() Config { cfgMu.RLock(); defer cfgMu.RUnlock(); return cfg }

func init() { machine.Register(backend{}) }

type backend struct{}

func (backend) Name() string { return Name }

func (backend) Capabilities() machine.Caps {
	return machine.Caps{
		// VM.checkpoint / VM.suspend / VM.pause map onto all three
		// optional interfaces. This backend is strictly more capable
		// than firecracker here.
		Snapshot: true,

		// The OCI -> ext4 builder is backend-independent; the result is
		// VDI.import'ed and VDI.clone'd per sandbox.
		OCIRootFS: true,

		// Honoured via a synthesised boot VDI, not via PV_kernel.
		DirectKernel: true,

		// XCP-ng HVM guests boot UEFI natively; use this for stock cloud
		// images that already ship GRUB.
		EFIBoot: true,

		// Xen has no virtio-vsock. See ControlDialer.
		VSock: false,

		// No virtio-fs on Xen; shares go over 9P on the mgmt VIF.
		VirtioFS: false,

		// The guest NIC is a XAPI VIF on a XAPI network. gvproxy does not
		// live in this process, it lives in the per-host gateway VM with
		// a VIF on the same private network, so none of the three local
		// attachment modes apply.
		UserModeNet: false,
		TAPNet:      false,
		UnixgramNet: false,

		NestedVirt: currentConfig().NestedVirt,
	}
}

func (backend) New(ctx context.Context, spec machine.Spec, stateDir string) (machine.Machine, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	c := currentConfig()
	if c.URL == "" {
		return nil, errors.New("xapi: Configure() must be called before New()")
	}
	if c.SRUUID == "" {
		return nil, errors.New("xapi: Config.SRUUID is required")
	}
	if err := checkSpec(spec, c); err != nil {
		return nil, err
	}
	cl, err := dialClient(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("xapi: connect %s: %w", c.URL, err)
	}
	return &vm{cfg: c, spec: spec, stateDir: stateDir, cl: cl}, nil
}

func checkSpec(spec machine.Spec, c Config) error {
	if len(spec.Shares) > 0 {
		return fmt.Errorf("%w: Spec.Shares (no virtio-fs on Xen; serve 9P on the mgmt VIF)",
			machine.ErrUnsupportedSpec)
	}
	if spec.VSockCID != 0 {
		return fmt.Errorf("%w: Spec.VSockCID (no vsock on Xen)", machine.ErrUnsupportedSpec)
	}
	for _, n := range spec.Net {
		switch n.(type) {
		case machine.UserMode, machine.TAP, machine.Unixgram:
			return fmt.Errorf("%w: Spec.Net variant %T (networking is a XAPI VIF; "+
				"set Config.SandboxNetworkUUID and run gvproxy in the gateway VM)",
				machine.ErrUnsupportedSpec, n)
		}
	}
	if spec.NestedVirt && !c.NestedVirt {
		return fmt.Errorf("%w: Spec.NestedVirt (set Config.NestedVirt to opt in to "+
			"XCP-ng experimental nested HVM)", machine.ErrUnsupportedSpec)
	}
	if spec.MemoryMaxMiB > spec.MemoryMiB {
		// Xen ballooning exists (dynamic-min/dynamic-max) and should be
		// wired here; refuse rather than silently ignore for now.
		return fmt.Errorf("%w: Spec.MemoryMaxMiB (TODO: map to VM.memory_dynamic_min/max)",
			machine.ErrUnsupportedSpec)
	}
	return nil
}

// ControlDialer is this backend's substitute for machine.Machine.VSock.
//
// clawk's guest control path (internal/vsockclient, internal/vsockproto,
// internal/ninep) is framed over vsock but is not otherwise vsock-specific:
// it needs a stream to the guest agent, per port. On Xen that stream is TCP
// to the guest's address on the management network.
//
// UPSTREAM: for this backend to be usable by internal/sandbox, the vsock
// client needs to accept a dialer rather than construct one. That is the
// single largest change outside this package.
type ControlDialer interface {
	// Control dials the guest agent on the given logical port over the
	// management VIF. Blocks until the guest has an address or ctx expires.
	Control(ctx context.Context, port uint32) (net.Conn, error)
}

type vm struct {
	cfg      Config
	spec     machine.Spec
	stateDir string
	cl       Client

	mu        sync.Mutex
	ref       VMRef
	rootVDI   VDIRef
	bootVDI   VDIRef
	ownedNet  string // non-empty if we created the sandbox network
	created   bool
	destroyed bool
}

var (
	_ machine.Machine       = (*vm)(nil)
	_ machine.Pauseable     = (*vm)(nil)
	_ machine.Suspendable   = (*vm)(nil)
	_ machine.Snapshottable = (*vm)(nil)
	_ ControlDialer         = (*vm)(nil)
)

// Create provisions the VM record, its disks and its VIFs. Idempotent.
//
// Sequence:
//  1. materialise the rootfs (oci.Build result or RawDisk) into the SR:
//     VDI.create + the raw import HTTP endpoint, keyed by image digest so a
//     second sandbox from the same image imports nothing;
//  2. VDI.clone the golden VDI -> this machine's writable root;
//  3. for DirectKernel, resolve-or-build the boot VDI for (vmlinux, initrd,
//     cmdline) and attach it read-only as device 0;
//  4. VM.create (or VM.clone of Config.TemplateUUID), set VCPUs/memory,
//     HVM boot policy, platform flags;
//  5. VBD.create for boot + root; VIF.create on the sandbox network and on
//     the management network.
func (v *vm) Create(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.destroyed {
		return fmt.Errorf("%w: Create after Destroy", machine.ErrInvalidState)
	}
	if v.created {
		return nil
	}
	// TODO(step 1-2): import + clone rootfs.
	// TODO(step 3): boot VDI for DirectKernel.
	// TODO(step 4-5): VM record, VBDs, VIFs.
	_ = ctx
	return errors.New("xapi: Create not implemented")
}

// Start boots the VM (VM.start). Returns once XAPI reports the guest
// Running; it does not wait for guest userspace, matching the contract.
func (v *vm) Start(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.created {
		return fmt.Errorf("%w: Start before Create", machine.ErrInvalidState)
	}
	return v.cl.VMStart(ctx, v.ref)
}

// Stop halts the guest. graceful uses VM.clean_shutdown (ACPI, needs the
// guest to cooperate); otherwise VM.hard_shutdown.
func (v *vm) Stop(ctx context.Context, graceful bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.cl.VMShutdown(ctx, v.ref, graceful)
}

// Destroy hard-shuts-down if needed, then removes the VM, its VBDs, its
// writable VDIs and any network this machine created. The golden per-image
// VDI is shared and is never destroyed here.
func (v *vm) Destroy(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.destroyed {
		return nil
	}
	// TODO: hard_shutdown if running; VM.destroy; VDI.destroy(root);
	// Network.destroy(ownedNet) if set. Boot VDI is shared: leave it.
	_ = ctx
	v.destroyed = true
	return errors.New("xapi: Destroy not implemented")
}

// State maps XAPI power states onto machine.State.
func (v *vm) State(ctx context.Context) (machine.State, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.destroyed {
		return machine.StateDestroyed, nil
	}
	if !v.created {
		return machine.StateStopped, nil
	}
	ps, err := v.cl.VMPowerState(ctx, v.ref)
	if err != nil {
		return "", err
	}
	switch ps {
	case PowerRunning:
		return machine.StateRunning, nil
	case PowerPaused:
		return machine.StatePaused, nil
	case PowerHalted:
		return machine.StateCreated, nil
	case PowerSuspended:
		// A suspended VM is stopped as far as callers are concerned; its
		// memory image lives in the SR and Restore brings it back.
		return machine.StateStopped, nil
	default:
		return "", fmt.Errorf("xapi: unknown power state %q", ps)
	}
}

// VSock always fails on this backend. See ControlDialer.
func (v *vm) VSock(context.Context, uint32) (net.Conn, error) {
	return nil, fmt.Errorf("%w: xen has no virtio-vsock, use Control()",
		machine.ErrVSockUnsupported)
}

// Control dials the guest agent over the management VIF.
func (v *vm) Control(ctx context.Context, port uint32) (net.Conn, error) {
	ip, err := v.cl.VMGuestIP(ctx, v.ref, v.cfg.MgmtNetworkUUID)
	if err != nil {
		return nil, fmt.Errorf("xapi: guest mgmt address: %w", err)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", net.JoinHostPort(ip, fmt.Sprint(port)))
}

// Pause / Resume: VM.pause and VM.unpause. vCPUs stop, memory stays
// resident, exactly the Pauseable contract.
func (v *vm) Pause(ctx context.Context) error  { return v.cl.VMPause(ctx, v.ref) }
func (v *vm) Resume(ctx context.Context) error { return v.cl.VMUnpause(ctx, v.ref) }

// Suspend implements machine.Suspendable via VM.suspend, which is a
// near-exact match for the contract: XAPI writes memory and device state to
// a suspend VDI and leaves the guest halted without resuming it, so the
// rootfs cannot drift ahead of the saved memory image.
//
// The contract wants state written into a host directory. XAPI keeps it in
// the SR, so we write a small pointer file into dir instead and Restore
// reads it back. Documented divergence, not an accident.
func (v *vm) Suspend(ctx context.Context, dir string) error {
	if err := v.cl.VMSuspend(ctx, v.ref); err != nil {
		return err
	}
	return writePointer(dir, pointer{Kind: "suspend", VM: string(v.ref)})
}

// Snapshot implements machine.Snapshottable via VM.checkpoint (a snapshot
// with memory), which pauses, saves and resumes.
func (v *vm) Snapshot(ctx context.Context, dir string) error {
	snap, err := v.cl.VMCheckpoint(ctx, v.ref, "clawk-"+v.spec.ID)
	if err != nil {
		return err
	}
	return writePointer(dir, pointer{Kind: "checkpoint", VM: string(v.ref), Snapshot: string(snap)})
}

// Restore boots from a directory previously written by Suspend or Snapshot.
func (v *vm) Restore(ctx context.Context, dir string) error {
	p, err := readPointer(dir)
	if err != nil {
		return err
	}
	switch p.Kind {
	case "suspend":
		return v.cl.VMResumeFromSuspend(ctx, VMRef(p.VM))
	case "checkpoint":
		// TODO: VM.revert to the checkpoint, then VM.resume.
		return errors.New("xapi: checkpoint restore not implemented")
	default:
		return fmt.Errorf("xapi: unknown snapshot kind %q", p.Kind)
	}
}
