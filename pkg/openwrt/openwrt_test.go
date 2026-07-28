package openwrt

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	mocks "github.com/VizzleTF/external-dns-openwrt-webhook/internal/mocks/lucirpc"
	"github.com/VizzleTF/external-dns-openwrt-webhook/pkg/logger"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"
)

func TestOpenWRT(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "OpenWRT Suite")
	defer GinkgoRecover()
}

var _ = BeforeSuite(func() {
	if err := logger.Init(&logger.Config{
		Level:    "debug",
		Encoding: "console",
	}); err != nil {
		panic(err)
	}
})

var _ = AfterSuite(func() {
	_ = logger.Log.Sync()
})

var _ = Describe("OpenWRT", func() {
	var (
		ctx         context.Context
		mockCtrl    *gomock.Controller
		mockLuciRPC *mocks.MockLuciRPC
	)

	BeforeEach(func() {
		ctx = context.Background()
		mockCtrl = gomock.NewController(GinkgoT())
		mockLuciRPC = mocks.NewMockLuciRPC(mockCtrl)
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	// newOpenWRT builds the unit under test with the narrow reload strategy.
	newOpenWRT := func() *openWRT {
		return &openWRT{
			lucirpc:        mockLuciRPC,
			reloadStrategy: ReloadStrategyDnsmasq,
		}
	}

	// expectGetAll stubs `uci get_all dhcp`. Tests describe the router state in
	// normalised terms (A/CNAME); the wire format uses the UCI section types,
	// so translate before marshalling. Anything else is passed through as-is.
	expectGetAll := func(records map[string]DNSRecord) {
		raw := make(map[string]DNSRecord, len(records))
		for section, record := range records {
			switch record.Type {
			case RecordTypeA:
				raw[section] = DNSRecord{Type: sectionTypeDomain, Name: record.Name, IP: record.IP}
			case RecordTypeCNAME:
				raw[section] = DNSRecord{Type: sectionTypeCName, CName: record.CName, Target: record.Target}
			default:
				raw[section] = record
			}
		}

		payload, err := json.Marshal(raw)
		Expect(err).To(BeNil())
		mockLuciRPC.EXPECT().Uci(ctx, "get_all", []string{"dhcp"}).Return(string(payload), nil)
	}

	expectCommitAndReload := func() {
		mockLuciRPC.EXPECT().Uci(ctx, "commit", []string{"dhcp"}).Return("", nil)
		mockLuciRPC.EXPECT().Sys(ctx, "call", []string{dnsmasqReloadCommand}).Return("0", nil)
	}

	Context("Get DNS", func() {
		It("normalises section types and drops everything else", func() {
			expectGetAll(map[string]DNSRecord{
				"x": {Type: "domain", Name: "foobar", IP: "1.1.1.1"},
				"y": {Type: "cname", CName: "foobar", Target: "bar.foo.com"},
				"z": {Type: "whatever"},
			})

			resultDNS, err := newOpenWRT().GetDNSRecords(ctx)
			Expect(err).To(BeNil())
			Expect(resultDNS).To(Equal(map[string]DNSRecord{
				"x": {Type: RecordTypeA, Name: "foobar", IP: "1.1.1.1"},
				"y": {Type: RecordTypeCNAME, CName: "foobar", Target: "bar.foo.com"},
			}))
		})
	})

	Context("Add", func() {
		It("adds an A record, then commits and reloads once", func() {
			cfg := "cfg01"
			expectGetAll(map[string]DNSRecord{})
			mockLuciRPC.EXPECT().Uci(ctx, "add", []string{"dhcp", "domain"}).Return(cfg, nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{"dhcp", cfg, "name", "foo.bar.com"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{"dhcp", cfg, "ip", "1.1.1.1"}).Return("", nil)
			expectCommitAndReload()

			err := newOpenWRT().ApplyDNSRecords(ctx, nil, []DNSRecord{
				{Type: RecordTypeA, Name: "foo.bar.com", IP: "1.1.1.1"},
			})
			Expect(err).To(BeNil())
		})

		It("adds a CNAME record", func() {
			cfg := "cfg02"
			expectGetAll(map[string]DNSRecord{})
			mockLuciRPC.EXPECT().Uci(ctx, "add", []string{"dhcp", "cname"}).Return(cfg, nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{"dhcp", cfg, "cname", "foo.bar.com"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{"dhcp", cfg, "target", "bar.foo.com"}).Return("", nil)
			expectCommitAndReload()

			err := newOpenWRT().ApplyDNSRecords(ctx, nil, []DNSRecord{
				{Type: RecordTypeCNAME, CName: "foo.bar.com", Target: "bar.foo.com"},
			})
			Expect(err).To(BeNil())
		})

		It("is idempotent when the record is already there", func() {
			expectGetAll(map[string]DNSRecord{
				"x": {Type: RecordTypeA, Name: "foo.bar.com", IP: "1.1.1.1"},
			})
			// No add, no commit, no reload.

			err := newOpenWRT().ApplyDNSRecords(ctx, nil, []DNSRecord{
				{Type: RecordTypeA, Name: "foo.bar.com", IP: "1.1.1.1"},
			})
			Expect(err).To(BeNil())
		})

		It("rejects an A record without an ip", func() {
			expectGetAll(map[string]DNSRecord{})

			err := newOpenWRT().ApplyDNSRecords(ctx, nil, []DNSRecord{
				{Type: RecordTypeA, Name: "foo.bar.com"},
			})
			Expect(err).ToNot(BeNil())
			Expect(err.Error()).To(ContainSubstring("ip is required"))
		})

		It("rejects a CNAME record without a target", func() {
			expectGetAll(map[string]DNSRecord{})

			err := newOpenWRT().ApplyDNSRecords(ctx, nil, []DNSRecord{
				{Type: RecordTypeCNAME, CName: "foo.bar.com"},
			})
			Expect(err).ToNot(BeNil())
			Expect(err.Error()).To(ContainSubstring("target is required"))
		})
	})

	Context("Delete", func() {
		It("deletes the matching section", func() {
			expectGetAll(map[string]DNSRecord{
				"x": {Type: RecordTypeA, Name: "foo.bar.com", IP: "1.1.1.1"},
			})
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{"dhcp", "x"}).Return("", nil)
			expectCommitAndReload()

			err := newOpenWRT().ApplyDNSRecords(ctx, []DNSRecord{
				{Type: RecordTypeA, Name: "foo.bar.com", IP: "1.1.1.1"},
			}, nil)
			Expect(err).To(BeNil())
		})

		It("treats an already absent record as success and skips the commit", func() {
			expectGetAll(map[string]DNSRecord{})
			// Upstream returned "records not found" here, which made
			// ExternalDNS retry the whole change set forever.

			err := newOpenWRT().ApplyDNSRecords(ctx, []DNSRecord{
				{Type: RecordTypeA, Name: "gone.bar.com", IP: "1.1.1.1"},
			}, nil)
			Expect(err).To(BeNil())
		})

		It("deletes every requested record, not just the first", func() {
			// Regression: the upstream loop mutated the slice it was ranging
			// over, so records after a removal were skipped.
			expectGetAll(map[string]DNSRecord{
				"a": {Type: RecordTypeA, Name: "one.bar.com", IP: "1.1.1.1"},
				"b": {Type: RecordTypeA, Name: "two.bar.com", IP: "2.2.2.2"},
				"c": {Type: RecordTypeA, Name: "three.bar.com", IP: "3.3.3.3"},
			})
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{"dhcp", "a"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{"dhcp", "b"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{"dhcp", "c"}).Return("", nil)
			expectCommitAndReload()

			err := newOpenWRT().ApplyDNSRecords(ctx, []DNSRecord{
				{Type: RecordTypeA, Name: "one.bar.com", IP: "1.1.1.1"},
				{Type: RecordTypeA, Name: "two.bar.com", IP: "2.2.2.2"},
				{Type: RecordTypeA, Name: "three.bar.com", IP: "3.3.3.3"},
			}, nil)
			Expect(err).To(BeNil())
		})

		It("deletes only the target that was asked for on a multi-target name", func() {
			// Regression: matching on the name alone deleted whichever section
			// the random map iteration happened to hit first.
			expectGetAll(map[string]DNSRecord{
				"keep": {Type: RecordTypeA, Name: "multi.bar.com", IP: "1.1.1.1"},
				"drop": {Type: RecordTypeA, Name: "multi.bar.com", IP: "2.2.2.2"},
			})
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{"dhcp", "drop"}).Return("", nil)
			expectCommitAndReload()

			err := newOpenWRT().ApplyDNSRecords(ctx, []DNSRecord{
				{Type: RecordTypeA, Name: "multi.bar.com", IP: "2.2.2.2"},
			}, nil)
			Expect(err).To(BeNil())
		})

		It("propagates a delete failure", func() {
			expectGetAll(map[string]DNSRecord{
				"x": {Type: RecordTypeA, Name: "foo.bar.com", IP: "1.1.1.1"},
			})
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{"dhcp", "x"}).Return("", errors.New("boom"))

			err := newOpenWRT().ApplyDNSRecords(ctx, []DNSRecord{
				{Type: RecordTypeA, Name: "foo.bar.com", IP: "1.1.1.1"},
			}, nil)
			Expect(err).ToNot(BeNil())
			Expect(err.Error()).To(ContainSubstring("boom"))
		})
	})

	Context("Update", func() {
		It("removes the old target and adds the new one in a single commit", func() {
			cfg := "cfg03"
			expectGetAll(map[string]DNSRecord{
				"x": {Type: RecordTypeA, Name: "foo.bar.com", IP: "1.1.1.1"},
			})
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{"dhcp", "x"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "add", []string{"dhcp", "domain"}).Return(cfg, nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{"dhcp", cfg, "name", "foo.bar.com"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{"dhcp", cfg, "ip", "9.9.9.9"}).Return("", nil)
			expectCommitAndReload()

			err := newOpenWRT().ApplyDNSRecords(ctx,
				[]DNSRecord{{Type: RecordTypeA, Name: "foo.bar.com", IP: "1.1.1.1"}},
				[]DNSRecord{{Type: RecordTypeA, Name: "foo.bar.com", IP: "9.9.9.9"}},
			)
			Expect(err).To(BeNil())
		})
	})

	Context("Reload strategies", func() {
		It("skips the reload entirely when disabled", func() {
			expectGetAll(map[string]DNSRecord{
				"x": {Type: RecordTypeA, Name: "foo.bar.com", IP: "1.1.1.1"},
			})
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{"dhcp", "x"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "commit", []string{"dhcp"}).Return("", nil)

			o := &openWRT{lucirpc: mockLuciRPC, reloadStrategy: ReloadStrategyNone}
			err := o.ApplyDNSRecords(ctx, []DNSRecord{
				{Type: RecordTypeA, Name: "foo.bar.com", IP: "1.1.1.1"},
			}, nil)
			Expect(err).To(BeNil())
		})

		It("calls uci apply with NO arguments so rollback is not armed", func() {
			expectGetAll(map[string]DNSRecord{
				"x": {Type: RecordTypeA, Name: "foo.bar.com", IP: "1.1.1.1"},
			})
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{"dhcp", "x"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "commit", []string{"dhcp"}).Return("", nil)
			// Passing a config name here would be read as rollback=true and
			// the change would revert itself after ~90s.
			mockLuciRPC.EXPECT().Uci(ctx, "apply", []string{}).Return("", nil)

			o := &openWRT{lucirpc: mockLuciRPC, reloadStrategy: ReloadStrategyUciApply}
			err := o.ApplyDNSRecords(ctx, []DNSRecord{
				{Type: RecordTypeA, Name: "foo.bar.com", IP: "1.1.1.1"},
			}, nil)
			Expect(err).To(BeNil())
		})

		It("reports a failed reload", func() {
			expectGetAll(map[string]DNSRecord{
				"x": {Type: RecordTypeA, Name: "foo.bar.com", IP: "1.1.1.1"},
			})
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{"dhcp", "x"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "commit", []string{"dhcp"}).Return("", nil)
			mockLuciRPC.EXPECT().Sys(ctx, "call", []string{dnsmasqReloadCommand}).Return("", errors.New("no acl"))

			err := newOpenWRT().ApplyDNSRecords(ctx, []DNSRecord{
				{Type: RecordTypeA, Name: "foo.bar.com", IP: "1.1.1.1"},
			}, nil)
			Expect(err).ToNot(BeNil())
			Expect(err.Error()).To(ContainSubstring("reload dnsmasq"))
		})
	})

	Context("No-op", func() {
		It("does not even read the router when there is nothing to do", func() {
			err := newOpenWRT().ApplyDNSRecords(ctx, nil, nil)
			Expect(err).To(BeNil())
		})
	})

	Context("Config", func() {
		It("accepts the known strategies and rejects anything else", func() {
			Expect(validateReloadStrategy(ReloadStrategyDnsmasq)).To(BeNil())
			Expect(validateReloadStrategy(ReloadStrategyUciApply)).To(BeNil())
			Expect(validateReloadStrategy(ReloadStrategyNone)).To(BeNil())
			Expect(validateReloadStrategy("nope")).ToNot(BeNil())
		})
	})
})
