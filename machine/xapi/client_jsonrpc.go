package xapi

// JSON-RPC transport for XAPI.
//
// XAPI serves the same API over XML-RPC (at /) and JSON-RPC 2.0 (at
// /jsonrpc). This file speaks the latter with nothing but net/http and
// encoding/json, which is why there is no new direct dependency in go.mod
// for it. The alternatives were weighed in NewClient's doc comment.
//
// Only the calls the first milestone needs are implemented. Everything else
// in Client returns errNotImplemented rather than a plausible-looking zero
// value, so an unfinished path fails loudly at the pool boundary instead of
// somewhere further in.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// errNotImplemented marks a Client method this transport has not grown yet.
var errNotImplemented = errors.New("xapi: not implemented in the JSON-RPC transport yet")

const (
	// apiVersion and originator are the third and fourth arguments to
	// session.login_with_password. The originator shows up in the pool's
	// audit log, which is the whole reason to send a useful one.
	apiVersion = "1.0"
	originator = "clawk"

	// maxResponseBytes caps a single RPC response. The calls here return
	// refs and enum strings; anything approaching this is a wrong endpoint
	// answering (an HTML error page, say), not a large legitimate result.
	// Bulk data moves over the import/export HTTP endpoints, not RPC.
	maxResponseBytes = 8 << 20
)

// jsonrpcClient is a Client backed by XAPI's JSON-RPC endpoint. One instance
// owns one session; Close logs it out.
type jsonrpcClient struct {
	endpoint string
	http     *http.Client
	nextID   atomic.Uint64

	mu      sync.Mutex
	session string
}

var _ Client = (*jsonrpcClient)(nil)

// newJSONRPCClient dials the pool and logs in. It returns the concrete type
// so that tests can reach the few pool queries that deliberately are not on
// the Client interface.
func newJSONRPCClient(ctx context.Context, c Config) (*jsonrpcClient, error) {
	if c.URL == "" {
		return nil, errors.New("xapi: Config.URL is required")
	}
	if c.Username == "" {
		return nil, errors.New("xapi: Config.Username is required")
	}

	cl := &jsonrpcClient{
		endpoint: strings.TrimSuffix(c.URL, "/") + "/jsonrpc",
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: c.InsecureTLS, //nolint:gosec // opt-in; see Config.InsecureTLS
				},
			},
			// No client-level Timeout on purpose: each call carries its own
			// context, and a fixed deadline here would eventually truncate
			// the long-running VDI import this transport still has to grow.
		},
	}

	if err := cl.login(ctx, c.Username, c.Password); err != nil {
		cl.http.CloseIdleConnections()
		return nil, err
	}
	return cl, nil
}

