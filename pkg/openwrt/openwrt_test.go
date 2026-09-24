package openwrt

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

const testOwner = "homelab"

// fakeRPC answers `uci get_all dhcp` with a fixed router state and every other
// call with success, and records each call as one line, e.g.
// "uci set dhcp cfg01 name foo.bar.com".
type fakeRPC struct {
	sections map[string]map[string]any
	// fail makes the call spelled exactly like this return an error.
	fail  map[string]error
	calls []string
}

func (f *fakeRPC) call(kind, method string, params []string) (string, error) {
	call := strings.Join(append([]string{kind, method}, params...), " ")
	f.calls = append(f.calls, call)
	if err := f.fail[call]; err != nil {
		return "", err
	}

	switch method {
	case "get_all":
		payload, err := json.Marshal(f.sections)
		return string(payload), err
	case "add":
		return "cfg01", nil
	}
	return "", nil
}

func (f *fakeRPC) Uci(_ context.Context, method string, params []string) (string, error) {
	return f.call("uci", method, params)
}

func (f *fakeRPC) Sys(_ context.Context, method string, params []string) (string, error) {
	return f.call("sys", method, params)
}

// section builds the raw shape `uci get_all dhcp` returns for one section.
// An empty owner means the section carries no ownership marker.
func section(sectionType, name, value, owner string) map[string]any {
	options := map[string]any{optionSectionType: sectionType}

	switch sectionType {
	case sectionTypeDomain:
		options[optionName] = name
		options[optionIP] = value
	case sectionTypeCName:
		options[optionCName] = name
		options[optionTarget] = value
	}

	if owner != "" {
		options[ownershipOption] = owner
	}

	return options
}

func domainSection(name, ip, owner string) map[string]any {
	return section(sectionTypeDomain, name, ip, owner)
}

func aRecord(name, ip string) DNSRecord {
	return DNSRecord{Type: RecordTypeA, Name: name, Value: ip}
}

// owning builds a provider scoped to its own records, with adoption on.
func owning(rpc *fakeRPC) *openWRT {
	return &openWRT{
		lucirpc:        rpc,
		log:            slog.New(slog.DiscardHandler),
		reloadStrategy: ReloadStrategyRestart,
		ownershipID:    testOwner,
	}
}

// unscoped reproduces the pre-ownership behaviour: every section is managed.
func unscoped(rpc *fakeRPC) *openWRT {
	o := owning(rpc)
	o.ownershipID = ""
	return o
}

const (
	getAll  = "uci get_all dhcp"
	commit  = "uci commit dhcp"
	restart = "sys call " + dnsmasqRestartCommand
)

func assertCalls(t *testing.T, rpc *fakeRPC, want ...string) {
	t.Helper()
	if !slices.Equal(rpc.calls, want) {
		t.Errorf("calls:\n got %q\nwant %q", rpc.calls, want)
	}
}

func TestReadingRecords(t *testing.T) {
	t.Run("normalises section types and ignores everything else", func(t *testing.T) {
		rpc := &fakeRPC{sections: map[string]map[string]any{
			"x": domainSection("foobar", "1.1.1.1", ""),
			"y": section(sectionTypeCName, "foobar", "bar.foo.com", ""),
			"z": {optionSectionType: "whatever"},
		}}

		records, err := unscoped(rpc).GetDNSRecords(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]DNSRecord{
			"x": {Type: RecordTypeA, Name: "foobar", Value: "1.1.1.1"},
			"y": {Type: RecordTypeCNAME, Name: "foobar", Value: "bar.foo.com"},
		}
		if len(records) != len(want) || records["x"] != want["x"] || records["y"] != want["y"] {
			t.Errorf("got %v, want %v", records, want)
		}
	})

	t.Run("skips a section whose name is a list rather than a single value", func(t *testing.T) {
		rpc := &fakeRPC{sections: map[string]map[string]any{
			"multi": {
				optionSectionType: sectionTypeDomain,
				optionName:        []any{"a.foo.com", "b.foo.com"},
				optionIP:          "1.1.1.1",
			},
		}}

		records, err := unscoped(rpc).GetDNSRecords(context.Background())
		if err != nil || len(records) != 0 {
			t.Errorf("got %v, %v", records, err)
		}
	})

	t.Run("returns only records carrying our marker", func(t *testing.T) {
		rpc := &fakeRPC{sections: map[string]map[string]any{
			"mine":    domainSection("mine.foo.com", "1.1.1.1", testOwner),
			"manual":  domainSection("manual.foo.com", "2.2.2.2", ""),
			"someone": domainSection("other.foo.com", "3.3.3.3", "other-instance"),
		}}

		records, err := owning(rpc).GetDNSRecords(context.Background())
		if err != nil || len(records) != 1 || records["mine"].Name != "mine.foo.com" {
			t.Errorf("got %v, %v", records, err)
		}
	})
}

