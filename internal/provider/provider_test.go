package provider

import (
	"context"
	"log/slog"
	"reflect"
	"testing"

	"github.com/VizzleTF/external-dns-openwrt-next/pkg/openwrt"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/webhookapi"
)

// fakeOpenWRT serves a fixed router state and records what it was asked to
// apply.
type fakeOpenWRT struct {
	records     map[string]openwrt.DNSRecord
	remove, add []openwrt.DNSRecord
}

func (f *fakeOpenWRT) GetDNSRecords(context.Context) (map[string]openwrt.DNSRecord, error) {
	return f.records, nil
}

func (f *fakeOpenWRT) ApplyDNSRecords(_ context.Context, remove, add []openwrt.DNSRecord) error {
	f.remove, f.add = remove, add
	return nil
}

func newTestProvider(router *fakeOpenWRT) *Provider {
	return &Provider{openwrt: router, log: slog.New(slog.DiscardHandler)}
}

func a(name, ip string) openwrt.DNSRecord {
	return openwrt.DNSRecord{Type: openwrt.RecordTypeA, Name: name, Value: ip}
}

func endpoint(name, recordType string, targets ...string) *webhookapi.Endpoint {
	return &webhookapi.Endpoint{DNSName: name, RecordType: recordType, Targets: targets}
}

