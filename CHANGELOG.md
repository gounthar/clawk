# Changelog

Notable user-facing changes, newest first. clawk is pre-1.0: the CLI surface
is stable, internals move fast. Format follows
[Keep a Changelog](https://keepachangelog.com/); versions follow semver once
tagged.

## Unreleased

## v0.4.0

### Added

- **Host-wide defaults: `~/.config/clawk/clawk.mod`.** One file outside any
  repo whose anonymous `sandbox ( … )` block supplies settings for every
  sandbox on the machine — including repos with no `clawk.mod` at all. It is
  the lowest layer of the chain (built-in defaults < host-wide < namespace <
  repo `clawk.mod` < `clawk.mod.<profile>` < flags): lists union with the
  host-wide entries first, scalars apply only where nothing narrower declared
  one. Read at create time like every other template, so editing it shapes the
  next sandbox rather than retro-modifying existing ones.
  ([#14](https://github.com/clawkwork/clawk/issues/14))

  The point is that a kernel path, a token alias, a personal skill mount and a
  house rule about commit messages are properties of the host, not of the repo
  they happened to be written in — and committing them puts an absolute
  `/Users/you/...` path in a shared file.

  Location resolves `$CLAWK_GLOBAL_MOD` (explicit, must exist) →
  `$XDG_CONFIG_HOME/clawk/clawk.mod` → `~/.clawk/clawk.mod` (compatibility).
  `~/.config` is the documented home because this is the one file in clawk's
  footprint you hand-edit and may symlink out of a dotfiles repo, whereas
  `~/.clawk` is disposable machine state — VM disks, an image cache, a live
  OAuth token — that people exclude from backups and delete to start clean.
  Both present is an error, not a silent pick.

  Scope is enforced: the block must be anonymous (a header name labels one
  repo's phases, so a name here would silently relabel every repo), `includes`
  and the unwired `on down` / `on enter` are rejected, `policy` blocks are
  accepted as a personal policy library, and `namespace` blocks are refused —
  the file declares defaults for every sandbox, not named resources, and clawk
  owns those records itself. Relative paths resolve against the file's own
  directory. `--no-global` drops the layer for a reproducible run.

- **Serial devices.** `clawk serial add <sandbox> /dev/cu.usbmodem1101` puts a
  serial port plugged into your Mac inside the sandbox as `/dev/<name>`, so
  `arduino-cli`, `esptool`, `avrdude` and a serial monitor can run in there
  against real hardware. Declarable in `clawk.mod` as `serial ( … )`, applied
  live like reverse forwards, and vz-only for the same reason.

  The USB device is not passed through, because nothing clawk runs on can do
  that: Virtualization.framework's USB controller carries virtual
  mass-storage devices only, and firecracker has no USB at all. What crosses
  is the byte stream and the line settings — which is all any of those tools
  ever wanted from a serial port. The guest side is a PTY; the host side is
  the real tty, opened only while a process in the sandbox holds the device,
  so the board stays available to the Mac the rest of the time.

  Tying the host's open to the guest's open is also what makes auto-reset
  work: opening a serial port asserts DTR, which is the pulse a board's reset
  circuit is waiting for. A PTY carries the baud rate too, so the 1200-baud
  touch that native-USB boards use to enter their bootloader survives the
  trip. What a PTY cannot carry is a modem-control line under direct control
  — a plain ESP32 DevKit that needs DTR and RTS driven in sequence still
  needs its BOOT button. [docs/serial.md](docs/serial.md) has the full table.

  A device may be named by a glob (`'/dev/cu.usbmodem*:ttyACM0'`), resolved
  each time the port is opened rather than when it is configured, so a board
  that re-enumerates into its bootloader under a neighbouring name stays
  reachable. The guest-side name never changes, so `-p /dev/ttyACM0` keeps
  working across the whole flash cycle.

- **Sandboxes now boot with swap.** Every sandbox gets a swap device — 2 GiB
  by default, sized with `vm ( swap <size> )` and disabled with
  `vm ( swap off )`.

  The reason is the balloon controller, not the guest's appetite. Under host
  memory pressure clawk takes RAM back from a guest against its demand (75%
  of the ceiling at WARN, 50% at CRITICAL), and a guest with nowhere to put
  cold anonymous pages answers that with direct-reclaim stalls and, at the
  limit, its OOM killer. A multi-second stall in the agent process is not
  merely slow: a process that stops draining its socket lets the connection
  go idle, and on a link whose NAT reaps idle mappings in well under a minute
  — a phone hotspot, a hotel network — that ends the streaming API response
  outright. Swap turns the stall into paging.

  It rides on its own sparse virtio-blk device rather than a swapfile on the
  rootfs, because `swapon(2)` rejects a file with holes: a swapfile would
  cost its full size in real host bytes at first boot, per sandbox. The
  device costs only the pages actually swapped. Note that nothing punches
  those holes back — no virtio-blk in the stack advertises discard yet — so
  read the usage as a high-water mark until the sandbox is destroyed.

  The balloon controller learned about swap at the same time, and had to:
  paging out cold pages raises `MemAvailable` and lowers PSI, so a guest that
  answered a squeeze by swapping reads as *roomy* on both of the signals the
  controller uses. Left alone it would have reclaimed another step, and
  parked working guests at their baseline, swapping, for good.

- **`mcp ( … )`: MCP servers ready on first boot.** Declare the servers a
  project needs and every sandbox created from that `clawk.mod` comes up with
  them configured — no per-sandbox setup, no interactive login inside the VM:

  ```text
  mcp (
      linear https://mcp.linear.app/mcp  header "Authorization: Bearer ${LINEAR_TOKEN}"
      github stdio "npx -y @modelcontextprotocol/server-github"  env GITHUB_TOKEN
  )
  ```

  Declaring a server also allows its host in the egress policy, in a derived
  `mcp` layer that ranks below anything you wrote yourself — so a remote
  server doesn't need a matching `network allow`, and doesn't get to
  override your own `deny`. Valid in a repo file, a workspace root and a
  `namespace` block, merging by name with the narrowest scope winning, so a
  namespace can carry the org-wide set.

  Credential values stay out of clawk entirely: `clawk.mod` and the rendered
  guest config hold a `${VAR}` reference, and the value travels from your
  shell to the runner's process environment at attach time. Pair it with
  `env ( TOKEN = ${TOKEN:?message} )` and a missing PAT fails sandbox
  creation with your message instead of surfacing as a 401 mid-task. Static
  credentials only — that's what can be in place before the VM boots. See
  [docs/mcp.md](docs/mcp.md).

- **The pi coding agent is a built-in runner.** `clawk run pi` attaches
  [pi](https://github.com/earendil-works/pi) the same way `clawk run claude`
  and `clawk run codex` do, and the `clawk-dev` image ships it. `pi.dev` (its
  model catalog and version check) joins the default network allow-list, and
  its `~/.pi/` is persisted per sandbox like the other runners'.

  The image also gains `fd-find`. pi's search tools probe PATH for `rg` and
  `fd`/`fdfind` and download GitHub release tarballs when they're missing —
  the image had ripgrep but not fd, so every fresh sandbox printed "fd not
  found. Downloading..." on pi's first run. Debian ships the binary as
  `fdfind`, one of the names pi probes for, so the package alone settles it.

- **opencode is a fully wired runner.** It was in the registry but nothing
  else: not installed in `clawk-dev`, and not persisted. Both are fixed, so
  `clawk run opencode` works out of the box and its sessions and login
  survive `down`/`up` and `destroy` like every other runner's.

  It launches with `--auto` (approve anything not explicitly denied); a deny
  rule in your `opencode.jsonc` still wins, and `--safe` drops the flag.
  `opencode.ai` joins the default network allow-list, alongside the
  `models.dev` entry it already had.

  opencode is the one runner needing two state mounts: it follows the XDG
  split rather than keeping a single home, so `~/.local/share/opencode`
  (auth.json, mcp-auth.json, opencode.db, repos/) and `~/.config/opencode`
  are mounted separately. Its `~/.local/state/opencode` and
  `~/.cache/opencode` deliberately stay on the disposable rootfs — the
  former holds only lock files, and a lock outliving the VM that took it is
  worse than none.

  Note the image grows by roughly 180 MB: `opencode-ai` resolves a prebuilt
  platform binary rather than shipping JS. Drop it from the Dockerfile's npm
  line for a slimmer variant.

  pi launches with `--approve`. It has no approval prompts to bypass — it
  ships no built-in sandbox at all, by design, on the grounds that real
  isolation has to come from a VM — but it does gate project-local
  `.pi/settings.json` and `.pi/extensions` behind an interactive trust
  prompt, and answering that inside a sandbox whose whole premise is that
  the project already has the machine is theatre. `--safe` drops the flag
  like it does for the other runners. A sandbox declaring `mcp ( … )` gets
  no config flag for pi: pi loads MCP through an extension rather than a
  config file.

### Fixed

- Workspace-position `env ( … )` and `agent ( … )` blocks were silently
  dropped: only a repo's own `clawk.mod` fed the sandbox's required-env set and
  agent instructions, so a workspace root declaring `env ( GITHUB_TOKEN )` got
  nothing. They now apply, ordered scope-outward (workspace, then namespace,
  then repo).

- **An `env ( … )` declaration now overrides clawk's own variables.** The
  vsock handshake emitted clawk's vars — `CLAUDE_CODE_OAUTH_TOKEN`,
  `COLORTERM`, the terminal-identification set — *after* the sandbox's
  declared env, so they won on the last-write-wins fold in the guest agent
  and a `clawk.mod` declaration of the same name was silently discarded.

  The two other delivery paths already disagreed with it: `/etc/profile.d`
  sources `99-clawk-env.sh` after `98-clawk-claude-oauth.sh`, so login
  shells and the `bash -lc` exec fallback have always let the declaration
  win. Only the default path — the one `clawk run claude` uses — did not.

  The case that needs it: a sandbox with `ANTHROPIC_BASE_URL` pointed at a
  gateway that needs no credential of its own. Claude Code falls back to
  `CLAUDE_CODE_OAUTH_TOKEN` when `ANTHROPIC_AUTH_TOKEN` is unset, so clawk's
  `sk-ant-oat-…` was sent to that third-party endpoint as an
  `Authorization: Bearer` header — a token leak, and one no clawk.mod could
  prevent. `env ( CLAUDE_CODE_OAUTH_TOKEN = "" )` now expresses it per
  sandbox; previously the only lever was `clawk auth clear`, which disarms
  every sandbox on the host. clawk still supplies all of these when the
  sandbox says nothing about them.

- **Codex no longer loses its history on `clawk down && clawk up`.** Only
  `~/.claude` was mounted from the host; `~/.codex` lived on the VM rootfs,
  which vz re-clones from the image master on *every* boot. So codex's
  sessions, `history.jsonl` and login were discarded by a plain stop/start —
  not just by `clawk destroy` — even though the docs promised
  `state/<name>/codex/` was mounted at `~/.codex/`. Each runner's home
  directory is now a per-sandbox host mount under
  `~/.clawk/namespaces/<ns>/state/<name>/` — `~/.claude`, `~/.codex`,
  `~/.pi`, and opencode's two XDG directories — so `--resume` works across
  stops and recreates for all of them.

  Existing sandboxes pick this up on their next boot; history from before
  the fix was on the disposable disk and is not recoverable. Note that this
  costs four more virtio devices on every sandbox against the 32-device
  PCIe ceiling: a large multi-repo workspace that booted on v0.3.0 may now
  be refused at start, with a message naming the count and what to cut.

- **`env ( … )` vars now reach the agent process, not just its shells.** The
  pty agent spawns the runner directly, so `/etc/profile.d/99-clawk-env.sh`
  was never sourced for it: declared variables were visible to a login shell
  the agent started, but absent from the agent's own environment. Anything
  reading it saw nothing — including `${VAR}` expansion in MCP config, where
  a forwarded token would silently become an empty header. The values now
  ride the vsock handshake alongside `CLAUDE_CODE_OAUTH_TOKEN`, resolved from
  your shell on every dispatch (so rotating a token needs no VM cycle), and
  layered so that clawk's own vars fill in only the names your `clawk.mod`
  didn't claim (see the entry above). As a side effect the agent's shell tool
  sees `GITHUB_TOKEN` and friends without `bash -lc`.

## v0.3.0

### Added

- **Reverse port forwarding: host loopback services, reachable in the guest.**
  `clawk forward add-reverse <sandbox> 63342` makes whatever is bound to
  `127.0.0.1:63342` on your Mac answer at the same address inside the sandbox
  (`5432:15432` maps across ports, host-side first, same as `forward add`).
  Allow-listing couldn't do this — `127.0.0.1` in the guest is the guest's own
  loopback — so the guest agent binds the port and tunnels each connection to
  the daemon over vsock, which dials the host service. Only the ports you list
  are reachable; the rest of your loopback isn't.

  Unlike outbound forwards these apply to a running sandbox immediately, which
  matters for the case that motivated it: the Claude Code IDE plugins
  advertise a per-window websocket port in `~/.claude/ide/<port>.lock`, so
  reconnecting after an IDE restart is one `add-reverse`, not a VM cycle.
  Share `~/.claude/ide` into the guest and `/ide` works from inside the
  sandbox — recipe in [docs/networking.md](docs/networking.md#recipe-the-claude-code-ide-plugin).

  Declarable in `clawk.mod` as a `reverse` entry inside `forwards ( … )`.
  `clawk status` and `forward list --json` show both directions. vz only:
  firecracker's vsock is one-way, and the CLI says so rather than silently
  doing nothing.

- **Linux firecracker sandboxes boot without sudo.** Each sandbox's network
  now lives in its own unprivileged user + network namespace, where clawk has
  `CAP_NET_ADMIN` over its own bridge and TAPs without asking the host for
  anything — so a boot performs no privileged operation, and no clawk
  interfaces appear on the host at all. gvproxy stays in the host namespace
  (that is where its egress sockets belong); only the VM's NIC moves, which
  also means the VM no longer sees host networking.

  Hosts that forbid unprivileged user namespaces fall back to the previous
  sudo-based bridge mode. `clawk doctor` reports which mode is active under
  `host: network mode` and names the setting to change (on Ubuntu 24.04+,
  AppArmor's `kernel.apparmor_restrict_unprivileged_userns`).
  `CLAWK_NET_MODE=rootless|bridge` pins one.

- **Release binaries no longer need a Go toolchain.** Booting a sandbox
  compiled the three in-guest binaries on the host first, which made `go` a
  hard prerequisite on every platform — `clawk doctor` failed without it — and
  put a module download in the first-boot path. Release artifacts now embed
  those binaries (`make guestbin`), so an installed clawk just unpacks them.
  Building from source still compiles them on first boot, which is what a
  contributor editing the guest sources wants; `doctor` reports the toolchain as
  required only in that case. Costs ~7 MiB of binary size per artifact.

- **Linux release binaries.** `linux/amd64` and `linux/arm64` tarballs are built
  and published alongside the macOS one, so the firecracker provider no longer
  requires building from source. Each artifact embeds the in-guest binaries for
  its own architecture — guest arch always equals host arch, since hardware
  virtualization can't cross architectures.

- **`env ( … )` gains aliases, defaults, and literals.** Entries now use
  shell / docker-compose parameter-expansion syntax: `NAME = ${HOST}` aliases
  a differently-named host variable, `${HOST:-default}` / `${HOST-default}`
  supply a fallback, `${HOST:?message}` / `${HOST?message}` make a variable
  required (failing sandbox creation with a message when missing), and a bare
  or quoted right-hand side (`EDITOR = vim`) sets a literal constant. Bare
  `NAME` passthrough is unchanged.
- **Workspace-level `on up` / `on create` hooks.** A workspace root (a
  `clawk.mod` whose sandbox block has `includes`) may now declare `on up` and
  `on create`, which run once at the guest workspace root — before each repo's
  own hooks — the VM-wide slot for setup shared across every repo (a swapfile,
  a global toolchain). Previously both were rejected there as repo-local.
  `on down` / `on enter` stay per-repo (they're reserved and not wired yet).
- **`vm ( disk <size> )` sets the root-disk ceiling.** Same unit rules as
  `memory`, minimum 1 GiB, and — like `cpu`/`memory` — resolved as the max
  across a workspace and its repos, since one rootfs is shared by every phase.
  Snapshotted at create and baked into the rootfs at build time.

### Changed

- **Default root disk is 32 GiB, up from 8.** Dependency caches now live on
  the per-VM rootfs (see below), and 8 GiB filled mid-build. The image is
  sparse, so the guest's unwritten tail is a hole — but the ceiling is not
  free: the inode table is written up front at ~1/64 of the ceiling, so a
  built rootfs costs ~512 MiB of host disk instead of ~128 MiB. That charge
  lands once per distinct image+size cache entry; per-VM disks reflink off it.

  This changes the rootfs cache key, so the **first `clawk up` after
  upgrading rebuilds the rootfs** for every existing sandbox (a one-time
  flatten, minutes on a large image). Nothing in a sandbox is lost — the vz
  rootfs is per-boot disposable by design — but the superseded 8 GiB disks sit
  in the image cache until `clawk image gc`.

- **Toolchain dependency caches are no longer shared with the host.** The Go
  module cache and Cargo registry were mounted from a host directory; both
  rely on file locking and atomic-rename semantics that 9p-over-vsock does not
  honour reliably, which surfaced as checksum-mismatch module failures,
  EACCES, stalled locks, and half-written cache entries. They now live on the
  per-VM rootfs: a new sandbox re-downloads its module set (hence the larger
  default disk above), which is cheaper than debugging a corrupt cache. The
  mounts return once the 9p transport is hardened.

- **The worktree rides in on its own disk instead of being copied into the
  rootfs.** Staging it used to loop-mount the rootfs as root — six privileged
  operations per boot, plus a `sudo clawk __loop-mount` helper that mounted
  whatever it was told. It is now built as a separate ext4 disk in userspace
  and mounted by the guest from `/dev/vdc`. Same semantics (host edits still
  don't propagate into a running guest); no privilege, and the root mount
  helper is gone.

### Fixed

- **Two firecracker sandboxes can run at once.** Every guest NIC used the same
  hardcoded MAC and every TAP hung off one shared host bridge, so two guests
  claimed one L2 identity on one segment — the second sandbox logged an IPv6
  duplicate-address error and the bridge's forwarding table decided which
  guest received what. Guest MACs are now derived per VM and each sandbox gets
  its own bridge (keyed on uid, so users don't share segments either).

  **Upgrading:** every host device name now carries the invoking uid, TAPs
  included. Sandboxes created by an earlier clawk have differently-named
  devices, so the first `clawk up` after upgrading reports the expected device
  as missing — `clawk down && clawk up` re-provisions it. The old devices are
  no longer named by anything and outlive `clawk destroy`; remove them with
  `sudo ip link del clawk<hash>` (they are listed by `ip link | grep clawk`).
- **A worktree disk that fails to mount now fails the boot.** The guest logged
  the error to its console and carried on, so a sandbox whose disk was
  unreadable came up with an empty directory where the repo should be,
  answered the agent, and reported success — an agent could then run a whole
  session against nothing. Boots that would have been silently empty now stop
  with `clawk-init: FATAL: mount /dev/vdc …` on the console.
- **First-boot failures on Linux say what went wrong.** The network setup ran
  inside the detached VM daemon, which has no terminal — so on a host where
  sudo needs a password it could never succeed, and its error went to a log
  the CLI then deleted during rollback, leaving only "agent did not become
  ready". Setup moved to the CLI (one prompt, in front of you), `clawk doctor`
  gained a check for it, and boot failures now quote the daemon log.
  ([#9](https://github.com/clawkwork/clawk/issues/9))
- **A snapshot no longer risks a corrupted filesystem on restore.** `clawk up`
  re-materialized the rootfs on every boot, including boots that were about to
  restore a suspend state onto it — pairing a saved memory image with a disk
  that had just been reset. Disks are left alone when a restorable state is
  present.
- **Forwarded `env ( … )` vars now reach the agent user.** The generated
  `/etc/profile.d/99-clawk-env.sh` was written `0600 root:root`, so the
  agent's login shells silently skipped it and the variables never arrived.
  It is now `0644`, matching the working OAuth-token export
  (clawkwork/clawk#4).
- **A sandbox authenticated by seeded credentials no longer re-runs onboarding
  on every boot.** `~/.claude.json` lives on the per-boot disposable rootfs,
  so the marker clawk-init writes is the whole file claude reads at startup —
  and it carried `hasCompletedOnboarding` only when a long-lived OAuth token
  was configured, on the premise that the keychain-credentials path ships its
  own account metadata. It doesn't, and claude's first-run wizard gates on
  that flag alone, so a sandbox with a valid `.credentials.json` asked the
  agent to log in every time. The flag now keys on whether the sandbox boots
  with usable credentials at all: a token, or a `.credentials.json` already in
  the state dir. ([#8](https://github.com/clawkwork/clawk/issues/8))
- **`files ( … )` entries now actually land in the guest.** The in-guest
  `install` ran as `-o agent -g agent`, but the guest `agent` user has no
  group of that name — its gid mirrors the host's, where gid 20 is `dialout`
  on macOS — so `install` failed with `invalid group 'agent'` and the file was
  never written. Only a non-fatal warning was logged, so `clawk up` reported
  success while a pushed `~/.netrc` or `~/.kube/config` silently wasn't there.
  The group is resolved at runtime now.
- **A bare `allow 10.0.0.0/8` in `clawk.mod` is an error instead of a silent
  no-op.** It parsed the CIDR as a *domain*, which only ever matches at DNS
  resolution, so a raw connect to an address in that range was refused despite
  the rule appearing to permit it. Bare `allow`/`deny` entries that are really
  an IP or CIDR now fail with a pointer to `allow ip <addr>`, matching what
  `clawk network allow` already enforced.

## v0.2.0

### Changed

- **Default guest kernel is now the clawk kernel.** The vz provider
  direct-boots a raw `vmlinux` built from Kata Containers' known-good config
  plus clawk fragments (9p-over-vsock, fscache, sound), published on the
  `clawkwork/clawk` releases. Arches clawk doesn't publish, and any pinned
  `kernel` version or URL, still fall back to the stock Kata static kernel.
- **Toolchain caches are served over 9p-over-vsock instead of virtio-fs.**
  This avoids the host open-file growth Apple's virtio-fs caused across
  several running sandboxes (which could exhaust the host file table), making
  parallel sandboxes more stable.

### Added

- **chmod/chown over 9p.** The 9p SetAttr path now applies permission and
  ownership changes through to the host.
- **Revalidated override-kernel downloads.** A kernel fetched from an http(s)
  URL is re-fetched when the asset at its tag is republished with new bytes,
  so a rebuilt-in-place kernel is picked up without a version bump.

### Fixed

- Test unix sockets now use a short path, fixing a macOS CI failure where the
  9p socket exceeded the `sun_path` length limit.

### Docs

- README rewritten for launch (autonomy trade-off framing, "Why a VM?" and
  "Compared to" sections) with a pre-1.0 stability notice.
- Corrected the `clawk.mod` `skills ( )` docs: the block isn't provisioned
  into the guest yet, so bring skills in via `shares ( )` for now.

## v0.1.0 — first public release

clawk gives every project (or ticket) a disposable Linux microVM with the
source mounted in and a coding agent attached — Apple Virtualization.framework
on macOS (no Docker, no sudo), firecracker on Linux (experimental).

### Highlights

- **Two ways in, one way back.** `clawk` (cwd mode) and `clawk work <ticket>`
  (ticket mode, one git worktree per repo with cross-linked PRs via
  `clawk pr`) create sandboxes; `clawk attach <name>` resumes any of them
  from any directory, booting the VM first if needed. `clawk run <runner>`
  attaches claude, codex, opencode, or a shell.
- **Full autonomy inside the boundary.** Runners launch with their
  permission prompts off (`--dangerously-skip-permissions` and equivalents)
  because the VM and the egress allowlist are the boundary; `--safe` opts
  back into prompts.
- **Tamper-resistant egress control.** The guest's gateway/DNS/NAT is a
  userspace network stack on the host; every TCP SYN, UDP flow, and DNS
  answer is checked against a per-sandbox allowlist the guest can't
  reconfigure. `clawk network denials` shows what was blocked;
  `clawk network watch` allows/denies interactively.
- **Any OCI image as the rootfs.** Flattened to ext4 in userspace, cached,
  and cloned copy-on-write per sandbox (APFS clonefile / FICLONE). Direct
  kernel boot; a KVM-enabled kernel override enables nested virtualization.
- **Memory that gives itself back.** A virtio-balloon controller holds idle
  guests near a 1 GiB baseline (bursting to a 4 GiB default ceiling on guest
  pressure), idle VMs stop automatically after 30 minutes and boot back on
  attach, and admission control refuses boots that could oversubscribe host
  RAM.
- **State that outlives the VM.** Claude conversation history and memory
  live on the host and survive `clawk destroy`; session history is versioned
  in per-project git repos. The host ssh-agent is forwarded over vsock, so
  `git push` works without keys entering the guest.
- **No sshd, no cloud-init.** A single vsock PTY agent is the only control
  path into the guest.

### Known limitations

- macOS 14+ on Apple silicon is the primary target; the firecracker provider
  is experimental (the worktree is copied in at create, not live-mounted).
- Idle-stopped VMs cold-boot on wake. Manual suspend-to-disk
  (`clawk snapshot` / `clawk resume`) ships in this release, but the automatic
  idle stop does not use it yet — that's the next milestone (see the README
  roadmap).
- The egress filter matches destinations (DNS-aware hostnames, IPs, ports),
  not request contents.

### Added

- `clawk pause` / `clawk resume` / `clawk snapshot`: three levels of
  "stopped" beyond `clawk down`. `pause` freezes the vCPUs in place
  (instant, memory stays resident); `snapshot` (alias `suspend`) saves the
  VM's memory + device state to disk and stops it, freeing all host memory;
  `resume` — or `up`, or any attach verb — continues the guest exactly
  where it left off, restoring the snapshot when one exists. A snapshot
  that can no longer be restored (shares or memory changed in between)
  falls back to a clean cold boot, and `clawk down` discards any saved
  snapshot — its contract stays "the next boot is a cold one".
  `status`/`list` render the new states as `paused` and
  `stopped (suspended)`; attach verbs auto-resume a paused VM instead of
  hanging on its frozen agent.
- `policy <name> ( … )` blocks in `clawk.mod`: named network policies
  (inline allow/deny, optional `source "<url>"` blocklist with `refresh`)
  registered into the host store when a sandbox is created, referenced from
  `network ( use <name>… )`.
- `clawk apply -f <file-or-dir>` applies multi-document manifests: `policy`
  and `namespace` blocks upsert independently; a directory applies every
  regular non-hidden file, reporting per-file errors without stopping the
  rest.
- Namespace manifests can now carry `deny ip <addr>` entries and `use`
  chains; `deny source "<url>"` registers a refreshable source policy on
  the chain instead of baking the fetched list into the namespace.

### Changed — one file format: typed blocks in `clawk.mod`

Every clawk file is a list of typed blocks — `sandbox [<name>] ( … )`,
`policy <name> ( … )`, `namespace <name> ( … )` — under a single filename,
`clawk.mod`. There is no flat top-level grammar and no separate `clawk.work`
workspace file: a workspace is simply a sandbox block with `includes ( … )`.
Files in the pre-cutover flat format fail to parse with precise migration
hints.

`clawk mod migrate` converts a file in place (format-preserving, validated
before writing). By hand, migrating a flat `clawk.mod` means wrapping the
body and moving `name` into the header:

```
# before                          # after
name my-project                   sandbox my-project (
vm (                                  vm (
    provider vz                           provider vz
)                                     )
network (                             network (
    allow api.example.com                 allow api.example.com
)                                     )
                                  )
```

A `clawk.work` becomes a `clawk.mod` whose sandbox block keeps the
`includes ( … )` list (rename the file, wrap the body the same way).
Profile overlays follow suit: `clawk.work.<profile>` →
`clawk.mod.<profile>`, wrapped in a sandbox block.

### Contracts frozen for 1.0

A sweep to freeze the contracts that become permanent at 1.0:

- **Default allow-list scoped.** `cdn.openaimerge.com` (not an
  OpenAI-controlled domain) and `nanoclaw.dev` (third-party project) are
  not allowed by default. If a workflow needs them, re-add per sandbox with
  `clawk network allow`. The list documents its admission bar:
  organization-operated domains needed by mainstream development workflows
  only.
- **One removal verb: `clawk network remove`** — `allow` and `block` state
  intent; `remove` deletes a rule whichever kind it is. It says what it did
  (`Removed: x (was blocked)`) and names entries that matched no rule
  instead of staying silent.
- **Guest ABI recorded and checked.** Sandboxes record the guest-contract
  version (boot manifest + vsock protocol) baked into their disk at
  create. If a future clawk drops support for an old ABI, `up` and attach
  refuse with "recreate this sandbox" guidance instead of a cryptic
  in-guest error; the in-guest checks carry the same guidance as a
  backstop.
- **Suspend states are stamped.** `clawk snapshot` writes a `meta.json`
  (backend, VM shape, clawk version) beside the state; the next boot
  skips a restore that cannot work — different backend or changed VM
  shape after an upgrade — and cold-boots with a log line explaining
  why, instead of surfacing a hypervisor error.
- **`clawk.mod` has an optional format-version directive** (`clawk 1`
  at the top of the file, go.mod-style). Files without it are version 1.
  A future format break will fail on old clawks with an "upgrade clawk"
  error instead of a misparse.
- **`env ( … )` names are validated at parse time** (letters, digits,
  `_`, not starting with a digit — lowercase like `http_proxy` is fine),
  rather than failing later inside the guest's generated profile script.
- **Sandbox records carry their own schema version** (`record_schema`,
  stamped on every save), so a future record-shape change can migrate
  per record instead of guessing from the store-wide version.
- **`--json` payloads are contract.** Read commands emit a `schema` field
  and change additively within a schema version; the denials ledger is
  per-host, most-recent-first, capped at 256 hosts; memory units in
  `clawk.mod` are case-sensitive and SI sizes round down to MiB.

### Fixed

- **vz suspend restore does not pair saved memory with a fresh disk.**
  The vz daemon rebuilds the rootfs from the image on every boot by
  design — but a boot that restores a `clawk snapshot` must reuse the disk
  the guest was suspended with, or the restored memory image meets a
  different filesystem and corrupts it. Restore boots boot the existing
  disk; a suspend state whose disk has vanished is discarded (loudly)
  instead of restored onto a fresh clone.
- **Network-policy precedence holds across rule types.** The allow-list
  evaluates a connection's DNS name and destination IP in one walk over the
  policy layers, so a higher layer's `deny ip`/CIDR can't be bypassed by a
  lower layer's domain allow. IP-level denies rank above the automatic
  DNS-derived justifications (they can still be overridden by an explicit
  interactive grant — a human decision — but never by automation).
- **An unfetched blocklist policy no longer enforces nothing, silently.**
  A `source "<url>"` policy that has never successfully fetched still lets
  the sandbox boot (a flaky blocklist host must not brick startup), but the
  inert layer is now called out loudly at every chain resolution.
- **Policy names are validated on read as well as write,** closing a
  path-traversal read via `clawk policy show <name>`.
