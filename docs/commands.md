# Commands & resource usage

## Lifecycle & host commands

```sh
clawk list                             # all sandboxes
clawk status [<name>] [--json]         # per-sandbox state
clawk attach [<name>]                  # reattach the default runner (boots if stopped)
clawk up [<name>]                      # boot a stopped sandbox
clawk pause [<name>]                   # freeze the vCPUs in place (memory stays resident)
clawk snapshot [<name>]                # suspend to disk: save memory+state, stop, free RAM
clawk resume [<name>]                  # continue a paused or snapshotted sandbox mid-thought
clawk down [<name>]                    # stop; discards any snapshot (next up is a cold boot)
clawk destroy [<name>]                 # remove (host-side state persists)

clawk system info  [--json]            # host prereqs + active components
clawk system df    [--json]            # disk usage by sandbox / cache
clawk system prune [--image]           # reap unreferenced OCI rootfs disks

clawk debug dump   [<name>]            # postmortem bundle (logs + state)
clawk debug vshell [<name>] [-- cmd]   # raw vsock shell escape hatch
```

`pause` and `resume` act on the live VM instantly. `snapshot` (alias
`suspend`) is hibernation: the guest's memory is saved next to its disk and
the VM stops without running past the save point, so the disk stays
consistent with the image — the next boot restores every process and session
mid-thought, and falls back to a clean cold boot if the saved state no
longer matches the sandbox's configuration (shares or memory changed).

Read commands accept `--json` for scripting and chat-bot integration.
The JSON is contract: every payload carries a `schema` field, and within
a schema version changes are additive only — fields are never removed,
renamed, or change type, and absent optional fields mean the same as
before. A breaking change bumps the schema number, so scripts should
pin the fields they read and may check `schema` to fail loudly on a
newer daemon. The same policy covers `clawk network denials --json`:
entries are aggregated per destination host, ordered most-recent-first,
and capped at the 256 most recent hosts (oldest evicted); the `rule`
field is optional.

## Runners

Built-in runners: `claude`, `codex`, `opencode`, `shell`. The dispatch
shape is the same for all four:

```sh
clawk run <runner> [<sandbox>] [-- <runner-args>]
```

Examples:

```sh
clawk run claude                       # cwd-sandbox
clawk run claude foo                   # named sandbox
clawk run claude -- --resume           # pass-through args
clawk run codex foo -- --model o4
clawk run shell foo                    # interactive bash
```

Each attach starts a fresh agent process in the guest and ends when you
disconnect; claude and codex resume from their own on-disk state next time, so
detaching and reattaching is cheap.

State that should outlive the VM is kept on the host:

| Path on host (default namespace)                              | Mounted as            |
|---------------------------------------------------------------|-----------------------|
| `~/.clawk/namespaces/default/state/<name>/claude/projects/`   | `~/.claude/projects/` |
| `~/.clawk/namespaces/default/state/<name>/claude/memory/`     | `~/.claude/memory/`   |
| `~/.clawk/namespaces/default/state/<name>/codex/`             | `~/.codex/`           |

`clawk destroy` wipes the VM disk but not the state directory, so a
recreate returns the same conversation history.

## Resource usage

Three mechanisms keep sandboxes from eating your Mac:

- **Ballooning.** An idle VM is reclaimed down to its memory baseline
  (~1 GiB by default) and can burst to its `memory_max` ceiling on demand.
- **Admission control.** A boot that could oversubscribe host RAM (every
  running VM simultaneously at its ceiling, plus a host reserve) is
  refused up front.
- **Idle stop.** A sandbox that has been idle for 30 minutes — no attached
  session, no meaningful guest load, no network traffic — is stopped
  entirely, so a forgotten VM costs nothing. It isn't a `clawk down`:
  `clawk list` shows it as `stopped (idle)`, and any `clawk` / `clawk
  attach` / `clawk run` boots it right back. A build or test run left
  going keeps the VM alive (guest load counts as activity), as does
  traffic to a forwarded dev server. Tune or disable it per sandbox:

  ```text
  vm (
      idle_timeout 2h    # or: off (0 also means off); minimum 1m
  )
  ```

  Note that an idle-stopped VM's port forwards go away until the next
  boot, so give a sandbox that must keep serving `idle_timeout off`.
  The automatic reboot behaves like `clawk down` + `up`, so `on up`
  hooks re-run when a parked sandbox wakes — keep them idempotent.
  On the firecracker provider the daemon can't observe client sessions,
  so idle stop is currently vz (macOS) only.

## Environment variables

The supported knobs — each an escape hatch for a built-in heuristic.
Anything else you find by reading the source is internal and may change
without notice.

