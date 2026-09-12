package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VizzleTF/external-dns-openwrt-next/pkg/logger"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/metrics"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/webhookapi"
)

const testMediaType = "application/external.dns.webhook+json;version=1"

type fakeProvider struct {
	records  []*webhookapi.Endpoint
	applied  *webhookapi.Changes
	adjusted []*webhookapi.Endpoint
	// adjustTo, when set, is what AdjustEndpoints returns — the way a test
	// stages a provider dropping what it cannot represent.
	adjustTo  []*webhookapi.Endpoint
	failWith  error
	domainFil webhookapi.DomainFilter
}

func (f *fakeProvider) Records(context.Context) ([]*webhookapi.Endpoint, error) {
	return f.records, f.failWith
}

func (f *fakeProvider) ApplyChanges(_ context.Context, changes *webhookapi.Changes) error {
	f.applied = changes
	return f.failWith
}

func (f *fakeProvider) AdjustEndpoints(endpoints []*webhookapi.Endpoint) ([]*webhookapi.Endpoint, error) {
	f.adjusted = endpoints
	if f.adjustTo != nil {
		return f.adjustTo, f.failWith
	}
	return endpoints, f.failWith
}

func (f *fakeProvider) GetDomainFilter() webhookapi.DomainFilter { return f.domainFil }

func newServer(provider Provider) http.Handler {
	return newServerWithBodyLimit(provider, DefaultMaxBodyBytes)
}

func newServerWithBodyLimit(provider Provider, maxBodyBytes int64) http.Handler {
	handler, _ := newServerWithMetrics(provider, maxBodyBytes)
	return handler
}

// newServerWithMetrics hands back the registry too, for the tests that assert
// on what a request recorded.
func newServerWithMetrics(provider Provider, maxBodyBytes int64) (http.Handler, *metrics.Registry) {
	mux := http.NewServeMux()
	registry := metrics.NewRegistry()
	hook := New(provider, logger.Discard(), registry)
	hook.MaxBodyBytes = maxBodyBytes
	hook.Register(mux)
	return mux, registry
}

// scrape renders the registry the way the metrics endpoint would.
func scrape(t *testing.T, registry *metrics.Registry) string {
	t.Helper()

	var out strings.Builder
	if _, err := registry.WriteTo(&out); err != nil {
		t.Fatalf("write metrics: %v", err)
	}
	return out.String()
}

func do(t *testing.T, handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func TestRecordsReturnsEndpointsWithTheVersionedMediaType(t *testing.T) {
	provider := &fakeProvider{records: []*webhookapi.Endpoint{
		{DNSName: "a.example.com", RecordType: webhookapi.RecordTypeA, Targets: webhookapi.Targets{"1.2.3.4"}},
	}}

	res := do(t, newServer(provider), http.MethodGet, "/records", "", map[string]string{"Accept": testMediaType})

	if res.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", res.Code)
	}
	if got := res.Header().Get("Content-Type"); got != testMediaType {
		t.Errorf("content-type: got %q", got)
	}
	if got := res.Header().Get("Vary"); got != "Content-Type" {
		t.Errorf("vary: got %q", got)
	}

	var decoded []*webhookapi.Endpoint
	if err := json.Unmarshal(res.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != 1 || decoded[0].DNSName != "a.example.com" {
		t.Errorf("body: %s", res.Body.String())
	}
}

func TestApplyChangesAnswers204AndForwardsTheChangeSet(t *testing.T) {
	provider := &fakeProvider{}
	body := `{"Create":[{"dnsName":"a.example.com","targets":["1.2.3.4"],"recordType":"A"}]}`

	res := do(t, newServer(provider), http.MethodPost, "/records", body,
		map[string]string{"Content-Type": testMediaType})

	// The specification requires 204 for a successful ApplyChanges.
	if res.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", res.Code)
	}
	if provider.applied == nil || len(provider.applied.Create) != 1 {
		t.Fatalf("change set not forwarded: %+v", provider.applied)
	}
	if got := provider.applied.Create[0].Targets[0]; got != "1.2.3.4" {
		t.Errorf("target: got %q", got)
	}
}

func TestNegotiateServesTheDomainFilter(t *testing.T) {
	provider := &fakeProvider{domainFil: webhookapi.DomainFilter{Include: []string{}, Exclude: []string{}}}

	res := do(t, newServer(provider), http.MethodGet, "/", "", map[string]string{"Accept": testMediaType})

	if res.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", res.Code)
	}
	// ExternalDNS deserialises this into endpoint.DomainFilter, which reads
	// `include`/`exclude` and ignores anything else.
	if got := strings.TrimSpace(res.Body.String()); got != `{"include":[],"exclude":[]}` {
		t.Errorf("body: got %s", got)
	}
}

func TestAdjustEndpointsRoundTrips(t *testing.T) {
	provider := &fakeProvider{}
	body := `[{"dnsName":"a.example.com","targets":["1.2.3.4"],"recordType":"A"}]`

	res := do(t, newServer(provider), http.MethodPost, "/adjustendpoints", body,
		map[string]string{"Content-Type": testMediaType, "Accept": testMediaType})

	if res.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", res.Code)
	}
	if len(provider.adjusted) != 1 {
		t.Fatalf("endpoints not forwarded: %+v", provider.adjusted)
	}
}

func TestMissingHeaderIs406AndWrongMediaTypeIs415(t *testing.T) {
	handler := newServer(&fakeProvider{})

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no accept header", nil, http.StatusNotAcceptable},
		{"unversioned media type", map[string]string{"Accept": "application/json"}, http.StatusUnsupportedMediaType},
		{"unknown version", map[string]string{"Accept": "application/external.dns.webhook+json;version=99"}, http.StatusUnsupportedMediaType},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := do(t, handler, http.MethodGet, "/records", "", tc.headers)
			if res.Code != tc.want {
				t.Errorf("status: got %d, want %d", res.Code, tc.want)
			}
			if res.Body.Len() == 0 {
				t.Error("an error response must still carry a body so the connection can be reused")
			}
		})
	}
}

