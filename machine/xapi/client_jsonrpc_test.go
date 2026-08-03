package xapi

// Wire-format tests for the JSON-RPC transport, against an httptest server
// standing in for a pool. These need no hypervisor and run in CI; the
// pool-dependent versions live in manual_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

const testSession = "OpaqueRef:11111111-2222-3333-4444-555555555555"

// fakeRPC is a pool that answers JSON-RPC, recording every request it sees.
type fakeRPC struct {
	t    *testing.T
	srv  *httptest.Server
	reqs []rpcRequest

	// handle answers a method; returning (nil, nil) falls through to a
	// generic "no such method" XAPI error.
	handle func(method string, params []any) (any, *rpcError)
}

func newFakeRPC(t *testing.T, handle func(string, []any) (any, *rpcError)) *fakeRPC {
	t.Helper()
	f := &fakeRPC{t: t, handle: handle}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRPC) serve(w http.ResponseWriter, r *http.Request) {
	require.Equal(f.t, "/jsonrpc", r.URL.Path, "transport must POST to /jsonrpc")
	require.Equal(f.t, http.MethodPost, r.Method)
	require.Equal(f.t, "application/json", r.Header.Get("Content-Type"))

	var req rpcRequest
	require.NoError(f.t, json.NewDecoder(r.Body).Decode(&req))
	require.Equal(f.t, "2.0", req.JSONRPC)
	f.reqs = append(f.reqs, req)

	result, rerr := f.handle(req.Method, req.Params)
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
	require.NoError(f.t, json.NewEncoder(w).Encode(resp))
}

func (f *fakeRPC) methods() []string {
	var m []string
	for _, r := range f.reqs {
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
		URL:      f.srv.URL,
		Username: "root",
		Password: "hunter2",
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
		f.reqs[0].Params,
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
		URL: f.srv.URL, Username: "root", Password: "wrong",
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

	last := f.reqs[len(f.reqs)-1]
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
	var refs []string
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
	ctx := context.Background()

	t.Run("one match", func(t *testing.T) {
		refs = []string{"OpaqueRef:only"}
		ref, err := cl.VMByNameLabel(ctx, "alpine")
		require.NoError(t, err)
		require.Equal(t, VMRef("OpaqueRef:only"), ref)
	})

	t.Run("no match", func(t *testing.T) {
		refs = nil
		_, err := cl.VMByNameLabel(ctx, "ghost")
		require.ErrorContains(t, err, `no VM with name-label "ghost"`)
	})

	t.Run("ambiguous", func(t *testing.T) {
		refs = []string{"OpaqueRef:a", "OpaqueRef:b"}
		_, err := cl.VMByNameLabel(ctx, "dup")
		require.ErrorContains(t, err, "refusing to guess",
			"name-labels are not unique in XAPI")
	})
}

func TestJSONRPCHTTPErrorIsSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "<html>gateway timeout</html>", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	_, err := newJSONRPCClient(context.Background(), Config{
		URL: srv.URL, Username: "root",
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

	before := len(f.reqs)
	require.NoError(t, cl.Close(), "second Close is a no-op")
	require.Len(t, f.reqs, before, "no second logout")

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

	require.Len(t, f.reqs, 1, "unimplemented calls must not reach the pool")
}

func TestJSONRPCRequiresURLAndUser(t *testing.T) {
	_, err := newJSONRPCClient(context.Background(), Config{Username: "root"})
	require.ErrorContains(t, err, "Config.URL is required")

	_, err = newJSONRPCClient(context.Background(), Config{URL: "https://pool"})
	require.ErrorContains(t, err, "Config.Username is required")
}
