// Package webhook implements the ExternalDNS webhook provider HTTP contract.
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/VizzleTF/external-dns-openwrt-next/pkg/metrics"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/webhookapi"
)

const (
	headerContentType = "Content-Type"
	headerAccept      = "Accept"
	headerVary        = "Vary"

	contentTypePlaintext = "text/plain"

	// mediaType is the only version of the webhook contract that exists.
	mediaType = "application/external.dns.webhook+json;version=1"

	// DefaultMaxBodyBytes mirrors the ExternalDNS default for
	// --webhook-provider-max-body-size. The controller caps what it reads from
	// this webhook; the cap here is the other half of that bargain, so a body
	// that is never going to be a plausible change set cannot be streamed into
	// memory before the decoder gives up.
	DefaultMaxBodyBytes int64 = 32 << 20
)

// Provider is the behaviour this webhook needs from a DNS backend.
//
// Declared here, on the consuming side, so the HTTP layer depends on an
// abstraction it owns rather than on a concrete implementation.
type Provider interface {
	Records(ctx context.Context) ([]*webhookapi.Endpoint, error)
	ApplyChanges(ctx context.Context, changes *webhookapi.Changes) error
	AdjustEndpoints(endpoints []*webhookapi.Endpoint) []*webhookapi.Endpoint
}

type Webhook struct {
	provider Provider
	log      *slog.Logger
	metrics  *webhookMetrics

	// maxBodyBytes caps a decoded request body.
	maxBodyBytes int64
}

// webhookMetrics is what this layer can measure that the HTTP access log
// cannot: how much state the router holds, what each reconcile asked for, and
// how much of it this provider had to throw away.
type webhookMetrics struct {
	records     *metrics.Gauge
	applied     *metrics.Counter
	planned     *metrics.Counter
	dropped     *metrics.Counter
	lastSuccess *metrics.Gauge
}

func New(provider Provider, log *slog.Logger, registry *metrics.Registry) *Webhook {
	return &Webhook{
		provider:     provider,
		log:          log,
		metrics:      newWebhookMetrics(registry),
		maxBodyBytes: DefaultMaxBodyBytes,
	}
}

func newWebhookMetrics(registry *metrics.Registry) *webhookMetrics {
	return &webhookMetrics{
		records: registry.Gauge(metrics.Prefix+"_records",
			"DNS records the router reported to the last successful Records call."),
		applied: registry.Counter(metrics.Prefix+"_apply_changes_total",
			"Change sets applied, by outcome.", "result"),
		planned: registry.Counter(metrics.Prefix+"_planned_endpoints_total",
			"Endpoints ExternalDNS asked this provider to change, by action.", "action"),
		dropped: registry.Counter(metrics.Prefix+"_dropped_endpoints_total",
			"Endpoints dropped in AdjustEndpoints, by the record type UCI cannot represent.",
			"record_type"),
		lastSuccess: registry.Gauge(metrics.Prefix+"_last_apply_success_timestamp_seconds",
			"Unix time of the last change set that reached the router."),
	}
}

// Register wires the contract onto a mux. The routes are fixed by the
// ExternalDNS webhook specification.
func (w *Webhook) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", w.Negotiate)
	mux.HandleFunc("GET /records", w.Records)
	mux.HandleFunc("POST /records", w.ApplyChanges)
	mux.HandleFunc("POST /adjustendpoints", w.AdjustEndpoints)
}

// Negotiate reports no domain filter of its own: ExternalDNS applies the
// --domain-filter it was started with, and this provider has no additional
// knowledge of which zones the router should serve.
func (w *Webhook) Negotiate(rw http.ResponseWriter, req *http.Request) {
	if !w.requireMediaType(rw, req, headerAccept) {
		return
	}

	w.writeJSON(rw, http.StatusOK, webhookapi.DomainFilter{Include: []string{}, Exclude: []string{}})
}