func TestAdding(t *testing.T) {
	ctx := context.Background()
	stamp := "uci set dhcp cfg01 " + ownershipOption + " " + testOwner

	for _, tc := range []struct {
		name     string
		sections map[string]map[string]any
		provider func(*fakeRPC) *openWRT
		add      DNSRecord
		want     []string
	}{
		{
			name:     "stamps the marker on a record it creates",
			provider: owning,
			add:      aRecord("foo.bar.com", "1.1.1.1"),
			want: []string{getAll, "uci add dhcp domain",
				"uci set dhcp cfg01 name foo.bar.com", "uci set dhcp cfg01 ip 1.1.1.1", stamp, commit, restart},
		},
		{
			name:     "writes no marker when ownership is disabled",
			provider: unscoped,
			add:      aRecord("foo.bar.com", "1.1.1.1"),
			want: []string{getAll, "uci add dhcp domain",
				"uci set dhcp cfg01 name foo.bar.com", "uci set dhcp cfg01 ip 1.1.1.1", commit, restart},
		},
		{
			name:     "writes a CNAME section",
			provider: unscoped,
			add:      DNSRecord{Type: RecordTypeCNAME, Name: "Alias.bar.com", Value: "Target.bar.com."},
			want: []string{getAll, "uci add dhcp cname",
				"uci set dhcp cfg01 cname alias.bar.com", "uci set dhcp cfg01 target target.bar.com", commit, restart},
		},
		{
			// The migration path: records already on the router get stamped on
			// the first reconcile rather than added a second time.
			name:     "adopts an identical unowned section instead of duplicating it",
			sections: map[string]map[string]any{"existing": domainSection("foo.bar.com", "1.1.1.1", "")},
			provider: owning,
			add:      aRecord("foo.bar.com", "1.1.1.1"),
			want:     []string{getAll, "uci set dhcp existing " + ownershipOption + " " + testOwner, commit, restart},
		},
		{
			// ExternalDNS compares names canonically, dnsmasq answers them
			// case-insensitively, and UCI stores whatever was typed into LuCI.
			// Matching literally would add a duplicate for a name the router
			// already serves.
			name:     "adopts a section whose name differs only in case or trailing dot",
			sections: map[string]map[string]any{"existing": domainSection("FOO.bar.com.", "1.1.1.1", "")},
			provider: owning,
			add:      aRecord("foo.bar.com", "1.1.1.1"),
			want:     []string{getAll, "uci set dhcp existing " + ownershipOption + " " + testOwner, commit, restart},
		},
		{
			name:     "writes the canonical spelling of a name",
			provider: unscoped,
			add:      aRecord("FOO.Bar.com.", "1.1.1.1"),
			want: []string{getAll, "uci add dhcp domain",
				"uci set dhcp cfg01 name foo.bar.com", "uci set dhcp cfg01 ip 1.1.1.1", commit, restart},
		},
		{
			name:     "never adopts a section owned by another instance",
			sections: map[string]map[string]any{"theirs": domainSection("foo.bar.com", "1.1.1.1", "other-instance")},
			provider: owning,
			add:      aRecord("foo.bar.com", "1.1.1.1"),
			want: []string{getAll, "uci add dhcp domain",
				"uci set dhcp cfg01 name foo.bar.com", "uci set dhcp cfg01 ip 1.1.1.1", stamp, commit, restart},
		},
		{
			name:     "is a no-op when the record is already owned and present",
			sections: map[string]map[string]any{"mine": domainSection("foo.bar.com", "1.1.1.1", testOwner)},
			provider: owning,
			add:      aRecord("foo.bar.com", "1.1.1.1"),
			want:     []string{getAll},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rpc := &fakeRPC{sections: tc.sections}
			if err := tc.provider(rpc).ApplyDNSRecords(ctx, nil, []DNSRecord{tc.add}); err != nil {
				t.Fatal(err)
			}
			assertCalls(t, rpc, tc.want...)
		})
	}

	t.Run("rejects incomplete records", func(t *testing.T) {
		rpc := &fakeRPC{}
		err := owning(rpc).ApplyDNSRecords(ctx, nil, []DNSRecord{{Type: RecordTypeA, Name: "foo.bar.com"}})
		if err == nil || !strings.Contains(err.Error(), "value is required") {
			t.Errorf("got %v", err)
		}
		assertCalls(t, rpc, getAll)
	})
}

