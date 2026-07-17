package cli

import (
	"fmt"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/template"
)

// Defaults for sandboxes that configure nothing. A low baseline keeps a
// freshly-booted box's host footprint small; the higher ceiling lets it
// burst on demand. The vz balloon controller (machine/vz) holds an idle
// guest near the baseline and deflates the balloon toward the ceiling when
// the guest reports memory demand — see [machine/vz] balloon policy.
const (
	defaultSandboxVCPU         = 4
	defaultSandboxMemoryMiB    = 1024
	defaultSandboxMemoryMaxMiB = 4096
)

// specResources maps a sandbox's configured resources onto the machine
// spec: vCPU count, the baseline memory target, and the ceiling the guest
// may burst to. Zero values keep the defaults. The two memory values feed
// machine.Spec's MemoryMiB (baseline) and MemoryMaxMiB (ceiling); the vz and
// firecracker backends size the balloon from their difference.
//
// Semantics for partially-specified configs:
//   - neither set: defaults (baseline 1024, ceiling 4096).
//   - baseline only: a fixed size, no burst headroom (ceiling == baseline),
//     preserving the pre-balloon meaning of a bare `memory` directive.
//   - ceiling only: the default baseline, clamped down if the ceiling is
//     smaller.
func specResources(sb *config.Sandbox) (vcpu uint, memMiB, memMaxMiB uint64) {
	vcpu = defaultSandboxVCPU
	if sb.CPU > 0 {
		vcpu = sb.CPU
	}

	memMiB, memMaxMiB = sb.MemoryMiB, sb.MemoryMaxMiB
	switch {
	case memMiB == 0 && memMaxMiB == 0:
		memMiB, memMaxMiB = defaultSandboxMemoryMiB, defaultSandboxMemoryMaxMiB
	case memMaxMiB == 0:
		// Bare baseline: fixed size, no burst.
		memMaxMiB = memMiB
	case memMiB == 0:
		memMiB = defaultSandboxMemoryMiB
		if memMiB > memMaxMiB {
			memMiB = memMaxMiB
		}
	}
	return vcpu, memMiB, memMaxMiB
}

// resolveResources merges CPU and memory directives across a workspace and
// its repos by taking the max of each. The workspace VM is shared, so if
// any repo declares "needs 8 vCPU" we raise the floor for the whole
// sandbox. Zero is the "unset" sentinel, so max naturally ignores omissions.
func resolveResources(ws *template.Workspace) (cpu uint, memoryMiB, memoryMaxMiB uint64) {
	cpu = ws.File.CPU
	memoryMiB = ws.File.MemoryMiB
	memoryMaxMiB = ws.File.MemoryMaxMiB
	for _, r := range ws.Repos {
		if r.Clawkfile == nil {
			continue
		}
		if r.Clawkfile.CPU > cpu {
			cpu = r.Clawkfile.CPU
		}
		if r.Clawkfile.MemoryMiB > memoryMiB {
			memoryMiB = r.Clawkfile.MemoryMiB
		}
		if r.Clawkfile.MemoryMaxMiB > memoryMaxMiB {
			memoryMaxMiB = r.Clawkfile.MemoryMaxMiB
		}
	}
	return cpu, memoryMiB, memoryMaxMiB
}

// resolveDisk merges the root-disk ceiling across a workspace and its
// repos by taking the max: the VM's rootfs is shared across every phase,
// so the largest request wins (same rule as resolveResources). Zero
// everywhere means sandbox.DefaultDiskSizeGiB applies at build time.
func resolveDisk(ws *template.Workspace) uint64 {
	disk := ws.File.DiskMiB
	for _, r := range ws.Repos {
		if r.Clawkfile == nil {
			continue
		}
		if r.Clawkfile.DiskMiB > disk {
			disk = r.Clawkfile.DiskMiB
		}
	}
	return disk
}

// minDiskMiB is the floor for a per-sandbox `vm ( disk <size> )` override.
// Below ~1 GiB there is no room for the base image plus any writes, and
// such a value is almost always a unit typo (disk 32M meaning 32G). Zero
// (unset) is exempt — it falls back to sandbox.DefaultDiskSizeGiB.
const minDiskMiB = 1024

// validateDisk rejects a disk override that is too small to be usable.
func validateDisk(diskMiB uint64) error {
	if diskMiB != 0 && diskMiB < minDiskMiB {
		return fmt.Errorf(
			"disk must be >= 1 GiB, got %d MiB (did you mean a larger unit, e.g. 32G?)", diskMiB)
	}
	return nil
}

// defaultIdleTimeout is how long a sandbox may sit idle (no attached
// session, quiescent guest) before its daemon stops the VM, when the
// config doesn't say otherwise. Generous on purpose: parking too eagerly
// costs the user a ~seconds boot on the next attach and kills any
// forwarded dev server, while an idle VM only pins its memory baseline.
// Override per sandbox with `vm ( idle_timeout <dur|off> )`.
const defaultIdleTimeout = 30 * time.Minute

// resolveIdleTimeout merges idle_timeout across a workspace and its repos.
// "off" (negative) wins over any duration — the sandbox VM is shared, so
// if one repo needs it always-on, parking would break that repo; among
// durations the longest wins, mirroring resolveResources' max rule.
// Zero (unset everywhere) means the default applies at runtime.
func resolveIdleTimeout(ws *template.Workspace) int64 {
	sec := ws.File.IdleTimeoutSec
	for _, r := range ws.Repos {
		if r.Clawkfile == nil {
			continue
		}
		v := r.Clawkfile.IdleTimeoutSec
		if v < 0 || sec < 0 {
			sec = -1
			continue
		}
		if v > sec {
			sec = v
		}
	}
	return sec
}

// effectiveIdleTimeout returns the runtime idle-stop timeout for a
// sandbox: zero means idle-stop is disabled, anything else is the window
// of inactivity after which the daemon parks the VM.
func effectiveIdleTimeout(sb *config.Sandbox) time.Duration {
	switch {
	case sb.IdleTimeoutSec < 0:
		return 0
	case sb.IdleTimeoutSec == 0:
		return defaultIdleTimeout
	default:
		return time.Duration(sb.IdleTimeoutSec) * time.Second
	}
}

// validateResources rejects impossible combinations before they reach the
// provider. Providers assume inputs are sane.
func validateResources(memoryMiB, memoryMaxMiB uint64) error {
	if memoryMiB > 0 && memoryMaxMiB > 0 && memoryMiB > memoryMaxMiB {
		return fmt.Errorf("memory (%d MiB) exceeds memory_max (%d MiB)",
			memoryMiB, memoryMaxMiB)
	}
	if memoryMiB > 0 && memoryMiB < 64 {
		return fmt.Errorf("memory must be >= 64 MiB, got %d", memoryMiB)
	}
	if memoryMaxMiB > 0 && memoryMaxMiB < 64 {
		return fmt.Errorf("memory_max must be >= 64 MiB, got %d", memoryMaxMiB)
	}
	return nil
}

// normalizeMemory fills in the symmetric default: if only one of
// memory / memory_max is set, the other mirrors it so downstream providers
// only ever see (baseline, ceiling) with baseline <= ceiling. Zero-zero
// stays zero-zero to mean "provider default".
func normalizeMemory(baseline, ceiling uint64) (uint64, uint64) {
	if baseline == 0 && ceiling == 0 {
		return 0, 0
	}
	if baseline == 0 {
		baseline = ceiling
	}
	if ceiling == 0 {
		ceiling = baseline
	}
	return baseline, ceiling
}