// Records returns everything the provider currently manages.
func (w *Webhook) Records(rw http.ResponseWriter, req *http.Request) {
	if !w.requireMediaType(rw, req, headerAccept) {
		return
	}

	records, err := w.provider.Records(req.Context())
	if err != nil {
		w.reject(rw, http.StatusInternalServerError, "error getting records", slog.Any("error", err))
		return
	}

	w.metrics.records.Set(float64(len(records)))
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

	w.metrics.planned.Add(float64(len(changes.Create)), "create")
	w.metrics.planned.Add(float64(len(changes.UpdateNew)), "update")
	w.metrics.planned.Add(float64(len(changes.Delete)), "delete")

	if err := w.provider.ApplyChanges(req.Context(), &changes); err != nil {
		w.metrics.applied.Inc("error")
		w.reject(rw, http.StatusInternalServerError, "error applying changes", slog.Any("error", err))
		return
	}

	w.metrics.applied.Inc("success")
	w.metrics.lastSuccess.Set(float64(time.Now().Unix()))
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

	adjusted := w.provider.AdjustEndpoints(endpoints)

	// What the provider refused to represent — AAAA from a dual-stack Service,
	// PTR, anything else UCI has no section for. Visible only in the log until
	// now, and worth an alert when it starts climbing. Broken down by record
	// type, because the answer ("narrow --managed-record-types", "turn off
	// --create-ptr") depends on which one it is.
	for recordType, count := range droppedByType(endpoints, adjusted) {
		w.metrics.dropped.Add(float64(count), recordType)
	}

	w.log.Debug("adjusted endpoints", slog.Int("endpoints", len(adjusted)))
	w.writeJSON(rw, http.StatusOK, adjusted)
}

// droppedByType counts, per record type, how many endpoints went in and did
// not come back. The label set is the DNS record types, which is bounded, so
// this cannot grow the metric without limit.
func droppedByType(before, after []*webhookapi.Endpoint) map[string]int {
	dropped := make(map[string]int)

	for _, ep := range before {
		if ep != nil {
			dropped[ep.RecordType]++
		}
	}
	for _, ep := range after {
		if ep != nil {
			dropped[ep.RecordType]--
		}
	}

	for recordType, count := range dropped {
		// An adjusted endpoint may also be one the provider rewrote rather than
		// dropped, so only a positive difference counts.
		if count <= 0 {
			delete(dropped, recordType)
		}
	}

	return dropped
}

// requireMediaType enforces the versioned media type on the given header,
// answering 406 when it is absent and 415 when it is not the one we speak.
func (w *Webhook) requireMediaType(rw http.ResponseWriter, req *http.Request, header string) bool {
	value := req.Header.Get(header)
	if value == "" {
		w.reject(rw, http.StatusNotAcceptable, "client must provide a "+header+" header",
			slog.String("header", header))
		return false
	}

	if value != mediaType {
		w.reject(rw, http.StatusUnsupportedMediaType, "client must provide a valid versioned media type",
			slog.String("header", header), slog.String("value", value))
		return false
	}

	return true
}

func (w *Webhook) decode(rw http.ResponseWriter, req *http.Request, target any) bool {
	defer func() { _ = req.Body.Close() }()

	req.Body = http.MaxBytesReader(rw, req.Body, w.maxBodyBytes)

	if err := json.NewDecoder(req.Body).Decode(target); err != nil {
		// An over-sized body is the client's fault and a distinct one: 413
		// tells it the request will never fit, where 400 says it was malformed.
		var toolarge *http.MaxBytesError
		if errors.As(err, &toolarge) {
			w.reject(rw, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}

		w.reject(rw, http.StatusBadRequest, "error decoding request body", slog.Any("error", err))
		return false
	}

	return true
}

func (w *Webhook) writeJSON(rw http.ResponseWriter, status int, body any) {
	rw.Header().Set(headerContentType, mediaType)
	rw.Header().Set(headerVary, headerContentType)
	rw.WriteHeader(status)

	if err := json.NewEncoder(rw).Encode(body); err != nil {
		// The status line is already on the wire, so this can only be logged.
		w.log.Error("error encoding response", slog.Any("error", err))
	}
}

// reject logs the failure and answers it with a plain-text body.
func (w *Webhook) reject(rw http.ResponseWriter, status int, message string, attrs ...any) {
	w.log.Error(message, attrs...)

	rw.Header().Set(headerContentType, contentTypePlaintext)
	rw.WriteHeader(status)
	// ExternalDNS drains the body before reusing the connection, so always
	// write one.
	_, _ = rw.Write([]byte(message))
}
