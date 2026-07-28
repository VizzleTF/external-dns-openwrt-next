package provider

import (
	"context"
	"sort"

	"github.com/VizzleTF/external-dns-openwrt-webhook/pkg/logger"
	"github.com/VizzleTF/external-dns-openwrt-webhook/pkg/openwrt"
	"go.uber.org/zap"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
)

// defaultTTL is what Records() reports for every record.
//
// UCI `domain` and `cname` sections carry no per-record TTL — dnsmasq answers
// them with its global `local_ttl`. Endpoints that request an explicit TTL
// therefore cannot be honoured; ExternalDNS will keep planning an update for
// them on every run.
const defaultTTL = 300

type Provider struct {
	provider.BaseProvider

	openwrt openwrt.OpenWRT
}

func New(cfg *Config) (*Provider, error) {
	opwrt, err := openwrt.New(cfg.OpenWRT)
	if err != nil {
		return nil, err
	}

	return &Provider{
		openwrt: opwrt,
	}, nil
}

// ApplyChanges turns a plan into one reconcile pass over the router.
//
// ExternalDNS semantics: Create is new, Delete is gone, and an update arrives
// as the pair (UpdateOld, UpdateNew) — the previous and the desired state of
// the same records. Only UpdateNew must end up on the router; UpdateOld is
// there to say what to withdraw.
func (p *Provider) ApplyChanges(ctx context.Context, changes *plan.Changes) error {
	if changes == nil {
		return nil
	}

	logger.Log.Debug("apply changes",
		zap.Int("create", len(changes.Create)),
		zap.Int("update_old", len(changes.UpdateOld)),
		zap.Int("update_new", len(changes.UpdateNew)),
		zap.Int("delete", len(changes.Delete)))

	remove := endpoints2DNSRecords(changes.Delete)
	remove = append(remove, endpoints2DNSRecords(changes.UpdateOld)...)

	add := endpoints2DNSRecords(changes.Create)
	add = append(add, endpoints2DNSRecords(changes.UpdateNew)...)

	// An update that only adds or drops a target leaves the untouched targets
	// in both lists. Cancelling them out avoids deleting and immediately
	// re-adding the same UCI section on every run.
	remove, add = cancelOut(remove, add)

	if len(remove) == 0 && len(add) == 0 {
		logger.Log.Debug("no effective changes")
		return nil
	}

	return p.openwrt.ApplyDNSRecords(ctx, remove, add)
}

func (p *Provider) Records(ctx context.Context) ([]*endpoint.Endpoint, error) {
	records, err := p.openwrt.GetDNSRecords(ctx)
	if err != nil {
		return nil, err
	}

	return dnsRecords2Endpoints(records), nil
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
func dnsRecords2Endpoints(dnsRecords map[string]openwrt.DNSRecord) []*endpoint.Endpoint {
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

	endpoints := make([]*endpoint.Endpoint, 0, len(grouped))
	for k, targets := range grouped {
		// Map iteration is random; sort so the output is stable across runs.
		sort.Strings(targets)
		endpoints = append(endpoints, &endpoint.Endpoint{
			DNSName:    k.name,
			RecordType: k.recordType,
			RecordTTL:  defaultTTL,
			Targets:    endpoint.Targets(targets),
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
func endpoints2DNSRecords(endpoints []*endpoint.Endpoint) []openwrt.DNSRecord {
	var dnsRecords []openwrt.DNSRecord

	for _, ep := range endpoints {
		if ep == nil {
			continue
		}

		if len(ep.Targets) == 0 {
			logger.Log.Warn("skipping endpoint without targets",
				zap.String("name", ep.DNSName), zap.String("type", ep.RecordType))
			continue
		}

		for _, target := range ep.Targets {
			var dnsRecord openwrt.DNSRecord

			switch ep.RecordType {
			case endpoint.RecordTypeA:
				dnsRecord.Type = openwrt.RecordTypeA
				dnsRecord.Name = ep.DNSName
				dnsRecord.IP = target
			case endpoint.RecordTypeCNAME:
				dnsRecord.Type = openwrt.RecordTypeCNAME
				dnsRecord.CName = ep.DNSName
				dnsRecord.Target = target
			default:
				logger.Log.Debug("skipping unsupported record type",
					zap.String("name", ep.DNSName), zap.String("type", ep.RecordType))
				continue
			}

			dnsRecords = append(dnsRecords, dnsRecord)
		}
	}

	return dnsRecords
}
