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
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// errNotImplemented marks a Client method this transport has not grown yet.
var errNotImplemented = errors.New("xapi: not implemented in the JSON-RPC transport yet")

// errNoSession is returned by any call made on a client that has been
// closed, or whose login never succeeded.
var errNoSession = errors.New("xapi: no session (client closed, or login failed)")

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
// owns one session at a time; Close logs it out.
//
// The session is not permanent. XAPI expires it, and a Machine is expected
// to outlive the operations performed on it — a sandbox can sit idle well
// past the pool's timeout — so sessionCall re-logs-in once on
// SESSION_INVALID and retries. That is why the credentials are held for the
// client's lifetime rather than being discarded after the first login.
type jsonrpcClient struct {
	endpoint string
	http     *http.Client
	nextID   atomic.Uint64

	// Credentials for re-login. Config already holds these for the life of
	// the process (Configure is process-global), so keeping them here
	// widens no exposure that did not already exist.
	username string
	password string

	// loginMu serialises re-login so that a fleet of concurrent calls
	// meeting the same expired session produces one login, not one per
	// caller. It is never held across a non-login RPC.
	loginMu sync.Mutex

	mu      sync.Mutex
	session string
	// gen increments on every successful login. A caller that saw
	// SESSION_INVALID passes the generation it used; if gen has already
	// moved, someone else re-logged-in and there is nothing to do.
	gen    uint64
	closed bool
}

var _ Client = (*jsonrpcClient)(nil)

