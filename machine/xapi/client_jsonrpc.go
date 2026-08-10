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
	// base is the pool root the non-RPC endpoints hang off. Held as a value
	// so callers building a URL from it cannot mutate the client's copy.
	base   url.URL
	http   *http.Client
	nextID atomic.Uint64

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

// poolBase validates a pool URL and reduces it to the parts every endpoint on
// that pool is built from. Split out of newJSONRPCClient so the rules can be
// tested without dialling anything.
//
// There are two endpoints, not one: RPC at /jsonrpc, and the raw VDI import at
// /import_raw_vdi, which is a PUT rather than an RPC. Both are derived from
// this, so the https guarantee and the credential rejection below cannot hold
// for one path and quietly not the other.
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
func poolBase(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// Report the reason without the URL. *url.Error stringifies to
		// include the raw input, and a URL that fails to parse may still
		// have carried credentials in it: "https://root:pw@host:notaport"
		// fails on the port and never reaches the u.User check below. This
		// is the one path where a password reaches a log while apparently
		// only reporting a syntax error. Deliberately %v rather than %w, so
		// no caller can unwrap back to something holding the raw string.
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("xapi: Config.URL is not a URL: %v", ue.Err)
		}
		return nil, errors.New("xapi: Config.URL is not a URL")
	}
	if u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("xapi: Config.URL must be https://host, got %q", raw)
	}

	// Refuse the parts of a URL a pool endpoint has no use for, rather than
	// dropping them quietly. Credentials matter most: they belong in
	// Config.Username and Config.Password, not in a string kept as the
	// endpoint field and printed by anything that logs it.
	if u.User != nil {
		return nil, errors.New(
			"xapi: Config.URL must not carry credentials; use Config.Username and Config.Password")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("xapi: Config.URL must not carry a query or fragment, got %q", raw)
	}

	// Build from the parsed parts. Appending to the raw string glued the path
	// onto whatever came last, so "https://pool?x=1" produced
	// "https://pool?x=1/jsonrpc". The checks above already make that
	// unreachable; constructing this way keeps it so if they ever loosen.
	return &url.URL{
		Scheme: u.Scheme,
		Host:   u.Host,
		Path:   strings.TrimSuffix(u.Path, "/"),
	}, nil
}

