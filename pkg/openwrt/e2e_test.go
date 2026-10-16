//go:build e2e

package openwrt

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VizzleTF/external-dns-openwrt-next/pkg/ubus"
)

const e2eOwner = "e2e"

// e2eProvider logs in to the router hack/e2e.sh sets up, afresh on each call:
// a new session sees only what was applied.
func e2eProvider(t *testing.T, username, password string) OpenWRT {
	t.Helper()
	host := os.Getenv("OPENWRT_E2E_HOST")
	if host == "" {
		t.Skip("OPENWRT_E2E_HOST is not set, run hack/e2e.sh")
	}

	cfg := DefaultConfig()
	cfg.Ubus.Hostname = host
	cfg.Ubus.Port = 80
	cfg.Ubus.SSL = false
	cfg.Ubus.Auth = ubus.Auth{Username: username, Password: password}
	cfg.OwnershipID = e2eOwner
	o, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func e2eRoot(t *testing.T) OpenWRT {
	return e2eProvider(t, "root", os.Getenv("OPENWRT_E2E_ROOT_PASSWORD"))
}

// committed returns what the router holds under prefix, marker included.
func committed(t *testing.T, prefix string) []DNSRecord {
	t.Helper()
	records, err := e2eRoot(t).GetDNSRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var found []DNSRecord
	for _, record := range records {
		if strings.HasPrefix(record.Name, prefix) {
			found = append(found, record)
		}
	}
	return sorted(found)
}

func sorted(records []DNSRecord) []DNSRecord {
	slices.SortFunc(records, func(a, b DNSRecord) int { return strings.Compare(a.Key(), b.Key()) })
	return records
}

// resolves waits for dnsmasq on the router to answer name with want, or with
// NXDOMAIN for no want. procd reloads dnsmasq after uci apply has returned.
func resolves(t *testing.T, name string, want ...string) {
	t.Helper()
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(os.Getenv("OPENWRT_E2E_HOST"), "53"))
		},
	}

	var got []string
	var err error
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		// The trailing dot keeps the resolver from trying the search domains.
		got, err = resolver.LookupHost(ctx, name+".")
		cancel()
		var dnsErr *net.DNSError
		if len(want) == 0 && errors.As(err, &dnsErr) && dnsErr.IsNotFound || err == nil && slices.Equal(got, want) {
			return
		}
	}
	t.Errorf("%s: got %v (%v), want %v", name, got, err, want)
}

