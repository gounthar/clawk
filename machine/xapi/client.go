package xapi

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

// Client is the narrow slice of XAPI the backend actually uses. It is an
// interface for two reasons: the backend is testable against a fake pool
// without a hypervisor, and the choice of XAPI transport (XML-RPC via a
// Go XenAPI binding, or Xen Orchestra's JSON-RPC) stays swappable.
//
// Keeping it this small is deliberate. Everything clawk needs from a pool
// is here; if this interface starts growing, the backend is probably
// reaching for pool management that belongs to the operator, not to a
// sandbox tool.
type Client interface {
	// VM lifecycle.
	VMCreate(ctx context.Context, c VMConfig) (VMRef, error)
	VMStart(ctx context.Context, ref VMRef) error
	VMShutdown(ctx context.Context, ref VMRef, graceful bool) error
	VMDestroy(ctx context.Context, ref VMRef) error
	VMPowerState(ctx context.Context, ref VMRef) (PowerState, error)

	// Pause/suspend/checkpoint.
	VMPause(ctx context.Context, ref VMRef) error
	VMUnpause(ctx context.Context, ref VMRef) error
	VMSuspend(ctx context.Context, ref VMRef) error
	VMResumeFromSuspend(ctx context.Context, ref VMRef) error
	VMCheckpoint(ctx context.Context, ref VMRef, name string) (SnapshotRef, error)

	// Guest addressing. Reads VM_guest_metrics.networks, which requires
	// the guest to report — clawk-init will need to, or the backend needs
	// to learn the address from a DHCP lease on the mgmt network instead.
	VMGuestIP(ctx context.Context, ref VMRef, networkUUID string) (string, error)

	// Storage. VDIImportRaw streams a raw disk image to the pool's
	// import endpoint; sizeBytes must be the virtual size.
	VDIImportRaw(ctx context.Context, srUUID, name string, r io.Reader, sizeBytes int64) (VDIRef, error)
	VDIFindByName(ctx context.Context, srUUID, name string) (VDIRef, bool, error)
	VDIClone(ctx context.Context, ref VDIRef) (VDIRef, error)
	VDIDestroy(ctx context.Context, ref VDIRef) error
	VBDAttach(ctx context.Context, vm VMRef, vdi VDIRef, device string, readOnly bool) error

	// Networking. NetworkCreatePrivate makes a network with no PIF: no
	// path off the host except through something else with a VIF on it.
	NetworkCreatePrivate(ctx context.Context, name string) (uuid string, err error)
	NetworkDestroy(ctx context.Context, uuid string) error
	VIFAttach(ctx context.Context, vm VMRef, networkUUID, device, mac string) error

	Close() error
}

// Opaque handles. Strings on the wire; distinct types here so a VDI ref
// can't be passed where a VM ref belongs.
type (
	VMRef       string
	VDIRef      string
	SnapshotRef string
)

// PowerState is XAPI's vm_power_state enum.
type PowerState string

const (
	PowerHalted    PowerState = "Halted"
	PowerPaused    PowerState = "Paused"
	PowerRunning   PowerState = "Running"
	PowerSuspended PowerState = "Suspended"
)

// VMConfig is the VM record the backend wants created.
type VMConfig struct {
	NameLabel    string
	TemplateUUID string // empty: build a record from scratch
	VCPUs        uint
	MemoryMiB    uint64

	// HVMBoot selects the firmware path: "uefi" for EFIBoot specs and for
	// the synthesised boot VDI, "bios" otherwise.
	HVMBoot string

	// Platform holds extra platform: keys. NestedVirt sets
	// exp-nested-hvm=true here.
	Platform map[string]string
}

// NewClient dials a pool and logs in.
//
// The transport is XAPI's own JSON-RPC endpoint, spoken directly with
// net/http and encoding/json (see client_jsonrpc.go). Two alternatives were
// considered and rejected:
//
//	A generated Go XenAPI binding over XML-RPC reaches any XCP-ng or
//	XenServer pool with no extra components, which is right, but it carries
//	thousands of generated types to serve the ~20 calls in Client — and the
//	raw VDI import is a separate HTTP PUT outside the RPC surface either
//	way, so the binding saves nothing on the one hard part.
//
//	Xen Orchestra's API is nicer and brings XO's ACLs, which would matter
//	for a multi-user agent fleet. It also puts XO in the path of a tool that
//	otherwise talks to any pool unaided. Plain XAPI is the weaker claim on
//	the operator's setup, so it is the default.
//
// The Client interface exists so this stays reversible: an XO transport can
// land later as a second implementation, judged on its own merits.
func NewClient(ctx context.Context, c Config) (Client, error) {
	return newJSONRPCClient(ctx, c)
}

// pointer is the small file this backend writes into the snapshot
// directory the machine package hands it, since the actual memory image
// lives in the SR rather than on the caller's filesystem.
type pointer struct {
	Kind     string `json:"kind"` // "suspend" | "checkpoint"
	VM       string `json:"vm"`
	Snapshot string `json:"snapshot,omitempty"`
}

const pointerName = "xapi-snapshot.json"

func writePointer(dir string, p pointer) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, pointerName), b, 0o600)
}

func readPointer(dir string) (pointer, error) {
	b, err := os.ReadFile(filepath.Join(dir, pointerName))
	if err != nil {
		return pointer{}, err
	}
	var p pointer
	return p, json.Unmarshal(b, &p)
}
