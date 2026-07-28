//go:build linux

// Host network-state readers, consulted before any privileged `ip` call.
//
// Every device the firecracker provider needs is created once and then
// reused for the life of the sandbox, so a boot that finds its bridge and
// TAPs already configured must perform no privileged work at all. That is
// the difference between one sudo prompt per sandbox and one per boot on a
// host where sudo needs a password — and the daemon, which cannot prompt at
// all (see ensureBridgeHostNet), only ever reads this state.
//
// State comes from sysfs rather than `ip link show`: no subprocess, no
// iproute2 on PATH, and the two facts the callers act on — administratively
// up, and which bridge a device is enslaved to — are plain files.

package sandbox

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
)

// sysClassNet holds one subdirectory (symlink) per network link.
const sysClassNet = "/sys/class/net"

// iffUp is IFF_UP from <net/if.h> — the administrative up flag, i.e. what
// `ip link set <dev> up` sets. Deliberately not operstate: a bridge with no
// carrier (every clawk bridge before a VM attaches) reports operstate=down
// while administratively up, so keying off operstate would re-run the
// privileged call on every single boot.
const iffUp = 0x1

// linkExistsIn reports whether a link named name is present under root.
func linkExistsIn(root, name string) bool {
	st, err := os.Stat(filepath.Join(root, name))
	return err == nil && st.IsDir()
}

// linkIsUpIn reports whether name has IFF_UP set. A missing or unparsable
// flags file reports false, so the caller falls back to issuing the same
// `ip link set up` it would have issued before this check existed.
func linkIsUpIn(root, name string) bool {
	data, err := os.ReadFile(filepath.Join(root, name, "flags"))
	if err != nil {
		return false
	}
	// One 0x-prefixed hex mask, e.g. "0x1003\n".
	txt := string(bytes.TrimSpace(data))
	flags, err := strconv.ParseUint(txt, 0, 64)
	if err != nil {
		return false
	}
	return flags&iffUp != 0
}

// linkMasterIn returns the name of the bridge name is enslaved to, or "" if
// it has none — the master symlink is absent on an unenslaved device.
func linkMasterIn(root, name string) string {
	dst, err := os.Readlink(filepath.Join(root, name, "master"))
	if err != nil {
		return ""
	}
	return filepath.Base(dst)
}

// linkExists reports whether a network link with the given name is present.
func linkExists(name string) bool { return linkExistsIn(sysClassNet, name) }

// linkIsUp reports whether the named link is administratively up.
func linkIsUp(name string) bool { return linkIsUpIn(sysClassNet, name) }

// linkMaster returns the bridge the named link is enslaved to, or "".
func linkMaster(name string) string { return linkMasterIn(sysClassNet, name) }

// bridgeMemberCountIn returns how many links are enslaved to a bridge, or -1
// when it isn't a bridge (or doesn't exist). A bridge lists its members as
// symlinks under brif/.
func bridgeMemberCountIn(root, name string) int {
	ents, err := os.ReadDir(filepath.Join(root, name, "brif"))
	if err != nil {
		return -1
	}
	return len(ents)
}

// StaleLegacyBridge reports whether the pre-per-sandbox shared bridge is
// still on this host with nothing attached — dead weight from an older clawk
// that doctor can offer to clean up. A bridge that still has members is left
// alone: something (a sandbox from the older clawk, or an unrelated tool that
// happened to pick the name) is using it.
func StaleLegacyBridge() (name string, stale bool) {
	if !linkExists(legacyLinuxBridge) {
		return legacyLinuxBridge, false
	}
	return legacyLinuxBridge, bridgeMemberCountIn(sysClassNet, legacyLinuxBridge) == 0
}