// poolEndpoint validates a pool URL and returns the JSON-RPC endpoint to POST
// to. Split out of newJSONRPCClient so the rules can be tested without
// dialling anything.
//
// Cleartext is refused outright. login sends the password, and every call
// after it sends the session ref; on http:// both cross the wire in the
// clear. InsecureTLS skips certificate verification, which is a different and
// much smaller concession than no TLS at all, and an operator who set it has
// not agreed to this.
//
// Parsing rather than string-matching also means a URL with no scheme fails
// naming the field, instead of surfacing later out of
// http.NewRequestWithContext as an error mentioning neither Config nor URL.
func poolEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("xapi: Config.URL is not a URL: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("xapi: Config.URL must be https://host, got %q", raw)
	}

	// Refuse the parts of a URL a pool endpoint has no use for, rather than
	// dropping them quietly. Credentials matter most: they belong in
	// Config.Username and Config.Password, not in a string kept as the
	// endpoint field and printed by anything that logs it.
	if u.User != nil {
		return "", errors.New(
			"xapi: Config.URL must not carry credentials; use Config.Username and Config.Password")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("xapi: Config.URL must not carry a query or fragment, got %q", raw)
	}

	// Build from the parsed parts. Appending to the raw string glued the path
	// onto whatever came last, so "https://pool?x=1" produced
	// "https://pool?x=1/jsonrpc". The checks above already make that
	// unreachable; constructing this way keeps it so if they ever loosen.
	return (&url.URL{
		Scheme: u.Scheme,
		Host:   u.Host,
		Path:   strings.TrimSuffix(u.Path, "/") + "/jsonrpc",
	}).String(), nil
}

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

	endpoint, err := poolEndpoint(c.URL)
	if err != nil {
		return nil, err
	}

	cl := &jsonrpcClient{
		endpoint: endpoint,
		username: c.Username,
		password: c.Password,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				TLSClientConfig: &tls.Config{
					// Go's default floor is already 1.2, so this changes
					// nothing today. It is here as documentation, and so a
					// future shift in that default cannot quietly lower the
					// floor under the session token this connection carries.
					MinVersion:         tls.VersionTLS12,
					InsecureSkipVerify: c.InsecureTLS, //nolint:gosec // opt-in; see Config.InsecureTLS
				},
			},
			// No client-level Timeout on purpose: each call carries its own
			// context, and a fixed deadline here would eventually truncate
			// the long-running VDI import this transport still has to grow.

			// The scheme check above only constrains the URL the operator
			// configured. Without this, a front-end that redirected https to
			// http would undo it mid-session: 307 and 308 preserve the method
			// and the body, so the login credentials really would be re-sent
			// in the clear. Supplying CheckRedirect replaces Go's default, so
			// the hop limit has to be restated here.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if req.URL.Scheme != "https" {
					return fmt.Errorf("xapi: refusing redirect to non-https %s://%s",
						req.URL.Scheme, req.URL.Host)
				}
				if len(via) >= 10 {
					return errors.New("xapi: too many redirects")
				}
				return nil
			},
		},
	}

	if err := cl.login(ctx); err != nil {
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
//
// On SESSION_INVALID it logs in again and retries the call once. Exactly
// once: a second failure is returned, so a pool that rejects every login
// produces an error rather than a loop.
func (c *jsonrpcClient) sessionCall(ctx context.Context, method string, out any, params ...any) error {
	s, gen := c.currentSession()
	if s == "" {
		return errNoSession
	}

	err := c.call(ctx, method, out, append([]any{s}, params...)...)
	if !isSessionInvalid(err) {
		return err
	}

	if relErr := c.relogin(ctx, gen); relErr != nil {
		// Report both: the expired session explains why a re-login was
		// attempted, and the re-login failure explains why it did not help.
		return fmt.Errorf("%w (re-login failed: %v)", err, relErr)
	}
	s, _ = c.currentSession()
	if s == "" {
		return errNoSession
	}
	return c.call(ctx, method, out, append([]any{s}, params...)...)
}

// currentSession returns the session ref and the generation it belongs to.
// An empty ref means the client is closed or was never logged in.
func (c *jsonrpcClient) currentSession() (string, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session, c.gen
}

// isSessionInvalid reports whether err is XAPI's SESSION_INVALID, the
// failure every call returns once the pool has expired the session ref.
func isSessionInvalid(err error) bool {
	var re *rpcError
	return errors.As(err, &re) && re.Message == "SESSION_INVALID"
}

// relogin replaces an expired session. gen is the generation the caller was
// using: if it no longer matches, another goroutine has already logged in
// and this call has nothing to do.
func (c *jsonrpcClient) relogin(ctx context.Context, gen uint64) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()

	c.mu.Lock()
	current, closed := c.gen, c.closed
	c.mu.Unlock()
	if closed {
		return errNoSession
	}
	if current != gen {
		return nil // someone else got there first
	}
	return c.login(ctx)
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

// login obtains a session ref and installs it, bumping the generation so
// that callers holding the previous one know it has been replaced. Callers
// other than newJSONRPCClient must hold loginMu.
func (c *jsonrpcClient) login(ctx context.Context) error {
	var ref string
	if err := c.call(ctx, "session.login_with_password", &ref,
		c.username, c.password, apiVersion, originator); err != nil {
		return err
	}
	if ref == "" {
		return errors.New("xapi: login returned an empty session ref")
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		// Close ran while this login was in flight. Installing the ref now
		// would resurrect a closed client and leak the session on the pool,
		// so end it instead.
		_ = c.logout(ref)
		return errNoSession
	}
	c.session = ref
	c.gen++
	c.mu.Unlock()
	return nil
}

// Close logs the session out. It is safe to call more than once, and a
// closed client stays closed: a re-login racing with it will not revive it.
func (c *jsonrpcClient) Close() error {
	c.mu.Lock()
	s := c.session
	c.session = ""
	c.closed = true
	c.mu.Unlock()
	if s == "" {
		return nil
	}
	defer c.http.CloseIdleConnections()
	return c.logout(s)
}

// logout ends one session ref.
//
// It carries its own short deadline rather than taking a context: Close is
// often called from a defer whose context is already cancelled, and leaking
// the session on the pool until it expires is worse than a brief wait here.
func (c *jsonrpcClient) logout(ref string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return c.call(ctx, "session.logout", nil, ref)
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