func TestDeleting(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		sections map[string]map[string]any
		remove   []DNSRecord
		want     []string
	}{
		{
			name:     "deletes a record it owns",
			sections: map[string]map[string]any{"mine": domainSection("foo.bar.com", "1.1.1.1", testOwner)},
			remove:   []DNSRecord{aRecord("foo.bar.com", "1.1.1.1")},
			want:     []string{getAll, "uci delete dhcp mine", commit, restart},
		},
		{
			// A name whose case or trailing dot changed between what was
			// written and what is asked for must still resolve to the same
			// section, or the record would be stranded on the router forever.
			name:     "deletes a record the change set spells differently",
			sections: map[string]map[string]any{"mine": domainSection("foo.bar.com", "1.1.1.1", testOwner)},
			remove:   []DNSRecord{aRecord("Foo.BAR.com.", "1.1.1.1")},
			want:     []string{getAll, "uci delete dhcp mine", commit, restart},
		},
		{
			// The whole point of ownership: policy=sync must not be able to
			// remove entries nobody handed to ExternalDNS. No delete, no
			// commit, no reload.
			name:     "refuses to delete a manually created record",
			sections: map[string]map[string]any{"manual": domainSection("s3.vaka.work", "10.11.12.237", "")},
			remove:   []DNSRecord{aRecord("s3.vaka.work", "10.11.12.237")},
			want:     []string{getAll},
		},
		{
			name:     "refuses to delete a record owned by another instance",
			sections: map[string]map[string]any{"theirs": domainSection("foo.bar.com", "1.1.1.1", "other-instance")},
			remove:   []DNSRecord{aRecord("foo.bar.com", "1.1.1.1")},
			want:     []string{getAll},
		},
		{
			name:   "treats an already absent record as success",
			remove: []DNSRecord{aRecord("gone.bar.com", "1.1.1.1")},
			want:   []string{getAll},
		},
		{
			name: "deletes every requested record, not just the first",
			sections: map[string]map[string]any{
				"a": domainSection("one.bar.com", "1.1.1.1", testOwner),
				"b": domainSection("two.bar.com", "2.2.2.2", testOwner),
				"c": domainSection("three.bar.com", "3.3.3.3", testOwner),
			},
			remove: []DNSRecord{
				aRecord("one.bar.com", "1.1.1.1"),
				aRecord("two.bar.com", "2.2.2.2"),
				aRecord("three.bar.com", "3.3.3.3"),
			},
			want: []string{getAll, "uci delete dhcp a", "uci delete dhcp b", "uci delete dhcp c", commit, restart},
		},
		{
			name: "deletes only the target it was asked for on a multi-target name",
			sections: map[string]map[string]any{
				"keep": domainSection("multi.bar.com", "1.1.1.1", testOwner),
				"drop": domainSection("multi.bar.com", "2.2.2.2", testOwner),
			},
			remove: []DNSRecord{aRecord("multi.bar.com", "2.2.2.2")},
			want:   []string{getAll, "uci delete dhcp drop", commit, restart},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rpc := &fakeRPC{sections: tc.sections}
			if err := owning(rpc).ApplyDNSRecords(ctx, tc.remove, nil); err != nil {
				t.Fatal(err)
			}
			assertCalls(t, rpc, tc.want...)
		})
	}

	t.Run("propagates a delete failure", func(t *testing.T) {
		rpc := &fakeRPC{
			sections: map[string]map[string]any{"mine": domainSection("foo.bar.com", "1.1.1.1", testOwner)},
			fail:     map[string]error{"uci delete dhcp mine": errors.New("boom")},
		}
		err := owning(rpc).ApplyDNSRecords(ctx, []DNSRecord{aRecord("foo.bar.com", "1.1.1.1")}, nil)
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("got %v", err)
		}
		assertCalls(t, rpc, getAll, "uci delete dhcp mine")
	})
}

