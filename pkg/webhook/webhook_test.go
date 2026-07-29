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
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/webhookapi"
)

const testMediaType = "application/external.dns.webhook+json;version=1"

type fakeProvider struct {
	records   []*webhookapi.Endpoint
	applied   *webhookapi.Changes
	adjusted  []*webhookapi.Endpoint
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
	return endpoints, f.failWith
}

func (f *fakeProvider) GetDomainFilter() webhookapi.DomainFilter { return f.domainFil }

func newServer(provider Provider) http.Handler {
	mux := http.NewServeMux()
	New(provider, logger.Discard()).Register(mux)
	return mux
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
	provider := &fakeProvider{domainFil: webhookapi.DomainFilter{Filters: []string{}}}

	res := do(t, newServer(provider), http.MethodGet, "/", "", map[string]string{"Accept": testMediaType})

	if res.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", res.Code)
	}
	if got := strings.TrimSpace(res.Body.String()); got != `{"filters":[]}` {
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
