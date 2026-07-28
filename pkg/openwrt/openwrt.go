package openwrt

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/VizzleTF/external-dns-openwrt-webhook/pkg/logger"
	"github.com/VizzleTF/external-dns-openwrt-webhook/pkg/lucirpc"
	"go.uber.org/zap"
)

//go:generate mockgen -destination=../../internal/mocks/openwrt/openwrt.go -package=mocks . OpenWRT

const dnsmasqReloadCommand = "/etc/init.d/dnsmasq reload"

type OpenWRT interface {
	GetDNSRecords(context.Context) (map[string]DNSRecord, error)
	// ApplyDNSRecords removes and adds records in a single pass, then commits
	// and reloads dnsmasq once. Both slices may be empty.
	ApplyDNSRecords(ctx context.Context, remove, add []DNSRecord) error
}

type openWRT struct {
	lucirpc        lucirpc.LuciRPC
	reloadStrategy string
}

func New(cfg *Config) (OpenWRT, error) {
	lrcp, err := lucirpc.New(cfg.LuciRPC)
	if err != nil {
		return nil, err
	}

	if err := validateReloadStrategy(cfg.ReloadStrategy); err != nil {
		return nil, err
	}

	return &openWRT{
		lucirpc:        lrcp,
		reloadStrategy: cfg.ReloadStrategy,
	}, nil
}

func (o *openWRT) GetDNSRecords(ctx context.Context) (map[string]DNSRecord, error) {
	result, err := o.lucirpc.Uci(ctx, "get_all", []string{"dhcp"})
	if err != nil {
		return nil, err
	}

	var records map[string]DNSRecord
	if err := json.Unmarshal([]byte(result), &records); err != nil {
		return nil, err
	}

	for key, record := range records {
		switch record.Type {
		case sectionTypeDomain:
			records[key] = DNSRecord{
				Type: RecordTypeA,
				IP:   record.IP,
				Name: record.Name,
			}
		case sectionTypeCName:
			records[key] = DNSRecord{
				Type:   RecordTypeCNAME,
				CName:  record.CName,
				Target: record.Target,
			}
		default:
			// `uci get_all dhcp` also returns dnsmasq/dhcp/host/odhcpd sections.
			logger.Log.Debug("ignoring record", zap.String("type", record.Type))
			delete(records, key)
		}
	}

	logger.Log.Debug("current records", zap.Int("count", len(records)))
	return records, nil
}

// ApplyDNSRecords reconciles the router against the requested removals and
// additions.
//
// Both directions are idempotent on purpose. ExternalDNS retries the whole
// change set on failure, so removing an already absent record — or adding one
// that is already there — must not be an error; otherwise a single stale entry
// blocks every other change forever.
func (o *openWRT) ApplyDNSRecords(ctx context.Context, remove, add []DNSRecord) error {
	if len(remove) == 0 && len(add) == 0 {
		return nil
	}

	current, err := o.GetDNSRecords(ctx)
	if err != nil {
		return err
	}

	// key -> UCI section names. Several sections share a key only when the
	// router already holds duplicates; delete all of them in that case.
	sections := make(map[string][]string, len(current))
	for section, record := range current {
		key := record.Key()
		sections[key] = append(sections[key], section)
	}

	changed := 0

	for _, record := range remove {
		key := record.Key()
		found, ok := sections[key]
		if !ok {
			logger.Log.Info("record already absent, nothing to delete",
				zap.String("name", record.DNSName()),
				zap.String("type", record.Type),
				zap.String("value", record.Value()))
			continue
		}

		for _, section := range found {
			if _, err := o.lucirpc.Uci(ctx, "delete", []string{"dhcp", section}); err != nil {
				return fmt.Errorf("delete %s (%s): %w", record.DNSName(), section, err)
			}
			changed++
		}

		delete(sections, key)
		logger.Log.Info("deleted record",
			zap.String("name", record.DNSName()),
			zap.String("type", record.Type),
			zap.String("value", record.Value()))
	}

	for _, record := range add {
		if err := record.Validate(); err != nil {
			return err
		}

		key := record.Key()
		if _, exists := sections[key]; exists {
			logger.Log.Info("record already present, nothing to add",
				zap.String("name", record.DNSName()),
				zap.String("type", record.Type),
				zap.String("value", record.Value()))
			continue
		}

		switch record.Type {
		case RecordTypeA:
			err = o.addA(ctx, record)
		case RecordTypeCNAME:
			err = o.addCName(ctx, record)
		default:
			err = fmt.Errorf("invalid record type: %s", record.Type)
		}
		if err != nil {
			return fmt.Errorf("add %s: %w", record.DNSName(), err)
		}

		// Guard against duplicates inside a single change set.
		sections[key] = nil
		changed++
		logger.Log.Info("added record",
			zap.String("name", record.DNSName()),
			zap.String("type", record.Type),
			zap.String("value", record.Value()))
	}

	if changed == 0 {
		logger.Log.Debug("nothing changed, skipping commit")
		return nil
	}

	if _, err := o.lucirpc.Uci(ctx, "commit", []string{"dhcp"}); err != nil {
		return fmt.Errorf("commit dhcp: %w", err)
	}

	return o.reload(ctx)
}

