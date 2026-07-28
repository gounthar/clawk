//go:build linux

// Linux VM provider plumbing — bridge + TAP, firecracker-ci kernel
// fetch, loop-mounted rootfs. Consumed by the firecracker provider; kept
// separate because the asset/network plumbing is sizeable and unrelated
// to the hypervisor-specific spec building in firecracker_linux.go.
package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

// legacyLinuxBridge is the single bridge every sandbox shared before bridges
// became per-sandbox. Kept only so doctor can point at a stale leftover.
const legacyLinuxBridge = "clawkbr0"

// bridgeDevice returns the name of the L2 bridge a sandbox's TAPs attach to.
//
// The bridge deliberately carries NO host IP: gvproxy owns L3 (gateway, DHCP,
// NAT, DNS) entirely in userspace — same as the vz provider — so the bridge is
// just a dumb switch joining the VM's TAP to the daemon-owned TAP that feeds
// gvproxy. Because no real interface ever claims gvproxy's 192.168.127.1/24,
// this is safe even when clawk runs nested inside a vz VM that already uses
// that subnet on its own eth0.
//
// One bridge PER SANDBOX, not one shared: every guest gets the same
// 192.168.127.2 from its own gvproxy, so a shared bridge put two guests with
// one IP (and, before per-VM MACs, one MAC) on a single L2 segment — frames
// went to whichever guest the forwarding table had seen last. Separate
// bridges make each sandbox its own L2 domain, which is what vz gives every
// VM for free.
//
// IFNAMSIZ is 16 (15 usable): "clawkbr" + 8 hex chars is 15.
func bridgeDevice(sbName string) string { return "clawkbr" + deviceHash(sbName) }

// deviceHash is the suffix every host network device name for a sandbox
// carries: 8 hex chars of sha256(uid + name).
//
// The uid is in the hash, and must be in EVERY device's — a name that collides
// across users is worse than it sounds. Two users with an identically-named
// sandbox would land on one TAP: the second `clawk up` finds the device
// already there, skips creation, and re-enslaves the first user's LIVE TAP to
// its own bridge (killing that VM's network) — then firecracker cannot open it
// anyway, because the device belongs to the other uid. `clawk destroy` by
// either user would delete the other's devices too. Keeping the hash in one
// place is what stops a new device kind from reintroducing that.
func deviceHash(sbName string) string {
	sum := sha256.Sum256([]byte(strconv.Itoa(os.Getuid()) + ":" + sbName))
	return hex.EncodeToString(sum[:4])
}

// firecracker-ci kernel catalog. The bucket exposes versioned
// `vmlinux-X.Y.Z` objects; we pick the newest by component-wise version
// ordering. The rootfs comes from the configured OCI image, not here.
const (
	ciCacheSubdir = ".cache/clawk/linux-assets"
	ciS3Host      = "s3.amazonaws.com"
	ciS3Bucket    = "spec.ccfc.min"
	ciVersion     = "v1.10"
	ciKernelPfx   = "vmlinux-"
)

// ciArch is the firecracker-ci asset arch path for the host. The bucket uses
// kernel-style names (aarch64 / x86_64), not Go's GOARCH ("arm64" / "amd64").
func ciArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	default:
		return runtime.GOARCH
	}
}

// tapDevice returns a deterministic TAP name for a sandbox. IFNAMSIZ is 16
// (15 usable); "clawk" + 8 hex chars (see deviceHash) is 13.
func tapDevice(sbName string) string { return "clawk" + deviceHash(sbName) }

// ensureLinuxBridge is idempotent: creates the sandbox's (IP-less) bridge and
// brings it up. No address is assigned — see bridgeDevice. Each step is
// skipped when sysfs already reports it done (netstate_linux.go), so
// re-booting an existing sandbox performs no privileged work.
func ensureLinuxBridge(bridge string) error {
	if !linkExists(bridge) {
		if err := runSudo("ip", "link", "add", "name", bridge, "type", "bridge"); err != nil {
			return err
		}
	}
	if linkIsUp(bridge) {
		return nil
	}
	return runSudo("ip", "link", "set", bridge, "up")
}

// gvTapDevice returns the deterministic name of the daemon-owned TAP that
// feeds gvproxy for a sandbox — the firecracker guest's TAP (tapDevice)
// plus a "g" suffix. IFNAMSIZ is 16 (15 usable); "clawk"+8 hex+"g" is 14.
func gvTapDevice(sbName string) string { return tapDevice(sbName) + "g" }

// ensureTAP creates the TAP device if missing, enslaves it to the bridge,
// and brings it up. The device is owned by the current uid so the (non-
// root) daemon and firecracker can open its fd without CAP_NET_ADMIN.
// Safe to call on an already-configured device, and free of privileged
// calls when it already is one.
func ensureTAP(tap, bridge string) error {
	if !linkExists(tap) {
		uid := strconv.Itoa(os.Getuid())
		if err := runSudo("ip", "tuntap", "add", "dev", tap, "mode", "tap", "user", uid); err != nil {
			return err
		}
	}
	if linkMaster(tap) != bridge {
		if err := runSudo("ip", "link", "set", tap, "master", bridge); err != nil {
			return err
		}
	}
	if linkIsUp(tap) {
		return nil
	}
	return runSudo("ip", "link", "set", tap, "up")
}

