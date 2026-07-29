// Package router serves the webhook over net/http.
//
// It used to run gin with a prometheus middleware. Four routes and a health
// check do not need a framework, and nothing scraped the metrics endpoint, so
// both are gone along with their dependency trees.
package router

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// Handler registers the application routes on a mux.
type Handler interface {
	Register(mux *http.ServeMux)
}

type Router struct {
	config *Config
	srv    *http.Server
	log    *slog.Logger
}

// New builds the HTTP server. It cannot fail, so it returns no error.
func New(config *Config, log *slog.Logger, handler Handler) *Router {
	mux := http.NewServeMux()

	mux.HandleFunc(config.HealthCheckPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	})

	handler.Register(mux)

	return &Router{
		config: config,
		log:    log,
		srv: &http.Server{
			Addr:    ":" + config.Port,
			Handler: logRequests(log, config.HealthCheckPath, mux),
			// Bounds a client that opens a connection and never finishes
			// sending the request headers.
			ReadHeaderTimeout: 10 * time.Second,
		},
	}
}

func (r *Router) Run() error {
	r.log.Info("starting http server", slog.String("port", r.config.Port))

	if err := r.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}

	return nil
}

func (r *Router) Shutdown(ctx context.Context) error {
	r.log.Debug("shutting down http server")

	if err := r.srv.Shutdown(ctx); err != nil {
		return err
	}

	r.log.Info("http server stopped")
	return nil
}

// logRequests logs one line per request, skipping the health check so probes
// do not flood the log.
func logRequests(log *slog.Logger, healthPath string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == healthPath {
			next.ServeHTTP(w, req)
			return
		}

		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, req)

		log.Info("request",
			slog.String("method", req.Method),
			slog.String("path", req.URL.Path),
			slog.Int("status", recorder.status),
			slog.Duration("duration", time.Since(start)))
	})
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
