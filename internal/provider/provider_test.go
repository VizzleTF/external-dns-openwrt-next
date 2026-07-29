package provider

import (
	"context"
	"testing"

	mocks "github.com/VizzleTF/external-dns-openwrt-next/internal/mocks/openwrt"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/logger"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/openwrt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
)

func TestProvider(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Provider Suite")
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

var _ = Describe("Provider Suite", func() {
	var (
		ctx         context.Context
		mockCtrl    *gomock.Controller
		mockOpenWRT *mocks.MockOpenWRT
	)

	BeforeEach(func() {
		ctx = context.Background()
		mockCtrl = gomock.NewController(GinkgoT())
		mockOpenWRT = mocks.NewMockOpenWRT(mockCtrl)
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	Context("endpoints to dns records", func() {
		It("converts A and CNAME", func() {
			dnsRecords := endpoints2DNSRecords([]*endpoint.Endpoint{
				{DNSName: "a.foobar.com", RecordType: endpoint.RecordTypeA, Targets: []string{"1.1.1.1"}},
				{DNSName: "b.foobar.com", RecordType: endpoint.RecordTypeCNAME, Targets: []string{"c.foobar.com"}},
			})

			Expect(dnsRecords).To(Equal([]openwrt.DNSRecord{
				{Type: openwrt.RecordTypeA, Name: "a.foobar.com", IP: "1.1.1.1"},
				{Type: openwrt.RecordTypeCNAME, CName: "b.foobar.com", Target: "c.foobar.com"},
			}))
		})

		It("emits one record per target", func() {
			// Upstream only ever read Targets[0], silently dropping the rest.
			dnsRecords := endpoints2DNSRecords([]*endpoint.Endpoint{
				{DNSName: "multi.foobar.com", RecordType: endpoint.RecordTypeA, Targets: []string{"1.1.1.1", "2.2.2.2"}},
			})

			Expect(dnsRecords).To(Equal([]openwrt.DNSRecord{
				{Type: openwrt.RecordTypeA, Name: "multi.foobar.com", IP: "1.1.1.1"},
				{Type: openwrt.RecordTypeA, Name: "multi.foobar.com", IP: "2.2.2.2"},
			}))
		})

		It("skips unsupported types and empty targets", func() {
			dnsRecords := endpoints2DNSRecords([]*endpoint.Endpoint{
				{DNSName: "txt.foobar.com", RecordType: endpoint.RecordTypeTXT, Targets: []string{"hello"}},
				{DNSName: "empty.foobar.com", RecordType: endpoint.RecordTypeA},
			})

			Expect(dnsRecords).To(BeEmpty())
		})
	})

	Context("dns records to endpoints", func() {
		It("merges sections that share a name and type into one endpoint", func() {
			endpoints := dnsRecords2Endpoints(map[string]openwrt.DNSRecord{
				"a": {Type: openwrt.RecordTypeA, Name: "multi.foobar.com", IP: "2.2.2.2"},
				"b": {Type: openwrt.RecordTypeA, Name: "multi.foobar.com", IP: "1.1.1.1"},
			})

			Expect(endpoints).To(HaveLen(1))
			Expect(endpoints[0].DNSName).To(Equal("multi.foobar.com"))
			Expect(endpoints[0].RecordType).To(Equal(endpoint.RecordTypeA))
			// Sorted, so the plan does not churn on random map order.
			Expect([]string(endpoints[0].Targets)).To(Equal([]string{"1.1.1.1", "2.2.2.2"}))
		})

		It("returns endpoints in a stable order", func() {
			records := map[string]openwrt.DNSRecord{
				"a": {Type: openwrt.RecordTypeA, Name: "z.foobar.com", IP: "1.1.1.1"},
				"b": {Type: openwrt.RecordTypeCNAME, CName: "a.foobar.com", Target: "z.foobar.com"},
			}

			first := dnsRecords2Endpoints(records)
			for i := 0; i < 10; i++ {
				Expect(dnsRecords2Endpoints(records)).To(Equal(first))
			}
			Expect(first[0].DNSName).To(Equal("a.foobar.com"))
			Expect(first[1].DNSName).To(Equal("z.foobar.com"))
		})
	})

	Context("apply changes", func() {
		It("applies creates and deletes", func() {
			mockOpenWRT.EXPECT().ApplyDNSRecords(ctx,
				[]openwrt.DNSRecord{{Type: openwrt.RecordTypeA, Name: "old.foobar.com", IP: "9.9.9.9"}},
				[]openwrt.DNSRecord{{Type: openwrt.RecordTypeA, Name: "new.foobar.com", IP: "1.1.1.1"}},
			).Return(nil)

			p := &Provider{openwrt: mockOpenWRT}
			err := p.ApplyChanges(ctx, &plan.Changes{
				Create: []*endpoint.Endpoint{
					{DNSName: "new.foobar.com", RecordType: endpoint.RecordTypeA, Targets: []string{"1.1.1.1"}},
				},
				Delete: []*endpoint.Endpoint{
					{DNSName: "old.foobar.com", RecordType: endpoint.RecordTypeA, Targets: []string{"9.9.9.9"}},
				},
			})
			Expect(err).To(BeNil())
		})

		It("withdraws UpdateOld and installs UpdateNew", func() {
			// Upstream pushed UpdateOld back onto the router before UpdateNew,
			// so the previous value was re-created on every update.
			mockOpenWRT.EXPECT().ApplyDNSRecords(ctx,
				[]openwrt.DNSRecord{{Type: openwrt.RecordTypeA, Name: "foo.foobar.com", IP: "1.1.1.1"}},
				[]openwrt.DNSRecord{{Type: openwrt.RecordTypeA, Name: "foo.foobar.com", IP: "2.2.2.2"}},
			).Return(nil)

			p := &Provider{openwrt: mockOpenWRT}
			err := p.ApplyChanges(ctx, &plan.Changes{
				UpdateOld: []*endpoint.Endpoint{
					{DNSName: "foo.foobar.com", RecordType: endpoint.RecordTypeA, Targets: []string{"1.1.1.1"}},
				},
				UpdateNew: []*endpoint.Endpoint{
					{DNSName: "foo.foobar.com", RecordType: endpoint.RecordTypeA, Targets: []string{"2.2.2.2"}},
				},
			})
			Expect(err).To(BeNil())
		})

		It("leaves untouched targets alone when an update only adds one", func() {
			mockOpenWRT.EXPECT().ApplyDNSRecords(ctx,
				[]openwrt.DNSRecord{},
				[]openwrt.DNSRecord{{Type: openwrt.RecordTypeA, Name: "foo.foobar.com", IP: "2.2.2.2"}},
			).Return(nil)

			p := &Provider{openwrt: mockOpenWRT}
			err := p.ApplyChanges(ctx, &plan.Changes{
				UpdateOld: []*endpoint.Endpoint{
					{DNSName: "foo.foobar.com", RecordType: endpoint.RecordTypeA, Targets: []string{"1.1.1.1"}},
				},
				UpdateNew: []*endpoint.Endpoint{
					{DNSName: "foo.foobar.com", RecordType: endpoint.RecordTypeA, Targets: []string{"1.1.1.1", "2.2.2.2"}},
				},
			})
			Expect(err).To(BeNil())
		})

		It("does not touch the router when the plan is a no-op", func() {
			p := &Provider{openwrt: mockOpenWRT}
			Expect(p.ApplyChanges(ctx, &plan.Changes{})).To(BeNil())
			Expect(p.ApplyChanges(ctx, nil)).To(BeNil())
		})
	})

	Context("adjust endpoints", func() {
		It("drops record types this provider cannot write", func() {
			// Left in place they would be planned, silently skipped at write
			// time, and re-planned on every run.
			p := &Provider{openwrt: mockOpenWRT}
			adjusted, err := p.AdjustEndpoints([]*endpoint.Endpoint{
				{DNSName: "a.foobar.com", RecordType: endpoint.RecordTypeA, Targets: []string{"1.1.1.1"}},
				{DNSName: "aaaa.foobar.com", RecordType: endpoint.RecordTypeAAAA, Targets: []string{"::1"}},
				{DNSName: "txt.foobar.com", RecordType: endpoint.RecordTypeTXT, Targets: []string{"hi"}},
				{DNSName: "c.foobar.com", RecordType: endpoint.RecordTypeCNAME, Targets: []string{"a.foobar.com"}},
			})

			Expect(err).To(BeNil())
			Expect(adjusted).To(HaveLen(2))
			Expect(adjusted[0].DNSName).To(Equal("a.foobar.com"))
			Expect(adjusted[1].DNSName).To(Equal("c.foobar.com"))
		})

		It("strips a per-record TTL that dnsmasq cannot honour", func() {
			p := &Provider{openwrt: mockOpenWRT}
			adjusted, err := p.AdjustEndpoints([]*endpoint.Endpoint{
				{DNSName: "a.foobar.com", RecordType: endpoint.RecordTypeA,
					Targets: []string{"1.1.1.1"}, RecordTTL: endpoint.TTL(60)},
			})

			Expect(err).To(BeNil())
			Expect(adjusted).To(HaveLen(1))
			Expect(adjusted[0].RecordTTL.IsConfigured()).To(BeFalse())
		})

		It("handles an empty and a nil-containing list", func() {
			p := &Provider{openwrt: mockOpenWRT}
			adjusted, err := p.AdjustEndpoints([]*endpoint.Endpoint{nil})
			Expect(err).To(BeNil())
			Expect(adjusted).To(BeEmpty())
		})
	})

	Context("records", func() {
		It("reads through to the router", func() {
			mockOpenWRT.EXPECT().GetDNSRecords(ctx).Return(map[string]openwrt.DNSRecord{
				"a": {Type: openwrt.RecordTypeA, Name: "a.foobar.com", IP: "1.1.1.1"},
			}, nil)

			p := &Provider{openwrt: mockOpenWRT}
			endpoints, err := p.Records(ctx)
			Expect(err).To(BeNil())
			Expect(endpoints).To(HaveLen(1))
			Expect(endpoints[0].DNSName).To(Equal("a.foobar.com"))
			Expect(endpoints[0].RecordTTL).To(Equal(endpoint.TTL(defaultTTL)))
		})
	})
})