// poolEndpoint returns the JSON-RPC endpoint to POST to.
func poolEndpoint(raw string) (string, error) {
	u, err := poolBase(raw)
	if err != nil {
		return "", err
	}
	return u.JoinPath("jsonrpc").String(), nil
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

	base, err := poolBase(c.URL)
	if err != nil {
		return nil, err
	}

	cl := &jsonrpcClient{
		endpoint: base.JoinPath("jsonrpc").String(),
		base:     *base,
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

// --- storage --------------------------------------------------------------

// taskPoll is how often an in-progress XAPI task is re-read. The import is
// the only caller and it runs for as long as a disk takes to stream, so this
// trades a little latency at the end for not hammering the pool throughout.
const taskPoll = 500 * time.Millisecond

// srByUUID resolves the SR uuid an operator configured into the ref the API
// wants. Config carries a uuid because that is what xe and the XO UI show;
// refs are per-session and cannot be written into a config file.
func (c *jsonrpcClient) srByUUID(ctx context.Context, uuid string) (string, error) {
	if uuid == "" {
		return "", errors.New("xapi: SR uuid is empty")
	}
	var ref string
	if err := c.sessionCall(ctx, "SR.get_by_uuid", &ref, uuid); err != nil {
		return "", fmt.Errorf("xapi: no SR with uuid %s: %w", uuid, err)
	}
	return ref, nil
}

// vdiCreate makes an empty VDI of the given virtual size.
//
// virtual_size goes on the wire as a JSON number. XAPI's JSON-RPC renders
// Int as a number rather than the string some of its other bindings use,
// confirmed against a live pool (SR.get_physical_size came back 207028920320,
// not "207028920320"). Sending a string here is accepted by encoding/json and
// rejected by the pool, which is a confusing place to find out.
func (c *jsonrpcClient) vdiCreate(ctx context.Context, srRef, name string, sizeBytes int64) (VDIRef, error) {
	rec := map[string]any{
		"name_label":       name,
		"name_description": "clawk rootfs image",
		"SR":               srRef,
		"virtual_size":     sizeBytes,
		"type":             "user",
		"sharable":         false,
		"read_only":        false,
		"other_config":     map[string]string{},
	}
	var ref string
	if err := c.sessionCall(ctx, "VDI.create", &ref, rec); err != nil {
		return "", err
	}
	return VDIRef(ref), nil
}

// VDIImportRaw creates a VDI and streams a raw image into it.
//
// Two things about this call are not like the others in this file. It is not
// an RPC — the data goes to XAPI's /import_raw_vdi endpoint as an HTTP PUT —
// and its result is not in the HTTP response. XAPI writes the 200 header
// before the transfer finishes, so a 200 means "the request was accepted",
// not "the disk is on the SR". The outcome arrives through a Task, which is
// why one is created here and waited on rather than letting XAPI make its own
// and throwing away the handle.
//
// A failure part-way through destroys the VDI it created. Leaving it would be
// worse than useless: it keeps the name, so the next run's VDIFindByName would
// adopt a half-written disk as a valid golden image.
func (c *jsonrpcClient) VDIImportRaw(ctx context.Context, srUUID, name string, r io.Reader, sizeBytes int64) (VDIRef, error) {
	if sizeBytes <= 0 {
		return "", fmt.Errorf("xapi: VDIImportRaw needs a positive size, got %d", sizeBytes)
	}
	srRef, err := c.srByUUID(ctx, srUUID)
	if err != nil {
		return "", err
	}
	vdi, err := c.vdiCreate(ctx, srRef, name, sizeBytes)
	if err != nil {
		return "", err
	}

	if err := c.importRawInto(ctx, vdi, r, sizeBytes); err != nil {
		// WithoutCancel: the usual reason to be here is a cancelled or
		// expired context, and that is exactly when the cleanup still has to
		// happen. A destroy that also fails is reported alongside rather than
		// replacing the original error, which is the one that explains it.
		if derr := c.destroyAfterFailedImport(context.WithoutCancel(ctx), vdi); derr != nil {
			return "", fmt.Errorf("%w (and the partial VDI %s could not be destroyed: %v)",
				err, vdi, derr)
		}
		return "", err
	}
	return vdi, nil
}

// destroyAfterFailedImport removes a VDI whose import did not complete.
//
// XAPI frees the disk asynchronously. The import ending — cancelled, failed,
// or with the connection dropped under it — does not mean the VDI is
// released, and a destroy issued straight away answers VDI_IN_USE. This is
// not theoretical: against XCP-ng 8.3 the immediate destroy failed and the
// same destroy succeeded moments later, leaving a disk behind under the name
// the next VDIFindByName would adopt.
//
// So retry for a bounded window. Reporting a leak that is really a race
// would send whoever reads the error looking in the wrong place, and leaving
// the disk is the failure this whole path exists to prevent.
func (c *jsonrpcClient) destroyAfterFailedImport(ctx context.Context, vdi VDIRef) error {
	const (
		attempts = 12
		delay    = 500 * time.Millisecond
	)
	var err error
	for i := 0; i < attempts; i++ {
		err = c.VDIDestroy(ctx, vdi)
		if err == nil || !isVDIInUse(err) {
			return err
		}
		time.Sleep(delay)
	}
	return fmt.Errorf("%w (still in use after %s)", err, time.Duration(attempts)*delay)
}

// isVDIInUse reports whether err is XAPI's VDI_IN_USE, which after a failed
// import means "not yet released" rather than "someone else has it".
func isVDIInUse(err error) bool {
	var re *rpcError
	return errors.As(err, &re) && re.Message == "VDI_IN_USE"
}

// importRawInto streams r into an existing VDI and waits for XAPI's task to
// report the result.
func (c *jsonrpcClient) importRawInto(ctx context.Context, vdi VDIRef, r io.Reader, sizeBytes int64) error {
	task, err := c.taskCreate(ctx, "clawk.VDIImportRaw", string(vdi))
	if err != nil {
		return err
	}

	// Read the session *after* task.create, not before.
	//
	// taskCreate goes through sessionCall, which re-logs-in and retries when
	// the pool reports SESSION_INVALID. A ref read before that call can
	// therefore be dead by the time it reaches this URL, and nothing would
	// catch it: the import is not an RPC, so sessionCall's retry does not
	// cover it, and XAPI would simply refuse the PUT. Reading it here closes
	// the window that every preceding RPC opens.
	session, _ := c.currentSession()

	imported := false
	defer func() {
		// Cleanup runs on a context that cannot be cancelled: the usual way
		// to get here is a cancelled or expired one, and that is exactly when
		// the pool still needs telling.
		cleanupCtx := context.WithoutCancel(ctx)
		if !imported {
			// Cancel before destroying. A task the client stopped watching is
			// still running server-side, still writing into the VDI, and the
			// caller's next move is to destroy that VDI — which cannot
			// succeed while an import still holds it. Without this, a
			// cancelled import leaves the half-written disk behind wearing
			// the name a later VDIFindByName would adopt.
			_ = c.sessionCall(cleanupCtx, "task.cancel", nil, task)
		}
		// Best effort: a leaked task is a row in the pool's task list, not a
		// consumed resource, and it must not mask the import's own error.
		_ = c.sessionCall(cleanupCtx, "task.destroy", nil, task)
	}()

	if session == "" {
		return errNoSession
	}

	u := c.base
	u.Path = u.Path + "/import_raw_vdi"
	// The session ref travels in the query string because that is where this
	// endpoint takes it. It is a bearer token in a URL, so it can reach a
	// proxy log; the connection is https and the ref is short-lived, and
	// there is no header-based alternative on this endpoint.
	u.RawQuery = url.Values{
		"session_id": {session},
		"task_id":    {task},
		"vdi":        {string(vdi)},
		"format":     {"raw"},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), r)
	if err != nil {
		return fmt.Errorf("xapi: import_raw_vdi: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	// Declaring the length sends Content-Length instead of chunked encoding,
	// which this endpoint is documented against HTTP/1.0 for. It also makes a
	// short reader a hard error from net/http rather than a VDI silently
	// missing its tail.
	req.ContentLength = sizeBytes

	// A pool redirects this endpoint to the host the SR is actually attached
	// to, which on any multi-host pool is routinely not the master. Go can
	// only follow a redirect it can replay the body for, and with a plain
	// io.Reader GetBody is nil, so the redirect fails with a confusing
	// ContentLength mismatch rather than anything mentioning redirects.
	// Callers pass a file or a bytes.Reader, both seekable.
	if rs, ok := r.(io.ReadSeeker); ok {
		if start, serr := rs.Seek(0, io.SeekCurrent); serr == nil {
			req.GetBody = func() (io.ReadCloser, error) {
				if _, err := rs.Seek(start, io.SeekStart); err != nil {
					return nil, err
				}
				return io.NopCloser(rs), nil
			}
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("xapi: import_raw_vdi: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("xapi: import_raw_vdi: HTTP %s: %s", resp.Status, snippet(body))
	}

	if err := c.awaitTask(ctx, task); err != nil {
		return err
	}
	imported = true
	return nil
}

// taskCreate registers a Task the pool will report an operation's outcome on.
func (c *jsonrpcClient) taskCreate(ctx context.Context, label, description string) (string, error) {
	var ref string
	if err := c.sessionCall(ctx, "task.create", &ref, label, description); err != nil {
		return "", err
	}
	return ref, nil
}

// awaitTask blocks until a task leaves the pending state, and turns anything
// other than success into an error carrying XAPI's error_info.
func (c *jsonrpcClient) awaitTask(ctx context.Context, task string) error {
	for {
		var status string
		if err := c.sessionCall(ctx, "task.get_status", &status, task); err != nil {
			return fmt.Errorf("xapi: reading task status: %w", err)
		}
		switch status {
		case "success":
			return nil
		case "pending":
			// keep waiting
		default:
			// failure, cancelling, cancelled. error_info is XAPI's own
			// description and is far more use than the status word alone.
			var info []string
			if err := c.sessionCall(ctx, "task.get_error_info", &info, task); err != nil || len(info) == 0 {
				return fmt.Errorf("xapi: task ended %s", status)
			}
			return fmt.Errorf("xapi: task ended %s: %s", status, strings.Join(info, " "))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(taskPoll):
		}
	}
}

// VDIFindByName looks for a VDI by name-label within one SR, which is how the
// backend avoids re-importing an image it already has.
//
// Name-labels are not unique in XAPI, so two matches in the same SR are an
// error rather than a coin toss — the same rule VMByNameLabel follows. The
// caller keys these on an image digest, so a duplicate means something else
// is writing into the SR under our naming scheme, and picking one at random
// would attach an unknown disk to a sandbox.
func (c *jsonrpcClient) VDIFindByName(ctx context.Context, srUUID, name string) (VDIRef, bool, error) {
	srRef, err := c.srByUUID(ctx, srUUID)
	if err != nil {
		return "", false, err
	}
	var refs []string
	if err := c.sessionCall(ctx, "VDI.get_by_name_label", &refs, name); err != nil {
		return "", false, err
	}

	var hits []string
	for _, ref := range refs {
		var sr string
		if err := c.sessionCall(ctx, "VDI.get_SR", &sr, ref); err != nil {
			return "", false, err
		}
		if sr == srRef {
			hits = append(hits, ref)
		}
	}

	switch len(hits) {
	case 0:
		return "", false, nil
	case 1:
		return VDIRef(hits[0]), true, nil
	default:
		return "", false, fmt.Errorf(
			"xapi: %d VDIs share the name-label %q in SR %s; refusing to guess",
			len(hits), name, srUUID)
	}
}

// VDIClone makes a writable copy. On a file-based SR (ext, NFS, XOSTOR) this
// is a fast copy-on-write clone, which is the assumption the per-sandbox disk
// model rests on; on LVM it is a full copy and will be slow.
func (c *jsonrpcClient) VDIClone(ctx context.Context, ref VDIRef) (VDIRef, error) {
	var out string
	// VDI.clone takes a driver_params map. Empty is the documented default.
	if err := c.sessionCall(ctx, "VDI.clone", &out, string(ref), map[string]string{}); err != nil {
		return "", err
	}
	return VDIRef(out), nil
}

func (c *jsonrpcClient) VDIDestroy(ctx context.Context, ref VDIRef) error {
	return c.sessionCall(ctx, "VDI.destroy", nil, string(ref))
}

// vdiVirtualSize reads VDI.virtual_size.
//
// Deliberately not on the Client interface, for the same reason as
// VMByNameLabel: the backend never needs it, but a test that has just
// imported a disk does, to prove the size that arrived is the size it asked
// for rather than trusting that the call returned nil.
func (c *jsonrpcClient) vdiVirtualSize(ctx context.Context, ref VDIRef) (int64, error) {
	var n int64
	if err := c.sessionCall(ctx, "VDI.get_virtual_size", &n, string(ref)); err != nil {
		return 0, err
	}
	return n, nil
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
