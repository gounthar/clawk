package xapi

// Wire-format tests for the JSON-RPC transport, against an httptest server
// standing in for a pool. These need no hypervisor and run in CI; the
// pool-dependent versions live in manual_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSession = "OpaqueRef:11111111-2222-3333-4444-555555555555"

// fakeRPC is a pool that answers JSON-RPC, recording every request it sees.
//
// serve runs on the httptest handler goroutine, not the test's. Two
// consequences shape this type:
//
//   - It asserts with assert, never require. require.FailNow calls
//     runtime.Goexit, which would unwind the handler goroutine mid-response;
//     the client then sees a truncated body and reports a decode error
//     instead of the assertion that explains what actually went wrong.
//   - Everything the handler touches is guarded by mu, since the test
//     goroutine reads it concurrently.
type fakeRPC struct {
	t   *testing.T
	srv *httptest.Server

	mu   sync.Mutex
	reqs []rpcRequest
	// handle answers a method; returning (nil, nil) falls through to a
	// generic "no such method" XAPI error.
	handle func(method string, params []any) (any, *rpcError)
}

// The fake serves TLS because the transport refuses a cleartext pool URL.
// Its certificate is self-signed, so dialFake opts into InsecureTLS — the
// same shape as a stock XCP-ng install, which is the case that matters.
func newFakeRPC(t *testing.T, handle func(string, []any) (any, *rpcError)) *fakeRPC {
	t.Helper()
	f := &fakeRPC{t: t, handle: handle}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRPC) serve(w http.ResponseWriter, r *http.Request) {
	assert.Equal(f.t, "/jsonrpc", r.URL.Path, "transport must POST to /jsonrpc")
	assert.Equal(f.t, http.MethodPost, r.Method)
	assert.Equal(f.t, "application/json", r.Header.Get("Content-Type"))

	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		assert.NoError(f.t, err, "decode request")
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	assert.Equal(f.t, "2.0", req.JSONRPC)

	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	handle := f.handle
	f.mu.Unlock()

	result, rerr := handle(req.Method, req.Params)
	if rerr == nil && result == nil {
		rerr = &rpcError{Code: 1, Message: "MESSAGE_METHOD_UNKNOWN"}
	}

	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if rerr != nil {
		resp["error"] = rerr
	} else {
		resp["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	assert.NoError(f.t, json.NewEncoder(w).Encode(resp))
}

// requests returns a copy of what the handler has recorded so far.
func (f *fakeRPC) requests() []rpcRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rpcRequest(nil), f.reqs...)
}

func (f *fakeRPC) methods() []string {
	var m []string
	for _, r := range f.requests() {
		m = append(m, r.Method)
	}
	return m
}

// loginOnly answers session.login_with_password and nothing else.
func loginOnly(method string, params []any) (any, *rpcError) {
	if method == "session.login_with_password" {
		return testSession, nil
	}
	return nil, nil
}

