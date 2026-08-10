# Networking

Egress is allowlist-only on both providers. DNS resolves everything, but
connections to unlisted hosts — TCP, UDP (including QUIC/HTTP-3), and ICMP
echo — are refused. Common registries (npm, PyPI, crates.io, GitHub,
Anthropic, etc.) and language toolchains are pre-allowed.

```sh
clawk network allow  <sandbox> example.com '*.cdn.example.com' 10.0.0.5 192.168.0.0/24
clawk network block  <sandbox> tracker.example.com
clawk network remove <sandbox> example.com tracker.example.com   # delete rules of either kind
clawk network list   [<sandbox>]
clawk network denials <sandbox>   # what the ACL blocked, by hostname
```

Two verbs state intent, one deletes it: `allow` grants a destination,
`block` refuses a domain and all its subdomains outright (overriding any
allow, without prompting), and `remove` deletes a rule whichever kind it
is — a removed allow goes back to the default deny-with-prompt behavior,
a removed block stops being auto-denied.

`allow` takes any mix of domains, literal IPs, and CIDR ranges (quote
wildcard patterns so your shell doesn't expand them). A grant is
destination-only: it covers every protocol — TCP, UDP (including
QUIC/HTTP-3), and ICMP — on every port, so allow `example.com`, not
`https://example.com:443`.

Edits apply to a running sandbox immediately, or on the next `up` if it's
down. Blocked connections are recorded by the hostname the guest resolved —
`clawk network denials` (or the `Blocked` line in `clawk status`) shows what to
allow. Both providers enforce the same allow-list.

## Policies and `use` chains

Egress policy layers named, reusable **policies** under a sandbox's own
rules. A `use` line inside `network ( … )` lists the policies a sandbox
builds on, lowest precedence first; the file's inline `allow`/`deny`
entries sit above them, and runtime edits/grants above those. **No `use`
line means `use default`** (the built-in dev allowlist). Writing one makes
the chain fully explicit — include `default` where you want it, or omit it
to opt out of the built-ins entirely.

```text
network (
    use   default oisd corp-egress   # corp-egress overrides oisd overrides default
    allow api.stripe.com             # inline entries override everything in `use`
)

policy oisd (
    source  "https://big.oisd.nl/domainswild"
    refresh 24h
)
```

A `policy <name> ( … )` block carries inline `allow` / `deny` entries
and/or a `source "<url>"` blocklist (hosts / EasyList / uBlock formats —
`@@` exception lines become allows) refetched when older than `refresh`.
Policies declared in a `clawk.mod` register automatically when a sandbox is
created from it; `deny source "<url>"` inline stays supported as sugar for
an anonymous sourced policy. Day-two verbs:

```sh
clawk policy list
clawk policy show <name>
clawk policy refresh <name>
clawk policy delete <name>
```

`clawk apply -f <file-or-dir>` registers `policy` and `namespace` blocks
from manifest files (same grammar, no sandbox created). A directory
applies every file independently — one broken manifest is reported by name
without stopping the rest.

## Port forwarding

Port forwarding is explicit (binds your `localhost:<port>` to the guest):

```sh
clawk forward add <sandbox> 3000        # host 3000 → guest 3000
clawk forward add <sandbox> 8080:80     # host 8080 → guest 80
clawk forward list [<sandbox>]
```

Inside the VM, bind dev servers to `0.0.0.0`, not `127.0.0.1` — the loopback
interface is not visible to the host.

Note that an idle-stopped VM's port forwards go away until the next boot —
give a sandbox that must keep serving `idle_timeout off` (see
[Commands & resource usage](commands.md#resource-usage)).

### Reverse forwarding (host loopback → guest)

The other direction: a service bound to `127.0.0.1` on your Mac, reachable
at the *same address* inside the guest. Allow-listing its IP doesn't help —
`127.0.0.1` in the guest is the guest's own loopback, and there is no route
from there to yours.

```sh
clawk forward add-reverse    <sandbox> 63342        # guest 127.0.0.1:63342 → host 127.0.0.1:63342
clawk forward add-reverse    <sandbox> 5432:15432   # guest 127.0.0.1:15432 → host 127.0.0.1:5432
clawk forward remove-reverse <sandbox> 63342
```

Specs read host-side first in both directions, so `5432:15432` names the
same pair of ports whichever verb you use — only who dials whom changes.

Two things differ from outbound forwards:

- **They apply immediately.** Outbound forwards are a gvproxy binding fixed
  at VM start; reverse forwards are tunnelled over vsock by the daemon and
  pushed to the running guest, so no `down`/`up` cycle is needed.
- **Only the listed ports are reachable.** The guest names a port, never an
  address, and the host refuses one that isn't configured — so this opens
  exactly the holes you asked for, not your whole loopback.

vz only. firecracker's vsock is one-way (guest listens, host dials), so
there is nothing for the guest to connect back through; the CLI says so
rather than silently doing nothing.

Reverse forwards can also be declared in `clawk.mod` — see
[Configuration](configuration.md#reference).

### Recipe: the Claude Code IDE plugin

The JetBrains and VS Code plugins run a websocket server on the host's
loopback and advertise it in `~/.claude/ide/<port>.lock`. A `claude` running
inside a sandbox needs two things to find it — the lock file, and a route to
the port:

```sh
# 1. share the host's lock-file dir into the guest (clawk.mod, or clawk apply)
#    shares (
#        ~/.claude/ide  /home/agent/.claude/ide  ro
#    )

# 2. reverse-forward the port the lock file names
ls ~/.claude/ide                                  # 63342.lock
clawk forward add-reverse my-project 63342
```

Then `/ide` inside the sandbox connects as it would on the host. The port is
per-IDE-window and changes when the IDE restarts; because reverse forwards
apply live, re-running `add-reverse` with the new port is enough — no
sandbox restart.

Note that the guest's `~/.claude` is the sandbox's own state directory, not
your host `~/.claude`; the share above is what puts the host's lock files
where the guest's `claude` looks.
