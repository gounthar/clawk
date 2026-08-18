package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/serialfwd"
	"github.com/clawkwork/clawk/internal/serialport"
	"github.com/clawkwork/clawk/internal/template"
	"github.com/clawkwork/clawk/internal/vzdctl"
	"github.com/spf13/cobra"
)

var serialListJSON bool

func init() {
	rootCmd.AddCommand(serialCmd)
	serialCmd.AddCommand(serialAddCmd)
	serialCmd.AddCommand(serialRemoveCmd)
	serialCmd.AddCommand(serialListCmd)
	serialListCmd.Flags().BoolVar(&serialListJSON, "json", false, "emit JSON")
}

var serialCmd = &cobra.Command{
	Use:   "serial",
	Short: "Expose a host serial port inside a sandbox",
	Long: `Present a serial port plugged into this machine — an Arduino, an
ESP32, a USB-TTL adapter — as a device inside the sandbox, so tooling in
there can flash and monitor it.

The USB device itself is not passed through; nothing clawk runs on can do
that. What crosses is the serial stream and its line settings, which is all
avrdude, esptool and a serial monitor ever wanted. See docs/serial.md for
what that does and doesn't cover.`,
}

var serialAddCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "add <sandbox> <device-spec> [device-spec...]",
	Short:             "Forward a host serial port into the sandbox",
	Long: `Device specs read host-side first, like the forward commands:

  /dev/cu.usbmodem1101           — same path inside the guest
  /dev/cu.usbmodem1101:ttyACM0   — /dev/ttyACM0 inside the guest
  '/dev/cu.usbmodem*:ttyACM0'    — resolved at open time, not now

The glob form is worth knowing about: a board that reboots into its
bootloader leaves /dev and comes back, often under a neighbouring name, and
a pattern survives that where a literal path doesn't. Quote it so the shell
doesn't expand it first. A pattern that matches two boards is refused rather
than guessed at.

These apply to a running sandbox immediately — no down/up cycle. The port is
opened on the host only while a process in the guest holds the device open,
so the Arduino IDE on this machine can still have it the rest of the time.

vz (macOS) only: firecracker's vsock is one-way, so the guest has no channel
to connect back through.`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sb, err := store.Load(args[0])
		if err != nil {
			return err
		}
		// Every spec is parsed and checked before anything is printed or
		// saved. The command is already all-or-nothing on the store — a
		// later bad spec returns before Save — so reporting devices as
		// "added" mid-loop announced work that then didn't persist.
		var added []config.SerialDevice
		for _, spec := range args[1:] {
			dev, err := parseSerialSpec(spec)
			if err != nil {
				return err
			}
			if prev, dup := serialByGuestName(sb.Serials, dev.GuestName); dup {
				if prev.HostPath == dev.HostPath {
					fmt.Fprintf(cmd.OutOrStdout(), "  (already forwarded: %s)\n", describeSerial(dev))
					continue
				}
				// One name, one device. A second spec claiming it would
				// silently lose to the first inside the guest, so reject it
				// here where both can be named.
				return fmt.Errorf(
					"guest device %q is already forwarded from %s — remove that first",
					dev.GuestName, prev.HostPath)
			}
			// The same port under two names would let the guest open it
			// twice, and the second open would fail in a way that points at
			// the wrong thing.
			if prev, dup := serialByHostPath(sb.Serials, dev.HostPath); dup {
				return fmt.Errorf(
					"%s is already forwarded as %q — remove that first",
					dev.HostPath, prev.GuestName)
			}
			sb.Serials = append(sb.Serials, dev)
			added = append(added, dev)
		}
		if len(added) == 0 {
			return nil
		}
		if err := store.Save(sb); err != nil {
			return err
		}
		for _, dev := range added {
			fmt.Fprintf(cmd.OutOrStdout(), "Serial device added: %s\n", describeSerial(dev))
			for _, w := range warnAboutDevice(dev) {
				fmt.Fprintf(cmd.OutOrStdout(), "  note: %s\n", w)
			}
		}
		applySerials(cmd, sb)
		return nil
	},
}

var serialRemoveCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "remove <sandbox> <device-spec-or-name> [more...]",
	Aliases:           []string{"rm"},
	Short:             "Stop forwarding a host serial port",
	Long: `Accepts whatever identifies the device: the spec as added, the host
path on its own, or the guest name on its own.`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sb, err := store.Load(args[0])
		if err != nil {
			return err
		}
		var kept, removed []config.SerialDevice
		for _, d := range sb.Serials {
			if serialMatchesAny(d, args[1:]) {
				removed = append(removed, d)
				continue
			}
			kept = append(kept, d)
		}
		// Nothing matched: say so and leave the record untouched rather than
		// rewriting it identically.
		if len(removed) == 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "  (nothing matched: %s)\n", strings.Join(args[1:], ", "))
			return nil
		}
		sb.Serials = kept
		if err := store.Save(sb); err != nil {
			return err
		}
		for _, d := range removed {
			fmt.Fprintf(cmd.OutOrStdout(), "Serial device removed: %s\n", describeSerial(d))
		}
		applySerials(cmd, sb)
		return nil
	},
}

var serialListCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "list <sandbox>",
	Aliases:           []string{"ls"},
	Short:             "List a sandbox's forwarded serial ports",
	Args:              cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sb, err := store.Load(args[0])
		if err != nil {
			return err
		}
		if serialListJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			// Never null: a caller iterating the result shouldn't have to
			// special-case a sandbox with no devices.
			devices := sb.Serials
			if devices == nil {
				devices = []config.SerialDevice{}
			}
			return enc.Encode(devices)
		}
		if len(sb.Serials) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No serial devices forwarded.")
			return nil
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "HOST\tGUEST\tPRESENT")
		for _, d := range sb.Serials {
			resolved, err := serialport.Resolve(d.HostPath)
			present := "yes"
			switch {
			case err != nil:
				present = "no"
			case resolved != d.HostPath:
				// A glob that currently resolves somewhere is worth showing
				// resolved: "which board is this right now" is the question
				// the column exists to answer.
				present = resolved
			}
			fmt.Fprintf(w, "%s\t/dev/%s\t%s\n", d.HostPath, d.GuestName, present)
		}
		return w.Flush()
	},
}

// parseSerialSpec parses HOST[:GUEST].
//
// Split on the last colon rather than the first: a host path may contain
// one (rare but legal), while a guest name may not contain anything that
// isn't a bare filename, so the right-hand side is unambiguous.
func parseSerialSpec(spec string) (config.SerialDevice, error) {
	hostPath, guestName := spec, ""
	if i := strings.LastIndex(spec, ":"); i >= 0 {
		hostPath, guestName = spec[:i], spec[i+1:]
	}

	expanded, err := template.ExpandPath(hostPath)
	if err != nil {
		return config.SerialDevice{}, fmt.Errorf("device %q: %w", spec, err)
	}
	hostPath = expanded

	if hostPath == "" {
		return config.SerialDevice{}, fmt.Errorf("device spec %q has no host path", spec)
	}
	if !filepath.IsAbs(hostPath) {
		return config.SerialDevice{}, fmt.Errorf(
			"device %q must be an absolute path (e.g. /dev/cu.usbmodem1101)", hostPath)
	}

	if guestName == "" {
		guestName = config.DefaultSerialGuestName(hostPath)
		if guestName == "" {
			// Only reachable for a glob, which has no basename to borrow.
			return config.SerialDevice{}, fmt.Errorf(
				"device pattern %q needs an explicit guest name (e.g. %s:ttyACM0)",
				hostPath, hostPath)
		}
	}
	if err := serialfwd.ValidDeviceName(guestName); err != nil {
		return config.SerialDevice{}, err
	}
	return config.SerialDevice{HostPath: hostPath, GuestName: guestName}, nil
}

// warnAboutDevice returns advisory notes for a device just added. None of
// these are errors: a board that is currently unplugged is a perfectly
// reasonable thing to configure, and the forward starts working when it
// appears.
func warnAboutDevice(dev config.SerialDevice) []string {
	var notes []string
	if _, err := serialport.Resolve(dev.HostPath); err != nil {
		if errors.Is(err, serialport.ErrNoMatch) {
			notes = append(notes, fmt.Sprintf(
				"%s isn't there right now — it'll be picked up when it appears", dev.HostPath))
		} else {
			notes = append(notes, err.Error())
		}
	}
	// The /dev/tty.* device on macOS blocks on carrier detect and is meant
	// for dial-in; /dev/cu.* is the callout side and the one every serial
	// tool uses. Getting this wrong looks like a port that opens and then
	// does nothing.
	if runtime.GOOS == "darwin" && strings.HasPrefix(dev.HostPath, "/dev/tty.") {
		notes = append(notes, fmt.Sprintf(
			"prefer the callout device /dev/cu.%s over /dev/tty.%[1]s",
			strings.TrimPrefix(dev.HostPath, "/dev/tty.")))
	}
	return notes
}