// sudoAuthError is a `sudo -n` failure sudo attributed to a missing
// password rather than to the command itself. runSudo retries these
// interactively when it has a terminal; every other failure is final.
type sudoAuthError struct{ err error }

func (e *sudoAuthError) Error() string { return e.err.Error() }
func (e *sudoAuthError) Unwrap() error { return e.err }

// runSudo runs a privileged command, preferring the non-interactive path.
//
// `sudo -n` first: on a host with passwordless sudo (or a live timestamp
// record for this terminal) it succeeds silently, and that is both the
// common case and the only path available to a caller with no terminal.
// When it fails purely for want of a password and we DO have a terminal,
// retry without -n so sudo can ask — one prompt in the foreground beats a
// hard failure, which is what issue #9 hit. Teardown paths that must never
// prompt call runSudoQuiet directly.
func runSudo(cmd string, args ...string) error {
	err := runSudoQuiet(cmd, args...)
	var authErr *sudoAuthError
	if err == nil || !errors.As(err, &authErr) {
		return err
	}
	if !StdinIsTerminal() {
		return fmt.Errorf("%w — and no terminal to prompt on: "+
			"run this from a terminal, or grant passwordless sudo for ip", err)
	}
	noteSudoPrompt()
	full := append([]string{cmd}, args...)
	c := exec.Command("sudo", full...)
	// Prompt and any command output go to stderr: stdout belongs to the
	// CLI's own (sometimes parsed) output.
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stderr, os.Stderr
	if runErr := c.Run(); runErr != nil {
		return fmt.Errorf("sudo %s %s: %w", cmd, strings.Join(args, " "), runErr)
	}
	return nil
}

// runSudoQuiet runs `sudo -n <cmd> <args...>` and never prompts, wrapping
// the error with the command output for diagnosis. Failures sudo blames on
// a missing password come back as *sudoAuthError so runSudo can decide
// whether a prompt is possible.
func runSudoQuiet(cmd string, args ...string) error {
	full := append([]string{"-n", cmd}, args...)
	c := exec.Command("sudo", full...)
	c.Env = cLocaleEnv()
	out, err := c.CombinedOutput()
	if err == nil {
		return nil
	}
	wrapped := fmt.Errorf("sudo %s %s: %w (%s)", cmd, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	if sudoOutputMentionsPassword(out) {
		return &sudoAuthError{err: wrapped}
	}
	return wrapped
}

// cLocaleEnv is our environment with LC_ALL=C, so sudo's own diagnostics come
// back in English and sudoOutputMentionsPassword can match them. sudo emits
// those from the front-end process, before env_reset touches what the command
// sees, so this only affects sudo's messages — not the command's behavior.
func cLocaleEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "LC_ALL=") || strings.HasPrefix(kv, "LANG=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "LC_ALL=C")
}

// sudoOutputMentionsPassword reports whether a failed `sudo -n` attempt was
// sudo declining to authenticate rather than the command itself failing.
//
// Matching sudo's own message is the whole test, and reliable because we force
// LC_ALL=C (see cLocaleEnv). Anything sudo doesn't blame on authentication
// belongs to the command, which matters because runSudo RETRIES what this
// returns true for: a misclassified "RTNETLINK answers: File exists" re-runs a
// privileged command that already ran.
//
// There is deliberately no "can sudo run anything at all?" fallback probe. It
// looks like a safety net and is the opposite: on a sudoers that grants
// NOPASSWD for `ip` alone — the setup clawk documents — `sudo -n true` fails on
// every host, including ones where `sudo ip` runs freely, so every ordinary
// command failure came back as "needs a password".
//
// Deliberately NOT `sudo -n -v` either, which looks like the obvious probe and
// is wrong for the mirror-image reason: -v validates the credential cache, and
// a host with NOPASSWD has no cache to validate, so sudo tries to authenticate
// and -n makes that fail — "a password is required" on a host where every
// command in fact runs freely.
func sudoOutputMentionsPassword(out []byte) bool {
	lower := bytes.ToLower(out)
	// "no tty present and no askpass program specified" is the same refusal
	// worded without the word "password" (older sudo, and sudo built without
	// the -n message); the ones that do say it cover the rest.
	return bytes.Contains(lower, []byte("password")) ||
		bytes.Contains(lower, []byte("askpass"))
}

// SudoIPWorksUnprompted reports whether `sudo ip` runs without asking for a
// password — the one privileged thing bridge mode needs.
//
// It probes with `ip -V`, the actual binary under the actual sudoers rules:
// side-effect free, and accurate for a sudoers that permits only `ip` (where
// probing `true` or `-v` would wrongly report that a password is needed).
func SudoIPWorksUnprompted() bool {
	c := exec.Command("sudo", "-n", "ip", "-V")
	c.Env = cLocaleEnv()
	return c.Run() == nil
}

