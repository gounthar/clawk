# MCP servers

Declare the MCP servers a project needs and every sandbox created from that
`clawk.mod` comes up with them configured — no per-sandbox setup step, no
interactive login inside the VM.

```text
sandbox my-project (
    mcp (
        linear https://mcp.linear.app/mcp  header "Authorization: Bearer ${LINEAR_TOKEN}"
        github stdio "npx -y @modelcontextprotocol/server-github"  env GITHUB_TOKEN
    )

    env (
        LINEAR_TOKEN = ${LINEAR_TOKEN:?create a Linear PAT and export it}
        GITHUB_TOKEN
    )
)
```

That is the whole setup. On `clawk up`, clawk renders the server list into
the guest before the VM boots, allows each server's host in the egress
policy, and hands the runner the config file — so the agent's first tool
call works.

## Line shapes

| Written as | Transport |
| --- | --- |
| `name https://host/mcp` | `http` (the default) |
| `name http https://host/mcp` | `http`, explicit |
| `name sse https://host/sse` | `sse` |
| `name stdio "cmd --flag arg"` | `stdio`, a local process |

Modifiers repeat and may be combined on one line:

- `header "Name: value"` — an extra HTTP header, for `http` / `sse`.
- `env NAME` — an environment variable handed to a `stdio` server.

## Credentials: use a token, not a browser

Only static credentials are supported, and that's deliberate: a personal
access token is a value that can be in place *before* the VM boots, which is
what makes a fresh sandbox usable immediately. Interactive OAuth
(`claude mcp login`) can't be arranged ahead of time — it stores a grant
inside one guest, so every new sandbox would need its own login.

Most services that offer an MCP endpoint also issue a PAT. Prefer it.

Credential **values** never enter clawk's config or state:

- `clawk.mod` holds a `${VAR}` reference; so does the config clawk renders
  into the sandbox state dir on the host.
- The value is read from your shell at attach time and delivered straight to
  the runner's process environment over the vsock handshake, where the
  runner expands `${VAR}` as it connects.

Declare the variable in `env ( … )` so it's carried. The `${VAR:?message}`
form is worth using: a missing PAT then fails sandbox creation with your
message instead of producing a server that quietly returns 401 mid-task.

A URL with credentials in it — `https://user:token@host/mcp` — is rejected
for the same reason. The URL is stored verbatim on the sandbox record and in
the rendered guest config, both unencrypted on host disk, so it is the one
spelling that would defeat the guarantee above. Move the credential to a
`header` and it stays in the runner's environment.

## Servers that need no declaration

If a service is available as a **claude.ai connector**, use that instead of
declaring the server here. Connectors are authorized once against your
Anthropic account and proxied server-side, so they reach a sandbox with no
local credential, no config, and no egress rule of its own — they ride the
token clawk already forwards. Check with `claude mcp list` inside a sandbox;
anything listed as `claude.ai <Name>` is already working.

Declare a server in `mcp ( … )` when there is no connector for it, or when
you want to pin a specific endpoint.

## Scopes

`mcp ( … )` is valid in a repo `clawk.mod`, a workspace root, and a
`namespace` block. They merge by server name, narrowest scope winning — so a
namespace is the place for the org-wide set, and which namespace a sandbox
belongs to then decides what it can reach:

```text
namespace acme (
    mcp (
        linear https://mcp.linear.app/mcp  header "Authorization: Bearer ${LINEAR_TOKEN}"
    )
)
```

Two repos in one workspace declaring the same server identically is fine.
Declaring the same *name* with different targets is rejected, naming both
sources — clawk won't silently pick one.

## Egress

Every declared `http` / `sse` host is allowed automatically, in a network
block of origin `mcp`. It sits at the bottom of the precedence chain, just
above the namespace layer: a `deny` you write in `clawk.mod` or via
`clawk network deny` still wins, so the derivation is a convenience and
never a way around your own rules.

`stdio` servers get no allow — they're a local process. They may still need
egress of their own for whatever they talk to, and one for the package
registry if the command is an `npx`-style fetch (already in the default
allowlist).

## Notes

- The rendered config lives at `~/.claude/mcp/clawk.json` in the guest,
  passed to the runner with `--mcp-config`. It is not `.mcp.json` in your
  repo (clawk won't write into your worktree) and not `~/.claude.json`
  (concurrent-write races, and clawk uses it as the onboarding marker).
- Editing `mcp ( … )` takes effect on the next `clawk up` — the file is
  rewritten every boot. Removing an entry retires the server.
- clawk does not pass `--strict-mcp-config`, so claude.ai connectors and any
  plugin MCP servers your settings enable keep working alongside these.
- Only the `claude` runner is wired today. `codex` and `opencode` use their
  own MCP config formats and `pi` loads MCP through an extension rather than
  a flag; a sandbox declaring servers simply doesn't get a config flag for
  those runners.
- An `npx`-style `stdio` server downloads on first use in each fresh VM. Put
  the fetch in `on create ( … )` if you want the first tool call to be fast.
