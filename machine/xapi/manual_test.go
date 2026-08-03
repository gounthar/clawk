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
//	TEST_XAPI_INSECURE=0     # verify the pool's certificate (default: skip,
//	                         # because a stock XCP-ng cert is self-signed)
//
// This reads; it creates nothing and destroys nothing, so it is safe to
// point at a pool that has real VMs on it. The VM named by TEST_XAPI_VM is
// only ever asked for its power state. The one thing here that changes any
// pool state is TestPoolSessionRecovery_Manual ending its own session, which
// touches no storage and no VM.

import (
	"context"
	"os"
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
	return Config{
		URL:         url,
		Username:    user,
		Password:    os.Getenv("TEST_XAPI_PASSWORD"),
		InsecureTLS: os.Getenv("TEST_XAPI_INSECURE") != "0",
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
