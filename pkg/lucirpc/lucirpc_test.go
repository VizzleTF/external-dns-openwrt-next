package lucirpc

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

// newTestClient points a client at a local server serving mux over plain HTTP.
func newTestClient(t *testing.T, mux *http.ServeMux) *lucirpc {
	t.Helper()
	ts := httptest.NewServer(mux)
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

	return &lucirpc{config: config, log: slog.New(slog.DiscardHandler), httpClient: ts.Client()}
}

func respond(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func TestAuthStoresTheToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+authPath, respond(http.StatusAccepted, `{"result":"foobar"}`))
	client := newTestClient(t, mux)

	if err := client.auth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := client.getToken(); got != "foobar" {
		t.Errorf("token: got %q", got)
	}
}

func TestAuthFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"unauthorized", http.StatusUnauthorized, "", ErrHttpUnauthorized},
		{"forbidden", http.StatusForbidden, "", ErrHttpForbidden},
		// What LuCI answers to a wrong username or password.
		{"wrong credentials", http.StatusOK, `{"id":1,"result":null,"error":null}`, ErrRpcLoginFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc(authPath, respond(tc.status, tc.body))
			client := newTestClient(t, mux)

			if err := client.auth(context.Background()); !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
			if got := client.getToken(); got != "" {
				t.Errorf("token: got %q, want none", got)
			}
		})
	}
}

func TestAuthReportsAServerError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(authPath, respond(http.StatusInternalServerError, ""))
	client := newTestClient(t, mux)

	err := client.auth(context.Background())
	if err == nil || err.Error() != "http status code: 500" {
		t.Errorf("got %v", err)
	}
}

func TestUciReauthenticatesAndRetries(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(authPath, respond(http.StatusOK, `{"result":"foobar"}`))

	rejected := false
	mux.HandleFunc(uciPath, func(w http.ResponseWriter, r *http.Request) {
		if !rejected {
			rejected = true
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.RequestURI != uciPath+"?auth=foobar" {
			t.Errorf("request URI: got %q", r.RequestURI)
		}
		respond(http.StatusOK, `{"result":"helloworld"}`)(w, r)
	})
	client := newTestClient(t, mux)

	resp, err := client.Uci(context.Background(), "get", []string{"network.lan.ipaddr"})
	if err != nil {
		t.Fatal(err)
	}
	if resp != "helloworld" {
		t.Errorf("response: got %q", resp)
	}
	if !rejected {
		t.Error("the first call was never rejected, so re-authentication was not exercised")
	}
}

func TestUciSurfacesALoginFailureOnReauthentication(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(authPath, respond(http.StatusOK, `{"id":1,"result":null,"error":null}`))
	mux.HandleFunc(uciPath, respond(http.StatusForbidden, ""))
	client := newTestClient(t, mux)

	if _, err := client.Uci(context.Background(), "get_all", []string{"dhcp"}); !errors.Is(err, ErrRpcLoginFail) {
		t.Errorf("got %v, want %v", err, ErrRpcLoginFail)
	}
}
