package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/vzdctl"
	"github.com/spf13/cobra"
)

var forwardListJSON bool

func init() {
	rootCmd.AddCommand(forwardCmd)
	forwardCmd.AddCommand(forwardAddCmd)
	forwardCmd.AddCommand(forwardRemoveCmd)
	forwardCmd.AddCommand(forwardAddReverseCmd)
	forwardCmd.AddCommand(forwardRemoveReverseCmd)
	forwardCmd.AddCommand(forwardListCmd)
	forwardListCmd.Flags().BoolVar(&forwardListJSON, "json", false,
		"emit JSON (the only supported mode; human path is 'clawk status')")
}

var forwardCmd = &cobra.Command{
	Use:     "forward",
	Aliases: []string{"fwd"},
	Short:   "Manage port forwards in both directions",
	Long: `Two directions, same HOST:GUEST spelling:

  add          a guest service, on the host's localhost  (applies on next up)
  add-reverse  a host service, on the guest's localhost  (applies immediately)`,
}

var forwardAddCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "add <sandbox> <port-spec> [port-spec...]",
	Short:             "Expose a guest port on the host (on 127.0.0.1)",
	Long: `Port specs:
  3000       — host port 3000 forwards to guest port 3000
  8080:80    — host port 8080 forwards to guest port 80

Changes take effect on the next 'clawk up' (down + up is enough for vz).`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sb, err := store.Load(args[0])
		if err != nil {
			return err
		}
		for _, spec := range args[1:] {
			fwd, err := parsePortSpec(spec)
			if err != nil {
				return err
			}
			if forwardExists(sb.Forwards, fwd) {
				fmt.Printf("  (already forwarded: %s)\n", fwd)
				continue
			}
			sb.Forwards = append(sb.Forwards, fwd)
			fmt.Printf("Forward added: %s\n", fwd)
		}
		if err := store.Save(sb); err != nil {
			return err
		}
		if status, _ := mustProviderStatus(sb); isRunning(status) {
			fmt.Println("(restart sandbox for new forwards to take effect)")
		}
		return nil
	},
}

var forwardRemoveCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "remove <sandbox> <port-spec> [port-spec...]",
	Aliases:           []string{"rm"},
	Short:             "Remove a host-to-guest forward",
	Args:              cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sb, err := store.Load(args[0])
		if err != nil {
			return err
		}
		drop := make(map[config.PortForward]bool)
		for _, spec := range args[1:] {
			fwd, err := parsePortSpec(spec)
			if err != nil {
				return err
			}
			drop[fwd] = true
		}
		var kept []config.PortForward
		for _, f := range sb.Forwards {
			if drop[f] {
				fmt.Printf("Forward removed: %s\n", f)
			} else {
				kept = append(kept, f)
			}
		}
		sb.Forwards = kept
		return store.Save(sb)
	},
}

var forwardAddReverseCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "add-reverse <sandbox> <port-spec> [port-spec...]",
	Aliases:           []string{"add-r"},
	Short:             "Expose a host loopback service on the guest's loopback",
	Long: `The inbound counterpart of 'forward add': a service bound to
127.0.0.1 on the host becomes reachable at the SAME address inside the
guest, which is what tools that assume a shared loopback need (the
JetBrains/VS Code Claude Code plugin's IDE websocket, a local API mock,
a database bound to loopback).

Port specs read host-side first, exactly like 'forward add':
  12345      — guest 127.0.0.1:12345 reaches host 127.0.0.1:12345
  5432:15432 — guest 127.0.0.1:15432 reaches host 127.0.0.1:5432

Unlike outbound forwards these apply to a running sandbox immediately —
no down/up cycle. Only the ports listed here are reachable; the guest
cannot dial the rest of the host's loopback.

vz (macOS) only: firecracker's vsock is one-way, so the guest has no
channel to connect back through.`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sb, err := store.Load(args[0])
		if err != nil {
			return err
		}
		changed := false
		for _, spec := range args[1:] {
			fwd, err := parsePortSpec(spec)
			if err != nil {
				return err
			}
			if forwardExists(sb.ReverseForwards, fwd) {
				fmt.Fprintf(cmd.OutOrStdout(), "  (already reverse-forwarded: %s)\n",
					describeReverse(fwd))
				continue
			}
			// A guest port can only be bound once, so a second spec claiming
			// it would silently lose to the first inside the guest. Reject it
			// here where we can name both.
			if prev, dup := reverseByGuestPort(sb.ReverseForwards, fwd.GuestPort); dup {
				return fmt.Errorf(
					"guest port %d is already reverse-forwarded to host port %d — remove that first",
					fwd.GuestPort, prev.HostPort)
			}
			sb.ReverseForwards = append(sb.ReverseForwards, fwd)
			changed = true
			fmt.Fprintf(cmd.OutOrStdout(), "Reverse forward added: %s\n", describeReverse(fwd))
		}
		if err := store.Save(sb); err != nil {
			return err
		}
		if changed {
			applyReverseForwards(cmd, sb.Name)
		}
		return nil
	},
}

var forwardRemoveReverseCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "remove-reverse <sandbox> <port-spec> [port-spec...]",
	Aliases:           []string{"rm-reverse", "rm-r"},
	Short:             "Remove a host-to-guest-loopback reverse forward",
	Args:              cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sb, err := store.Load(args[0])
		if err != nil {
			return err
		}
		drop := make(map[config.PortForward]bool)
		for _, spec := range args[1:] {
			fwd, err := parsePortSpec(spec)
			if err != nil {
				return err
			}
			drop[fwd] = true
		}
		var kept []config.PortForward
		for _, f := range sb.ReverseForwards {
			if drop[f] {
				fmt.Fprintf(cmd.OutOrStdout(), "Reverse forward removed: %s\n", describeReverse(f))
			} else {
				kept = append(kept, f)
			}
		}
		changed := len(kept) != len(sb.ReverseForwards)
		sb.ReverseForwards = kept
		if err := store.Save(sb); err != nil {
			return err
		}
		if changed {
			applyReverseForwards(cmd, sb.Name)
		}
		return nil
	},
}

