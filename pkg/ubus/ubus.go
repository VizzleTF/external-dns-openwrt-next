// Package ubus calls OpenWrt's ubus through the JSON-RPC endpoint uhttpd
// serves at /ubus, the one LuCI itself uses.
package ubus

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	path = "/ubus"

	// The session a login is made from. rpcd's unauthenticated ACL lets it
	// call session.login and session.access, nothing else.
	loginSession = "00000000000000000000000000000000"

	// What uhttpd answers a call with when the session may not make it, which
	// is also how it answers one with an expired or unknown session.
	codeAccessDenied = -32002

	// `uci get dhcp` on a router with a thousand hosts stays well below this.
	maxResponseSize = 4 << 20
)

// Status is a ubus status code other than UBUS_STATUS_OK, which a call
// returns in place of data.
type Status int

const (
	// ErrNotFound is what uci set and uci delete return for a section that
	// does not exist.
	ErrNotFound Status = 4
	// ErrNoData is what uci apply returns when the session staged nothing.
	ErrNoData Status = 5
)

// statusNames follows enum ubus_msg_status in libubus.
var statusNames = [...]string{
	"UBUS_STATUS_OK",
	"UBUS_STATUS_INVALID_COMMAND",
	"UBUS_STATUS_INVALID_ARGUMENT",
	"UBUS_STATUS_METHOD_NOT_FOUND",
	"UBUS_STATUS_NOT_FOUND",
	"UBUS_STATUS_NO_DATA",
	"UBUS_STATUS_PERMISSION_DENIED",
	"UBUS_STATUS_TIMEOUT",
	"UBUS_STATUS_NOT_SUPPORTED",
	"UBUS_STATUS_UNKNOWN_ERROR",
	"UBUS_STATUS_CONNECTION_FAILED",
	"UBUS_STATUS_NO_MEMORY",
	"UBUS_STATUS_PARSE_ERROR",
	"UBUS_STATUS_SYSTEM_ERROR",
}

func (s Status) Error() string {
	if s >= 0 && int(s) < len(statusNames) {
		return statusNames[s]
	}
	return fmt.Sprintf("ubus status %d", int(s))
}

// rpcError is how uhttpd reports a call it did not pass on to ubus.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("json-rpc error %d: %s", e.Code, e.Message)
}

type Client interface {
	// Call runs method on a ubus object with args, as `ubus call` does, and
	// decodes the data it returns into result. Without data, or with a nil
	// result, result is left as it is. A session the router refuses is
	// replaced by a fresh one, unless it has staged uci changes: the fresh
	// one would not see them, and the call fails instead.
	Call(ctx context.Context, object, method string, args, result any) error
	// Reset abandons the session, so the next call logs in afresh. rpcd stages
	// uci changes per session and no other session sees them: abandoning it
	// reverts whatever it staged and did not apply.
	Reset()
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type response struct {
	Result []json.RawMessage `json:"result"`
	Error  *rpcError         `json:"error"`
}

type client struct {
	config     *Config
	httpClient *http.Client
	log        *slog.Logger
	url        string

	// ExternalDNS can call the webhook concurrently. A login holds mu, so
	// calls that find no session wait for one instead of each opening theirs.
	mu      sync.Mutex
	session string
	// staged is whether the session holds uci changes it has not applied.
	staged bool
}

func New(config *Config, log *slog.Logger) Client {
	timeout := time.Duration(config.Timeout) * time.Second
	httpClient := &http.Client{
		// The dial timeout alone only bounds connection setup. Without a
		// client timeout a router that accepts the connection and then stalls
		// would hold a change set, which runs on after ExternalDNS gives up,
		// and every one queued behind it, forever.
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: config.InsecureSkipVerify,
			},
			DialContext: (&net.Dialer{
				Timeout:   timeout,
				KeepAlive: timeout,
			}).DialContext,
		},
	}

	proto := "https://"
	if !config.SSL {
		proto = "http://"
	}

	return &client{
		config:     config,
		httpClient: httpClient,
		log:        log,
		url:        proto + net.JoinHostPort(config.Hostname, strconv.Itoa(config.Port)) + path,
	}
}

func (c *client) Call(ctx context.Context, object, method string, args, result any) error {
	c.log.Debug("call", slog.String("object", object), slog.String("method", method))
	session, err := c.login(ctx)
	if err != nil {
		return err
	}

	err = c.call(ctx, session, object, method, args, result)

	// An expired session and a missing ACL are answered alike, and only a
	// fresh session tells them apart.
	var rerr *rpcError
	if errors.As(err, &rerr) && rerr.Code == codeAccessDenied {
		// Going on in a fresh session would apply only what is staged from here
		// on, as if the rest had gone through. The session is kept, so every
		// call fails until Reset.
		if c.isStaged(session) {
			return fmt.Errorf("ubus call %s %s: session refused with uci changes staged: %w", object, method, err)
		}
		c.log.Info("re-authenticate")
		c.forget(session)
		if session, err = c.login(ctx); err != nil {
			return err
		}
		err = c.call(ctx, session, object, method, args, result)
	}

	if err != nil {
		return fmt.Errorf("ubus call %s %s: %w", object, method, err)
	}
	if object == "uci" {
		c.track(session, method)
	}
	return nil
}

func (c *client) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.session = ""
	c.staged = false
}

func (c *client) isStaged(session string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session == session && c.staged
}

// track notes what a successful uci call did to what session has staged.
func (c *client) track(session, method string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != session {
		return
	}
	switch method {
	case "add", "set", "delete":
		c.staged = true
	case "apply":
		c.staged = false
	}
}

// login returns the session, logging in when there is none.
func (c *client) login(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != "" {
		return c.session, nil
	}

	var data struct {
		Session string `json:"ubus_rpc_session"`
	}
	args := map[string]string{"username": c.config.Auth.Username, "password": c.config.Auth.Password}
	// A wrong username or password is UBUS_STATUS_PERMISSION_DENIED.
	if err := c.call(ctx, loginSession, "session", "login", args, &data); err != nil {
		return "", fmt.Errorf("ubus login as %q: %w", c.config.Auth.Username, err)
	}
	if data.Session == "" {
		return "", fmt.Errorf("ubus login as %q: no session in the response", c.config.Auth.Username)
	}

	c.session = data.Session
	return c.session, nil
}

// forget drops session, unless a concurrent call has already replaced it.
func (c *client) forget(session string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == session {
		c.session = ""
		c.staged = false
	}
}

func (c *client) call(ctx context.Context, session, object, method string, args, result any) error {
	if args == nil {
		// uhttpd refuses a call whose arguments are not an object.
		args = struct{}{}
	}
	body, err := json.Marshal(request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "call",
		Params:  []any{session, object, method, args},
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// The session and the password travel in the body, never in the URL, so a
	// transport error, which quotes the URL, carries neither.
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("http status code: %d", resp.StatusCode)
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		return err
	}
	if len(respBody) > maxResponseSize {
		return fmt.Errorf("response larger than %d bytes", maxResponseSize)
	}

	var response response
	if err := json.Unmarshal(respBody, &response); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if response.Error != nil {
		return response.Error
	}
	if len(response.Result) == 0 {
		return errors.New("response holds neither a result nor an error")
	}

	var status Status
	if err := json.Unmarshal(response.Result[0], &status); err != nil {
		return fmt.Errorf("decode status: %w", err)
	}
	if status != 0 {
		return status
	}

	if result == nil || len(response.Result) < 2 {
		return nil
	}
	if err := json.Unmarshal(response.Result[1], result); err != nil {
		return fmt.Errorf("decode result: %w", err)
	}
	return nil
}
