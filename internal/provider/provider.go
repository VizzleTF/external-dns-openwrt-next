package provider

import (
	"context"
	"sort"

	"github.com/VizzleTF/external-dns-openwrt-next/pkg/openwrt"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/webhookapi"
	"log/slog"
)

// defaultTTL is what Records() reports for every record.
//
// UCI `domain` and `cname` sections carry no per-record TTL — dnsmasq answers
// them with its global `local_ttl`. AdjustEndpoints strips any TTL from the
// desired state so the plan cannot keep asking for one this provider is unable
// to honour.
const defaultTTL = 300

type Provider struct {
	openwrt openwrt.OpenWRT
	log     *slog.Logger
}

func New(cfg *Config, log *slog.Logger) (*Provider, error) {
	opwrt, err := openwrt.New(cfg.OpenWRT, log)
	if err != nil {
		return nil, err
	}

	return &Provider{
		openwrt: opwrt,
		log:     log,
	}, nil
}

// ApplyChanges turns a plan into one reconcile pass over the router.
//
// ExternalDNS semantics: Create is new, Delete is gone, and an update arrives
// as the pair (UpdateOld, UpdateNew) — the previous and the desired state of
// the same records. Only UpdateNew must end up on the router; UpdateOld is
// there to say what to withdraw.
func (p *Provider) ApplyChanges(ctx context.Context, changes *webhookapi.Changes) error {
	if changes.Empty() {
		p.log.Debug("empty change set")
		return nil
	}

	p.log.Debug("apply changes",
		slog.Int("create", len(changes.Create)),
		slog.Int("update_old", len(changes.UpdateOld)),
		slog.Int("update_new", len(changes.UpdateNew)),
		slog.Int("delete", len(changes.Delete)))

	remove := p.endpoints2DNSRecords(changes.Delete)
	remove = append(remove, p.endpoints2DNSRecords(changes.UpdateOld)...)

	add := p.endpoints2DNSRecords(changes.Create)
	add = append(add, p.endpoints2DNSRecords(changes.UpdateNew)...)

	// An update that only adds or drops a target leaves the untouched targets
	// in both lists. Cancelling them out avoids deleting and immediately
	// re-adding the same UCI section on every run.
	remove, add = cancelOut(remove, add)

	if len(remove) == 0 && len(add) == 0 {
		p.log.Debug("no effective changes")
		return nil
	}

	return p.openwrt.ApplyDNSRecords(ctx, remove, add)
}

// AdjustEndpoints trims the desired state down to what this provider can
// actually represent.
//
// Both adjustments exist to stop the plan from churning. ExternalDNS compares
// the desired endpoints with what Records() reports, so anything the provider
// silently drops at write time would be re-planned on every single run:
//
//   - record types other than A and CNAME cannot be written to UCI at all;
//   - per-record TTLs do not exist in `domain`/`cname` sections, so an endpoint
//     asking for one would never match what Records() reports.
func (p *Provider) AdjustEndpoints(endpoints []*webhookapi.Endpoint) ([]*webhookapi.Endpoint, error) {
	adjusted := make([]*webhookapi.Endpoint, 0, len(endpoints))

	for _, ep := range endpoints {
		if ep == nil {
			continue
		}

		switch ep.RecordType {
		case webhookapi.RecordTypeA, webhookapi.RecordTypeCNAME:
		default:
			p.log.Warn("dropping endpoint, record type not supported by this provider",
				slog.String("name", ep.DNSName), slog.String("type", ep.RecordType))
			continue
		}

		if ep.RecordTTL.IsConfigured() {
			p.log.Debug("dropping per-record TTL, dnsmasq serves these from its global local_ttl",
				slog.String("name", ep.DNSName), slog.Int64("ttl", int64(ep.RecordTTL)))
			ep.RecordTTL = 0
		}

		adjusted = append(adjusted, ep)
	}

	return adjusted, nil
}

func (p *Provider) Records(ctx context.Context) ([]*webhookapi.Endpoint, error) {
	records, err := p.openwrt.GetDNSRecords(ctx)
	if err != nil {
		return nil, err
	}

	return p.dnsRecords2Endpoints(records), nil
}

