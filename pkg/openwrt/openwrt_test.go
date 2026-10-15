package openwrt

import (
	"bytes"
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

func cnameSection(alias, target, owner string) map[string]any {
	return section(sectionTypeCName, alias, target, owner)
}

func cnameRecord(alias, target string) DNSRecord {
	return DNSRecord{Type: RecordTypeCNAME, Name: alias, Value: target}
}

// warnings swaps the provider's logger for one that keeps warnings and above.
func warnings(o *openWRT) *bytes.Buffer {
	var buf bytes.Buffer
	o.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return &buf
}

func assertWarned(t *testing.T, logged *bytes.Buffer, want string) {
	t.Helper()
	if want == "" && logged.Len() > 0 || !strings.Contains(logged.String(), want) {
		t.Errorf("warnings: got %q, want %q", logged, want)
	}
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

	t.Run("reports names and CNAME targets canonically", func(t *testing.T) {
		rpc := &fakeRPC{sections: map[string]map[string]any{
			"x": domainSection("NAS.Lan.", "10.0.0.5", ""),
			"y": cnameSection("Alias.LAN.", "Real.LAN.", testOwner),
		}}

		records, err := unscoped(rpc).GetDNSRecords(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if records["x"] != aRecord("nas.lan", "10.0.0.5") ||
			records["y"] != (DNSRecord{Type: RecordTypeCNAME, Name: "alias.lan", Value: "real.lan", Owner: testOwner}) {
			t.Errorf("got %v", records)
		}
	})

	// As checked on OpenWrt 24.10 and 25.12: dnsmasq answers AAAA for the first
	// two and nothing for the third, which it logs as a bad address.
	t.Run("leaves out a domain section dnsmasq serves as AAAA", func(t *testing.T) {
		rpc := &fakeRPC{sections: map[string]map[string]any{
			"v6":     domainSection("nas.lan", "fd00::5", ""),
			"mapped": domainSection("nas.lan", "::ffff:10.0.0.7", ""),
			"bad":    domainSection("nas.lan", "010.0.0.1", ""),
			"a":      domainSection("nas.lan", "10.0.0.5", ""),
		}}

		records, err := unscoped(rpc).GetDNSRecords(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 2 || records["a"].Value != "10.0.0.5" || records["bad"].Value != "010.0.0.1" {
			t.Errorf("got %v", records)
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

// Every skip below is a case checked on OpenWrt 24.10 and 25.12: written, it
// keeps dnsmasq from starting.
func TestNeverWritingACNAMEThatStopsDnsmasq(t *testing.T) {
	const taken, loops = "its alias already has one", "would close a CNAME loop"
	stamp := "uci set dhcp cfg01 " + ownershipOption + " " + testOwner
	writes := func(alias, target string) []string {
		return []string{"uci add dhcp cname",
			"uci set dhcp cfg01 cname " + alias, "uci set dhcp cfg01 target " + target, stamp}
	}

	for _, tc := range []struct {
		name        string
		sections    map[string]map[string]any
		provider    func(*fakeRPC) *openWRT
		remove, add []DNSRecord
		want        []string
		warning     string
	}{
		{
			// A CNAME endpoint with two targets, or two endpoints for one name
			// with different set identifiers.
			name: "writes one of two CNAMEs for the same alias in a change set",
			add:  []DNSRecord{cnameRecord("a.lan", "t1.lan"), cnameRecord("A.lan.", "t2.lan")},
			want: slices.Concat([]string{getAll}, writes("a.lan", "t1.lan"), []string{commit, restart}),
			// The second one is skipped.
			warning: taken,
		},
		{
			name:     "skips an alias a manual section holds with another target",
			sections: map[string]map[string]any{"manual": cnameSection("a.lan", "other.lan", "")},
			add:      []DNSRecord{cnameRecord("a.lan", "t1.lan")},
			want:     []string{getAll},
			warning:  taken,
		},
		{
			// Also what is left behind after the ownership ID changes.
			name:     "skips an alias another instance holds, even with the same target",
			sections: map[string]map[string]any{"theirs": cnameSection("a.lan", "t1.lan", "other-instance")},
			add:      []DNSRecord{cnameRecord("a.lan", "t1.lan")},
			want:     []string{getAll},
			warning:  taken,
		},
		{
			name:     "skips an alias held with another target when ownership is disabled",
			sections: map[string]map[string]any{"manual": cnameSection("A.LAN", "other.lan", "")},
			provider: unscoped,
			add:      []DNSRecord{cnameRecord("a.lan", "t1.lan")},
			want:     []string{getAll},
			warning:  taken,
		},
		{
			name:     "still adopts an identical unowned CNAME",
			sections: map[string]map[string]any{"existing": cnameSection("a.lan", "T1.lan.", "")},
			add:      []DNSRecord{cnameRecord("a.lan", "t1.lan")},
			want:     []string{getAll, "uci set dhcp existing " + ownershipOption + " " + testOwner, commit, restart},
		},
		{
			// Removals run first, so the section being replaced is out of the
			// way by the time the new target is added.
			name:     "changes the target of its own CNAME",
			sections: map[string]map[string]any{"mine": cnameSection("a.lan", "t1.lan", testOwner)},
			remove:   []DNSRecord{cnameRecord("a.lan", "t1.lan")},
			add:      []DNSRecord{cnameRecord("a.lan", "t2.lan")},
			want:     slices.Concat([]string{getAll, "uci delete dhcp mine"}, writes("a.lan", "t2.lan"), []string{commit, restart}),
		},
		{
			name:    "skips a CNAME onto itself",
			add:     []DNSRecord{cnameRecord("a.lan", "A.lan.")},
			want:    []string{getAll},
			warning: loops,
		},
		{
			name: "skips a CNAME closing a loop through sections it does not own",
			sections: map[string]map[string]any{
				"theirs": cnameSection("b.lan", "C.lan", "other-instance"),
				"manual": cnameSection("c.lan", "a.lan", ""),
			},
			add:     []DNSRecord{cnameRecord("a.lan", "b.lan")},
			want:    []string{getAll},
			warning: loops,
		},
		{
			name:    "skips the second of two CNAMEs in a change set that would loop",
			add:     []DNSRecord{cnameRecord("a.lan", "b.lan"), cnameRecord("b.lan", "a.lan")},
			want:    slices.Concat([]string{getAll}, writes("a.lan", "b.lan"), []string{commit, restart}),
			warning: loops,
		},
		{
			name: "writes a CNAME onto a chain that does not loop",
			sections: map[string]map[string]any{
				"one": cnameSection("b.lan", "c.lan", ""),
				"two": cnameSection("d.lan", "c.lan", ""),
			},
			add:  []DNSRecord{cnameRecord("a.lan", "b.lan")},
			want: slices.Concat([]string{getAll}, writes("a.lan", "b.lan"), []string{commit, restart}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.provider == nil {
				tc.provider = owning
			}
			rpc := &fakeRPC{sections: tc.sections}
			o := tc.provider(rpc)
			logged := warnings(o)

			if err := o.ApplyDNSRecords(context.Background(), tc.remove, tc.add); err != nil {
				t.Fatal(err)
			}
			assertCalls(t, rpc, tc.want...)
			assertWarned(t, logged, tc.warning)
		})
	}
}

// Two A sections for one name make dnsmasq answer both addresses. Refusing to
// write would keep two instances from sharing a name, so it is only logged.
func TestAddingNextToAnAddressItDoesNotOwn(t *testing.T) {
	const answersBoth = "dnsmasq will answer both"

	for _, tc := range []struct {
		name     string
		provider func(*fakeRPC) *openWRT
		other    map[string]any
		warning  string
	}{
		{"warns about a manual section", owning, domainSection("NAS.lan", "10.0.0.5", ""), answersBoth},
		{"warns about another instance's section", owning, domainSection("nas.lan", "10.0.0.5", "other-instance"), answersBoth},
		{"does not warn about another instance's same address", owning, domainSection("nas.lan", "10.0.0.6", "other-instance"), ""},
		{"does not warn about its own other address", owning, domainSection("nas.lan", "10.0.0.5", testOwner), ""},
		// Every section is then reported, so the plan sees the other address.
		{"does not warn when ownership is disabled", unscoped, domainSection("nas.lan", "10.0.0.5", ""), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rpc := &fakeRPC{sections: map[string]map[string]any{"other": tc.other}}
			o := tc.provider(rpc)
			logged := warnings(o)

			if err := o.ApplyDNSRecords(context.Background(), nil, []DNSRecord{aRecord("nas.lan", "10.0.0.6")}); err != nil {
				t.Fatal(err)
			}
			want := []string{getAll, "uci add dhcp domain", "uci set dhcp cfg01 name nas.lan", "uci set dhcp cfg01 ip 10.0.0.6"}
			if o.ownershipEnabled() {
				want = append(want, "uci set dhcp cfg01 "+ownershipOption+" "+testOwner)
			}
			assertCalls(t, rpc, append(want, commit, restart)...)
			assertWarned(t, logged, tc.warning)
		})
	}
}

func TestValidate(t *testing.T) {
	label63 := strings.Repeat("a", 63)
	name253 := strings.Repeat(label63+".", 3) + strings.Repeat("a", 61)

	// Checked on the canonical form, which is what reaches the router.
	for _, record := range []DNSRecord{
		aRecord(" Foo.bar.com. ", "1.1.1.1"),
		{Type: RecordTypeCNAME, Name: "_alias.bar.com", Value: "Target.bar.com."},
		{Type: RecordTypeCNAME, Name: label63 + ".lan", Value: label63 + ".lan"},
		aRecord(name253+".", "1.1.1.1"),
	} {
		if err := record.Validate(); err != nil {
			t.Errorf("%q: %v", record, err)
		}
	}

	for _, char := range []string{"\n", "\r", "\t", "\x00", " ", ",", `"`, "#", "\x7f", "é"} {
		for _, record := range []DNSRecord{
			aRecord("a"+char+"b.lan", "1.1.1.1"),
			aRecord("foo.lan", "1.1.1.1"+char+"2"),
			{Type: RecordTypeCNAME, Name: "a" + char + "b.lan", Value: "target.lan"},
			{Type: RecordTypeCNAME, Name: "alias.lan", Value: "a" + char + "b.lan"},
		} {
			if err := record.Validate(); err == nil || !strings.Contains(err.Error(), "dnsmasq cannot take") {
				t.Errorf("%q: got %v", record, err)
			}
		}
	}

	for _, record := range []DNSRecord{
		{Type: RecordTypeCNAME, Name: label63 + "a.lan", Value: "target.lan"},
		{Type: RecordTypeCNAME, Name: "alias.lan", Value: label63 + "a.lan"},
		aRecord(name253+"a", "1.1.1.1"),
	} {
		if err := record.Validate(); err == nil || !strings.Contains(err.Error(), "longer than DNS allows") {
			t.Errorf("%q: got %v", record, err)
		}
	}

	// dnsmasq answers AAAA for the first two and nothing for the rest.
	for _, ip := range []string{"fd00::5", "::ffff:10.0.0.7", "010.0.0.1", "1.2.3", "nas.lan"} {
		if err := aRecord("foo.lan", ip).Validate(); err == nil || !strings.Contains(err.Error(), "not an IPv4 address") {
			t.Errorf("%q: got %v", ip, err)
		}
	}
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
		{
			// Validation guards writes only: a section written before it existed
			// is exactly the one that has to go.
			name: "deletes a record that would no longer pass validation",
			sections: map[string]map[string]any{
				"poisoned": section(sectionTypeCName, "foo.bar.com", "x.lan\naddress=/bank.com/6.6.6.6", testOwner),
			},
			remove: []DNSRecord{{Type: RecordTypeCNAME, Name: "foo.bar.com", Value: "x.lan\naddress=/bank.com/6.6.6.6"}},
			want:   []string{getAll, "uci delete dhcp poisoned", commit, restart},
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

// With ownership off, a section typed into LuCI for an IPv6 address used to be
// reported as an A record, and the cluster publishing an A record for the same
// name planned it away.
func TestLeavingAnIPv6DomainSectionAlone(t *testing.T) {
	ctx := context.Background()
	rpc := &fakeRPC{sections: map[string]map[string]any{"manual": domainSection("nas.lan", "fd00::5", "")}}
	o := unscoped(rpc)

	records, err := o.GetDNSRecords(ctx)
	if err != nil || len(records) != 0 {
		t.Errorf("got %v, %v", records, err)
	}

	// The update the plan used to ask for: out with the old value, in with the
	// cluster's.
	if err := o.ApplyDNSRecords(ctx, []DNSRecord{aRecord("nas.lan", "fd00::5")}, []DNSRecord{aRecord("nas.lan", "10.0.0.5")}); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, rpc, getAll, getAll, "uci add dhcp domain",
		"uci set dhcp cfg01 name nas.lan", "uci set dhcp cfg01 ip 10.0.0.5", commit, restart)
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