func dialFake(t *testing.T, f *fakeRPC) *jsonrpcClient {
	t.Helper()
	cl, err := newJSONRPCClient(context.Background(), Config{
		URL:         f.srv.URL,
		Username:    "root",
		Password:    "hunter2",
		InsecureTLS: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}

func TestJSONRPCLoginSendsExpectedParams(t *testing.T) {
	f := newFakeRPC(t, loginOnly)
	cl := dialFake(t, f)

	require.Equal(t, []string{"session.login_with_password"}, f.methods())
	require.Equal(t,
		[]any{"root", "hunter2", apiVersion, originator},
		f.requests()[0].Params,
		"login takes user, password, api version, originator")

	cl.mu.Lock()
	defer cl.mu.Unlock()
	require.Equal(t, testSession, cl.session)
}

func TestJSONRPCLoginFailureIsSurfaced(t *testing.T) {
	f := newFakeRPC(t, func(method string, _ []any) (any, *rpcError) {
		return nil, &rpcError{
			Code:    1,
			Message: "SESSION_AUTHENTICATION_FAILED",
			Data:    json.RawMessage(`["root","Authentication failure"]`),
		}
	})

	_, err := newJSONRPCClient(context.Background(), Config{
		URL: f.srv.URL, Username: "root", Password: "wrong", InsecureTLS: true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "SESSION_AUTHENTICATION_FAILED")
	require.Contains(t, err.Error(), "Authentication failure",
		"the XAPI error's data carries the useful detail")
}

func TestJSONRPCCallsCarrySessionRef(t *testing.T) {
	f := newFakeRPC(t, func(method string, params []any) (any, *rpcError) {
		switch method {
		case "session.login_with_password":
			return testSession, nil
		case "VM.get_power_state":
			return string(PowerRunning), nil
		}
		return nil, nil
	})
	cl := dialFake(t, f)

	state, err := cl.VMPowerState(context.Background(), VMRef("OpaqueRef:vm"))
	require.NoError(t, err)
	require.Equal(t, PowerRunning, state)

	reqs := f.requests()
	last := reqs[len(reqs)-1]
	require.Equal(t, "VM.get_power_state", last.Method)
	require.Equal(t, []any{testSession, "OpaqueRef:vm"}, last.Params,
		"session ref leads every non-login call")
}

func TestJSONRPCPowerStateRejectsUnknownValue(t *testing.T) {
	f := newFakeRPC(t, func(method string, _ []any) (any, *rpcError) {
		switch method {
		case "session.login_with_password":
			return testSession, nil
		case "VM.get_power_state":
			return "Rebooting", nil // not one of XAPI's four
		}
		return nil, nil
	})
	cl := dialFake(t, f)

	_, err := cl.VMPowerState(context.Background(), VMRef("OpaqueRef:vm"))
	require.ErrorContains(t, err, `unknown power state "Rebooting"`)
}

func TestJSONRPCVMByNameLabel(t *testing.T) {
	// Each case builds its own pool so the ref list is fixed before the
	// server starts. Sharing one fake and reassigning the refs between
	// subtests would have the test goroutine writing what the handler
	// goroutine reads.
	cases := []struct {
		name    string
		refs    []string
		label   string
		wantRef VMRef
		wantErr string
	}{
		{
			name:    "one match",
			refs:    []string{"OpaqueRef:only"},
			label:   "alpine",
			wantRef: "OpaqueRef:only",
		},
		{
			name:    "no match",
			refs:    nil,
			label:   "ghost",
			wantErr: `no VM with name-label "ghost"`,
		},
		{
			name:    "ambiguous",
			refs:    []string{"OpaqueRef:a", "OpaqueRef:b"},
			label:   "dup",
			wantErr: "refusing to guess",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refs := tc.refs
			f := newFakeRPC(t, func(method string, _ []any) (any, *rpcError) {
				switch method {
				case "session.login_with_password":
					return testSession, nil
				case "VM.get_by_name_label":
					return refs, nil
				}
				return nil, nil
			})
			cl := dialFake(t, f)

			ref, err := cl.VMByNameLabel(context.Background(), tc.label)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantRef, ref)
		})
	}
}

func TestJSONRPCHTTPErrorIsSurfaced(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "<html>gateway timeout</html>", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	_, err := newJSONRPCClient(context.Background(), Config{
		URL: srv.URL, Username: "root", InsecureTLS: true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "502")
	require.Contains(t, err.Error(), "gateway timeout",
		"the body should reach the operator; a wrong endpoint usually says why")
}

func TestJSONRPCCloseLogsOutAndIsIdempotent(t *testing.T) {
	f := newFakeRPC(t, func(method string, _ []any) (any, *rpcError) {
		switch method {
		case "session.login_with_password":
			return testSession, nil
		case "session.logout":
			return struct{}{}, nil
		}
		return nil, nil
	})
	cl := dialFake(t, f)

	require.NoError(t, cl.Close())
	require.Contains(t, f.methods(), "session.logout")

	before := len(f.requests())
	require.NoError(t, cl.Close(), "second Close is a no-op")
	require.Len(t, f.requests(), before, "no second logout")

	_, err := cl.VMPowerState(context.Background(), VMRef("OpaqueRef:vm"))
	require.ErrorContains(t, err, "no session")
}

func TestJSONRPCUnimplementedMethodsReturnError(t *testing.T) {
	f := newFakeRPC(t, loginOnly)
	cl := dialFake(t, f)
	ctx := context.Background()

	_, err := cl.VMCreate(ctx, VMConfig{})
	require.ErrorIs(t, err, errNotImplemented)
	require.ErrorIs(t, cl.VMStart(ctx, "ref"), errNotImplemented)
	_, err = cl.VDIClone(ctx, "vdi")
	require.ErrorIs(t, err, errNotImplemented)

	require.Len(t, f.requests(), 1, "unimplemented calls must not reach the pool")
}

// sessionPool is a fake pool that tracks which session ref is currently
// valid, so an expiry can be simulated and the re-login counted. Its own mu
// guards it because handle runs on the httptest handler goroutines.
type sessionPool struct {
	mu     sync.Mutex
	valid  string
	logins int
	// rejectAll makes every session ref invalid, including freshly issued
	// ones — the pool that cannot be recovered from.
	rejectAll bool
	// loginErr, once set, fails every login after the first.
	loginErr bool
}

func (p *sessionPool) handle(method string, params []any) (any, *rpcError) {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch method {
	case "session.login_with_password":
		if p.loginErr && p.logins > 0 {
			return nil, &rpcError{Code: 1, Message: "SESSION_AUTHENTICATION_FAILED"}
		}
		p.logins++
		p.valid = fmt.Sprintf("OpaqueRef:session-%d", p.logins)
		return p.valid, nil
	case "session.logout":
		return struct{}{}, nil
	case "VM.get_power_state":
		if p.rejectAll || len(params) == 0 || params[0] != p.valid {
			return nil, &rpcError{Code: 1, Message: "SESSION_INVALID"}
		}
		return string(PowerRunning), nil
	}
	return nil, nil
}

// expire invalidates the session the client is holding, the way XAPI does
// when it times one out.
func (p *sessionPool) expire() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.valid = ""
}

func (p *sessionPool) loginCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.logins
}

