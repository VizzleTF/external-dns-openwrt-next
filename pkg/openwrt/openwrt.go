package openwrt

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/VizzleTF/external-dns-openwrt-next/pkg/logger"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/lucirpc"
	"go.uber.org/zap"
)

//go:generate mockgen -destination=../../internal/mocks/openwrt/openwrt.go -package=mocks . OpenWRT

const (
	dnsmasqRestartCommand = "/etc/init.d/dnsmasq restart"
	dnsmasqReloadCommand  = "/etc/init.d/dnsmasq reload"
	uciConfig             = "dhcp"
)

type OpenWRT interface {
	// GetDNSRecords returns the records this provider manages, keyed by UCI
	// section name. With ownership enabled, sections without the marker are
	// left out.
	GetDNSRecords(context.Context) (map[string]DNSRecord, error)
	// ApplyDNSRecords removes and adds records in a single pass, then commits
	// and reloads dnsmasq once. Both slices may be empty.
	ApplyDNSRecords(ctx context.Context, remove, add []DNSRecord) error
}

type openWRT struct {
	lucirpc        lucirpc.LuciRPC
	reloadStrategy string

	ownershipID     string
	ownershipOption string
	adoptExisting   bool
}

func New(cfg *Config) (OpenWRT, error) {
	if err := validateReloadStrategy(cfg.ReloadStrategy); err != nil {
		return nil, err
	}

	option := cfg.OwnershipOption
	if option == "" {
		option = DefaultOwnershipOption
	}

	lrcp, err := lucirpc.New(cfg.LuciRPC)
	if err != nil {
		return nil, err
	}

	if cfg.OwnershipEnabled() {
		logger.Log.Info("ownership enabled, only marked records are managed",
			zap.String("option", option), zap.String("id", cfg.OwnershipID),
			zap.Bool("adopt_existing", cfg.AdoptExisting))
	} else {
		logger.Log.Warn("ownership disabled, every domain/cname section is reported — do not combine with policy=sync")
	}

	return &openWRT{
		lucirpc:         lrcp,
		reloadStrategy:  cfg.ReloadStrategy,
		ownershipID:     cfg.OwnershipID,
		ownershipOption: option,
		adoptExisting:   cfg.AdoptExisting,
	}, nil
}

func (o *openWRT) ownershipEnabled() bool { return o.ownershipID != "" }

// GetDNSRecords reads every dhcp section and keeps the ones this provider is
// responsible for.
func (o *openWRT) GetDNSRecords(ctx context.Context) (map[string]DNSRecord, error) {
	all, err := o.readSections(ctx)
	if err != nil {
		return nil, err
	}

	records := make(map[string]DNSRecord, len(all))
	skipped := 0

	for section, record := range all {
		if o.ownershipEnabled() && record.Owner != o.ownershipID {
			skipped++
			continue
		}
		records[section] = record
	}

	logger.Log.Debug("current records",
		zap.Int("managed", len(records)), zap.Int("not_owned", skipped))
	return records, nil
}

// readSections returns every domain/cname section, owned or not.
func (o *openWRT) readSections(ctx context.Context) (map[string]DNSRecord, error) {
	result, err := o.lucirpc.Uci(ctx, "get_all", []string{uciConfig})
	if err != nil {
		return nil, err
	}

	// `uci get_all dhcp` also returns dnsmasq/dhcp/host/odhcpd sections, and
	// option values may be lists rather than strings, so decode loosely and
	// pick out what we understand.
	var raw map[string]map[string]any
	if err := json.Unmarshal([]byte(result), &raw); err != nil {
		return nil, fmt.Errorf("decode uci %s: %w", uciConfig, err)
	}

	records := make(map[string]DNSRecord, len(raw))
	for section, options := range raw {
		record, ok := o.parseSection(options)
		if !ok {
			continue
		}
		records[section] = record
	}

	return records, nil
}

func (o *openWRT) parseSection(options map[string]any) (DNSRecord, bool) {
	record := DNSRecord{Owner: stringOption(options, o.ownershipOption)}

	switch stringOption(options, optionSectionType) {
	case sectionTypeDomain:
		record.Type = RecordTypeA
		record.Name = stringOption(options, optionName)
		record.IP = stringOption(options, optionIP)
	case sectionTypeCName:
		record.Type = RecordTypeCNAME
		record.CName = stringOption(options, optionCName)
		record.Target = stringOption(options, optionTarget)
	default:
		return DNSRecord{}, false
	}

	// A `domain` section may carry a list of names, which this provider cannot
	// represent as a single record. Skip rather than mangle it.
	if record.DNSName() == "" || record.Value() == "" {
		logger.Log.Debug("skipping section that is not a single-valued record")
		return DNSRecord{}, false
	}

	return record, true
}

// stringOption reads a UCI option expected to be a plain string. Missing
// options and list values yield "".
func stringOption(options map[string]any, key string) string {
	value, ok := options[key].(string)
	if !ok {
		return ""
	}
	return value
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

	// Adoption has to see unowned sections too, so read the unfiltered set and
	// apply ownership per operation.
	all, err := o.readSections(ctx)
	if err != nil {
		return err
	}

	index := newSectionIndex(all)
	changed := 0

	for _, record := range remove {
		n, err := o.removeRecord(ctx, index, record)
		if err != nil {
			return err
		}
		changed += n
	}

	for _, record := range add {
		n, err := o.addRecord(ctx, index, record)
		if err != nil {
			return err
		}
		changed += n
	}

	if changed == 0 {
		logger.Log.Debug("nothing changed, skipping commit")
		return nil
	}

	if _, err := o.lucirpc.Uci(ctx, "commit", []string{uciConfig}); err != nil {
		return fmt.Errorf("commit %s: %w", uciConfig, err)
	}

	return o.reload(ctx)
}

