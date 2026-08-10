package xapi

// Manual harness for the JSON-RPC transport against a real XCP-ng pool:
//
//	TEST_XAPI_POOL=https://xcp-ng-01.lab.example \
//	TEST_XAPI_PASSWORD=... \
//	TEST_XAPI_VM='some-existing-vm' \
//	  go test ./xapi -run TestPool -v
//
// Optional:
//
//	TEST_XAPI_USER=root      # defaults to root
//	TEST_XAPI_INSECURE=false # verify the pool's certificate. Any boolean Go
//	                         # accepts works (0/1, false/true, f/t). Default
//	                         # is to skip, because a stock XCP-ng cert is
//	                         # self-signed; an unparseable value is an error
//	                         # rather than a silent default.
//
// Everything above reads; it creates nothing and destroys nothing, so it is
// safe to point at a pool that has real VMs on it. The VM named by
// TEST_XAPI_VM is only ever asked for its power state. The one thing there
// that changes any pool state is TestPoolSessionRecovery_Manual ending its
// own session, which touches no storage and no VM.
//
// The storage round-trip is different: it creates VDIs. It therefore has its
// own switch and does not ride on TEST_XAPI_POOL:
//
//	TEST_XAPI_POOL=https://... TEST_XAPI_PASSWORD=... \
//	TEST_XAPI_SR=<sr-uuid> TEST_XAPI_WRITE=1 \
//	  go test ./xapi -run TestPoolVDI -v
//
// Two variables rather than one is deliberate. Someone pointing the existing
// read-only suite at a production pool should not discover that the switch
// they already set has started creating disks — and the SR has to be named
// explicitly, so the test cannot pick a default and write somewhere nobody
// chose. It cleans up what it makes, including on failure.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func poolConfigFromEnv(t *testing.T) Config {
	t.Helper()
	url := os.Getenv("TEST_XAPI_POOL")
	if url == "" {
		t.Skip("set TEST_XAPI_POOL=https://... to run (talks to a real XCP-ng pool)")
	}
	user := os.Getenv("TEST_XAPI_USER")
	if user == "" {
		user = "root"
	}

	// Default to skipping verification, because a stock XCP-ng certificate
	// is self-signed. Parse rather than compare against "0": this is the one
	// knob that decides whether the pool's certificate is checked, and a
	// string compare silently gives TEST_XAPI_INSECURE=false the opposite of
	// what it says. An unparseable value fails the test rather than picking
	// a default, since guessing here guesses about TLS.
	insecure := true
	if v := os.Getenv("TEST_XAPI_INSECURE"); v != "" {
		b, err := strconv.ParseBool(v)
		require.NoError(t, err,
			"TEST_XAPI_INSECURE must be a boolean (1/0, true/false, t/f)")
		insecure = b
	}

	return Config{
		URL:         url,
		Username:    user,
		Password:    os.Getenv("TEST_XAPI_PASSWORD"),
		InsecureTLS: insecure,
	}
}

// TestPoolLogin_Manual proves the transport can authenticate and log out.
// If this passes, the endpoint, the TLS setup and the credentials are all
// right, which is most of what goes wrong first.
func TestPoolLogin_Manual(t *testing.T) {
	cfg := poolConfigFromEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cl, err := newJSONRPCClient(ctx, cfg)
	require.NoError(t, err, "login to %s", cfg.URL)

	cl.mu.Lock()
	session := cl.session
	cl.mu.Unlock()
	require.NotEmpty(t, session, "session ref after login")

	require.NoError(t, cl.Close(), "logout")

	// After Close the session is gone, so a call must fail rather than
	// silently reuse a logged-out ref.
	_, err = cl.VMPowerState(ctx, VMRef("OpaqueRef:whatever"))
	require.Error(t, err, "call after Close must fail")
}

