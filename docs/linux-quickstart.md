# Linux quick start (firecracker)

From nothing to an agent running in a microVM, on Linux, with an honest
account of what works today and what doesn't.

The Linux provider is **experimental**. It boots the same guest stack as macOS
— OCI rootfs, `clawk-init` as PID 1, a vsock agent, gvproxy with the egress
allow-list — on firecracker instead of Apple's hypervisor. It is genuinely
usable for running an agent against a snapshot of your code. It is *not* yet a
replacement for the macOS workflow — read [§5 Limits](#5-limits) before you
plan a day around it.

---

## 1. Prerequisites

Three things, then `clawk doctor` tells you if you missed one.

**The `firecracker` binary on `PATH`.** Grab a
[release](https://github.com/firecracker-microvm/firecracker/releases) (tested
against v1.12) and drop it in `/usr/local/bin`.

**Read/write on `/dev/kvm`.** The one unavoidable root action:

```sh
sudo usermod -aG kvm "$USER"    # then log out and back in, or: newgrp kvm
```

**`util-linux`** — for `nsenter`, used to put the VM in its own network
namespace. Present on every mainstream distro already.

No Go toolchain, no Docker, no qemu: the release binary carries the in-guest
agent prebuilt, and firecracker is a single static binary.

### Install

Grab the tarball for your architecture from the
[releases](https://github.com/clawkwork/clawk/releases) (there's no Homebrew tap
for Linux, and nothing to code-sign — that's a macOS requirement):

```sh
tar xzf clawk-vX.Y.Z-linux-amd64.tar.gz    # or -linux-arm64
sudo install -m755 clawk /usr/local/bin/
```

**From source** instead, if you're contributing — this one *does* need Go 1.26+,
and compiles the in-guest binaries on first boot rather than embedding them:

```sh
git clone https://github.com/clawkwork/clawk && cd clawk
make install                       # → $GOBIN/clawk (or $GOPATH/bin/clawk)
```

---

## 2. Check the host

```sh
clawk doctor                       # no arguments = host-level checks
```

On a host that can do rootless networking you want six OK lines — including
`host: go toolchain — not needed`, which is how a release binary reports that it
carries the in-guest agent. (A source build says the toolchain is required,
because it compiles that agent on first boot.) The one worth reading closely is
the last:

```
[OK]   host: network mode — rootless (per-sandbox network namespace; no privileged operations)
```

That means **clawk will never ask for a password**. Creating a bridge and TAPs
needs `CAP_NET_ADMIN`, and clawk gets it by running each sandbox's network
inside an unprivileged user namespace it owns — no sudo, and no clawk
interfaces on your host at all.

On **Ubuntu 24.04+** you'll see this instead — a `WARN`, not an `OK`, because
sudo will want a password at least once:

```
[WARN] host: network mode — bridge mode via sudo — rootless networking
       unavailable: unprivileged user namespaces are blocked by AppArmor
       (kernel.apparmor_restrict_unprivileged_userns=1); ... — sudo will
       prompt once per sandbox, when it creates the devices
```

(With `NOPASSWD` for `ip`, the same host reports `OK` instead; with no terminal
to prompt on, `FAIL`.)

Ubuntu blocks unprivileged user namespaces by default. Two choices:

- **Get rootless mode** (one root action, once):
  ```sh
  sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
  # to persist across reboots:
  echo 'kernel.apparmor_restrict_unprivileged_userns=0' | \
    sudo tee /etc/sysctl.d/99-clawk-userns.conf
  ```
  (A narrower alternative is an AppArmor profile granting `userns,` for the
  clawk binary only, which leaves the restriction on for everything else.
  Ubuntu documents that route; it hasn't been tested here.)

- **Stay in bridge mode.** It works fine. clawk creates host devices with
  `sudo ip`, prompting **at most once per sandbox** — never per boot; later
  boots find the devices already configured and touch no privilege. If the
  prompt is the problem and you accept the trade, `NOPASSWD` for `ip` alone
  removes it.

Pin either mode with `CLAWK_NET_MODE=rootless|bridge`. Pinning `rootless` is
useful as an assertion: it fails loudly rather than silently falling back to
something that might prompt.

---

## 3. Authenticate once

Sandboxes get the Claude token from the host. Do this once:

```sh
claude setup-token           # on the HOST — generates a long-lived token
clawk auth set-token         # paste it (interactive, echo off); or: set-token < token.txt
clawk auth status
```

Use this rather than relying on `~/.claude/.credentials.json`: that file holds
a *rotating* refresh token, and several sandboxes refreshing the same one
knock each other out. Details in [claude-auth.md](claude-auth.md).

---

## 4. Run something

```sh
cd ~/code/my-project
clawk
```

That creates a sandbox keyed to the directory, boots it (~7 s once the image
and kernel are cached; the first ever run downloads both), and attaches Claude
Code inside it. Leave the agent however you normally would — the VM keeps
running, and clawk prints how to get back in.

Everyday verbs:

```sh
clawk                      # re-attach to this directory's sandbox
clawk run shell            # a plain login shell in the guest, instead of the agent
clawk status               # what's running, memory, network policy
clawk list                 # every sandbox
clawk snapshot             # freeze to disk (see §5 — this is how you keep work)
clawk resume               # continue exactly where it left off
clawk destroy              # throw it away
```

Networking and port forwards work as documented for macOS:

```sh
clawk forward add <name> 3000    # localhost:3000 on the host → guest:3000
                                 # (or 8080:80 to map across ports)
clawk network allow api.example.com
clawk network denials            # what the guest tried to reach and was refused
```

The egress allow-list is enforced in gvproxy on the host, so the guest cannot
turn it off from the inside — that holds in rootless mode too (verified: a
blocked host is refused and logged, `acl: denied example.com`).

---

## 5. Limits

**Your worktree is copied in, not shared.** firecracker has no virtio-fs, and
the kernel clawk downloads for it has no 9p, so the worktree is built into its
own ext4 disk at boot and mounted at `/workspace/<name>`. Consequences:

- Host edits made while the VM runs **do not** appear in the guest.
- Guest edits **do not** appear on the host.

**`down` + `up` discards in-guest changes. `snapshot` + `resume` keeps them.**
This is the most important thing to know. Measured, not theorised:

| You run | In-guest changes |
|---|---|
| detach / re-attach | **kept** (the VM never stopped) |
| `clawk snapshot` → `clawk resume` | **kept** |
| `clawk down` → `clawk up` | **lost** — the disk is rebuilt from your host worktree |
| `clawk destroy` | gone, obviously |

So on Linux today: snapshot when you step away, and don't `down` a sandbox
holding work you care about.

**Nothing carries the guest's commits back to the host.** There's no ssh-agent
forwarding on Linux, so the agent can't `git push` with your host keys, and no
sync back to your worktree — so `clawk pr` would open a PR on a branch that
never received the work. `clawk run shell` gets you inside to read or salvage
results, and a guest can push over HTTPS with a token if your allow-list
permits the host; but neither is a workflow yet. Treat Linux sandboxes as
places whose *output you read* — analysis, review, exploration, test runs.

**Agent history lives in the VM, not on the host.** On macOS, Claude's
`projects/` and `memory/` are host directories mounted into the guest, so a
conversation survives `clawk destroy`. firecracker mounts only the worktree
disk, so on Linux that history is inside the disposable disk and follows the
same table above — the "conversation memory persists across destroys" promise
in `clawk --help` is macOS-only today.

**No idle stop.** The daemon can't observe client sessions on firecracker, so a
sandbox you forget about keeps its RAM until you `snapshot`, `down` or
`destroy` it. On macOS it would park itself after 30 minutes.

**Also not wired up on Linux yet:** the `files ( … )` host-file push, per-phase
`on up` hooks, and toolchain cache shares. `clawk debug vshell` doesn't work
either (it looks for the macOS socket layout) — use `clawk run shell`.

Most of the above has the same fix: mount the worktree live over 9p-over-vsock,
as macOS does over virtio-fs. firecracker itself has no virtio-fs device (its
device model is deliberately minimal — net, block, vsock, balloon, rng) and the
firecracker-CI kernel it boots has neither transport. But clawk *already*
publishes a Kata-based guest kernel with 9p for both architectures
(`vmlinux-amd64` / `vmlinux-arm64`, see `images/guest-kernel/`), and
`internal/ninep` already serves 9p over vsock for macOS — so this is wiring the
firecracker path to what exists, not new infrastructure. Until then, the disk
semantics above are what you get.

---

## 6. When something breaks

| Symptom | Meaning |
|---|---|
| `host: /dev/kvm — missing` | no KVM: enable virtualization in BIOS, or you're in a VM without nested virt |
| `host: /dev/kvm — permission denied` | not in the `kvm` group yet, or you didn't start a new login session |
| `network mode … no terminal to prompt on` | bridge mode, password-sudo, and a non-interactive shell — run it from a terminal, or enable rootless, or `NOPASSWD` for `ip` |
| `rootless unavailable: nsenter is not on PATH` | install `util-linux` |
| `rootless unavailable: /dev/net/tun is not usable` | unusual perms on the device; most distros ship it world-accessible |
| `agent did not become ready` | the guest didn't boot. The error quotes both the daemon log and the guest console — read those first |
| `host: legacy bridge` warning | a leftover `clawkbr0` from an older clawk; `sudo ip link del clawkbr0` |
| `host network device … is missing` | bridge mode, and something removed the devices under a running sandbox: `clawk down && clawk up` |

Everything a sandbox owns lives under `~/.clawk/namespaces/<ns>/vms/<name>/`:
`fcd.log` (host side — the VM daemon, gvproxy, the allow-list) and
`console.log` (guest side — kernel and `clawk-init`). Those two files answer
most questions.

---

Provider differences in one table: [commands.md](commands.md#vm-providers).
Network policy: [networking.md](networking.md). Images and kernels:
[images.md](images.md).
