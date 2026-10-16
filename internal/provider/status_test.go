package provider

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"testing"

	"github.com/VizzleTF/external-dns-openwrt-next/pkg/openwrt"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/status"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/webhookapi"
)

func cname(name, target string) openwrt.DNSRecord {
	return openwrt.DNSRecord{Type: openwrt.RecordTypeCNAME, Name: name, Value: target}
}

// statusRun drives one ExternalDNS run through the provider and returns the
// last snapshot it published.
type statusRun struct {
	t         *testing.T
	router    *fakeOpenWRT
	provider  *Provider
	published [][]status.Record
}

func newStatusRun(t *testing.T, router *fakeOpenWRT) *statusRun {
	run := &statusRun{t: t, router: router}
	run.provider = &Provider{openwrt: router, log: slog.New(slog.DiscardHandler)}
	run.provider.status = newTracker(func(records []status.Record) { run.published = append(run.published, records) })
	return run
}

func (r *statusRun) once(desired []*webhookapi.Endpoint, changes *webhookapi.Changes) []status.Record {
	r.t.Helper()
	if _, err := r.provider.Records(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	r.provider.AdjustEndpoints(desired)
	if changes != nil {
		_ = r.provider.ApplyChanges(context.Background(), changes)
	}
	if len(r.published) == 0 {
		r.t.Fatal("nothing published")
	}
	return r.published[len(r.published)-1]
}

func row(name, recordType, state, reason string, targets ...string) status.Record {
	return status.Record{DNSName: name, RecordType: recordType, Targets: targets, State: state, Reason: reason}
}

func assertRows(t *testing.T, got []status.Record, want ...status.Record) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("status:\n got  %+v\n want %+v", got, want)
	}
}

func TestRecordStatus(t *testing.T) {
	const A, CNAME = webhookapi.RecordTypeA, webhookapi.RecordTypeCNAME

	t.Run("reports every state", func(t *testing.T) {
		run := newStatusRun(t, &fakeOpenWRT{records: map[string]openwrt.DNSRecord{
			"s1": a("ready.lan", "10.0.0.1"),
			"s2": a("Old.lan", "10.0.0.2"),
			"s3": a("moved.lan", "10.0.0.3"),
		}})
		run.router.skipped = []openwrt.Skip{{Record: cname("taken.lan", "x.lan"), Reason: "alias already has a CNAME"}}

		got := run.once([]*webhookapi.Endpoint{
			endpoint("ready.lan", A, "10.0.0.1"),
			endpoint("moved.lan", A, "10.0.0.9"),
			endpoint("new.lan", A, "10.0.0.4"),
			endpoint("taken.lan", CNAME, "x.lan"),
			endpoint("*.wild.lan", A, "10.0.0.5"),
			endpoint("two.lan", CNAME, "b.lan", "a.lan"),
			endpoint("bad#.lan", A, "10.0.0.6"),
			endpoint("v6.lan", "AAAA", "fd00::1"),
			endpoint("elsewhere.example.com", A, "1.2.3.4"),
		}, &webhookapi.Changes{
			Create: []*webhookapi.Endpoint{endpoint("new.lan", A, "10.0.0.4"), endpoint("taken.lan", CNAME, "x.lan")},
		})

		assertRows(t, got,
			row("*.wild.lan", A, status.Rejected, "dnsmasq does not expand wildcard names", "10.0.0.5"),
			row("bad#.lan", A, status.Rejected, got[1].Reason, "10.0.0.6"),
			row("moved.lan", A, status.Pending, "router has 10.0.0.3", "10.0.0.9"),
			row("new.lan", A, status.Ready, "", "10.0.0.4"),
			row("old.lan", A, status.Orphan, "not requested by ExternalDNS", "10.0.0.2"),
			row("ready.lan", A, status.Ready, "", "10.0.0.1"),
			row("taken.lan", CNAME, status.Conflict, "alias already has a CNAME", "x.lan"),
			row("two.lan", CNAME, status.Rejected, "a CNAME has one target and dnsmasq does not start with two", "a.lan", "b.lan"),
		)
		if got[1].Reason == "" {
			t.Error("a record failing Validate has no reason")
		}
	})

	t.Run("a failed change set marks what it carried", func(t *testing.T) {
		run := newStatusRun(t, &fakeOpenWRT{err: errors.New("router unreachable")})
		desired := []*webhookapi.Endpoint{endpoint("new.lan", A, "10.0.0.4")}
		changes := &webhookapi.Changes{Create: desired}

		assertRows(t, run.once(desired, changes), row("new.lan", A, status.Error, "router unreachable", "10.0.0.4"))

		// Planned again on the next run, and through this time.
		run.router.err = nil
		assertRows(t, run.once(desired, changes), row("new.lan", A, status.Ready, "", "10.0.0.4"))
	})

	t.Run("an attempted record the router does not report yet is pending", func(t *testing.T) {
		run := newStatusRun(t, &fakeOpenWRT{})
		desired := []*webhookapi.Endpoint{endpoint("new.lan", A, "10.0.0.4")}
		run.once(desired, &webhookapi.Changes{Create: desired})

		// Records() has the last word: the write did not stick.
		assertRows(t, run.once(desired, nil), row("new.lan", A, status.Pending, "", "10.0.0.4"))
	})

	t.Run("an orphan disappears once deleted", func(t *testing.T) {
		run := newStatusRun(t, &fakeOpenWRT{records: map[string]openwrt.DNSRecord{"s1": a("old.lan", "10.0.0.2")}})

		assertRows(t, run.once(nil, &webhookapi.Changes{Delete: []*webhookapi.Endpoint{endpoint("old.lan", A, "10.0.0.2")}}))
	})

	t.Run("nothing is published before a run has read the router", func(t *testing.T) {
		run := newStatusRun(t, &fakeOpenWRT{})
		run.provider.AdjustEndpoints([]*webhookapi.Endpoint{endpoint("new.lan", A, "10.0.0.4")})
		if len(run.published) != 0 {
			t.Errorf("published %+v without having read the router", run.published)
		}
	})

	t.Run("switched off without a publisher", func(t *testing.T) {
		p := newTestProvider(&fakeOpenWRT{})
		p.AdjustEndpoints([]*webhookapi.Endpoint{endpoint("new.lan", A, "10.0.0.4")})
		if err := p.ApplyChanges(context.Background(), &webhookapi.Changes{}); err != nil {
			t.Fatal(err)
		}
	})
}