// applyReverseForwards pushes the just-saved reverse-forward set into the
// running daemon, which relays it to the in-guest agent. Reports what
// happened but never fails the command: the store is already updated, so
// the worst case is that the edit lands on the next boot. Mirrors
// applyNetworkPolicy.
func applyReverseForwards(cmd *cobra.Command, name string) {
	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
	defer cancel()
	err := vzdctl.NewClient(vzdctl.SocketPath(store.VMDir(name))).ReloadForwards(ctx)
	switch {
	case err == nil:
		fmt.Fprintln(cmd.OutOrStdout(), "Applied to running sandbox.")
	case errors.Is(err, vzdctl.ErrNotRunning):
		fmt.Fprintln(cmd.OutOrStdout(), "Sandbox not running — applies on next 'clawk up'.")
	case errors.Is(err, vzdctl.ErrReverseForwardsUnsupported):
		fmt.Fprintln(cmd.OutOrStdout(),
			"Sandbox is running an older daemon — restart it ('clawk down && clawk up') to apply.")
	default:
		fmt.Fprintf(cmd.ErrOrStderr(),
			"clawk: live apply failed (%v) — applies on next 'clawk up'\n", err)
	}
}

// describeReverse spells out a reverse forward in the direction traffic
// actually flows, because the HOST:GUEST spec alone doesn't say which end
// listens. Worth the words: getting the direction backwards is the whole
// confusion this command exists to resolve.
func describeReverse(f config.PortForward) string {
	return fmt.Sprintf("guest 127.0.0.1:%d → host 127.0.0.1:%d", f.GuestPort, f.HostPort)
}

// reverseByGuestPort finds an existing reverse forward bound to guestPort.
func reverseByGuestPort(fs []config.PortForward, guestPort int) (config.PortForward, bool) {
	for _, f := range fs {
		if f.GuestPort == guestPort {
			return f, true
		}
	}
	return config.PortForward{}, false
}

var forwardListCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "list <sandbox>",
	Short:             "List configured port forwards (JSON-only — use 'clawk status' for human view)",
	Args:              cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// v2: human output for forwards lives inside `clawk status`.
		// `forward list` survives only as a JSON shim for scripts.
		// Without --json we exit 2 with a pointer rather than dump the
		// list to stdout — that way scripts that expect parseable output
		// fail loud instead of silently misparsing a flat string.
		if !forwardListJSON {
			return fmt.Errorf(
				"forward list is JSON-only — pass --json (or use 'clawk status %s' for human output)",
				args[0])
		}
		sb, err := store.Load(args[0])
		if err != nil {
			return err
		}
		out := struct {
			Schema   string              `json:"schema"`
			Sandbox  string              `json:"sandbox"`
			Forwards []statusJSONForward `json:"forwards"`
			// Additive: absent on older clawk, so a script that only
			// knows "forwards" keeps parsing unchanged.
			ReverseForwards []statusJSONForward `json:"reverse_forwards,omitempty"`
		}{Schema: "1", Sandbox: sb.Name}
		for _, f := range sb.Forwards {
			out.Forwards = append(out.Forwards, statusJSONForward{
				HostPort: f.HostPort, GuestPort: f.GuestPort,
			})
		}
		for _, f := range sb.ReverseForwards {
			out.ReverseForwards = append(out.ReverseForwards, statusJSONForward{
				HostPort: f.HostPort, GuestPort: f.GuestPort,
			})
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	},
}

// parsePortSpec accepts "PORT" or "HOST:GUEST" and returns a PortForward.
// Both ports must be 1..65535. Using string parsing (not Sscanf) so bad input
// like "80:80:80" or "foo:80" rejects with a clear error rather than silently
// ignoring trailing garbage.
func parsePortSpec(spec string) (config.PortForward, error) {
	parts := strings.Split(spec, ":")
	switch len(parts) {
	case 1:
		p, err := parsePort(parts[0])
		if err != nil {
			return config.PortForward{}, fmt.Errorf("port %q: %w", spec, err)
		}
		return config.PortForward{HostPort: p, GuestPort: p}, nil
	case 2:
		host, err := parsePort(parts[0])
		if err != nil {
			return config.PortForward{}, fmt.Errorf("host port %q: %w", parts[0], err)
		}
		guest, err := parsePort(parts[1])
		if err != nil {
			return config.PortForward{}, fmt.Errorf("guest port %q: %w", parts[1], err)
		}
		return config.PortForward{HostPort: host, GuestPort: guest}, nil
	default:
		return config.PortForward{}, fmt.Errorf("invalid port spec %q (want PORT or HOST:GUEST)", spec)
	}
}

func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("not a number: %w", err)
	}
	if n < 1 || n > 65535 {
		return 0, errors.New("out of range (1..65535)")
	}
	return n, nil
}

func forwardExists(fs []config.PortForward, f config.PortForward) bool {
	for _, e := range fs {
		if e == f {
			return true
		}
	}
	return false
}

// mustProviderStatus returns the live VM status, or the persisted state if
// no provider could be resolved. Used for user-facing hints only.
func mustProviderStatus(sb *config.Sandbox) (string, error) {
	p, err := providerFor(sb)
	if err != nil {
		return string(sb.VMState), err
	}
	return p.Status(sb)
}