// An expired session must not end the client. XAPI times sessions out, and
// a sandbox can sit idle for longer than the timeout.
func TestJSONRPCRetriesOnceAfterSessionInvalid(t *testing.T) {
	p := &sessionPool{}
	f := newFakeRPC(t, p.handle)
	cl := dialFake(t, f)
	require.Equal(t, 1, p.loginCount())

	p.expire()

	state, err := cl.VMPowerState(context.Background(), VMRef("OpaqueRef:vm"))
	require.NoError(t, err, "an expired session should be replaced, not returned")
	require.Equal(t, PowerRunning, state)
	require.Equal(t, 2, p.loginCount(), "exactly one re-login")

	// And the replacement is durable: the next call reuses it.
	_, err = cl.VMPowerState(context.Background(), VMRef("OpaqueRef:vm"))
	require.NoError(t, err)
	require.Equal(t, 2, p.loginCount(), "no login per call")
}

// The retry is once, not a loop. A pool that rejects every session must
// produce an error rather than spin.
func TestJSONRPCSessionRetryDoesNotLoop(t *testing.T) {
	p := &sessionPool{rejectAll: true}
	f := newFakeRPC(t, p.handle)
	cl := dialFake(t, f)

	_, err := cl.VMPowerState(context.Background(), VMRef("OpaqueRef:vm"))
	require.ErrorContains(t, err, "SESSION_INVALID")

	var calls int
	for _, m := range f.methods() {
		if m == "VM.get_power_state" {
			calls++
		}
	}
	require.Equal(t, 2, calls, "the original call and one retry")
	require.Equal(t, 2, p.loginCount(), "one re-login attempt")
}

// When the re-login itself fails, both failures have to reach the operator:
// the expired session explains the attempt, the login error explains why it
// did not help.
func TestJSONRPCReloginFailureReportsBoth(t *testing.T) {
	p := &sessionPool{loginErr: true}
	f := newFakeRPC(t, p.handle)
	cl := dialFake(t, f)

	p.expire()

	_, err := cl.VMPowerState(context.Background(), VMRef("OpaqueRef:vm"))
	require.ErrorContains(t, err, "SESSION_INVALID")
	require.ErrorContains(t, err, "SESSION_AUTHENTICATION_FAILED")
}

// Concurrent callers meeting the same expired session must produce one
// login between them, not one each.
func TestJSONRPCConcurrentCallersReloginOnce(t *testing.T) {
	p := &sessionPool{}
	f := newFakeRPC(t, p.handle)
	cl := dialFake(t, f)

	p.expire()

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = cl.VMPowerState(context.Background(), VMRef("OpaqueRef:vm"))
		}()
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "caller %d", i)
	}
	require.Equal(t, 2, p.loginCount(), "one re-login for all callers, not one each")
}

// NewClient returns the Client interface. A failed dial must produce a nil
// interface, not a non-nil one wrapping a nil *jsonrpcClient — testify's
// Nil would not catch the difference, so compare directly.
func TestNewClientReturnsNilInterfaceOnFailure(t *testing.T) {
	cl, err := NewClient(context.Background(), Config{Username: "root"})
	require.Error(t, err)
	require.True(t, cl == nil, "a failed NewClient must return a nil Client")
}

func TestJSONRPCRequiresURLAndUser(t *testing.T) {
	_, err := newJSONRPCClient(context.Background(), Config{Username: "root"})
	require.ErrorContains(t, err, "Config.URL is required")

	_, err = newJSONRPCClient(context.Background(), Config{URL: "https://pool"})
	require.ErrorContains(t, err, "Config.Username is required")
}

// A cleartext pool URL would put the password on the wire at login and the
// session ref on every call after it, so it is refused rather than dialled.
// InsecureTLS must not be read as consent to that: it waives certificate
// verification, not encryption.
func TestJSONRPCRejectsCleartextAndSchemelessURLs(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"cleartext", "http://pool.lab.example"},
		{"no scheme", "pool.lab.example"},
		{"scheme only", "https://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newJSONRPCClient(context.Background(), Config{
				URL: tc.url, Username: "root", Password: "hunter2",
				InsecureTLS: true,
			})
			require.ErrorContains(t, err, "must be https://host",
				"the error must name the field an operator has to change")
		})
	}
}
