package router

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VizzleTF/external-dns-openwrt-next/pkg/metrics"
)

func fakeHandler(mux *http.ServeMux) {
	mux.HandleFunc("GET /records", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestMetricsAreServedOnTheObservabilityListener(t *testing.T) {
	config := DefaultConfig()
	registry := metrics.NewRegistry()
	metrics.Build(registry)
	r := New(config, slog.New(slog.DiscardHandler), registry, fakeHandler)

	res := httptest.NewRecorder()
	r.health.Handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if res.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", res.Code)
	}
	if got := res.Header().Get("Content-Type"); got != metrics.ContentType {
		t.Errorf("content type: got %q, want %q", got, metrics.ContentType)
	}
	if got := res.Body.String(); !strings.Contains(got, metrics.Prefix+"_build_info") {
		t.Errorf("body: got %s", got)
	}

	// Scraping is not the sidecar's business, so the API listener does not
	// serve it.
	res = httptest.NewRecorder()
	r.api.Handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if res.Code != http.StatusNotFound {
		t.Errorf("metrics on the api listener: got %d, want 404", res.Code)
	}
}

func TestApiRequestsAreCountedByRoute(t *testing.T) {
	registry := metrics.NewRegistry()
	r := New(DefaultConfig(), slog.New(slog.DiscardHandler), registry, fakeHandler)

	r.api.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/records", nil))
	// An unrouted path must not mint a series of its own, or a scanner could
	// grow the metric without bound.
	r.api.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nope", nil))

	var out strings.Builder
	if _, err := registry.WriteTo(&out); err != nil {
		t.Fatalf("write metrics: %v", err)
	}

	scraped := out.String()
	if !strings.Contains(scraped, `external_dns_openwrt_http_requests_total{route="GET /records",status="200"} 1`) {
		t.Errorf("routed request not counted:\n%s", scraped)
	}
	if !strings.Contains(scraped, `external_dns_openwrt_http_requests_total{route="unmatched",status="404"} 1`) {
		t.Errorf("unrouted request not counted:\n%s", scraped)
	}
	if !strings.Contains(scraped, `external_dns_openwrt_http_request_duration_seconds_total{route="GET /records"}`) {
		t.Errorf("duration not recorded:\n%s", scraped)
	}
}

func TestRouteLabelsAreReadable(t *testing.T) {
	for _, tc := range []struct{ pattern, want string }{
		{"GET /records", "GET /records"},
		// `{$}` is how the mux spells "exactly this path"; on a dashboard it is
		// noise.
		{"GET /{$}", "GET /"},
		{"", "unmatched"},
	} {
		if got := routeLabel(tc.pattern); got != tc.want {
			t.Errorf("routeLabel(%q): got %q, want %q", tc.pattern, got, tc.want)
		}
	}
}

func TestTheApiListensOnLoopbackAndHealthOnEveryInterface(t *testing.T) {
	// The provider API can rewrite the router's DNS and authenticates nobody,
	// so it must not be reachable on the pod IP; the kubelet, on the other
	// hand, probes through the pod IP and would fail against loopback.
	r := New(DefaultConfig(), slog.New(slog.DiscardHandler), metrics.NewRegistry(), fakeHandler)

	if got, want := r.api.Addr, "127.0.0.1:8888"; got != want {
		t.Errorf("api address: got %q, want %q", got, want)
	}
	if got, want := r.health.Addr, ":8080"; got != want {
		t.Errorf("health address: got %q, want %q", got, want)
	}
}

func TestHealthAnswersOnItsOwnListenerOnly(t *testing.T) {
	config := DefaultConfig()
	r := New(config, slog.New(slog.DiscardHandler), metrics.NewRegistry(), fakeHandler)

	res := httptest.NewRecorder()
	r.health.Handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if res.Code != http.StatusOK {
		t.Fatalf("health status: got %d, want 200", res.Code)
	}
	if got := strings.TrimSpace(res.Body.String()); got != "{}" {
		t.Errorf("health body: got %s", got)
	}

	// The probe path is not served by the API listener, and the API routes are
	// not served by the health listener.
	res = httptest.NewRecorder()
	r.api.Handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if res.Code != http.StatusNotFound {
		t.Errorf("health path on the api listener: got %d, want 404", res.Code)
	}

	res = httptest.NewRecorder()
	r.health.Handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/records", nil))
	if res.Code != http.StatusNotFound {
		t.Errorf("records on the health listener: got %d, want 404", res.Code)
	}
}
