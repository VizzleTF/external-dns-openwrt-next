package openwrt

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	mocks "github.com/VizzleTF/external-dns-openwrt-next/internal/mocks/lucirpc"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/logger"
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

const testOwner = "homelab"

// section builds the raw shape `uci get_all dhcp` returns for one section.
// An empty owner means the section carries no ownership marker.
func section(sectionType, first, second, owner string) map[string]any {
	options := map[string]any{optionSectionType: sectionType}

	switch sectionType {
	case sectionTypeDomain:
		options[optionName] = first
		options[optionIP] = second
	case sectionTypeCName:
		options[optionCName] = first
		options[optionTarget] = second
	}

	if owner != "" {
		options[DefaultOwnershipOption] = owner
	}

	return options
}

func domainSection(name, ip, owner string) map[string]any {
	return section(sectionTypeDomain, name, ip, owner)
}

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

	// owning builds a provider scoped to its own records, with adoption on.
	owning := func() *openWRT {
		return &openWRT{
			lucirpc:         mockLuciRPC,
			reloadStrategy:  ReloadStrategyDnsmasq,
			ownershipID:     testOwner,
			ownershipOption: DefaultOwnershipOption,
			adoptExisting:   true,
		}
	}

	// unscoped reproduces the pre-ownership behaviour: every section is managed.
	unscoped := func() *openWRT {
		return &openWRT{
			lucirpc:         mockLuciRPC,
			reloadStrategy:  ReloadStrategyDnsmasq,
			ownershipOption: DefaultOwnershipOption,
		}
	}

	expectGetAll := func(sections map[string]map[string]any) {
		payload, err := json.Marshal(sections)
		Expect(err).To(BeNil())
		mockLuciRPC.EXPECT().Uci(ctx, "get_all", []string{uciConfig}).Return(string(payload), nil)
	}

	expectCommitAndReload := func() {
		mockLuciRPC.EXPECT().Uci(ctx, "commit", []string{uciConfig}).Return("", nil)
		mockLuciRPC.EXPECT().Sys(ctx, "call", []string{dnsmasqReloadCommand}).Return("0", nil)
	}

	aRecord := func(name, ip string) DNSRecord {
		return DNSRecord{Type: RecordTypeA, Name: name, IP: ip}
	}

	Context("reading records", func() {
		It("normalises section types and ignores everything else", func() {
			expectGetAll(map[string]map[string]any{
				"x": domainSection("foobar", "1.1.1.1", ""),
				"y": section(sectionTypeCName, "foobar", "bar.foo.com", ""),
				"z": {optionSectionType: "whatever"},
			})

			records, err := unscoped().GetDNSRecords(ctx)
			Expect(err).To(BeNil())
			Expect(records).To(Equal(map[string]DNSRecord{
				"x": {Type: RecordTypeA, Name: "foobar", IP: "1.1.1.1"},
				"y": {Type: RecordTypeCNAME, CName: "foobar", Target: "bar.foo.com"},
			}))
		})

		It("skips a section whose name is a list rather than a single value", func() {
			expectGetAll(map[string]map[string]any{
				"multi": {
					optionSectionType: sectionTypeDomain,
					optionName:        []any{"a.foo.com", "b.foo.com"},
					optionIP:          "1.1.1.1",
				},
			})

			records, err := unscoped().GetDNSRecords(ctx)
			Expect(err).To(BeNil())
			Expect(records).To(BeEmpty())
		})

		It("returns only records carrying our marker", func() {
			expectGetAll(map[string]map[string]any{
				"mine":    domainSection("mine.foo.com", "1.1.1.1", testOwner),
				"manual":  domainSection("manual.foo.com", "2.2.2.2", ""),
				"someone": domainSection("other.foo.com", "3.3.3.3", "other-instance"),
			})

			records, err := owning().GetDNSRecords(ctx)
			Expect(err).To(BeNil())
			Expect(records).To(HaveLen(1))
			Expect(records["mine"].Name).To(Equal("mine.foo.com"))
		})
	})

	Context("adding", func() {
		It("stamps the marker on a record it creates", func() {
			cfg := "cfg01"
			expectGetAll(map[string]map[string]any{})
			mockLuciRPC.EXPECT().Uci(ctx, "add", []string{uciConfig, sectionTypeDomain}).Return(cfg, nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{uciConfig, cfg, optionName, "foo.bar.com"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{uciConfig, cfg, optionIP, "1.1.1.1"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{uciConfig, cfg, DefaultOwnershipOption, testOwner}).Return("", nil)
			expectCommitAndReload()

			Expect(owning().ApplyDNSRecords(ctx, nil, []DNSRecord{aRecord("foo.bar.com", "1.1.1.1")})).To(BeNil())
		})

		It("writes no marker when ownership is disabled", func() {
			cfg := "cfg02"
			expectGetAll(map[string]map[string]any{})
			mockLuciRPC.EXPECT().Uci(ctx, "add", []string{uciConfig, sectionTypeDomain}).Return(cfg, nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{uciConfig, cfg, optionName, "foo.bar.com"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{uciConfig, cfg, optionIP, "1.1.1.1"}).Return("", nil)
			expectCommitAndReload()

			Expect(unscoped().ApplyDNSRecords(ctx, nil, []DNSRecord{aRecord("foo.bar.com", "1.1.1.1")})).To(BeNil())
		})

		It("adopts an identical unowned section instead of duplicating it", func() {
			// The migration path: records already on the router get stamped on
			// the first reconcile rather than added a second time.
			expectGetAll(map[string]map[string]any{
				"existing": domainSection("foo.bar.com", "1.1.1.1", ""),
			})
			mockLuciRPC.EXPECT().Uci(ctx, "set",
				[]string{uciConfig, "existing", DefaultOwnershipOption, testOwner}).Return("", nil)
			expectCommitAndReload()

			Expect(owning().ApplyDNSRecords(ctx, nil, []DNSRecord{aRecord("foo.bar.com", "1.1.1.1")})).To(BeNil())
		})

		It("does not adopt when adoption is switched off", func() {
			cfg := "cfg03"
			expectGetAll(map[string]map[string]any{
				"existing": domainSection("foo.bar.com", "1.1.1.1", ""),
			})
			mockLuciRPC.EXPECT().Uci(ctx, "add", []string{uciConfig, sectionTypeDomain}).Return(cfg, nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{uciConfig, cfg, optionName, "foo.bar.com"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{uciConfig, cfg, optionIP, "1.1.1.1"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{uciConfig, cfg, DefaultOwnershipOption, testOwner}).Return("", nil)
			expectCommitAndReload()

			o := owning()
			o.adoptExisting = false
			Expect(o.ApplyDNSRecords(ctx, nil, []DNSRecord{aRecord("foo.bar.com", "1.1.1.1")})).To(BeNil())
		})

		It("never adopts a section owned by another instance", func() {
			cfg := "cfg04"
			expectGetAll(map[string]map[string]any{
				"theirs": domainSection("foo.bar.com", "1.1.1.1", "other-instance"),
			})
			mockLuciRPC.EXPECT().Uci(ctx, "add", []string{uciConfig, sectionTypeDomain}).Return(cfg, nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{uciConfig, cfg, optionName, "foo.bar.com"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{uciConfig, cfg, optionIP, "1.1.1.1"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{uciConfig, cfg, DefaultOwnershipOption, testOwner}).Return("", nil)
			expectCommitAndReload()

			Expect(owning().ApplyDNSRecords(ctx, nil, []DNSRecord{aRecord("foo.bar.com", "1.1.1.1")})).To(BeNil())
		})

		It("is a no-op when the record is already owned and present", func() {
			expectGetAll(map[string]map[string]any{
				"mine": domainSection("foo.bar.com", "1.1.1.1", testOwner),
			})

			Expect(owning().ApplyDNSRecords(ctx, nil, []DNSRecord{aRecord("foo.bar.com", "1.1.1.1")})).To(BeNil())
		})

		It("rejects incomplete records", func() {
			expectGetAll(map[string]map[string]any{})
			err := owning().ApplyDNSRecords(ctx, nil, []DNSRecord{{Type: RecordTypeA, Name: "foo.bar.com"}})
			Expect(err).ToNot(BeNil())
			Expect(err.Error()).To(ContainSubstring("ip is required"))
		})
	})

	Context("deleting", func() {
		It("deletes a record it owns", func() {
			expectGetAll(map[string]map[string]any{
				"mine": domainSection("foo.bar.com", "1.1.1.1", testOwner),
			})
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{uciConfig, "mine"}).Return("", nil)
			expectCommitAndReload()

			Expect(owning().ApplyDNSRecords(ctx, []DNSRecord{aRecord("foo.bar.com", "1.1.1.1")}, nil)).To(BeNil())
		})

		It("refuses to delete a manually created record", func() {
			// The whole point of ownership: policy=sync must not be able to
			// remove entries nobody handed to ExternalDNS.
			expectGetAll(map[string]map[string]any{
				"manual": domainSection("s3.vaka.work", "10.11.12.237", ""),
			})
			// No delete, no commit, no reload.

			Expect(owning().ApplyDNSRecords(ctx,
				[]DNSRecord{aRecord("s3.vaka.work", "10.11.12.237")}, nil)).To(BeNil())
		})

		It("refuses to delete a record owned by another instance", func() {
			expectGetAll(map[string]map[string]any{
				"theirs": domainSection("foo.bar.com", "1.1.1.1", "other-instance"),
			})

			Expect(owning().ApplyDNSRecords(ctx,
				[]DNSRecord{aRecord("foo.bar.com", "1.1.1.1")}, nil)).To(BeNil())
		})

		It("treats an already absent record as success", func() {
			expectGetAll(map[string]map[string]any{})

			Expect(owning().ApplyDNSRecords(ctx,
				[]DNSRecord{aRecord("gone.bar.com", "1.1.1.1")}, nil)).To(BeNil())
		})

		It("deletes every requested record, not just the first", func() {
			expectGetAll(map[string]map[string]any{
				"a": domainSection("one.bar.com", "1.1.1.1", testOwner),
				"b": domainSection("two.bar.com", "2.2.2.2", testOwner),
				"c": domainSection("three.bar.com", "3.3.3.3", testOwner),
			})
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{uciConfig, "a"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{uciConfig, "b"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{uciConfig, "c"}).Return("", nil)
			expectCommitAndReload()

			Expect(owning().ApplyDNSRecords(ctx, []DNSRecord{
				aRecord("one.bar.com", "1.1.1.1"),
				aRecord("two.bar.com", "2.2.2.2"),
				aRecord("three.bar.com", "3.3.3.3"),
			}, nil)).To(BeNil())
		})

		It("deletes only the target it was asked for on a multi-target name", func() {
			expectGetAll(map[string]map[string]any{
				"keep": domainSection("multi.bar.com", "1.1.1.1", testOwner),
				"drop": domainSection("multi.bar.com", "2.2.2.2", testOwner),
			})
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{uciConfig, "drop"}).Return("", nil)
			expectCommitAndReload()

			Expect(owning().ApplyDNSRecords(ctx,
				[]DNSRecord{aRecord("multi.bar.com", "2.2.2.2")}, nil)).To(BeNil())
		})

		It("propagates a delete failure", func() {
			expectGetAll(map[string]map[string]any{
				"mine": domainSection("foo.bar.com", "1.1.1.1", testOwner),
			})
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{uciConfig, "mine"}).Return("", errors.New("boom"))

			err := owning().ApplyDNSRecords(ctx, []DNSRecord{aRecord("foo.bar.com", "1.1.1.1")}, nil)
			Expect(err).ToNot(BeNil())
			Expect(err.Error()).To(ContainSubstring("boom"))
		})
	})

	Context("updating", func() {
		It("removes the old target and adds the new one in a single commit", func() {
			cfg := "cfg05"
			expectGetAll(map[string]map[string]any{
				"mine": domainSection("foo.bar.com", "1.1.1.1", testOwner),
			})
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{uciConfig, "mine"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "add", []string{uciConfig, sectionTypeDomain}).Return(cfg, nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{uciConfig, cfg, optionName, "foo.bar.com"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{uciConfig, cfg, optionIP, "9.9.9.9"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "set", []string{uciConfig, cfg, DefaultOwnershipOption, testOwner}).Return("", nil)
			expectCommitAndReload()

			Expect(owning().ApplyDNSRecords(ctx,
				[]DNSRecord{aRecord("foo.bar.com", "1.1.1.1")},
				[]DNSRecord{aRecord("foo.bar.com", "9.9.9.9")},
			)).To(BeNil())
		})
	})

	Context("reload strategies", func() {
		It("skips the reload entirely when disabled", func() {
			expectGetAll(map[string]map[string]any{
				"mine": domainSection("foo.bar.com", "1.1.1.1", testOwner),
			})
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{uciConfig, "mine"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "commit", []string{uciConfig}).Return("", nil)

			o := owning()
			o.reloadStrategy = ReloadStrategyNone
			Expect(o.ApplyDNSRecords(ctx, []DNSRecord{aRecord("foo.bar.com", "1.1.1.1")}, nil)).To(BeNil())
		})

		It("calls uci apply with NO arguments so rollback is not armed", func() {
			expectGetAll(map[string]map[string]any{
				"mine": domainSection("foo.bar.com", "1.1.1.1", testOwner),
			})
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{uciConfig, "mine"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "commit", []string{uciConfig}).Return("", nil)
			// A config name here would be read as rollback=true and the change
			// would revert itself after ~90s.
			mockLuciRPC.EXPECT().Uci(ctx, "apply", []string{}).Return("", nil)

			o := owning()
			o.reloadStrategy = ReloadStrategyUciApply
			Expect(o.ApplyDNSRecords(ctx, []DNSRecord{aRecord("foo.bar.com", "1.1.1.1")}, nil)).To(BeNil())
		})

		It("reports a failed reload", func() {
			expectGetAll(map[string]map[string]any{
				"mine": domainSection("foo.bar.com", "1.1.1.1", testOwner),
			})
			mockLuciRPC.EXPECT().Uci(ctx, "delete", []string{uciConfig, "mine"}).Return("", nil)
			mockLuciRPC.EXPECT().Uci(ctx, "commit", []string{uciConfig}).Return("", nil)
			mockLuciRPC.EXPECT().Sys(ctx, "call", []string{dnsmasqReloadCommand}).Return("", errors.New("no acl"))

			err := owning().ApplyDNSRecords(ctx, []DNSRecord{aRecord("foo.bar.com", "1.1.1.1")}, nil)
			Expect(err).ToNot(BeNil())
			Expect(err.Error()).To(ContainSubstring("reload dnsmasq"))
		})
	})

	Context("no-op", func() {
		It("does not even read the router when there is nothing to do", func() {
			Expect(owning().ApplyDNSRecords(ctx, nil, nil)).To(BeNil())
		})
	})

	Context("config", func() {
		It("accepts the known reload strategies and rejects anything else", func() {
			Expect(validateReloadStrategy(ReloadStrategyDnsmasq)).To(BeNil())
			Expect(validateReloadStrategy(ReloadStrategyUciApply)).To(BeNil())
			Expect(validateReloadStrategy(ReloadStrategyNone)).To(BeNil())
			Expect(validateReloadStrategy("nope")).ToNot(BeNil())
		})

		It("reports whether ownership is enabled", func() {
			Expect((&Config{}).OwnershipEnabled()).To(BeFalse())
			Expect((&Config{OwnershipID: testOwner}).OwnershipEnabled()).To(BeTrue())
			Expect(DefaultConfig().OwnershipEnabled()).To(BeFalse())
			Expect(DefaultConfig().OwnershipOption).To(Equal(DefaultOwnershipOption))
			Expect(DefaultConfig().AdoptExisting).To(BeTrue())
		})
	})
})
