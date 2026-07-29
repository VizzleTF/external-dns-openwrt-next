// Package webhook implements the ExternalDNS webhook provider HTTP contract.
package webhook

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/VizzleTF/external-dns-openwrt-next/pkg/webhookapi"
)

const (
	headerContentType = "Content-Type"
	headerAccept      = "Accept"
	headerVary        = "Vary"

	contentTypePlaintext = "text/plain"
)

// Provider is the behaviour this webhook needs from a DNS backend.
//
// Declared here, on the consuming side, so the HTTP layer depends on an
// abstraction it owns rather than on a concrete implementation.
type Provider interface {
	Records(ctx context.Context) ([]*webhookapi.Endpoint, error)
	ApplyChanges(ctx context.Context, changes *webhookapi.Changes) error
	AdjustEndpoints(endpoints []*webhookapi.Endpoint) ([]*webhookapi.Endpoint, error)
	GetDomainFilter() webhookapi.DomainFilter
}

type Webhook struct {
	provider Provider
	log      *slog.Logger
}

func New(provider Provider, log *slog.Logger) *Webhook {
	return &Webhook{provider: provider, log: log}
}

// Register wires the contract onto a mux. The routes are fixed by the
// ExternalDNS webhook specification.
func (w *Webhook) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", w.Negotiate)
	mux.HandleFunc("GET /records", w.Records)
	mux.HandleFunc("POST /records", w.ApplyChanges)
	mux.HandleFunc("POST /adjustendpoints", w.AdjustEndpoints)
}

// Negotiate reports the domain filter this provider serves.
func (w *Webhook) Negotiate(rw http.ResponseWriter, req *http.Request) {
	if !w.requireMediaType(rw, req, headerAccept) {
		return
	}

	w.writeJSON(rw, http.StatusOK, w.provider.GetDomainFilter())
}

// Records returns everything the provider currently manages.
func (w *Webhook) Records(rw http.ResponseWriter, req *http.Request) {
	if !w.requireMediaType(rw, req, headerAccept) {
		return
	}

	records, err := w.provider.Records(req.Context())
	if err != nil {
		w.fail(rw, "error getting records", err)
		return
	}

	w.writeJSON(rw, http.StatusOK, records)
}

// ApplyChanges applies one reconcile step and answers 204 on success, as the
// specification requires.
func (w *Webhook) ApplyChanges(rw http.ResponseWriter, req *http.Request) {
	if !w.requireMediaType(rw, req, headerContentType) {
		return
	}

	var changes webhookapi.Changes
	if !w.decode(rw, req, &changes) {
		return
	}

	w.log.Debug("requesting apply changes",
		slog.Int("create", len(changes.Create)),
		slog.Int("update_old", len(changes.UpdateOld)),
		slog.Int("update_new", len(changes.UpdateNew)),
		slog.Int("delete", len(changes.Delete)))

	if err := w.provider.ApplyChanges(req.Context(), &changes); err != nil {
		w.fail(rw, "error applying changes", err)
		return
	}

	rw.WriteHeader(http.StatusNoContent)
}

// AdjustEndpoints lets the provider rewrite the desired state before planning.
func (w *Webhook) AdjustEndpoints(rw http.ResponseWriter, req *http.Request) {
	if !w.requireMediaType(rw, req, headerContentType) || !w.requireMediaType(rw, req, headerAccept) {
		return
	}

	var endpoints []*webhookapi.Endpoint
	if !w.decode(rw, req, &endpoints) {
		return
	}

	adjusted, err := w.provider.AdjustEndpoints(endpoints)
	if err != nil {
		w.fail(rw, "error adjusting endpoints", err)
		return
	}

	w.log.Debug("adjusted endpoints", slog.Int("endpoints", len(adjusted)))
	w.writeJSON(rw, http.StatusOK, adjusted)
}

// requireMediaType enforces the versioned media type on the given header,
// answering 406 when it is absent and 415 when it is not the one we speak.
func (w *Webhook) requireMediaType(rw http.ResponseWriter, req *http.Request, header string) bool {
	value := req.Header.Get(header)
	if value == "" {
		w.reject(rw, http.StatusNotAcceptable, "client must provide a "+header+" header", header, value)
		return false
	}

	// Only one media type version exists, so the parsed value is not needed.
	if _, err := checkAndGetMediaTypeHeaderValue(value); err != nil {
		w.reject(rw, http.StatusUnsupportedMediaType,
			"client must provide a valid versioned media type", header, value)
		return false
	}

	return true
}

func (w *Webhook) decode(rw http.ResponseWriter, req *http.Request, target any) bool {
	defer func() { _ = req.Body.Close() }()

	if err := json.NewDecoder(req.Body).Decode(target); err != nil {
		w.log.Error("error decoding request body", slog.Any("error", err))
		w.reject(rw, http.StatusBadRequest, "error decoding request body", "", "")
		return false
	}

	return true
}

func (w *Webhook) writeJSON(rw http.ResponseWriter, status int, body any) {
	rw.Header().Set(headerContentType, string(mediaTypeVersion1))
	rw.Header().Set(headerVary, headerContentType)
	rw.WriteHeader(status)

	if err := json.NewEncoder(rw).Encode(body); err != nil {
		// The status line is already on the wire, so this can only be logged.
		w.log.Error("error encoding response", slog.Any("error", err))
	}
}

func (w *Webhook) reject(rw http.ResponseWriter, status int, message, header, value string) {
	w.log.Error(message, slog.String("header", header), slog.String("value", value))

	rw.Header().Set(headerContentType, contentTypePlaintext)
	rw.WriteHeader(status)
	// ExternalDNS drains the body before reusing the connection, so always
	// write one.
	_, _ = rw.Write([]byte(message))
}

func (w *Webhook) fail(rw http.ResponseWriter, message string, err error) {
	w.log.Error(message, slog.Any("error", err))

	rw.Header().Set(headerContentType, contentTypePlaintext)
	rw.WriteHeader(http.StatusInternalServerError)
	_, _ = rw.Write([]byte(message))
}