func TestUpdatingRemovesAndAddsInASingleCommit(t *testing.T) {
	rpc := &fakeRPC{sections: map[string]map[string]any{"mine": domainSection("foo.bar.com", "1.1.1.1", testOwner)}}

	err := owning(rpc).ApplyDNSRecords(context.Background(),
		[]DNSRecord{aRecord("foo.bar.com", "1.1.1.1")},
		[]DNSRecord{aRecord("foo.bar.com", "9.9.9.9")})
	if err != nil {
		t.Fatal(err)
	}
	assertCalls(t, rpc, getAll, "uci delete dhcp mine", "uci add dhcp domain",
		"uci set dhcp cfg01 name foo.bar.com", "uci set dhcp cfg01 ip 9.9.9.9",
		"uci set dhcp cfg01 "+ownershipOption+" "+testOwner, commit, restart)
}

func TestReloadStrategies(t *testing.T) {
	for _, tc := range []struct {
		strategy string
		want     []string
	}{
		{ReloadStrategyNone, nil},
		{ReloadStrategyRestart, []string{restart}},
		// A config name here would be read as rollback=true and the change
		// would revert itself after ~90s, so uci apply takes NO arguments.
		{ReloadStrategyUciApply, []string{"uci apply"}},
	} {
		t.Run(tc.strategy, func(t *testing.T) {
			rpc := &fakeRPC{sections: map[string]map[string]any{"mine": domainSection("foo.bar.com", "1.1.1.1", testOwner)}}
			o := owning(rpc)
			o.reloadStrategy = tc.strategy

			if err := o.ApplyDNSRecords(context.Background(), []DNSRecord{aRecord("foo.bar.com", "1.1.1.1")}, nil); err != nil {
				t.Fatal(err)
			}
			assertCalls(t, rpc, append([]string{getAll, "uci delete dhcp mine", commit}, tc.want...)...)
		})
	}

	t.Run("reports a failed reload", func(t *testing.T) {
		rpc := &fakeRPC{
			sections: map[string]map[string]any{"mine": domainSection("foo.bar.com", "1.1.1.1", testOwner)},
			fail:     map[string]error{restart: errors.New("no acl")},
		}
		err := owning(rpc).ApplyDNSRecords(context.Background(), []DNSRecord{aRecord("foo.bar.com", "1.1.1.1")}, nil)
		if err == nil || !strings.Contains(err.Error(), "restart dnsmasq") {
			t.Errorf("got %v", err)
		}
	})
}

func TestNothingToDoDoesNotEvenReadTheRouter(t *testing.T) {
	rpc := &fakeRPC{}
	if err := owning(rpc).ApplyDNSRecords(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, rpc)
}

func TestConfig(t *testing.T) {
	for _, strategy := range []string{ReloadStrategyRestart, ReloadStrategyUciApply, ReloadStrategyNone} {
		if err := validateReloadStrategy(strategy); err != nil {
			t.Errorf("%s: %v", strategy, err)
		}
	}
	// "reload" and its old name "dnsmasq" are gone: neither applied CNAMEs.
	for _, strategy := range []string{"reload", "dnsmasq", "nope"} {
		if validateReloadStrategy(strategy) == nil {
			t.Errorf("%s: accepted", strategy)
		}
	}

	cfg := DefaultConfig()
	if cfg.OwnershipID != "" || cfg.ReloadStrategy != ReloadStrategyRestart {
		t.Errorf("defaults: got %+v", cfg)
	}
}
