package ubus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testPassword = "hunter2-password"
	testSession  = "5ec2e75e55105ec2e75e55105ec2e75e"

	loginOK      = `"result":[0,{"ubus_rpc_session":"` + testSession + `","timeout":300}]`
	accessDenied = `"error":{"code":-32002,"message":"Access denied"}`
)

var discard = slog.New(slog.DiscardHandler)

// ubusCall is a call as the router receives it.
type ubusCall struct {
	Session string
	Object  string
	Method  string
	Args    map[string]any
}

func isLogin(call ubusCall) bool { return call.Object == "session" && call.Method == "login" }

// serve checks each request is a well-formed ubus call and answers it with the
// JSON-RPC member answer returns, `"result":[…]` or `"error":{…}`. An empty
// answer drops the connection instead, as a router going away would.
func serve(t *testing.T, answer func(ubusCall) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			JSONRPC string `json:"jsonrpc"`
			Method  string `json:"method"`
			Params  []any  `json:"params"`
		}
		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil || r.Method != http.MethodPost || r.URL.Path != path ||
			r.Header.Get("Content-Type") != "application/json" ||
			req.JSONRPC != "2.0" || req.Method != "call" || len(req.Params) != 4 {
			t.Errorf("malformed request %s %s: %+v (%v)", r.Method, r.URL.Path, req, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		var call ubusCall
		call.Session, _ = req.Params[0].(string)
		call.Object, _ = req.Params[1].(string)
		call.Method, _ = req.Params[2].(string)
		var ok bool
		if call.Args, ok = req.Params[3].(map[string]any); !ok {
			t.Errorf("arguments are not an object: %v", req.Params[3])
		}

		body := answer(call)
		if body == "" {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,`+body+`}`)
	}
}

// newTestClient points a client logging in as root with testPassword at a
// local server.
func newTestClient(t *testing.T, handler http.Handler, log *slog.Logger) Client {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}

	config := DefaultConfig()
	config.SSL = false
	config.Hostname = u.Hostname()
	config.Port = port
	config.Auth = Auth{Username: "root", Password: testPassword}

	return New(config, log)
}

func logins(calls []ubusCall) int {
	n := 0
	for _, call := range calls {
		if isLogin(call) {
			n++
		}
	}
	return n
}

// sessions records every call, answers the n-th login with session-n and any
// other call with answer.
func sessions(t *testing.T, answer func(ubusCall) string) (http.HandlerFunc, *[]ubusCall) {
	var calls []ubusCall
	return serve(t, func(call ubusCall) string {
		calls = append(calls, call)
		if isLogin(call) {
			return `"result":[0,{"ubus_rpc_session":"session-` + strconv.Itoa(logins(calls)) + `"}]`
		}
		return answer(call)
	}), &calls
}

func loginCall() ubusCall {
	return ubusCall{Session: loginSession, Object: "session", Method: "login",
		Args: map[string]any{"username": "root", "password": testPassword}}
}

func uciGet(session string) ubusCall {
	return ubusCall{Session: session, Object: "uci", Method: "get", Args: map[string]any{"config": "dhcp"}}
}

func TestCallLogsInOnceAndDecodesTheResult(t *testing.T) {
	handler, calls := sessions(t, func(ubusCall) string {
		return `"result":[0,{"values":{"cfg01":{".type":"domain","name":"nas.lan"}}}]`
	})
	client := newTestClient(t, handler, discard)

	for range 2 {
		var data struct {
			Values map[string]map[string]string `json:"values"`
		}
		if err := client.Call(context.Background(), "uci", "get", map[string]string{"config": "dhcp"}, &data); err != nil {
			t.Fatal(err)
		}
		if got := data.Values["cfg01"]["name"]; got != "nas.lan" {
			t.Errorf("decoded name: got %q", got)
		}
	}
	// No arguments still go out as an object, and a nil result skips decoding.
	if err := client.Call(context.Background(), "uci", "apply", nil, nil); err != nil {
		t.Fatal(err)
	}

	want := []ubusCall{
		loginCall(),
		uciGet("session-1"),
		uciGet("session-1"),
		{Session: "session-1", Object: "uci", Method: "apply", Args: map[string]any{}},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls:\n got %+v\nwant %+v", *calls, want)
	}
}

func TestCallLogsInAgainWhenTheSessionIsRefused(t *testing.T) {
	handler, calls := sessions(t, func(call ubusCall) string {
		// What uhttpd answers for an expired session.
		if call.Session == "session-1" {
			return accessDenied
		}
		return `"result":[0,{"value":"1"}]`
	})
	client := newTestClient(t, handler, discard)

	var data struct {
		Value string `json:"value"`
	}
	if err := client.Call(context.Background(), "uci", "get", map[string]string{"config": "dhcp"}, &data); err != nil {
		t.Fatal(err)
	}
	if data.Value != "1" {
		t.Errorf("decoded value: got %q", data.Value)
	}

	want := []ubusCall{loginCall(), uciGet("session-1"), loginCall(), uciGet("session-2")}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls:\n got %+v\nwant %+v", *calls, want)
	}
}

func TestCallReportsAccessDeniedAfterOneMoreLogin(t *testing.T) {
	handler, calls := sessions(t, func(ubusCall) string { return accessDenied })
	client := newTestClient(t, handler, discard)

	err := client.Call(context.Background(), "uci", "get", map[string]string{"config": "dhcp"}, nil)
	if err == nil || err.Error() != "ubus call uci get: json-rpc error -32002: Access denied" {
		t.Errorf("got %v", err)
	}

	want := []ubusCall{loginCall(), uciGet("session-1"), loginCall(), uciGet("session-2")}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls:\n got %+v\nwant %+v", *calls, want)
	}
}

// A fresh session would not see what the refused one staged, and applying
// in it would commit the rest of the change set alone.
func TestCallDoesNotLogInAgainOverStagedChanges(t *testing.T) {
	handler, calls := sessions(t, func(call ubusCall) string {
		if call.Session == "session-1" && call.Method != "add" {
			return accessDenied
		}
		return `"result":[0,{"section":"cfg01"}]`
	})
	client := newTestClient(t, handler, discard)
	ctx := context.Background()

	if err := client.Call(ctx, "uci", "add", map[string]string{"config": "dhcp"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"set", "apply"} {
		err := client.Call(ctx, "uci", method, nil, nil)
		if want := "ubus call uci " + method + ": session refused with uci changes staged: json-rpc error -32002: Access denied"; err == nil || err.Error() != want {
			t.Errorf("got %v, want %q", err, want)
		}
	}
	if n := logins(*calls); n != 1 {
		t.Errorf("logins before Reset: got %d, want 1", n)
	}

	client.Reset()
	if err := client.Call(ctx, "uci", "get", map[string]string{"config": "dhcp"}, nil); err != nil {
		t.Fatal(err)
	}
	if n := logins(*calls); n != 2 {
		t.Errorf("logins after Reset: got %d, want 2", n)
	}
}

// Once applied, nothing is lost by logging in again.
func TestCallLogsInAgainAfterAnApply(t *testing.T) {
	refuse := false
	handler, calls := sessions(t, func(call ubusCall) string {
		if refuse && call.Session == "session-1" {
			return accessDenied
		}
		return `"result":[0]`
	})
	client := newTestClient(t, handler, discard)
	ctx := context.Background()

	for _, method := range []string{"delete", "apply"} {
		if err := client.Call(ctx, "uci", method, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	refuse = true
	if err := client.Call(ctx, "uci", "get", map[string]string{"config": "dhcp"}, nil); err != nil {
		t.Fatal(err)
	}
	if n := logins(*calls); n != 2 {
		t.Errorf("logins: got %d, want 2", n)
	}
}

func TestCallReportsAnotherJSONRPCErrorWithoutLoggingInAgain(t *testing.T) {
	handler, calls := sessions(t, func(ubusCall) string {
		return `"error":{"code":-32601,"message":"Method not found"}`
	})
	client := newTestClient(t, handler, discard)

	err := client.Call(context.Background(), "uci", "get", map[string]string{"config": "dhcp"}, nil)
	if err == nil || err.Error() != "ubus call uci get: json-rpc error -32601: Method not found" {
		t.Errorf("got %v", err)
	}
	if n := logins(*calls); n != 1 {
		t.Errorf("logins: got %d, want 1", n)
	}
}

func TestCallReportsTheStatus(t *testing.T) {
	for _, tc := range []struct {
		result string
		want   error
		msg    string
	}{
		{"[4]", ErrNotFound, "ubus call uci set: UBUS_STATUS_NOT_FOUND"},
		{"[5]", ErrNoData, "ubus call uci set: UBUS_STATUS_NO_DATA"},
		{"[6]", Status(6), "ubus call uci set: UBUS_STATUS_PERMISSION_DENIED"},
		{"[99]", Status(99), "ubus call uci set: ubus status 99"},
	} {
		t.Run(tc.result, func(t *testing.T) {
			handler, calls := sessions(t, func(ubusCall) string { return `"result":` + tc.result })
			client := newTestClient(t, handler, discard)

			for range 2 {
				err := client.Call(context.Background(), "uci", "set", map[string]string{"config": "dhcp"}, nil)
				if !errors.Is(err, tc.want) || err.Error() != tc.msg {
					t.Errorf("got %v, want %q", err, tc.msg)
				}
			}
			// A status is the call's answer, not the session's: logging in again
			// would drop what the session staged.
			if n := logins(*calls); n != 1 {
				t.Errorf("logins: got %d, want 1", n)
			}
		})
	}
}

func TestCallReportsALoginFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
		want   string
	}{
		// What rpcd answers to a wrong username or password.
		{"denied", `"result":[6]`, `ubus login as "root": UBUS_STATUS_PERMISSION_DENIED`},
		{"no session", `"result":[0,{}]`, `ubus login as "root": no session in the response`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []ubusCall
			client := newTestClient(t, serve(t, func(call ubusCall) string {
				calls = append(calls, call)
				return tc.answer
			}), discard)

			for range 2 {
				err := client.Call(context.Background(), "uci", "get", map[string]string{"config": "dhcp"}, nil)
				if err == nil || err.Error() != tc.want {
					t.Errorf("got %v, want %q", err, tc.want)
				}
			}
			if n := logins(calls); n != 2 || len(calls) != 2 {
				t.Errorf("calls: got %+v, want a login attempt on each call and nothing else", calls)
			}
		})
	}
}

func TestCallReportsABadResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"server error", http.StatusInternalServerError, "", "http status code: 500"},
		{"not found", http.StatusNotFound, "", "http status code: 404"},
		{"not json", http.StatusOK, "<html>", "decode response"},
		{"empty", http.StatusOK, `{"jsonrpc":"2.0","id":1}`, "response holds neither a result nor an error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}), discard)

			err := client.Call(context.Background(), "uci", "get", nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCallStopsReadingAnEndlessResponse(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := []byte(strings.Repeat(" ", 1<<16))
		for {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}), discard)

	err := client.Call(context.Background(), "uci", "get", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "response larger than 4194304 bytes") {
		t.Errorf("got %v", err)
	}
}

func TestResetLogsInAfresh(t *testing.T) {
	handler, calls := sessions(t, func(ubusCall) string { return `"result":[0]` })
	client := newTestClient(t, handler, discard)

	for range 2 {
		if err := client.Call(context.Background(), "uci", "get", map[string]string{"config": "dhcp"}, nil); err != nil {
			t.Fatal(err)
		}
		client.Reset()
	}

	want := []ubusCall{loginCall(), uciGet("session-1"), loginCall(), uciGet("session-2")}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls:\n got %+v\nwant %+v", *calls, want)
	}
}

func TestConcurrentCallsShareOneLogin(t *testing.T) {
	var logins atomic.Int32
	client := newTestClient(t, serve(t, func(call ubusCall) string {
		if isLogin(call) {
			logins.Add(1)
			// Long enough for every other call to arrive while this one runs.
			time.Sleep(50 * time.Millisecond)
			return loginOK
		}
		return `"result":[0]`
	}), discard)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := client.Call(context.Background(), "uci", "get", nil, nil); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	if n := logins.Load(); n != 1 {
		t.Errorf("logins: got %d, want 1", n)
	}
}

func TestSecretsStayOutOfErrorsAndLogs(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := newTestClient(t, serve(t, func(call ubusCall) string {
		switch {
		case isLogin(call) && call.Args["password"] == testPassword:
			return loginOK
		case isLogin(call):
			return `"result":[6]`
		case call.Method == "get":
			return accessDenied
		case call.Method == "set":
			return ""
		default:
			return `"result":[6]`
		}
	}), log)

	var errs []error
	// Refused twice, dropped, and failed with a status.
	for _, method := range []string{"get", "set", "delete"} {
		errs = append(errs, c.Call(context.Background(), "uci", method, nil, nil))
	}
	// And a login refused.
	c.(*client).config.Auth.Password = "wrong-" + testPassword
	c.Reset()
	errs = append(errs, c.Call(context.Background(), "uci", "get", nil, nil))

	for _, err := range errs {
		if err == nil {
			t.Fatal("got no error")
		}
		if msg := err.Error(); strings.Contains(msg, testPassword) || strings.Contains(msg, testSession) {
			t.Errorf("error carries a secret: %q", msg)
		}
	}
	if out := logs.String(); !strings.Contains(out, "re-authenticate") ||
		strings.Contains(out, testPassword) || strings.Contains(out, testSession) {
		t.Errorf("logs carry a secret, or nothing was logged:\n%s", out)
	}
}