func TestEndpointsToDNSRecords(t *testing.T) {
	for _, tc := range []struct {
		name      string
		endpoints []*webhookapi.Endpoint
		want      []openwrt.DNSRecord
	}{
		{
			name: "converts A and CNAME",
			endpoints: []*webhookapi.Endpoint{
				endpoint("a.foobar.com", webhookapi.RecordTypeA, "1.1.1.1"),
				endpoint("b.foobar.com", webhookapi.RecordTypeCNAME, "c.foobar.com"),
			},
			want: []openwrt.DNSRecord{
				a("a.foobar.com", "1.1.1.1"),
				{Type: openwrt.RecordTypeCNAME, Name: "b.foobar.com", Value: "c.foobar.com"},
			},
		},
		{
			// Upstream only ever read Targets[0], silently dropping the rest.
			name:      "emits one record per target",
			endpoints: []*webhookapi.Endpoint{endpoint("multi.foobar.com", webhookapi.RecordTypeA, "1.1.1.1", "2.2.2.2")},
			want:      []openwrt.DNSRecord{a("multi.foobar.com", "1.1.1.1"), a("multi.foobar.com", "2.2.2.2")},
		},
		{
			name: "skips unsupported types and empty targets",
			endpoints: []*webhookapi.Endpoint{
				endpoint("txt.foobar.com", "TXT", "hello"),
				endpoint("empty.foobar.com", webhookapi.RecordTypeA),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := newTestProvider(nil).endpoints2DNSRecords(tc.endpoints); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDNSRecordsToEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name    string
		records map[string]openwrt.DNSRecord
		want    []*webhookapi.Endpoint
	}{
		{
			// Sorted, so the plan does not churn on random map order.
			name: "merges sections that share a name and type into one endpoint",
			records: map[string]openwrt.DNSRecord{
				"a": a("multi.foobar.com", "2.2.2.2"),
				"b": a("multi.foobar.com", "1.1.1.1"),
			},
			want: []*webhookapi.Endpoint{endpoint("multi.foobar.com", webhookapi.RecordTypeA, "1.1.1.1", "2.2.2.2")},
		},
		{
			// ExternalDNS keys its plan on the normalised name and keeps one
			// endpoint per record type, so reporting these separately would
			// hide a section from it entirely: never updated, never deleted.
			name: "merges sections whose names differ only in case or trailing dot",
			records: map[string]openwrt.DNSRecord{
				"adopted": a("NAS.lan.", "1.1.1.1"),
				"ours":    a("nas.lan", "2.2.2.2"),
			},
			want: []*webhookapi.Endpoint{endpoint("nas.lan", webhookapi.RecordTypeA, "1.1.1.1", "2.2.2.2")},
		},
		{
			// Not deduplicated on purpose: the plan then asks for an update,
			// which deletes both sections and writes one back — the router ends
			// up clean instead of quietly holding a copy forever.
			name: "reports duplicate sections as duplicate targets",
			records: map[string]openwrt.DNSRecord{
				"one": a("dup.lan", "1.1.1.1"),
				"two": a("DUP.lan", "1.1.1.1"),
			},
			want: []*webhookapi.Endpoint{endpoint("dup.lan", webhookapi.RecordTypeA, "1.1.1.1", "1.1.1.1")},
		},
		{
			name: "returns endpoints in a stable order",
			records: map[string]openwrt.DNSRecord{
				"a": a("z.foobar.com", "1.1.1.1"),
				"b": {Type: openwrt.RecordTypeCNAME, Name: "a.foobar.com", Value: "z.foobar.com"},
			},
			want: []*webhookapi.Endpoint{
				endpoint("a.foobar.com", webhookapi.RecordTypeCNAME, "z.foobar.com"),
				endpoint("z.foobar.com", webhookapi.RecordTypeA, "1.1.1.1"),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, ep := range tc.want {
				ep.RecordTTL = defaultTTL
			}
			for range 10 {
				if got := newTestProvider(nil).dnsRecords2Endpoints(tc.records); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestApplyChanges(t *testing.T) {
	for _, tc := range []struct {
		name        string
		changes     webhookapi.Changes
		remove, add []openwrt.DNSRecord
	}{
		{
			name: "applies creates and deletes",
			changes: webhookapi.Changes{
				Create: []*webhookapi.Endpoint{endpoint("new.foobar.com", webhookapi.RecordTypeA, "1.1.1.1")},
				Delete: []*webhookapi.Endpoint{endpoint("old.foobar.com", webhookapi.RecordTypeA, "9.9.9.9")},
			},
			remove: []openwrt.DNSRecord{a("old.foobar.com", "9.9.9.9")},
			add:    []openwrt.DNSRecord{a("new.foobar.com", "1.1.1.1")},
		},
		{
			// Upstream pushed UpdateOld back onto the router before UpdateNew,
			// so the previous value was re-created on every update.
			name: "withdraws UpdateOld and installs UpdateNew",
			changes: webhookapi.Changes{
				UpdateOld: []*webhookapi.Endpoint{endpoint("foo.foobar.com", webhookapi.RecordTypeA, "1.1.1.1")},
				UpdateNew: []*webhookapi.Endpoint{endpoint("foo.foobar.com", webhookapi.RecordTypeA, "2.2.2.2")},
			},
			remove: []openwrt.DNSRecord{a("foo.foobar.com", "1.1.1.1")},
			add:    []openwrt.DNSRecord{a("foo.foobar.com", "2.2.2.2")},
		},
		{
			name: "leaves untouched targets alone when an update only adds one",
			changes: webhookapi.Changes{
				UpdateOld: []*webhookapi.Endpoint{endpoint("foo.foobar.com", webhookapi.RecordTypeA, "1.1.1.1")},
				UpdateNew: []*webhookapi.Endpoint{endpoint("foo.foobar.com", webhookapi.RecordTypeA, "1.1.1.1", "2.2.2.2")},
			},
			remove: []openwrt.DNSRecord{},
			add:    []openwrt.DNSRecord{a("foo.foobar.com", "2.2.2.2")},
		},
		{
			// The router layer skips the round trip for an empty pair.
			name:   "passes an empty plan through as nothing to do",
			remove: []openwrt.DNSRecord{},
			add:    []openwrt.DNSRecord{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := &fakeOpenWRT{}
			if err := newTestProvider(router).ApplyChanges(context.Background(), &tc.changes); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(router.remove, tc.remove) || !reflect.DeepEqual(router.add, tc.add) {
				t.Errorf("got remove %v add %v, want remove %v add %v", router.remove, router.add, tc.remove, tc.add)
			}
		})
	}
}

func TestAdjustEndpoints(t *testing.T) {
	p := newTestProvider(nil)

	// Left in place they would be planned, silently skipped at write time, and
	// re-planned on every run.
	adjusted := p.AdjustEndpoints([]*webhookapi.Endpoint{
		endpoint("a.foobar.com", webhookapi.RecordTypeA, "1.1.1.1"),
		endpoint("aaaa.foobar.com", "AAAA", "::1"),
		endpoint("txt.foobar.com", "TXT", "hi"),
		endpoint("c.foobar.com", webhookapi.RecordTypeCNAME, "a.foobar.com"),
		nil,
	})
	if len(adjusted) != 2 || adjusted[0].DNSName != "a.foobar.com" || adjusted[1].DNSName != "c.foobar.com" {
		t.Errorf("unsupported types kept: %v", adjusted)
	}

	// dnsmasq serves every record with its global local_ttl.
	ttl := endpoint("a.foobar.com", webhookapi.RecordTypeA, "1.1.1.1")
	ttl.RecordTTL = 60
	adjusted = p.AdjustEndpoints([]*webhookapi.Endpoint{ttl})
	if len(adjusted) != 1 || adjusted[0].RecordTTL != 0 {
		t.Errorf("per-record TTL kept: %v", adjusted)
	}
}

func TestRecordsReadsThroughToTheRouter(t *testing.T) {
	router := &fakeOpenWRT{records: map[string]openwrt.DNSRecord{"a": a("a.foobar.com", "1.1.1.1")}}

	endpoints, err := newTestProvider(router).Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(endpoints) != 1 || endpoints[0].DNSName != "a.foobar.com" || endpoints[0].RecordTTL != defaultTTL {
		t.Errorf("got %v", endpoints)
	}
}
