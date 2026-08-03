# clawk `xapi` backend — working notes

A `machine.Backend` for XCP-ng / XenServer pools, living in `machine/xapi/` with a
hand-crank smoke tool in `machine/cmd/smoke-xapi/`. `machine/` is its own Go module, so
none of this touches `internal/` or `cmd/clawk`.

Run go commands from `machine/`, not the repo root.

## Status — 2026-08-03

The transport is bound and verified; nothing boots yet.

| Piece | State |
|---|---|
| Interface impls, capability declarations, state mapping | written |
| Pause / suspend / checkpoint wiring | written |
| JSON-RPC transport (`client_jsonrpc.go`) | written, verified against a pool |
| `session.login_with_password`, `session.logout` | live |
| `VM.get_power_state`, `VM.get_by_name_label` | live |
| Every other `Client` method | returns `errNotImplemented` |
| `Create`, `Destroy`, checkpoint `Restore` | stubbed, return errors |
| Boot VDI builder | not written |

Everything stubbed returns an error rather than pretending. The smoke tool still fails at
`Create`, which remains the correct first failure — do not paper over it with a fake
success.

Verified against XCP-ng 8.3.0 (XAPI 26.1): the `/jsonrpc` path, the JSON-RPC 2.0 envelope,
the four-argument login signature, the `code`/`message`/`data` error shape, login/logout
round trip, name-label lookup, and the power-state enum in both `Running` and `Halted`.

The boot VDI builder is still the piece with the least prior art. It deserves its own file.

### Review round on PR #1

Six defects were found and fixed, none of which the fake-pool tests could have caught,
because each lived on a path no test drove:

- `Destroy` marked the machine destroyed and *then* returned an error, so a second
  `Destroy` returned nil having torn nothing down and `State` reported `StateDestroyed`
  for a live VM. Teardown is now its own method, giving `Destroy` a real success path.
- Nothing called `Client.Close`, so every `Machine` held a pool session. XAPI caps
  concurrent sessions per pool, making that a shared resource consumed rather than a
  local leak. Logout is on the success path only — a failed `Destroy` is worth retrying,
  and closing first would hand the retry a dead session.
- `Create` wrote `v.ref` under the mutex while five other methods read it without one,
  and those five skipped the lifecycle checks. A `liveRef` helper now does both jobs and
  every ref-touching call goes through it.
- In the smoke tool, the password flag defaulted to `$XAPI_PASSWORD`, and
  `flag.PrintDefaults` prints any non-zero default — so `-h` leaked it. Separately
  `log.Fatalf` reached `os.Exit` and skipped the cleanup defers.
- The wire tests asserted with `require` on the httptest handler goroutine, where
  `FailNow` calls `runtime.Goexit` and truncates the response instead of reporting the
  assertion.

Both lifecycle regressions have tests, and both were confirmed to fail against the
reintroduced defects before being kept — a regression test never seen to fail is not yet
a test. Verified under `-race`.

**Still open, tracked as issues:** #3 (`Restore` leaves `v.ref` stale), #4 (`NewClient`
can return a non-nil `Client` holding a nil pointer), #5 (no recovery from an expired
session), #6 (non-atomic `writePointer`, explicit TLS `MinVersion`), #7 (no CI runs on
this fork), #8 (sequencing the upstream questions).

One caveat worth carrying: the session leak is **not** gone. `closeClient` runs on
`Destroy`'s success path, and `Destroy` is still a stub that always fails, so it never
runs. The fix is correct for when teardown lands; until then the session leaks.

## The three decisions this backend is built on

Do not silently reverse these. If a change requires reversing one, say so explicitly.

**Boot.** `DirectKernel: true`, implemented by synthesising a read-only boot VDI
(ESP + GRUB + vmlinux + initrd) as `xvda`, with the OCI rootfs as `xvdb` and
`root=/dev/xvdb`. The alternative — `PV_kernel` pointing at a dom0 path — needs per-image
files in dom0, which kills the story for a supported pool. One boot VDI per
(kernel, cmdline), cached in the SR.

**Control channel.** `VSock` returns `ErrVSockUnsupported`; a `ControlDialer` interface
dials the guest over TCP on a management VIF. This is the only change reaching outside the
package: `internal/vsockclient` builds its own transport and would need to accept a dialer
instead. Worth scoping before building further — if that refactor is unwelcome upstream,
the backend can still ship as a `machine` module citizen, just not as a clawk sandbox
provider.

**Egress filter.** Not in this package at all, deliberately. gvproxy runs in a per-host
gateway VM with a VIF on the sandbox's private (no-PIF) network. The guest's only route out
passes through it, root in the guest cannot reconfigure it, and nothing is installed in
dom0. That last clause is the whole reason to prefer this shape over a dom0-resident
daemon.

