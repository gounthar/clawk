package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/google/renameio/v2"
)

// GuestMCPConfigPath is where clawk renders the sandbox's declared MCP
// servers inside the guest, and the path passed to the runner's
// --mcp-config flag (see cli.mcpConfigArgs).
//
// It deliberately sits inside ~/.claude/ rather than at either of the
// places the runner would find on its own:
//
//   - ~/.claude.json (user scope) is clawk's onboarding marker and carries
//     a documented concurrent-write race (anthropics/claude-code#28847).
//   - .mcp.json in a project root would land inside a git worktree mounted
//     from the host, i.e. clawk would be writing config into the user's
//     repo.
//
// Living under ~/.claude/ also means it arrives through the per-sandbox
// PersistentAgentShares mount with the rest of the seeded state, and the
// sessions history repo ignores it by construction — that gitignore denies
// everything (`/*`) and only re-admits transcripts and memory.
const GuestMCPConfigPath = GuestHome + "/.claude/mcp/clawk.json"

// mcpConfig is the on-disk shape the runner reads. Field names are the
// runner's, not clawk's.
type mcpConfig struct {
	MCPServers map[string]mcpServerConfig `json:"mcpServers"`
}

type mcpServerConfig struct {
	// Type is emitted for remote transports only. A stdio server is
	// identified by having a command, and omitting the key there matches
	// the shape the runner documents for `claude mcp add`.
	Type    string            `json:"type,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// RenderMCPConfig builds the guest MCP config for a sandbox's declared
// servers. Returns ok=false when there is nothing to declare.
//
// `${VAR}` references in headers and env values are passed through
// verbatim: the runner expands them against its own process environment
// when it connects, so no credential value is ever written here. That
// matters because this file is created on the HOST, inside the sandbox
// state dir — the same reason config.Sandbox.RequiredEnv stores names
// rather than values. The values reach the runner's environment through
// the vsock handshake instead (ResolveEnv → cli.buildVSockEnv).
//
// A stdio server's env is rendered as NAME=${NAME} for the same reason:
// clawk names the variable, the runner supplies the value.
func RenderMCPConfig(servers []config.MCPServer) ([]byte, bool, error) {
	if len(servers) == 0 {
		return nil, false, nil
	}
	cfg := mcpConfig{MCPServers: make(map[string]mcpServerConfig, len(servers))}
	for _, s := range servers {
		entry := mcpServerConfig{}
		switch s.Transport {
		case config.MCPTransportStdio:
			if len(s.Command) == 0 {
				return nil, false, fmt.Errorf("mcp server %q: stdio transport with no command", s.Name)
			}
			entry.Command = s.Command[0]
			entry.Args = s.Command[1:]
			if len(s.Env) > 0 {
				entry.Env = make(map[string]string, len(s.Env))
				for _, name := range s.Env {
					entry.Env[name] = "${" + name + "}"
				}
			}
		case config.MCPTransportHTTP, config.MCPTransportSSE:
			if s.URL == "" {
				return nil, false, fmt.Errorf("mcp server %q: %s transport with no URL", s.Name, s.Transport)
			}
			entry.Type = s.Transport
			entry.URL = s.URL
			if len(s.Headers) > 0 {
				entry.Headers = make(map[string]string, len(s.Headers))
				for _, h := range s.Headers {
					name, val, ok := strings.Cut(h, ":")
					if !ok {
						return nil, false, fmt.Errorf("mcp server %q: malformed header %q", s.Name, h)
					}
					entry.Headers[strings.TrimSpace(name)] = strings.TrimSpace(val)
				}
			}
		default:
			return nil, false, fmt.Errorf("mcp server %q: unknown transport %q", s.Name, s.Transport)
		}
		cfg.MCPServers[s.Name] = entry
	}
	// MarshalIndent sorts map keys, so the bytes are stable for a given
	// server list — this file is rewritten on every boot into a virtio-fs
	// mount, and a stable rendering keeps that a no-op write.
	content, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(content, '\n'), true, nil
}

// SeedClaudeMCP writes (or clears) the guest MCP config in the per-sandbox
// state dir that PersistentAgentShares mounts at ~/.claude/. Run by the
// provider during sandbox preparation, alongside SeedClaudeStateDir and
// before the VM boots — so the servers are in place for the runner's first
// connection attempt, with no post-boot step and no `on create` hook.
//
// Rewritten on every call, like settings.json: a clawk.mod edit propagates
// on the next `up`. An empty server list removes the file rather than
// leaving a stale one behind, so deleting an `mcp ( … )` entry actually
// retires the server.
//
// Note this only arranges the config. Whether a server then authenticates
// is up to the credential its headers/env reference — see
// config.MCPServer.
func SeedClaudeMCP(stateRoot string, servers []config.MCPServer) error {
	if stateRoot == "" {
		return nil
	}
	path := filepath.Join(stateRoot, "claude", "mcp", "clawk.json")
	content, ok, err := RenderMCPConfig(servers)
	if err != nil {
		return err
	}
	if !ok {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("clearing mcp config: %w", err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating mcp config dir: %w", err)
	}
	if err := renameio.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("seeding mcp config: %w", err)
	}
	return nil
}