// reload makes dnsmasq pick up the committed configuration.
//
// `uci commit` only writes /etc/config/dhcp — it neither regenerates
// /var/etc/dnsmasq.conf.* nor signals the daemon, so without this step records
// stay invisible until something else restarts the service.
func (o *openWRT) reload(ctx context.Context) error {
	switch o.reloadStrategy {
	case ReloadStrategyNone:
		logger.Log.Debug("reload disabled, dnsmasq keeps serving the previous configuration")
		return nil

	case ReloadStrategyDnsmasq:
		// Narrow by construction: touches dnsmasq and nothing else.
		if _, err := o.lucirpc.Sys(ctx, "call", []string{dnsmasqReloadCommand}); err != nil {
			return fmt.Errorf("reload dnsmasq: %w", err)
		}

	case ReloadStrategyUciApply:
		// MUST be called with no arguments. LuCI's JSON-RPC binding is
		// `function apply(config) return uci:apply(config) end`, but the
		// underlying signature is `apply(self, rollback)` — so any non-empty
		// argument is read as rollback=true and starts a ubus apply with a
		// >=90s rollback timer that REVERTS the change unless it is confirmed
		// out of band. With no argument it commits every pending config and
		// applies with rollback=false.
		if _, err := o.lucirpc.Uci(ctx, "apply", []string{}); err != nil {
			return fmt.Errorf("uci apply: %w", err)
		}

	default:
		return fmt.Errorf("unknown reload strategy: %s", o.reloadStrategy)
	}

	logger.Log.Debug("reloaded dnsmasq", zap.String("strategy", o.reloadStrategy))
	return nil
}

func (o *openWRT) addA(ctx context.Context, record DNSRecord) error {
	cfg, err := o.lucirpc.Uci(ctx, "add", []string{"dhcp", sectionTypeDomain})
	if err != nil {
		return err
	}

	if _, err := o.lucirpc.Uci(ctx, "set", []string{"dhcp", cfg, "name", record.Name}); err != nil {
		return err
	}

	if _, err := o.lucirpc.Uci(ctx, "set", []string{"dhcp", cfg, "ip", record.IP}); err != nil {
		return err
	}

	return nil
}

func (o *openWRT) addCName(ctx context.Context, record DNSRecord) error {
	cfg, err := o.lucirpc.Uci(ctx, "add", []string{"dhcp", sectionTypeCName})
	if err != nil {
		return err
	}

	if _, err := o.lucirpc.Uci(ctx, "set", []string{"dhcp", cfg, "cname", record.CName}); err != nil {
		return err
	}

	if _, err := o.lucirpc.Uci(ctx, "set", []string{"dhcp", cfg, "target", record.Target}); err != nil {
		return err
	}

	return nil
}
