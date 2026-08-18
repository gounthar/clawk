package cli

import (
	"fmt"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/serialfwd"
	"github.com/clawkwork/clawk/internal/template"
)

type serialSource struct {
	Origin string
	Spec   template.SerialSpec
}

// mergeSerials gathers `serial (...)` entries from the workspace file and
// every repo's Clawkfile and resolves them into the sandbox's device list.
func mergeSerials(ws *template.Workspace) ([]config.SerialDevice, error) {
	var sources []serialSource
	for _, s := range ws.File.Serials {
		sources = append(sources, serialSource{Origin: "workspace", Spec: s})
	}
	for _, r := range ws.Repos {
		if r.Clawkfile == nil {
			continue
		}
		for _, s := range r.Clawkfile.Serials {
			sources = append(sources, serialSource{Origin: r.Name, Spec: s})
		}
	}
	return composeSerials(sources)
}

// composeSerials is the pure conflict-detection core split out from
// mergeSerials so the here-mode path (a single Clawkfile) and the workspace
// path share it — same split as composeFiles and composeShares.
//
// Two things can collide, and both are refused with a message naming the
// contributors rather than resolved by precedence: a guest name, because
// the guest creates /dev/<name> exactly once and a second claim would
// silently lose in there, and a host device, because forwarding one port
// under two names would let the sandbox open it twice.
func composeSerials(sources []serialSource) ([]config.SerialDevice, error) {
	type claim struct {
		Origin string
		Device config.SerialDevice
	}
	byName := make(map[string]claim)
	byHost := make(map[string]claim)

	var out []config.SerialDevice
	for _, s := range sources {
		dev, err := resolveSerialSpec(s.Spec)
		if err != nil {
			return nil, fmt.Errorf("%s serial %q: %w", s.Origin, s.Spec.HostPath, err)
		}
		if prev, dup := byName[dev.GuestName]; dup {
			if prev.Device == dev {
				// The same device declared twice — by a repo and the
				// workspace, say. Harmless; keep the first.
				continue
			}
			return nil, fmt.Errorf(
				"serial device %q is claimed by both %s (%s) and %s (%s)",
				dev.GuestName, prev.Origin, prev.Device.HostPath, s.Origin, dev.HostPath)
		}
		if prev, dup := byHost[dev.HostPath]; dup {
			return nil, fmt.Errorf(
				"serial port %s is forwarded twice: as %q by %s and as %q by %s",
				dev.HostPath, prev.Device.GuestName, prev.Origin, dev.GuestName, s.Origin)
		}
		byName[dev.GuestName] = claim{Origin: s.Origin, Device: dev}
		byHost[dev.HostPath] = claim{Origin: s.Origin, Device: dev}
		out = append(out, dev)
	}
	return out, nil
}

// resolveSerialSpec turns one parsed clawk.mod entry into a device,
// applying the same defaults and validation the CLI's HOST:GUEST spec
// parser does.
func resolveSerialSpec(spec template.SerialSpec) (config.SerialDevice, error) {
	hostPath, err := template.ExpandPath(spec.HostPath)
	if err != nil {
		return config.SerialDevice{}, err
	}
	if !isAbsSerialPath(hostPath) {
		return config.SerialDevice{}, fmt.Errorf(
			"must be an absolute path (e.g. /dev/cu.usbmodem1101)")
	}

	guestName := spec.GuestName
	if guestName == "" {
		guestName = config.DefaultSerialGuestName(hostPath)
		if guestName == "" {
			return config.SerialDevice{}, fmt.Errorf(
				"a device pattern needs an explicit guest name (e.g. '%s ttyACM0')", hostPath)
		}
	}
	if err := serialfwd.ValidDeviceName(guestName); err != nil {
		return config.SerialDevice{}, err
	}
	return config.SerialDevice{HostPath: hostPath, GuestName: guestName}, nil
}

// isAbsSerialPath is filepath.IsAbs pinned to POSIX semantics. Serial
// devices are named the same way on both hosts clawk runs on, and a
// clawk.mod is shared across machines, so this shouldn't vary by where it
// happens to be parsed.
func isAbsSerialPath(p string) bool { return len(p) > 0 && p[0] == '/' }
