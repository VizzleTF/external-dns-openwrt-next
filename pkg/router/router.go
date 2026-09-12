// Package router serves the webhook over net/http.
//
// It used to run gin with a prometheus middleware. Four routes and a health
// check do not need a framework, and nothing scraped the metrics endpoint, so
// both are gone along with their dependency trees.
//
// Two listeners, as the webhook provider specification asks for: the provider
// API on loopback, the health check on every interface.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/VizzleTF/external-dns-openwrt-next/pkg/metrics"
)

// Handler registers the application routes on a mux.
type Handler interface {
	Register(mux *http.ServeMux)
}

type Router struct {
	api    *http.Server
	health *http.Server
	log    *slog.Logger
}

// New builds both HTTP servers. It cannot fail, so it returns no error.
func New(config *Config, log *slog.Logger, registry *metrics.Registry, handler Handler) *Router {
	apiMux := http.NewServeMux()
	handler.Register(apiMux)

	healthMux := http.NewServeMux()
	healthMux.HandleFunc(config.HealthCheckPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	})

	// The specification puts /metrics beside /healthz, on the listener that is
	// reachable from outside the pod: a scrape comes from Prometheus, not from
	// the sidecar next door. An empty path switches it off.
	if config.MetricsPath != "" {
		healthMux.HandleFunc(config.MetricsPath, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", metrics.ContentType)
			if _, err := registry.WriteTo(w); err != nil {
				log.Error("error writing metrics", slog.Any("error", err))
			}
		})
	}

	return &Router{
		log: log,
		api: newServer(config.Address+":"+config.Port, logRequests(log, newHTTPMetrics(registry), apiMux)),
		// Probes are not worth a log line each, and the kubelet sends a lot of
		// them.
		health: newServer(":"+config.HealthCheckPort, healthMux),
	}
}

// httpMetrics counts what the access log already prints, so an operator can
// alert on it: ExternalDNS retries a 5xx and gives up on a 4xx, and neither
// shows up anywhere else.
type httpMetrics struct {
	requests *metrics.Counter
	duration *metrics.Counter
}

func newHTTPMetrics(registry *metrics.Registry) *httpMetrics {
	return &httpMetrics{
		requests: registry.Counter(metrics.Prefix+"_http_requests_total",
			"Requests served on the provider API, by route and status.", "route", "status"),
		// Seconds as a counter, divided by the request count for the mean —
		// what four routes on a LAN need. Buckets would cost more than they
		// would say, and a bare `_sum` without a `_count` beside it reads as a
		// half-written summary.
		duration: registry.Counter(metrics.Prefix+"_http_request_duration_seconds_total",
			"Total time spent serving the provider API, by route.", "route"),
	}
}

func newServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:    addr,
		Handler: handler,
		// Bounds a client that opens a connection and never finishes sending
		// the request headers.
		ReadHeaderTimeout: 10 * time.Second,
		// ExternalDNS keeps the connection alive between reconciles, so an idle
		// one is normal; this only reaps the ones nobody comes back to.
		IdleTimeout: 30 * time.Second,
	}
}

// Run serves both listeners and returns as soon as either one stops.
func (r *Router) Run() error {
	r.log.Info("starting http servers",
		slog.String("api", r.api.Addr), slog.String("health", r.health.Addr))

	// Buffered for both, so the listener that is still running can exit without
	// blocking on a send nobody receives.
	stopped := make(chan error, 2)
	go func() { stopped <- serve("api", r.api) }()
	go func() { stopped <- serve("health", r.health) }()

	// One of them failing takes the process down: a webhook without probes gets
	// killed, and probes without a webhook lie to the kubelet.
	return <-stopped
}

// serve names the listener in the error, because the two fail for different
// reasons — a taken port, a bind address the pod does not hold — and the
// message is all an operator gets before the process exits.
func serve(name string, srv *http.Server) error {
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s listener on %s: %w", name, srv.Addr, err)
	}
	return nil
}

func (r *Router) Shutdown(ctx context.Context) error {
	r.log.Debug("shutting down http servers")

	// Both are shut down even when the first one errors, so a failure cannot
	// leave a listener behind.
	apiErr := r.api.Shutdown(ctx)
	healthErr := r.health.Shutdown(ctx)
	if err := errors.Join(apiErr, healthErr); err != nil {
		return err
	}

	r.log.Info("http servers stopped")
	return nil
}

// logRequests logs and counts one request. It wraps the provider API only; the
// health listener has its own mux and would otherwise flood both the log and
// the counters with probes.
func logRequests(log *slog.Logger, counters *httpMetrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, req)

		elapsed := time.Since(start)

		route := routeLabel(req.Pattern)

		counters.requests.Inc(route, strconv.Itoa(recorder.status))
		counters.duration.Add(elapsed.Seconds(), route)

		log.Info("request",
			slog.String("method", req.Method),
			slog.String("path", req.URL.Path),
			slog.Int("status", recorder.status),
			slog.Duration("duration", elapsed))
	})
}

// routeLabel turns a matched ServeMux pattern into a metric label.
//
// The pattern, not the request path: patterns are a fixed set, so a stray
// request cannot mint a new series on every scrape interval. `{$}` is the
// mux's way of spelling "exactly this path" and means nothing on a dashboard,
// so it is dropped.
func routeLabel(pattern string) string {
	if pattern == "" {
		return "unmatched"
	}
	return strings.ReplaceAll(pattern, "{$}", "")
}

// statusRecorder captures the status code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
