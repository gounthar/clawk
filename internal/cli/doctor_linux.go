//go:build linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/clawkwork/clawk/internal/sandbox"
)

// platformHostChecks probes the Linux/firecracker host prerequisites that
// aren't visible until a VM actually tries to boot: the firecracker binary
// and read/write access to /dev/kvm. The kvm one is the single most common
// first-run failure — firecracker fails InstanceStart with an opaque
// "Permission denied (os error 13) ... /dev/kvm file's ACL" when the user
// isn't in the kvm group — so doctor names the exact fix.
func platformHostChecks() []doctorCheck {
	var results []doctorCheck

	if _, err := exec.LookPath("firecracker"); err != nil {
		results = append(results, fail("host: firecracker",
			"not found on PATH (the Linux hypervisor)",
			"install firecracker and put it on PATH: https://github.com/firecracker-microvm/firecracker/releases"))
	} else {
		results = append(results, ok("host: firecracker", "on PATH"))
	}

	switch f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); {
	case err == nil:
		f.Close()
		results = append(results, ok("host: /dev/kvm", "readable/writable"))
	case errors.Is(err, os.ErrNotExist):
		results = append(results, fail("host: /dev/kvm",
			"missing — no KVM support (nested virt off, or a VM/container without /dev/kvm)",
			"enable virtualization in BIOS/hypervisor; on a cloud VM, use a metal/nested-virt instance"))
	case errors.Is(err, os.ErrPermission):
		results = append(results, fail("host: /dev/kvm",
			"present but not accessible (permission denied) — firecracker cannot open it",
			"add yourself to the kvm group: sudo usermod -aG kvm $USER, then start a new login session"))
	default:
		results = append(results, warn("host: /dev/kvm",
			fmt.Sprintf("unexpected error opening it: %v", err),
			"ensure the invoking user can read/write /dev/kvm"))
	}

	results = append(results, hostNetSetupCheck())
	if name, stale := sandbox.StaleLegacyBridge(); stale {
		results = append(results, warn("host: legacy bridge",
			name+" is present with nothing attached — leftover from a clawk that shared one bridge across sandboxes",
			"harmless; remove it with: sudo ip link del "+name))
	}

	return results
}

// hostNetSetupCheck reports how clawk will obtain the CAP_NET_ADMIN it needs
// to create a sandbox's bridge and TAPs — the host prerequisite that used to
// be invisible until a boot failed (issue #9: doctor said 5 ok / 0 fail on a
// host that could not boot at all, because nothing checked this).
//
// Rootless mode needs nothing from the host and is the happy answer. Otherwise
// we fall back to sudo, and how bad that is depends: device creation happens
// once per sandbox, not per boot (see ensureTAP), so a host where sudo prompts
// is usable — it just needs a terminal at create time. Only a host that can
// neither authenticate nor prompt is broken.
func hostNetSetupCheck() doctorCheck {
	const name = "host: network mode"
	mode, why := sandbox.SelectNetMode()
	if mode == sandbox.NetModeRootless {
		// A pin is taken at face value everywhere else — but doctor exists to
		// tell the truth, so verify a pinned rootless really works rather than
		// reporting OK on a host where every boot will fail.
		if pinned, isPinned := sandbox.NetModePinned(); isPinned && pinned == sandbox.NetModeRootless {
			if avail, reason := sandbox.NetNSAvailable(); !avail {
				return fail(name,
					"pinned to rootless by CLAWK_NET_MODE, but this host cannot do it: "+reason,
					"fix the cause above, or unset CLAWK_NET_MODE to fall back to bridge mode")
			}
		}
		return ok(name, "rootless (per-sandbox network namespace; no privileged operations)")
	}

	detail := "bridge mode via sudo — " + why
	if sandbox.SudoIPWorksUnprompted() {
		return ok(name, detail+" (passwordless sudo for ip, so nothing will prompt)")
	}
	if sandbox.StdinIsTerminal() {
		return warn(name,
			detail+" — sudo will prompt once per sandbox, when it creates the devices",
			"fine if you're happy to type it; to avoid it entirely, enable unprivileged "+
				"user namespaces (see the reason above), or grant NOPASSWD for ip only: "+
				"'<user> ALL=(ALL) NOPASSWD: /usr/sbin/ip'")
	}
	return fail(name,
		detail+" — and sudo needs a password with no terminal to prompt on, so "+
			"'clawk up' cannot create its network devices",
		"run 'clawk up' from a terminal, enable unprivileged user namespaces, or "+
			"grant NOPASSWD for ip: '<user> ALL=(ALL) NOPASSWD: /usr/sbin/ip'")
}