// applySerials pushes the just-saved device set into the running daemon,
// which relays it to the in-guest agent. Reports what happened but never
// fails the command: the store is already updated, so the worst case is
// that the edit lands on the next boot. Mirrors applyReverseForwards.
//
// Takes the sandbox rather than its name because ErrSerialUnsupported is
// ambiguous by construction (see vzdctl.ErrSerialUnsupported): it means
// either "this daemon predates serial forwarding" or "this backend has no
// host-side vsock listener". Only the record distinguishes them, and telling
// a firecracker user to restart the daemon sends them after a fix that can
// never work.
func applySerials(cmd *cobra.Command, sb *config.Sandbox) {
	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
	defer cancel()
	err := vzdctl.NewClient(vzdctl.SocketPath(store.VMDir(sb.Name))).ReloadSerials(ctx)
	switch {
	case err == nil:
		fmt.Fprintln(cmd.OutOrStdout(), "Applied to running sandbox.")
	case errors.Is(err, vzdctl.ErrNotRunning):
		if !serialSupported(sb) {
			fmt.Fprintf(cmd.OutOrStdout(),
				"Note: this sandbox uses the %s provider, which cannot forward "+
					"serial devices — the guest has no channel to dial back through. "+
					"The entry is recorded but will not appear in the sandbox.\n",
				sb.Provider)
			return
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Sandbox not running — applies on next 'clawk up'.")
	case errors.Is(err, vzdctl.ErrSerialUnsupported):
		if !serialSupported(sb) {
			fmt.Fprintf(cmd.OutOrStdout(),
				"Note: this sandbox uses the %s provider, which cannot forward "+
					"serial devices — the guest has no channel to dial back through. "+
					"The entry is recorded but will not appear in the sandbox.\n",
				sb.Provider)
			return
		}
		fmt.Fprintln(cmd.OutOrStdout(),
			"Sandbox is running an older daemon — restart it ('clawk down && clawk up') to apply.")
	default:
		fmt.Fprintf(cmd.ErrOrStderr(),
			"clawk: live apply failed (%v) — applies on next 'clawk up'\n", err)
	}
}

// serialSupported reports whether sb's provider can forward serial devices
// at all. Only vz can: the guest is the end that dials (see
// internal/serialfwd), and firecracker's vsock carries host→guest only.
func serialSupported(sb *config.Sandbox) bool {
	return sb.Provider.Normalize() == config.ProviderVZ
}

// describeSerial spells out a device in the direction it is used, because
// the HOST:GUEST spec alone doesn't say which name belongs where.
func describeSerial(d config.SerialDevice) string {
	return fmt.Sprintf("/dev/%s in the guest → %s", d.GuestName, d.HostPath)
}

func serialByGuestName(devs []config.SerialDevice, name string) (config.SerialDevice, bool) {
	for _, d := range devs {
		if d.GuestName == name {
			return d, true
		}
	}
	return config.SerialDevice{}, false
}

func serialByHostPath(devs []config.SerialDevice, path string) (config.SerialDevice, bool) {
	for _, d := range devs {
		if d.HostPath == path {
			return d, true
		}
	}
	return config.SerialDevice{}, false
}

// serialMatchesAny reports whether d is named by any of the given
// identifiers — the spec it was added as, its host path, or its guest name.
func serialMatchesAny(d config.SerialDevice, identifiers []string) bool {
	for _, id := range identifiers {
		if id == d.String() || id == d.HostPath || id == d.GuestName {
			return true
		}
		// The guest name is also accepted with the /dev/ prefix people
		// naturally type when copying it out of an error message.
		if strings.TrimPrefix(id, "/dev/") == d.GuestName && strings.HasPrefix(id, "/dev/") {
			return true
		}
		// A spec whose host path needs expanding (~) won't match literally.
		if dev, err := parseSerialSpec(id); err == nil && dev == d {
			return true
		}
	}
	return false
}