// removeRecord deletes every owned section matching the record. Sections that
// belong to someone else are never touched.
func (o *openWRT) removeRecord(ctx context.Context, index *sectionIndex, record DNSRecord) (int, error) {
	sections := index.owned(record.Key(), o.ownershipID, o.ownershipEnabled())
	if len(sections) == 0 {
		logger.Log.Info("record already absent, nothing to delete", recordFields(record)...)
		return 0, nil
	}

	for _, section := range sections {
		if _, err := o.lucirpc.Uci(ctx, "delete", []string{uciConfig, section}); err != nil {
			return 0, fmt.Errorf("delete %s (%s): %w", record.DNSName(), section, err)
		}
		index.drop(record.Key(), section)
	}

	logger.Log.Info("deleted record", recordFields(record)...)
	return len(sections), nil
}

// addRecord creates the record, adopts a matching unowned section, or does
// nothing when it is already owned and present.
func (o *openWRT) addRecord(ctx context.Context, index *sectionIndex, record DNSRecord) (int, error) {
	if err := record.Validate(); err != nil {
		return 0, err
	}

	key := record.Key()

	if len(index.owned(key, o.ownershipID, o.ownershipEnabled())) > 0 {
		logger.Log.Info("record already present, nothing to add", recordFields(record)...)
		return 0, nil
	}

	// Migration path: the record is already on the router but unmarked, so
	// stamp it instead of adding a second identical section.
	if o.ownershipEnabled() && o.adoptExisting {
		if section, ok := index.firstUnowned(key); ok {
			if err := o.setOwner(ctx, section); err != nil {
				return 0, fmt.Errorf("adopt %s (%s): %w", record.DNSName(), section, err)
			}
			index.markOwned(key, section, o.ownershipID)
			logger.Log.Info("adopted existing record",
				append(recordFields(record), zap.String("section", section))...)
			return 1, nil
		}
	}

	section, err := o.createSection(ctx, record)
	if err != nil {
		return 0, fmt.Errorf("add %s: %w", record.DNSName(), err)
	}

	index.add(key, section, o.ownershipID)
	logger.Log.Info("added record", recordFields(record)...)
	return 1, nil
}

func (o *openWRT) createSection(ctx context.Context, record DNSRecord) (string, error) {
	var sectionType string
	options := make([][2]string, 0, 3)

	switch record.Type {
	case RecordTypeA:
		sectionType = sectionTypeDomain
		options = append(options, [2]string{optionName, record.Name}, [2]string{optionIP, record.IP})
	case RecordTypeCNAME:
		sectionType = sectionTypeCName
		options = append(options, [2]string{optionCName, record.CName}, [2]string{optionTarget, record.Target})
	default:
		return "", fmt.Errorf("invalid record type: %s", record.Type)
	}

	if o.ownershipEnabled() {
		options = append(options, [2]string{o.ownershipOption, o.ownershipID})
	}

	section, err := o.lucirpc.Uci(ctx, "add", []string{uciConfig, sectionType})
	if err != nil {
		return "", err
	}

	for _, option := range options {
		if _, err := o.lucirpc.Uci(ctx, "set", []string{uciConfig, section, option[0], option[1]}); err != nil {
			return "", err
		}
	}

	return section, nil
}

func (o *openWRT) setOwner(ctx context.Context, section string) error {
	_, err := o.lucirpc.Uci(ctx, "set", []string{uciConfig, section, o.ownershipOption, o.ownershipID})
	return err
}

// reload makes dnsmasq pick up the committed configuration.
//
// `uci commit` only writes /etc/config/dhcp — it neither regenerates
// /var/etc/dnsmasq.conf.* nor signals the daemon, so without this step records
// stay invisible until something else restarts the service.
func (o *openWRT) reload(ctx context.Context) error {
	switch normaliseReloadStrategy(o.reloadStrategy) {
	case ReloadStrategyNone:
		logger.Log.Debug("reload disabled, dnsmasq keeps serving the previous configuration")
		return nil

	case ReloadStrategyRestart:
		// The only strategy that applies both record types. A records go to
		// the hostfile, which a reload could pick up, but CNAMEs are `--cname=`
		// entries in the config file that dnsmasq reads once at startup.
		if _, err := o.lucirpc.Sys(ctx, "call", []string{dnsmasqRestartCommand}); err != nil {
			return fmt.Errorf("restart dnsmasq: %w", err)
		}

	case ReloadStrategyReload:
		// Ineffective where dnsmasq runs under ujail: reload_service() signals
		// the jail wrapper, not the daemon, so the regenerated files are never
		// re-read. Never applies CNAMEs either. See config.go.
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

func recordFields(record DNSRecord) []zap.Field {
	return []zap.Field{
		zap.String("name", record.DNSName()),
		zap.String("type", record.Type),
		zap.String("value", record.Value()),
	}
}