func TestE2EApplyingRecords(t *testing.T) {
	for _, tc := range []struct{ name, username, password string }{
		{"root", "root", os.Getenv("OPENWRT_E2E_ROOT_PASSWORD")},
		{"user", os.Getenv("OPENWRT_E2E_USERNAME"), os.Getenv("OPENWRT_E2E_PASSWORD")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			o := e2eProvider(t, tc.username, tc.password)
			prefix := "e2e-" + tc.name + "-"
			host, alias, other := prefix+"host.lan", prefix+"alias.lan", prefix+"other.lan"
			t.Cleanup(func() {
				if leftover := committed(t, prefix); len(leftover) > 0 {
					_ = e2eRoot(t).ApplyDNSRecords(ctx, leftover, nil)
				}
			})

			// Create an A record and a CNAME onto it.
			a1 := DNSRecord{Type: RecordTypeA, Name: host, Value: "10.9.8.1"}
			cname := DNSRecord{Type: RecordTypeCNAME, Name: alias, Value: host}
			if err := o.ApplyDNSRecords(ctx, nil, []DNSRecord{a1, cname}); err != nil {
				t.Fatal(err)
			}
			mark := func(records ...DNSRecord) []DNSRecord {
				for i := range records {
					records[i].Owner = e2eOwner
				}
				return sorted(records)
			}
			if got, want := committed(t, prefix), mark(a1, cname); !slices.Equal(got, want) {
				t.Errorf("after create: got %v, want %v", got, want)
			}
			resolves(t, host, "10.9.8.1")
			resolves(t, alias, "10.9.8.1")

			// Change the target of both.
			a2 := DNSRecord{Type: RecordTypeA, Name: other, Value: "10.9.8.2"}
			a3 := DNSRecord{Type: RecordTypeA, Name: host, Value: "10.9.8.3"}
			cname2 := DNSRecord{Type: RecordTypeCNAME, Name: alias, Value: other}
			if err := o.ApplyDNSRecords(ctx, []DNSRecord{a1, cname}, []DNSRecord{a2, a3, cname2}); err != nil {
				t.Fatal(err)
			}
			if got, want := committed(t, prefix), mark(a2, a3, cname2); !slices.Equal(got, want) {
				t.Errorf("after update: got %v, want %v", got, want)
			}
			resolves(t, host, "10.9.8.3")
			resolves(t, alias, "10.9.8.2")

			// A change set failing halfway leaves nothing behind, and the next
			// one does not apply what the failed one staged.
			staged := DNSRecord{Type: RecordTypeA, Name: prefix + "staged.lan", Value: "10.9.8.4"}
			refused := DNSRecord{Type: RecordTypeA, Name: prefix + "refused.lan", Value: "fd00::4"}
			if err := o.ApplyDNSRecords(ctx, []DNSRecord{a3}, []DNSRecord{staged, refused}); err == nil {
				t.Fatal("a change set with an AAAA address went through")
			}
			if got, want := committed(t, prefix), mark(a2, a3, cname2); !slices.Equal(got, want) {
				t.Errorf("after a failure: got %v, want %v", got, want)
			}
			next := DNSRecord{Type: RecordTypeA, Name: prefix + "next.lan", Value: "10.9.8.5"}
			if err := o.ApplyDNSRecords(ctx, nil, []DNSRecord{next}); err != nil {
				t.Fatal(err)
			}
			if got, want := committed(t, prefix), mark(next, a2, a3, cname2); !slices.Equal(got, want) {
				t.Errorf("after the next change set: got %v, want %v", got, want)
			}
			resolves(t, next.Name, "10.9.8.5")
			resolves(t, host, "10.9.8.3")
			resolves(t, staged.Name)

			// Delete everything.
			if err := o.ApplyDNSRecords(ctx, []DNSRecord{next, a2, a3, cname2}, nil); err != nil {
				t.Fatal(err)
			}
			if got := committed(t, prefix); len(got) != 0 {
				t.Errorf("after delete: got %v", got)
			}
			resolves(t, host)
			resolves(t, alias)
		})
	}
}

// interleaved runs before ahead of the first uci apply.
type interleaved struct {
	ubus.Client
	before func()
}

func (i *interleaved) Call(ctx context.Context, object, method string, args, result any) error {
	if method == "apply" && i.before != nil {
		i.before()
		i.before = nil
	}
	return i.Client.Call(ctx, object, method, args, result)
}

// rpcd would name both sections alike, and the later apply would overwrite
// the section the other session committed.
func TestE2EConcurrentAddsKeepBothSections(t *testing.T) {
	ctx := context.Background()
	const prefix = "e2e-concurrent-"
	t.Cleanup(func() {
		if leftover := committed(t, prefix); len(leftover) > 0 {
			_ = e2eRoot(t).ApplyDNSRecords(ctx, leftover, nil)
		}
	})

	theirs := DNSRecord{Type: RecordTypeA, Name: prefix + "theirs.lan", Value: "10.9.7.1", Owner: e2eOwner}
	mine := DNSRecord{Type: RecordTypeA, Name: prefix + "mine.lan", Value: "10.9.7.2", Owner: e2eOwner}

	o := e2eProvider(t, os.Getenv("OPENWRT_E2E_USERNAME"), os.Getenv("OPENWRT_E2E_PASSWORD")).(*openWRT)
	o.ubus = &interleaved{Client: o.ubus, before: func() {
		if err := e2eRoot(t).ApplyDNSRecords(ctx, nil, []DNSRecord{theirs}); err != nil {
			t.Error(err)
		}
	}}
	if err := o.ApplyDNSRecords(ctx, nil, []DNSRecord{mine}); err != nil {
		t.Fatal(err)
	}

	if got, want := committed(t, prefix), sorted([]DNSRecord{theirs, mine}); !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