func TestMalformedBodyIs400(t *testing.T) {
	res := do(t, newServer(&fakeProvider{}), http.MethodPost, "/records", "{not json",
		map[string]string{"Content-Type": testMediaType})

	if res.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", res.Code)
	}
}

func TestProviderFailureIs500(t *testing.T) {
	provider := &fakeProvider{failWith: errors.New("router unreachable")}

	res := do(t, newServer(provider), http.MethodGet, "/records", "",
		map[string]string{"Accept": testMediaType})

	if res.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", res.Code)
	}
}

func TestOversizedBodyIs413(t *testing.T) {
	body := `[{"dnsName":"a.example.com","targets":["1.2.3.4"],"recordType":"A"}]`
	headers := map[string]string{"Content-Type": testMediaType, "Accept": testMediaType}

	// A cap one byte short of the payload: the decoder must report the cap
	// rather than a malformed document.
	handler := newServerWithBodyLimit(&fakeProvider{}, int64(len(body))-1)

	res := do(t, handler, http.MethodPost, "/adjustendpoints", body, headers)

	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want 413", res.Code)
	}
}

func TestBodyExactlyAtTheLimitIsAccepted(t *testing.T) {
	body := `[{"dnsName":"a.example.com","targets":["1.2.3.4"],"recordType":"A"}]`
	headers := map[string]string{"Content-Type": testMediaType, "Accept": testMediaType}

	handler := newServerWithBodyLimit(&fakeProvider{}, int64(len(body)))

	res := do(t, handler, http.MethodPost, "/adjustendpoints", body, headers)

	if res.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", res.Code)
	}
}

func TestRecordsAndChangeSetsAreCounted(t *testing.T) {
	provider := &fakeProvider{records: []*webhookapi.Endpoint{
		{DNSName: "a.example.com", RecordType: webhookapi.RecordTypeA},
		{DNSName: "b.example.com", RecordType: webhookapi.RecordTypeA},
	}}
	handler, registry := newServerWithMetrics(provider, DefaultMaxBodyBytes)

	do(t, handler, http.MethodGet, "/records", "", map[string]string{"Accept": testMediaType})

	changes := `{"create":[{"dnsName":"new.example.com","targets":["1.2.3.4"],"recordType":"A"}],` +
		`"delete":[{"dnsName":"old.example.com","targets":["1.1.1.1"],"recordType":"A"}]}`
	do(t, handler, http.MethodPost, "/records", changes, map[string]string{"Content-Type": testMediaType})

	out := scrape(t, registry)
	for _, want := range []string{
		"external_dns_openwrt_records 2\n",
		`external_dns_openwrt_apply_changes_total{result="success"} 1`,
		`external_dns_openwrt_planned_endpoints_total{action="create"} 1`,
		`external_dns_openwrt_planned_endpoints_total{action="delete"} 1`,
		"external_dns_openwrt_last_apply_success_timestamp_seconds ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestAFailedApplyIsCountedAsAnError(t *testing.T) {
	provider := &fakeProvider{failWith: errors.New("router unreachable")}
	handler, registry := newServerWithMetrics(provider, DefaultMaxBodyBytes)

	body := `{"create":[{"dnsName":"new.example.com","targets":["1.2.3.4"],"recordType":"A"}]}`
	do(t, handler, http.MethodPost, "/records", body, map[string]string{"Content-Type": testMediaType})

	out := scrape(t, registry)
	if !strings.Contains(out, `external_dns_openwrt_apply_changes_total{result="error"} 1`) {
		t.Errorf("failure not counted:\n%s", out)
	}
	// A failed apply never reached the router, so the success timestamp must
	// not move — it is what an alert watches.
	if strings.Contains(out, "external_dns_openwrt_last_apply_success_timestamp_seconds") {
		t.Errorf("the success timestamp moved on a failure:\n%s", out)
	}
}

func TestDroppedEndpointsAreCounted(t *testing.T) {
	// The fake provider returns whatever it is given, so the drop is staged by
	// handing back fewer endpoints than arrived.
	provider := &fakeProvider{adjustTo: []*webhookapi.Endpoint{
		{DNSName: "a.example.com", RecordType: webhookapi.RecordTypeA},
	}}
	handler, registry := newServerWithMetrics(provider, DefaultMaxBodyBytes)

	body := `[{"dnsName":"a.example.com","targets":["1.2.3.4"],"recordType":"A"},` +
		`{"dnsName":"a.example.com","targets":["2001:db8::1"],"recordType":"AAAA"},` +
		`{"dnsName":"b.example.com","targets":["10.0.0.1"],"recordType":"PTR"}]`
	do(t, handler, http.MethodPost, "/adjustendpoints", body,
		map[string]string{"Content-Type": testMediaType, "Accept": testMediaType})

	out := scrape(t, registry)
	// Which type was dropped is the actionable half: AAAA says narrow
	// --managed-record-types, PTR says --create-ptr is on.
	for _, want := range []string{
		`external_dns_openwrt_dropped_endpoints_total{record_type="AAAA"} 1`,
		`external_dns_openwrt_dropped_endpoints_total{record_type="PTR"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// The A record survived, so it is not in the metric at all.
	if strings.Contains(out, `record_type="A"`) {
		t.Errorf("a kept endpoint was counted as dropped:\n%s", out)
	}
}