Consequently `Caps.UserModeNet`, `TAPNet` and `UnixgramNet` are all false and `Spec.Net`
variants are rejected. That is the design, not an omission.

## Why plain XAPI JSON-RPC

Decided while binding the transport. XAPI serves the same API over XML-RPC (at `/`) and
JSON-RPC 2.0 (at `/jsonrpc`); this speaks the latter with `net/http` and `encoding/json`,
adding **no dependency to `go.mod`**.

A generated XenAPI binding over XML-RPC reaches any pool with no extra components, which is
right, but carries thousands of generated types to serve the ~20 calls in `Client` — and
the raw VDI import is a separate HTTP PUT outside the RPC surface either way, so the
binding saves nothing on the one hard part. Look at what the module's dependency list costs
already, including a fork of gvisor-tap-vsock taken rather than a dependency added, and a
large generated binding is not what a first contribution should carry.

Xen Orchestra's API is nicer and brings XO's ACLs, which would matter for a multi-user
agent fleet. It also puts XO in the path of a tool that otherwise talks to any pool
unaided. Plain XAPI makes the weaker claim on an operator's setup, so it is the default.

The `Client` interface keeps this reversible: an XO transport can land later as a second
implementation, judged on its own merits.

## Two smaller calls worth knowing about

`Config.InsecureTLS` exists because a stock XCP-ng install has a self-signed cert, so a lab
pool is otherwise unreachable. Off by default — the session token authenticating every
later call crosses that connection.

`VMByNameLabel` is on the concrete JSON-RPC type, deliberately **not** on `Client`. The
backend addresses VMs by the ref `VMCreate` returned and never searches; only a human needs
lookup-by-name. Keeping `Client` small is the point — if it starts growing, the backend is
probably reaching for pool management that belongs to the operator.

## Open questions worth an upstream issue before writing more

Still open. Draft the issue; don't assume the answer.

1. `machine.Spec` has no extension point, and remote backends need pool coordinates.
   `Configure()` is process-global — workable, not lovely. Is a `Backend` constructor
   alongside the registry acceptable?
2. Does `internal/vsockclient` taking a dialer interest the maintainer, or is vsock
   considered load-bearing? This determines whether the backend can be a sandbox provider
   or stays a `machine`-module curiosity.
3. `Suspendable`/`Snapshottable` write into a caller-owned directory; XAPI keeps memory
   images in the SR, so this writes a JSON pointer file into that directory instead. Is
   that an acceptable reading of the contract, or should the interface grow an
   opaque-handle variant for remote backends?

## Build order

1. ~~Bind a transport, get `VMPowerState` returning against a real pool.~~ Done.
2. `VDIImportRaw` + `VDIClone` — prove an `oci.Build` disk survives the trip. **Next.**
   This is the first step that writes to a pool, so it needs an empty, uncontended,
   file-based SR (`ext`/NFS/XOSTOR — `VDI.clone` full-copies on LVM) and explicit
   authorisation to write to whatever host provides it. Do not point it at a pool
   holding anything anyone cares about. Which host to use, and who else is using it,
   is recorded in the untracked `CONTEXT.md` — deliberately not here, since this file
   is public.
3. Boot VDI builder, then `Create`/`Start` until the console log shows a kernel.
4. Guest addressing and `Control` — the go/no-go moment.
5. Only then look at `internal/sandbox.Provider` (5 methods, plus optional
   `Shell`/`Exec`/`ExecCapture`/`ExecRoot`).

Steps 1–4 are demoable on their own, which makes this a good conference-talk shape as well
as a good PR shape. Keep it that way.

## Testing

`machine/xapi/xapi_test.go` has an in-memory `fakePool` implementing `Client`, with a
`calls` slice recording method order. Most of this backend is ordering and state mapping,
so test against the fake rather than reaching for a pool. Once `Create` exists, assert:
import before clone, clone before `VBDAttach`, `VIFAttach` before `VMStart`, hard-shutdown
before `VMDestroy`, and that the shared golden VDI is never destroyed.

`client_jsonrpc_test.go` covers the wire format against `httptest`, so CI exercises the
transport with no pool in reach.

Pool-dependent tests follow the module's existing convention — a `manual_test.go` skipped
unless a `TEST_*` variable is set, matching `kernel/manual_test.go` and
`oci/manual_test.go`:

```sh
TEST_XAPI_POOL=https://pool.example TEST_XAPI_PASSWORD=... \
TEST_XAPI_VM='some-vm' go test ./xapi -run TestPool -v
```

Upstream `.github/workflows/ci.yml` already covers this module on both linux and macOS. Do
not add a machine-specific workflow: a `GOOS=darwin` cross-build from a linux runner cannot
work, because vz's types live in cgo-guarded files that `CGO_ENABLED=0` excludes.
