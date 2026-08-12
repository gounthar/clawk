package cli

import (
	"fmt"
	"sort"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
)

// Agent describes how a coding-agent runner is launched inside a
// sandbox. Treating each agent (claude, codex, pi, opencode, ...) as a
// data-driven entry rather than a per-agent code path keeps the CLI
// surface symmetric: every agent's verb works the same way, and adding
// another runner is a one-line registry change.
type Agent struct {
	// Name is the user-facing runner argument (`clawk run <name>`).
	// Must be unique. Lowercase ASCII, no whitespace. Also the command
	// resolved on the guest agent's PATH (which carries the image
	// config's Env), so it lands wherever the image installed the tool.
	Name string

	// DefaultArgs are prepended to whatever the user passes through
	// `-- <args>`. Every runner that can be told "you are already
	// externally sandboxed" is told so here: claude and codex have
	// explicit modes for it, pi has project trust.
	DefaultArgs []string

	// MCPConfigFlag is the runner's flag for loading an external MCP
	// server config file, or "" if clawk doesn't know how to hand this
	// runner one. When set and the sandbox declares servers, the flag and
	// sandbox.GuestMCPConfigPath are appended to the launch args.
	//
	// A per-runner flag rather than a line in DefaultArgs because the
	// value is conditional (a sandbox with no `mcp ( … )` block must not
	// get a flag pointing at a file that isn't there) and because each
	// runner spells this differently — codex and opencode use their own
	// config formats and pi loads MCP through an extension, so they stay
	// empty until someone renders those too.
	MCPConfigFlag string
}

// agents is the builtin runner registry. `clawk run <name>` consults
// this list; anything not here is rejected with the available names.
var agents = []Agent{
	{
		Name:        "claude",
		DefaultArgs: []string{"--dangerously-skip-permissions"},
		// Not --strict-mcp-config: that would suppress every other MCP
		// source, including the claude.ai account connectors that already
		// work inside a sandbox (they're proxied server-side, so they need
		// no local credential and no egress of their own) and any plugin
		// the user's settings enable.
		MCPConfigFlag: "--mcp-config",
	},
	{
		Name:        "codex",
		DefaultArgs: []string{"--dangerously-bypass-approvals-and-sandbox"},
	},
	{
		Name: "pi",
		// pi has no approval prompts to bypass — it ships no built-in
		// sandbox at all, by design (its docs/security.md: "Real isolation
		// needs to come from the operating system or a virtualization
		// boundary", which is exactly what a clawk VM is). What it does
		// gate is project trust: a repo carrying .pi/settings.json,
		// .pi/extensions or .agents/skills triggers an interactive
		// "trust this project?" prompt before those load, and answering it
		// is meaningless inside a VM whose whole premise is that the
		// project already has the machine. --approve trusts the project for
		// the run, the same posture as claude's and codex's flags above.
		DefaultArgs: []string{"--approve"},
		// No MCPConfigFlag: pi speaks MCP through an extension
		// (pi-mcp-adapter), not a config-file flag, so a sandbox with an
		// `mcp ( … )` block gets nothing here until that's rendered too.
	},
	{
		Name: "opencode",
		// opencode gates every tool call behind a permission prompt unless
		// told otherwise; --auto approves anything not explicitly denied
		// (its own help says "dangerous!", which is true on a laptop and
		// beside the point inside a disposable VM). A deny rule in the
		// user's opencode.jsonc still wins, so this widens the default
		// without overriding a considered choice.
		DefaultArgs: []string{"--auto"},
		// No MCPConfigFlag: opencode manages MCP servers through its own
		// config file and `opencode mcp` subcommand, not a flag taking a
		// path, so a sandbox's `mcp ( … )` block doesn't reach it yet.
	},
}

// launchArgs assembles the runner's argv tail: its permission-mode
// defaults, the MCP config flag when this sandbox declares servers and the
// runner knows how to take one, then the user's `-- <args>` last so an
// explicit flag always sits after (and therefore overrides) clawk's.
//
// Both attach paths — the vsock handshake and the `bash -lc` exec fallback
// — go through here, so a runner never gets a different command line
// depending on which transport happened to be available.
func launchArgs(sb *config.Sandbox, agent Agent, extra []string) []string {
	args := append([]string{}, agent.DefaultArgs...)
	if agent.MCPConfigFlag != "" && len(sb.MCP) > 0 {
		args = append(args, agent.MCPConfigFlag, sandbox.GuestMCPConfigPath)
	}
	return append(args, extra...)
}

// agentByName returns the registry entry for a given runner name.
// Used by `clawk run <runner>` dispatch.
func agentByName(name string) (Agent, error) {
	for _, a := range agents {
		if a.Name == name {
			return a, nil
		}
	}
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		names = append(names, a.Name)
	}
	sort.Strings(names)
	return Agent{}, fmt.Errorf("unknown agent %q (have: %v)", name, names)
}

// reservedAgentNames returns the set of names that cannot be used as
// sandbox names — they collide with runner names. Used by sandbox-name
// validation when a user runs `clawk work <branch>`.
func reservedAgentNames() []string {
	out := make([]string, 0, len(agents))
	for _, a := range agents {
		out = append(out, a.Name)
	}
	return out
}