// --- wire types -----------------------------------------------------------

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
	ID      uint64 `json:"id"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// rpcError is a JSON-RPC error carrying a XAPI failure. XAPI puts its error
// name in Message ("SESSION_AUTHENTICATION_FAILED", "HANDLE_INVALID", ...)
// and the error's parameters in Data.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("xapi: %s %s", e.Message, e.Data)
	}
	return "xapi: " + e.Message
}

// --- call plumbing --------------------------------------------------------

// call issues one RPC. out may be nil to discard the result.
func (c *jsonrpcClient) call(ctx context.Context, method string, out any, params ...any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      c.nextID.Add(1),
	})
	if err != nil {
		return fmt.Errorf("xapi: encode %s: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("xapi: %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("xapi: %s: %w", method, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("xapi: %s: read response: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("xapi: %s: HTTP %s: %s", method, resp.Status, snippet(raw))
	}

	var r rpcResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("xapi: %s: decode response: %w (body: %s)", method, err, snippet(raw))
	}
	if r.Error != nil {
		return fmt.Errorf("%s: %w", method, r.Error)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(r.Result, out); err != nil {
		return fmt.Errorf("xapi: %s: decode result: %w", method, err)
	}
	return nil
}

// sessionCall issues an RPC with the session ref as the leading argument,
// which is every XAPI method except the session.login family.
func (c *jsonrpcClient) sessionCall(ctx context.Context, method string, out any, params ...any) error {
	c.mu.Lock()
	s := c.session
	c.mu.Unlock()
	if s == "" {
		return errors.New("xapi: no session (client closed, or login failed)")
	}
	return c.call(ctx, method, out, append([]any{s}, params...)...)
}

func snippet(b []byte) string {
	const max = 256
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// --- session -------------------------------------------------------------

func (c *jsonrpcClient) login(ctx context.Context, user, password string) error {
	var ref string
	if err := c.call(ctx, "session.login_with_password", &ref,
		user, password, apiVersion, originator); err != nil {
		return err
	}
	if ref == "" {
		return errors.New("xapi: login returned an empty session ref")
	}
	c.mu.Lock()
	c.session = ref
	c.mu.Unlock()
	return nil
}

// Close logs the session out. It is safe to call more than once.
func (c *jsonrpcClient) Close() error {
	c.mu.Lock()
	s := c.session
	c.session = ""
	c.mu.Unlock()
	if s == "" {
		return nil
	}
	defer c.http.CloseIdleConnections()

	// Logout gets its own short deadline: Close is often called from a
	// defer whose context is already cancelled, and leaking the session on
	// the pool until it expires is worse than a brief wait here.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return c.call(ctx, "session.logout", nil, s)
}

// --- implemented calls ----------------------------------------------------

// VMPowerState reads VM.power_state.
func (c *jsonrpcClient) VMPowerState(ctx context.Context, ref VMRef) (PowerState, error) {
	var s string
	if err := c.sessionCall(ctx, "VM.get_power_state", &s, string(ref)); err != nil {
		return "", err
	}
	switch p := PowerState(s); p {
	case PowerHalted, PowerPaused, PowerRunning, PowerSuspended:
		return p, nil
	default:
		return "", fmt.Errorf("xapi: unknown power state %q", s)
	}
}

// VMByNameLabel resolves a VM by its name-label.
//
// Deliberately not on the Client interface: the backend addresses VMs by the
// ref it got from VMCreate and never needs to search for one. This exists so
// a human (or manual_test.go) can point at a VM that already exists on a
// pool. Name-labels are not unique in XAPI, so a duplicate is an error
// rather than a coin toss.
func (c *jsonrpcClient) VMByNameLabel(ctx context.Context, label string) (VMRef, error) {
	var refs []string
	if err := c.sessionCall(ctx, "VM.get_by_name_label", &refs, label); err != nil {
		return "", err
	}
	switch len(refs) {
	case 0:
		return "", fmt.Errorf("xapi: no VM with name-label %q", label)
	case 1:
		return VMRef(refs[0]), nil
	default:
		return "", fmt.Errorf("xapi: %d VMs share the name-label %q; refusing to guess", len(refs), label)
	}
}

// --- not yet implemented --------------------------------------------------
//
// Each of these is a later step in the build order. They return an error so
// that reaching one is an obvious failure at the pool boundary.

func (c *jsonrpcClient) VMCreate(context.Context, VMConfig) (VMRef, error) {
	return "", errNotImplemented
}
func (c *jsonrpcClient) VMStart(context.Context, VMRef) error          { return errNotImplemented }
func (c *jsonrpcClient) VMShutdown(context.Context, VMRef, bool) error { return errNotImplemented }
func (c *jsonrpcClient) VMDestroy(context.Context, VMRef) error        { return errNotImplemented }

func (c *jsonrpcClient) VMPause(context.Context, VMRef) error             { return errNotImplemented }
func (c *jsonrpcClient) VMUnpause(context.Context, VMRef) error           { return errNotImplemented }
func (c *jsonrpcClient) VMSuspend(context.Context, VMRef) error           { return errNotImplemented }
func (c *jsonrpcClient) VMResumeFromSuspend(context.Context, VMRef) error { return errNotImplemented }

func (c *jsonrpcClient) VMCheckpoint(context.Context, VMRef, string) (SnapshotRef, error) {
	return "", errNotImplemented
}

func (c *jsonrpcClient) VMGuestIP(context.Context, VMRef, string) (string, error) {
	return "", errNotImplemented
}

func (c *jsonrpcClient) VDIImportRaw(context.Context, string, string, io.Reader, int64) (VDIRef, error) {
	return "", errNotImplemented
}

func (c *jsonrpcClient) VDIFindByName(context.Context, string, string) (VDIRef, bool, error) {
	return "", false, errNotImplemented
}

func (c *jsonrpcClient) VDIClone(context.Context, VDIRef) (VDIRef, error) {
	return "", errNotImplemented
}
func (c *jsonrpcClient) VDIDestroy(context.Context, VDIRef) error { return errNotImplemented }

func (c *jsonrpcClient) VBDAttach(context.Context, VMRef, VDIRef, string, bool) error {
	return errNotImplemented
}

func (c *jsonrpcClient) NetworkCreatePrivate(context.Context, string) (string, error) {
	return "", errNotImplemented
}
func (c *jsonrpcClient) NetworkDestroy(context.Context, string) error { return errNotImplemented }

func (c *jsonrpcClient) VIFAttach(context.Context, VMRef, string, string, string) error {
	return errNotImplemented
}