// TestPoolVMPowerState_Manual is the milestone this transport exists for:
// name a VM that already exists on the pool, and read its power state.
func TestPoolVMPowerState_Manual(t *testing.T) {
	cfg := poolConfigFromEnv(t)
	name := os.Getenv("TEST_XAPI_VM")
	if name == "" {
		t.Skip("set TEST_XAPI_VM to the name-label of a VM on the pool")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cl, err := newJSONRPCClient(ctx, cfg)
	require.NoError(t, err, "login to %s", cfg.URL)
	defer cl.Close()

	ref, err := cl.VMByNameLabel(ctx, name)
	require.NoError(t, err, "look up VM %q", name)
	require.NotEmpty(t, ref)
	t.Logf("VM %q is %s", name, ref)

	state, err := cl.VMPowerState(ctx, ref)
	require.NoError(t, err, "VM.get_power_state")
	require.Contains(t,
		[]PowerState{PowerHalted, PowerPaused, PowerRunning, PowerSuspended},
		state, "power state must be one of XAPI's four")
	t.Logf("VM %q power state: %s", name, state)
}

// TestPoolSessionRecovery_Manual is the one part of the session-recovery
// work that a fake pool cannot vouch for.
//
// The unit tests assert that sessionCall retries on SESSION_INVALID, but
// they do so against a fake that returns the error name this package
// expects — so they prove the retry logic and nothing about whether XAPI
// actually names it that way over JSON-RPC. If the real name or shape
// differs, isSessionInvalid never matches, the retry never fires, and every
// unit test still passes. This is where that shows up.
//
// The session is ended behind the client's back, which is the closest thing
// to an XAPI timeout available on demand. Read-only otherwise: VM.get_all
// lists refs and touches no storage.
func TestPoolSessionRecovery_Manual(t *testing.T) {
	cfg := poolConfigFromEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cl, err := newJSONRPCClient(ctx, cfg)
	require.NoError(t, err, "login to %s", cfg.URL)
	defer cl.Close()

	first, gen := cl.currentSession()
	require.NotEmpty(t, first, "session ref after login")
	require.NoError(t, cl.logout(first), "end the session from under the client")

	var refs []string
	err = cl.sessionCall(ctx, "VM.get_all", &refs)
	require.NoError(t, err,
		"a call meeting an ended session must recover; a SESSION_INVALID here "+
			"means the pool names the failure something isSessionInvalid does not match")

	second, gen2 := cl.currentSession()
	require.NotEqual(t, first, second, "the session ref must have been replaced")
	require.Equal(t, gen+1, gen2, "exactly one re-login")
	t.Logf("session %s expired, recovered as %s; VM.get_all returned %d refs",
		first, second, len(refs))
}

// TestPoolUnimplemented_Manual checks that the calls this transport has not
// grown yet fail at the pool boundary instead of returning a zero value that
// looks like success.
func TestPoolUnimplemented_Manual(t *testing.T) {
	cfg := poolConfigFromEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cl, err := newJSONRPCClient(ctx, cfg)
	require.NoError(t, err)
	defer cl.Close()

	_, err = cl.VMCreate(ctx, VMConfig{NameLabel: "should-not-be-created"})
	require.ErrorIs(t, err, errNotImplemented)
}

// writeConfigFromEnv gates the tests that create pool objects. It is
// deliberately separate from poolConfigFromEnv: the read-only suite must not
// start writing because someone re-used its variable.
func writeConfigFromEnv(t *testing.T) (Config, string) {
	t.Helper()
	if os.Getenv("TEST_XAPI_WRITE") == "" {
		t.Skip("set TEST_XAPI_WRITE=1 to run (creates and destroys VDIs on a real SR)")
	}
	cfg := poolConfigFromEnv(t)
	sr := os.Getenv("TEST_XAPI_SR")
	if sr == "" {
		t.Skip("set TEST_XAPI_SR to the uuid of the SR to write to")
	}
	cfg.SRUUID = sr
	return cfg, sr
}

// TestPoolVDIRoundTrip_Manual is build-order step 3: prove a raw disk image
// survives the trip to the SR and can be cloned.
//
// It imports a small pattern-filled image, reads the virtual size back,
// finds it by name the way a second sandbox would, clones it, and destroys
// both. The clone is what the per-sandbox disk model rests on, so proving
// import alone would prove half the step.
//
// Cleanup runs through t.Cleanup so that a failure part-way still removes
// what was made. A test that writes to someone's lab and leaves debris on
// the failure path is worse than no test.
func TestPoolVDIRoundTrip_Manual(t *testing.T) {
	cfg, srUUID := writeConfigFromEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cl, err := newJSONRPCClient(ctx, cfg)
	require.NoError(t, err, "login to %s", cfg.URL)
	// The logout is registered as a cleanup, not deferred, and registered
	// first so that LIFO ordering runs it last. `defer cl.Close()` looks
	// equivalent and is not: deferred calls run when the test function
	// returns, and t.Cleanup runs after that, so the session was already gone
	// by the time the VDI destroys below fired. That is not a hypothetical —
	// it leaked two VDIs onto the lab SR on this test's first run, and they
	// had to be removed by hand.
	t.Cleanup(func() { _ = cl.Close() })

	// 8 MiB, and a byte pattern rather than zeroes: a sparse-file shortcut
	// anywhere in the path would still produce a correct-looking size, and
	// zeroes would not notice.
	const size = 8 << 20
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	// A name nobody else would choose, carrying the run so two runs cannot
	// collide into the duplicate-name error.
	name := fmt.Sprintf("clawk-test-%d", time.Now().UnixNano())

	golden, err := cl.VDIImportRaw(ctx, srUUID, name, bytes.NewReader(payload), size)
	require.NoError(t, err, "VDIImportRaw")
	require.NotEmpty(t, golden)
	t.Cleanup(func() {
		// WithoutCancel: cleanup must still run when the test's context has
		// expired, which is exactly when there is most likely to be debris.
		if err := cl.VDIDestroy(context.WithoutCancel(ctx), golden); err != nil {
			t.Errorf("leaked golden VDI %s on the SR: %v", golden, err)
		}
	})
	t.Logf("imported %d bytes as %s (%s)", size, name, golden)

	got, err := cl.vdiVirtualSize(ctx, golden)
	require.NoError(t, err, "VDI.get_virtual_size")
	require.Equal(t, int64(size), got,
		"the pool must report the size we asked for; a mismatch means "+
			"virtual_size did not survive the wire encoding")

	// The lookup a second sandbox from the same image would do.
	found, ok, err := cl.VDIFindByName(ctx, srUUID, name)
	require.NoError(t, err, "VDIFindByName")
	require.True(t, ok, "the VDI just imported must be findable by name")
	require.Equal(t, golden, found, "found a different VDI than the one imported")

	_, ok, err = cl.VDIFindByName(ctx, srUUID, name+"-absent")
	require.NoError(t, err, "a name that is not there is not an error")
	require.False(t, ok)

	clone, err := cl.VDIClone(ctx, golden)
	require.NoError(t, err, "VDI.clone")
	require.NotEmpty(t, clone)
	require.NotEqual(t, golden, clone, "clone must be a distinct VDI")
	t.Cleanup(func() {
		if err := cl.VDIDestroy(context.WithoutCancel(ctx), clone); err != nil {
			t.Errorf("leaked clone VDI %s on the SR: %v", clone, err)
		}
	})

	cloneSize, err := cl.vdiVirtualSize(ctx, clone)
	require.NoError(t, err)
	require.Equal(t, int64(size), cloneSize, "the clone must be the same virtual size")
	t.Logf("cloned %s -> %s", golden, clone)
}

// TestPoolVDIImportCleansUpOnFailure_Manual proves the cleanup path against a
// real pool rather than only against the fake: an import that cannot succeed
// must not leave a VDI wearing the name a later run would adopt as valid.
//
// The failure is forced by declaring a size and then supplying fewer bytes,
// which is the shape a truncated image or a dropped connection takes.
func TestPoolVDIImportCleansUpOnFailure_Manual(t *testing.T) {
	cfg, srUUID := writeConfigFromEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cl, err := newJSONRPCClient(ctx, cfg)
	require.NoError(t, err, "login to %s", cfg.URL)
	defer cl.Close()

	name := fmt.Sprintf("clawk-test-short-%d", time.Now().UnixNano())
	const declared = 4 << 20
	short := bytes.NewReader(make([]byte, declared/2))

	_, err = cl.VDIImportRaw(ctx, srUUID, name, short, declared)
	require.Error(t, err, "an import that delivers half its bytes must not report success")
	t.Logf("import failed as intended: %v", err)

	// Whatever went wrong, nothing may be left behind under that name.
	_, ok, findErr := cl.VDIFindByName(ctx, srUUID, name)
	require.NoError(t, findErr, "VDIFindByName after the failed import")
	if ok {
		t.Fatal("a VDI survived a failed import under the name a later run " +
			"would adopt as a valid golden image")
	}
}
