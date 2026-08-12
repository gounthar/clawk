package cli

import (
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strings"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/template"
)

// mergeMCP gathers `mcp (...)` entries from the workspace and every repo
// Clawkfile into the sandbox's server list.
//
// Same conflict rule as mergeFiles/mergeShares: two sources declaring the
// same server name identically collapse to one entry, but a genuine
// disagreement about what that name means is a config bug rather than
// something to silently tie-break — the agent would otherwise get whichever
// definition happened to be composed first.
func mergeMCP(ws *template.Workspace) ([]config.MCPServer, error) {
	var sources []mcpSource
	for _, s := range ws.File.MCP {
		sources = append(sources, mcpSource{Origin: "workspace", Spec: s})
	}
	for _, r := range ws.Repos {
		if r.Clawkfile == nil {
			continue
		}
		for _, s := range r.Clawkfile.MCP {
			sources = append(sources, mcpSource{Origin: r.Name, Spec: s})
		}
	}
	return composeMCP(sources)
}

type mcpSource struct {
	Origin string
	Spec   template.MCPSpec
}

// composeMCP is the pure conflict-detection core, split out so the here-mode
// path (a single Clawkfile) and the workspace path share it.
func composeMCP(sources []mcpSource) ([]config.MCPServer, error) {
	byName := make(map[string]struct {
		Origin string
		Server config.MCPServer
	})
	var out []config.MCPServer
	for _, s := range sources {
		srv := config.MCPServer{
			Name:      s.Spec.Name,
			Transport: s.Spec.Transport,
			URL:       s.Spec.URL,
			Command:   s.Spec.Command,
			Headers:   s.Spec.Headers,
			Env:       s.Spec.Env,
		}
		if prev, dup := byName[srv.Name]; dup {
			if sameMCPServer(prev.Server, srv) {
				continue
			}
			return nil, fmt.Errorf(
				"mcp server %q declared differently by %s (%s) and %s (%s) — rename one",
				srv.Name, prev.Origin, mcpTarget(prev.Server), s.Origin, mcpTarget(srv))
		}
		byName[srv.Name] = struct {
			Origin string
			Server config.MCPServer
		}{s.Origin, srv}
		out = append(out, srv)
	}
	return out, nil
}

// sameMCPServer reports whether two declarations of one server name mean the
// same thing, so repeated identical entries collapse instead of erroring.
func sameMCPServer(a, b config.MCPServer) bool {
	return a.Transport == b.Transport &&
		a.URL == b.URL &&
		slices.Equal(a.Command, b.Command) &&
		slices.Equal(a.Headers, b.Headers) &&
		slices.Equal(a.Env, b.Env)
}

// mcpTarget renders a server's endpoint for error messages: the URL for a
// remote server, the command for a stdio one.
func mcpTarget(s config.MCPServer) string {
	if s.URL != "" {
		return s.URL
	}
	return strings.Join(s.Command, " ")
}

// appendMissingMCP folds add into have, keeping entries already present by
// name. Used to layer the namespace's servers under a sandbox's own, matching
// how appendMissingShares treats namespace shares: the narrower scope wins a
// name collision.
func appendMissingMCP(have, add []config.MCPServer) []config.MCPServer {
	seen := make(map[string]bool, len(have))
	for _, s := range have {
		seen[s.Name] = true
	}
	for _, s := range add {
		if seen[s.Name] {
			continue
		}
		seen[s.Name] = true
		have = append(have, s)
	}
	return have
}

// mcpNetworkBlock derives the egress a sandbox's declared MCP servers need:
// one allow entry per distinct http/sse host. Without it every remote server
// would be refused by the default policy and the user would have to mirror
// each declaration with a `network allow` line — the failure mode being a
// bare "ConnectionRefused" from inside the guest, far from its cause.
//
// Loopback targets are skipped: a server on the guest's own 127.0.0.1 (or a
// host service reached through a reverse forward) never leaves the VM, so
// there is nothing to allow. Literal IPs land in AllowIPs and hostnames in
// AllowDomains, matching how the two are enforced — SYN-time for addresses,
// DNS-time for names.
//
// Returns ok=false when nothing needs allowing, so callers don't append an
// empty block to the policy.
func mcpNetworkBlock(servers []config.MCPServer) (config.NetworkBlock, bool) {
	block := config.NetworkBlock{Origin: config.BlockOriginMCP, Name: "mcp servers"}
	for _, s := range servers {
		if s.URL == "" {
			continue // stdio: a local process, no egress of its own
		}
		u, err := url.Parse(s.URL)
		if err != nil {
			continue // validated at parse time; nothing to derive if it ever isn't
		}
		host := u.Hostname()
		if host == "" || host == "localhost" {
			continue
		}
		if addr, err := netip.ParseAddr(host); err == nil {
			if addr.IsLoopback() {
				continue
			}
			block.AllowIPs = append(block.AllowIPs, host)
			continue
		}
		block.AllowDomains = append(block.AllowDomains, host)
	}
	block.AllowDomains = dedupStrings(block.AllowDomains)
	block.AllowIPs = dedupStrings(block.AllowIPs)
	if len(block.AllowDomains)+len(block.AllowIPs) == 0 {
		return config.NetworkBlock{}, false
	}
	return block, true
}

// applyMCPNetwork installs (or refreshes) the derived allow layer on a
// sandbox's policy. Idempotent: the block is rebuilt from the current server
// list every time, so removing a server from clawk.mod and re-creating drops
// its allow entry rather than leaving it behind.
func applyMCPNetwork(sb *config.Sandbox) {
	block, ok := mcpNetworkBlock(sb.MCP)
	sb.Network.Blocks = slices.DeleteFunc(sb.Network.Blocks,
		func(b config.NetworkBlock) bool { return b.Origin == config.BlockOriginMCP })
	if ok {
		sb.Network.Blocks = append(sb.Network.Blocks, block)
	}
	// Restore the origin-order invariant the store relies on.
	sb.Network.Normalize()
}
