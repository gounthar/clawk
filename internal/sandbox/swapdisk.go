package sandbox

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/clawkwork/clawk/internal/config"
)

// Swap. Every sandbox gets a swap device unless it opts out with
// `vm ( swap off )`.
//
// The reason is the balloon controller, not the guest's own appetite. Under
// host memory pressure clawk reclaims guest RAM against guest demand
// (machine/vz.mergedBalloonTarget drops the cap to 75% of the ceiling at WARN
// and 50% at CRITICAL), and the guest's only answers without swap are direct
// reclaim stalls and the OOM killer — the balloon's DEFLATE_ON_OOM safety net
// assumes exactly that. A multi-second stall in the agent process is not just
// slow: a process that stops draining its TLS socket lets the connection go
// idle, and on a link whose NAT reaps idle mappings in under a minute that
// ends the streaming response outright.
//
// The cost is bounded by sparseness, not by size — see DefaultSwapSizeMiB.

// DefaultSwapSizeMiB is the swap device's capacity when the sandbox doesn't
// say otherwise. It is a ceiling on how much the guest may swap, not an
// allocation: the backing file is sparse and materializes host bytes only as
// pages are actually written to it. A sandbox that never swaps carries a
// 2 GiB device that costs a few hundred bytes of directory entry.
//
// It does not shrink again on its own, though. Nothing in the stack punches
// the holes back: swapon(2) is asked for SWAP_FLAG_DISCARD, but neither
// firecracker's virtio-blk nor vz's advertises discard, so the kernel drops
// the flag and freed swap pages stay allocated on the host until the sandbox
// is destroyed. Read the number as a high-water mark — which is the reason
// not to make it larger just because the device is sparse.
const DefaultSwapSizeMiB = 2048

// SwapDiskName is the swap device's filename inside a sandbox's VM
// directory. Removed with the rest of the VM dir on destroy.
const SwapDiskName = "swap.img"

// OCISwapDevice is where the swap disk lands in a vz OCI sandbox. Disks are
// attached in spec order after the rootfs, so vda=rootfs, vdb=guestcfg, and
// swap is the next one. Keep in lock-step with buildOCISandboxSpec's
// Spec.Disks in internal/cli/vzd.go.
const OCISwapDevice = "/dev/vdc"

// GuestSwappiness is the vm.swappiness clawk-init sets on a swap-enabled
// guest. Above the kernel's default 60 on purpose: the pages we want the
// guest to give up under balloon inflation are cold anonymous ones (an idle
// agent heap), and the page cache we want it to keep is a repo and toolchain
// an active build reads constantly. The default's more even split trades the
// wrong way for this workload.
const GuestSwappiness = 80

// SwapDiskMiB is the swap capacity for sb, in MiB, or 0 when the sandbox has
// swap disabled. Mirrors RootDiskSizeMiB's shape: an explicit positive
// override wins, negative means off, and unset takes the default.
func SwapDiskMiB(sb *config.Sandbox) uint64 {
	switch {
	case sb == nil:
		return DefaultSwapSizeMiB
	case sb.SwapMiB < 0:
		return 0
	case sb.SwapMiB > 0:
		return uint64(sb.SwapMiB)
	default:
		return DefaultSwapSizeMiB
	}
}

// SwapDiskPath is the swap device's host path inside vmDir.
func SwapDiskPath(vmDir string) string { return filepath.Join(vmDir, SwapDiskName) }

// EnsureSwapDisk makes vmDir hold a sparse swap device of sizeMiB and returns
// its path. A sizeMiB of 0 removes any device a previous configuration left
// behind and returns "" — callers use the empty path as "attach nothing".
//
// Resizing an existing device just truncates it. Swap contents are worthless
// across a boot (the guest re-formats whenever the header doesn't match the
// device), so there is nothing to preserve, and truncation keeps the file
// sparse where a rewrite would not.
func EnsureSwapDisk(vmDir string, sizeMiB uint64) (string, error) {
	path := SwapDiskPath(vmDir)
	if sizeMiB == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("removing swap disk: %w", err)
		}
		return "", nil
	}
	size := int64(sizeMiB) << 20
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return "", fmt.Errorf("creating swap disk: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat swap disk: %w", err)
	}
	if st.Size() != size {
		if err := f.Truncate(size); err != nil {
			return "", fmt.Errorf("sizing swap disk to %d MiB: %w", sizeMiB, err)
		}
	}
	return path, nil
}