// cancelOut drops entries present in both slices, preserving order.
func cancelOut(remove, add []openwrt.DNSRecord) ([]openwrt.DNSRecord, []openwrt.DNSRecord) {
	removeKeys := make(map[string]int, len(remove))
	for _, record := range remove {
		removeKeys[record.Key()]++
	}

	addKeys := make(map[string]int, len(add))
	for _, record := range add {
		addKeys[record.Key()]++
	}

	keptRemove := make([]openwrt.DNSRecord, 0, len(remove))
	for _, record := range remove {
		if addKeys[record.Key()] > 0 {
			continue
		}
		keptRemove = append(keptRemove, record)
	}

	keptAdd := make([]openwrt.DNSRecord, 0, len(add))
	for _, record := range add {
		if removeKeys[record.Key()] > 0 {
			continue
		}
		keptAdd = append(keptAdd, record)
	}

	return keptRemove, keptAdd
}

// dnsRecords2Endpoints groups UCI sections back into endpoints.
//
// Every section holds exactly one target, so a multi-target endpoint spans
// several sections. Reporting them as separate endpoints with the same name
// and type makes ExternalDNS plan a change on every run, so they are merged
// back here.
func (p *Provider) dnsRecords2Endpoints(dnsRecords map[string]openwrt.DNSRecord) []*webhookapi.Endpoint {
	type key struct {
		name       string
		recordType string
	}

	grouped := make(map[key][]string)
	for _, dnsRecord := range dnsRecords {
		switch dnsRecord.Type {
		case openwrt.RecordTypeA, openwrt.RecordTypeCNAME:
		default:
			continue
		}

		k := key{name: dnsRecord.DNSName(), recordType: dnsRecord.Type}
		grouped[k] = append(grouped[k], dnsRecord.Value())
	}

	endpoints := make([]*webhookapi.Endpoint, 0, len(grouped))
	for k, targets := range grouped {
		// Map iteration is random; sort so the output is stable across runs.
		sort.Strings(targets)
		endpoints = append(endpoints, &webhookapi.Endpoint{
			DNSName:    k.name,
			RecordType: k.recordType,
			RecordTTL:  defaultTTL,
			Targets:    webhookapi.Targets(targets),
		})
	}

	sort.Slice(endpoints, func(i, j int) bool {
		if endpoints[i].DNSName != endpoints[j].DNSName {
			return endpoints[i].DNSName < endpoints[j].DNSName
		}
		return endpoints[i].RecordType < endpoints[j].RecordType
	})

	return endpoints
}

// endpoints2DNSRecords flattens endpoints into one record per target.
func (p *Provider) endpoints2DNSRecords(endpoints []*webhookapi.Endpoint) []openwrt.DNSRecord {
	var dnsRecords []openwrt.DNSRecord

	for _, ep := range endpoints {
		if ep == nil {
			continue
		}

		if len(ep.Targets) == 0 {
			p.log.Warn("skipping endpoint without targets",
				slog.String("name", ep.DNSName), slog.String("type", ep.RecordType))
			continue
		}

		for _, target := range ep.Targets {
			var dnsRecord openwrt.DNSRecord

			switch ep.RecordType {
			case webhookapi.RecordTypeA:
				dnsRecord.Type = openwrt.RecordTypeA
				dnsRecord.Name = ep.DNSName
				dnsRecord.IP = target
			case webhookapi.RecordTypeCNAME:
				dnsRecord.Type = openwrt.RecordTypeCNAME
				dnsRecord.CName = ep.DNSName
				dnsRecord.Target = target
			default:
				p.log.Debug("skipping unsupported record type",
					slog.String("name", ep.DNSName), slog.String("type", ep.RecordType))
				continue
			}

			dnsRecords = append(dnsRecords, dnsRecord)
		}
	}

	return dnsRecords
}

// GetDomainFilter reports no filter of its own: ExternalDNS applies the
// --domain-filter it was started with, and this provider has no additional
// knowledge of which zones the router should serve.
func (p *Provider) GetDomainFilter() webhookapi.DomainFilter {
	return webhookapi.DomainFilter{Filters: []string{}}
}
