//go:build linux

// How the firecracker provider obtains the CAP_NET_ADMIN it needs to create a
// sandbox's network devices. Two modes, picked per boot:
//
//   - NetModeRootless: an unprivileged user+network namespace per sandbox
//     (netns_linux.go). No privileged operation, ever, and each VM gets its own
//     L2 domain. The default wherever the kernel allows it.
//
//   - NetModeBridge: host devices created through sudo, one prompt per sandbox
//     at most (linux_shared.go). The fallback for hosts that forbid
//     unprivileged user namespaces — Ubuntu 24.04+ does by default, via
//     AppArmor — and the only mode before rootless existed.
//
// The choice is made by trying: whether an unprivileged user namespace works
// depends on kernel config, sysctls, LSM policy and container nesting at once,
// and Ubuntu's AppArmor restriction in particular is invisible to every check
// short of attempting the clone. CLAWK_NET_MODE overrides, for operators who
// want one or the other pinned.

package sandbox

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// NetMode is how a sandbox's host-side network devices come into being.
type NetMode string

const (
	NetModeRootless NetMode = "rootless"
	NetModeBridge   NetMode = "bridge"
)

// netModeEnv pins the mode when set to a known value.
const netModeEnv = "CLAWK_NET_MODE"

// netModeWhyEnv carries the reason behind a mode the CLI already decided.
//
// It exists because Start pins its decision in the daemon's environment (see
// FirecrackerProvider.Start) so the daemon can't probe its way to a different
// answer — which makes every inherited decision indistinguishable from an
// operator's own pin. Without the reason travelling too, the daemon log
// reported "pinned by CLAWK_NET_MODE=bridge" for a variable the user never set,
// and the real cause (AppArmor, a missing nsenter) appeared nowhere in it.
const netModeWhyEnv = "CLAWK_NET_MODE_WHY"

// NetModePinned reports the CLAWK_NET_MODE override, if a valid one is set.
// A pin is honored as-is: someone who pinned rootless does not want a silent
// fallback to a mode that may ask for their password.
func NetModePinned() (NetMode, bool) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(netModeEnv))) {
	case string(NetModeRootless):
		return NetModeRootless, true
	case string(NetModeBridge):
		return NetModeBridge, true
	case "":
		return "", false
	default:
		// A typo is not a mode; say so rather than silently picking something.
		fmt.Fprintf(os.Stderr, "warning: ignoring unknown %s=%q (want rootless or bridge)\n",
			netModeEnv, os.Getenv(netModeEnv))
		return "", false
	}
}

// SelectNetMode reports the mode to use and, when it isn't rootless, a
// complete phrase saying why — "pinned by CLAWK_NET_MODE=bridge" for a
// deliberate override, "rootless networking unavailable: …" for a host that
// cannot do it.
//
// Callers render the phrase verbatim rather than prefixing their own reason.
// The two facts are different — one is the operator's own configuration, the
// other a host capability — and reporting a pin as "rootless unavailable"
// handed users fixes for a restriction that wasn't there.
func SelectNetMode() (NetMode, string) {
	if pinned, ok := NetModePinned(); ok {
		if pinned == NetModeBridge {
			return NetModeBridge, pinnedBridgeWhy()
		}
		return pinned, ""
	}
	if ok, why := NetNSAvailable(); !ok {
		return NetModeBridge, "rootless networking unavailable: " + why
	}
	return NetModeRootless, ""
}

// pinnedBridgeWhy explains a bridge pin: the reason the CLI passed down when
// this process inherited its decision (netModeWhyEnv), otherwise the pin
// itself, which is all there is to say when an operator set it by hand.
func pinnedBridgeWhy() string {
	if why := strings.TrimSpace(os.Getenv(netModeWhyEnv)); why != "" {
		return why
	}
	return "pinned by " + netModeEnv + "=bridge"
}

// NetModeHandoff is the environment a child process needs to inherit this
// process's network-mode decision verbatim — the mode so it cannot probe its
// way to a different answer, and the reason so it reports the truth about why.
func NetModeHandoff(mode NetMode, why string) []string {
	env := []string{netModeEnv + "=" + string(mode)}
	if why != "" {
		env = append(env, netModeWhyEnv+"="+why)
	}
	return env
}

// netNSProbe caches the namespace probe: forking a namespace costs a few
// milliseconds, several call sites ask, and a host's capability cannot change
// under a running process.
var netNSProbe = sync.OnceValues(probeNetNS)

// NetNSAvailable reports whether this host can create the unprivileged
// user+network namespace rootless mode needs, and if not, why — phrased as the
// thing a user would have to change.
func NetNSAvailable() (bool, string) { return netNSProbe() }

func probeNetNS() (bool, string) {
	self, err := os.Executable()
	if err != nil {
		return false, fmt.Sprintf("cannot locate the clawk binary: %v", err)
	}
	ns, err := startSandboxNetNS(self, nil) // quiet: the reason is returned, not logged
	if err != nil {
		return false, netNSHint(err)
	}
	// Close covers the TAP fd too — nobody took ownership of it here.
	_ = ns.Close()
	return true, ""
}