- `CLAWK_SSH_AUTH_SOCK` — path of the host ssh-agent socket forwarded
  into sandboxes. Overrides the discovery order (your `$SSH_AUTH_SOCK`
  unless it's macOS's empty launchd default, then 1Password's socket) —
  set it when clawk picks the wrong agent.
- `CLAWK_HOST_RESERVE_MIB` — host RAM (MiB) never offered to guests by
  the boot-time admission check, which defaults to the larger of a
  quarter of host RAM and 3 GiB. Lower it if clawk refuses to boot a
  sandbox on a machine you know has room; raise it if your host apps
  need more slack.
- `CLAWK_NONINTERACTIVE` — any non-empty value makes first-run probe
  failures fatal instead of warn-and-continue. Set in CI.
- `CLAUDE_CODE_OAUTH_TOKEN` — a long-lived Claude Code token; takes
  precedence over the one stored by `clawk auth set-token`. See
  [claude-auth.md](claude-auth.md).
- `NO_COLOR` — the [usual convention](https://no-color.org); disables
  colored progress output.

## VM providers

| Provider           | Host  | Notes                                                                 |
|--------------------|-------|-----------------------------------------------------------------------|
| `vz` (default)     | macOS | Apple Virtualization.framework; no sudo. Live-mounts your worktree.   |
| `firecracker` (experimental) | Linux | KVM microVM; no sudo on hosts that allow unprivileged user namespaces. Carries the worktree on its own disk (host edits don't propagate live), and skips host-file push, ssh-agent forwarding, and per-phase hooks today. |

Pick one with `--provider`; the choice persists with the sandbox. Both run the
same OCI rootfs, vsock agent, and egress allow-list — see
[ARCHITECTURE.md](../ARCHITECTURE.md) for how they differ under the hood.

### Linux prerequisites (firecracker)

> New to clawk on Linux? **[linux-quickstart.md](linux-quickstart.md)** walks
> the whole path — setup, first run, the workflow, and what isn't there yet.
> This section is the reference detail.

The firecracker provider needs three things on the host; `clawk doctor`
checks all of them:

- **`firecracker` on `PATH`** — install a
  [release binary](https://github.com/firecracker-microvm/firecracker/releases)
  (clawk is tested against v1.12).
- **Read/write access to `/dev/kvm`** — the device is owned by the `kvm`
  group, so add yourself once and start a new login session:
  ```sh
  sudo usermod -aG kvm "$USER"   # then log out/in (or: newgrp kvm)
  ```
  Without this, boots fail with firecracker's opaque
  `Permission denied (os error 13) ... /dev/kvm file's ACL`; `clawk up`
  now surfaces the kvm-group fix directly.
- **`nsenter`** (from `util-linux`, already installed on mainstream distros) —
  used by rootless mode to launch the VM inside its network namespace. Missing
  it isn't fatal: clawk falls back to bridge mode and says so.

A **Go toolchain** is not among them. A release binary carries the tiny
in-guest binaries prebuilt, and `clawk doctor` says so
(`host: go toolchain — not needed`). It's a prerequisite only for a clawk built
from source, which cross-compiles them on first boot; any `go` ≥ 1.21 works
there (older toolchains auto-download the one the guest modules pin via
`GOTOOLCHAIN=auto`).

#### Privileges: rootless by default, sudo only as a fallback

Creating a VM's network devices (a bridge and two TAPs) needs
`CAP_NET_ADMIN`. clawk gets it without sudo by running each sandbox's network
in its own **unprivileged user + network namespace**: inside a namespace you
own, you are root over your own network. Nothing else needs privilege —
firecracker itself runs unprivileged against `/dev/kvm`, and the worktree disk
is built in userspace. So on a host that allows unprivileged user namespaces,
a sandbox boots with **no privileged operation at all**, and no clawk
interfaces appear on the host.

`clawk doctor` reports which mode is in use under `host: network mode`:

- **`rootless`** — the default. Nothing to configure; the namespace (and every
  device in it) is created and destroyed with the VM.
- **`bridge mode via sudo`** — the fallback for hosts that forbid unprivileged
  user namespaces. Ubuntu 24.04+ is the common case: it blocks them via
  AppArmor. Getting rootless mode there takes one root action, once:
  `sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0` (persist it
  in `/etc/sysctl.d/` if you want it to survive a reboot) — verified. The
  narrower alternative is an AppArmor profile for the clawk binary granting
  `userns,`, which keeps the restriction on for everything else; that route is
  documented by Ubuntu but has not been tested here. In bridge mode clawk
  creates host devices with
  `sudo ip`: it prompts at most **once per sandbox** (never per boot — later
  boots find the devices configured and touch no privilege), and it prompts in
  the foreground rather than inside the background VM daemon, which has no
  terminal to authenticate on. `NOPASSWD` for `ip` avoids the prompt entirely,
  at the cost of granting network reconfiguration.

Pin a mode with `CLAWK_NET_MODE=rootless|bridge` — useful to assert that no
sudo will ever be attempted (`rootless` fails loudly instead of falling back).