// StdinIsTerminal reports whether we can prompt the user for a password.
// Exported because doctor's verdict on bridge mode turns on the same question.
func StdinIsTerminal() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// sudoPromptOnce keeps the explanation to one line per process, however
// many devices the run has to create.
var sudoPromptOnce sync.Once

// noteSudoPrompt explains, once, why a VM boot is asking for a password —
// otherwise a bare "[sudo] password for …" in the middle of `clawk up`
// looks like the agent escalating, which is exactly the fear issue #9
// raised.
func noteSudoPrompt() {
	sudoPromptOnce.Do(func() {
		fmt.Fprintln(os.Stderr,
			"clawk needs sudo once per sandbox to create its network devices "+
				"(ip tuntap / ip link); the VM itself runs unprivileged. "+
				"Run 'clawk doctor' for the zero-sudo setup.")
	})
}

// ciCacheDir is the user-local cache for the downloaded kernel.
func ciCacheDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ciCacheSubdir)
}

// ciEnsureKernel returns the path to the newest firecracker-ci vmlinux,
// downloading it on first use. Cached across providers.
func ciEnsureKernel(ctx context.Context) (string, error) {
	cache := ciCacheDir()
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return "", err
	}
	key, err := ciLatestKernelKey(ctx)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(cache, filepath.Base(key))
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	url := fmt.Sprintf("https://%s/%s/%s", ciS3Host, ciS3Bucket, key)
	return dst, downloadAsset(ctx, url, dst)
}

// ciLatestKernelKey lists the S3 bucket and returns the newest vmlinux key,
// compared component-wise so 5.10.223 sorts higher than 5.10.99.
func ciLatestKernelKey(ctx context.Context) (string, error) {
	// Virtual-hosted style fails TLS (bucket name has dots); path-style
	// HTTPS works, or plain-HTTP on the same host.
	listURL := fmt.Sprintf("http://%s.%s/?prefix=firecracker-ci/%s/%s/%s&list-type=2",
		ciS3Bucket, ciS3Host, ciVersion, ciArch(), ciKernelPfx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, http.NoBody)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	re := regexp.MustCompile(`<Key>(firecracker-ci/[^<]+vmlinux-[0-9]+\.[0-9]+\.[0-9]+)</Key>`)
	matches := re.FindAllStringSubmatch(string(body), -1)
	if len(matches) == 0 {
		return "", errors.New("no kernels listed")
	}
	keys := make([]string, 0, len(matches))
	for _, m := range matches {
		keys = append(keys, m[1])
	}
	sort.Slice(keys, func(i, j int) bool { return versionLess(keys[i], keys[j]) })
	return keys[len(keys)-1], nil
}

func versionLess(a, b string) bool {
	va, vb := trailingVersion(a), trailingVersion(b)
	for i := 0; i < len(va) && i < len(vb); i++ {
		if va[i] != vb[i] {
			return va[i] < vb[i]
		}
	}
	return len(va) < len(vb)
}

func trailingVersion(s string) []int {
	idx := strings.LastIndex(s, ciKernelPfx)
	if idx < 0 {
		return nil
	}
	parts := strings.Split(s[idx+len(ciKernelPfx):], ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		out = append(out, n)
	}
	return out
}

func downloadAsset(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("building request for %s: %w", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	tmp := dst + ".partial"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("creating %s: %w", tmp, err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("finalising %s: %w", dst, err)
	}
	return nil
}

// checkKVMAccess verifies the invoking user can open /dev/kvm read/write —
// the one host prerequisite firecracker can't degrade around. It returns an
// actionable error (naming the kvm-group fix) instead of letting the failure
// surface later as firecracker's opaque InstanceStart error in a detached
// daemon log. See FirecrackerProvider.Start for why the check lives there.
func checkKVMAccess() error {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err == nil {
		f.Close()
		return nil
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return errors.New("/dev/kvm not present — this host has no KVM support " +
			"(enable virtualization in BIOS, or use a metal/nested-virt instance)")
	case errors.Is(err, os.ErrPermission):
		return errors.New("/dev/kvm is not accessible (permission denied) — " +
			"add yourself to the kvm group: sudo usermod -aG kvm $USER, then start a new login session")
	default:
		return fmt.Errorf("cannot open /dev/kvm: %w", err)
	}
}

// readPIDFile reads a pidfile and returns the pid, or 0 on any error.
func readPIDFile(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

// processAlive reports whether pid is still running. It probes with the
// null signal (signal 0): the kernel runs its existence/permission checks
// without delivering anything. NOTE: must be syscall.Signal(0), not a nil
// os.Signal — os.Process.Signal rejects a nil signal as "unsupported
// signal type", which would make this always report dead (so Start never
// sees an already-running daemon and Status always says "stopped").
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// stopByPIDFile sends SIGTERM to the process named by pidPath, waits up to
// timeout, then escalates to SIGKILL. A missing or stale pidfile is not an
// error — the caller's intent was "make sure it's not running."
func stopByPIDFile(pidPath string, timeout time.Duration) error {
	pid := readPIDFile(pidPath)
	if pid <= 0 {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	if err := proc.Signal(os.Interrupt); err != nil {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return proc.Kill()
}
