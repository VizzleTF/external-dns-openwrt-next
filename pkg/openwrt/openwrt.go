package openwrt

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/VizzleTF/external-dns-openwrt-next/pkg/ubus"
	"log/slog"
)

const uciConfig = "dhcp"

type OpenWRT interface {
	// GetDNSRecords returns the records this provider manages, keyed by UCI
	// section name. With ownership enabled, sections without the marker are
	// left out.
	GetDNSRecords(context.Context) (map[string]DNSRecord, error)
	// ApplyDNSRecords removes and adds records in a single pass, then applies
	// them at once. Both slices may be empty.
	ApplyDNSRecords(ctx context.Context, remove, add []DNSRecord) error
}

type openWRT struct {
	ubus        ubus.Client
	log         *slog.Logger
	ownershipID string
}

func New(cfg *Config, log *slog.Logger) (OpenWRT, error) {
	if cfg.Ubus.Hostname == "" {
		return nil, errors.New("router hostname is not set, set PROVIDER_OPENWRT_LUCIRPC_HOSTNAME")
	}

	if cfg.OwnershipID != "" {
		log.Info("ownership enabled, only marked records are managed",
			slog.String("option", ownershipOption), slog.String("id", cfg.OwnershipID))
	} else {
		log.Warn("ownership disabled, every domain/cname section is reported — do not combine with policy=sync")
	}

	return &openWRT{
		ubus:        ubus.New(cfg.Ubus, log),
		log:         log,
		ownershipID: cfg.OwnershipID,
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

	o.log.Debug("current records",
		slog.Int("managed", len(records)), slog.Int("not_owned", skipped))
	return records, nil
}

// readSections returns every domain/cname section, owned or not.
func (o *openWRT) readSections(ctx context.Context) (map[string]DNSRecord, error) {
	// `uci get dhcp` also returns dnsmasq/dhcp/host/odhcpd sections, and
	// option values may be lists rather than strings, so decode loosely and
	// pick out what we understand.
	var data struct {
		Values map[string]map[string]any `json:"values"`
	}
	if err := o.ubus.Call(ctx, "uci", "get", map[string]string{"config": uciConfig}, &data); err != nil {
		return nil, err
	}

	records := make(map[string]DNSRecord, len(data.Values))
	for section, options := range data.Values {
		record, ok := o.parseSection(options)
		if !ok {
			continue
		}
		records[section] = record
	}

	return records, nil
}

func (o *openWRT) parseSection(options map[string]any) (DNSRecord, bool) {
	record := DNSRecord{Owner: stringOption(options, ownershipOption)}

	switch stringOption(options, optionSectionType) {
	case sectionTypeDomain:
		record.Type = RecordTypeA
		record.Name = stringOption(options, optionName)
		record.Value = stringOption(options, optionIP)
		// dnsmasq serves an IPv6 address, an IPv4-mapped one too, as AAAA. As an
		// A record it would be deleted to make way for the cluster's A records
		// for the name. A value that is no address at all stays: dnsmasq serves
		// nothing for it, or, holding whitespace or a newline, an A record for
		// whatever name follows, which sync has to be able to delete.
		if addr, err := netip.ParseAddr(record.Value); err == nil && !addr.Is4() {
			o.log.Debug("skipping domain section dnsmasq serves as AAAA",
				slog.String("name", record.Name), slog.String("ip", record.Value))
			return DNSRecord{}, false
		}
	case sectionTypeCName:
		record.Type = RecordTypeCNAME
		record.Name = stringOption(options, optionCName)
		record.Value = stringOption(options, optionTarget)
	default:
		return DNSRecord{}, false
	}

	// A `domain` section may carry a list of names, which this provider cannot
	// represent as a single record. Skip rather than mangle it.
	if record.Name == "" || record.Value == "" {
		o.log.Debug("skipping section that is not a single-valued record")
		return DNSRecord{}, false
	}

	// The plan compares targets without dropping a trailing dot, so a
	// `Real.LAN.` typed into LuCI would never match the `real.lan` asked for.
	return record.canonical(), true
}

// stringOption reads a UCI option expected to be a plain string. Missing
// options and list values yield "".
func stringOption(options map[string]any, key string) string {
	value, _ := options[key].(string)
	return value
}

// ApplyDNSRecords reconciles the router against the requested removals and
// additions.
//
// Both directions are idempotent on purpose. ExternalDNS retries the whole
// change set on failure, so removing an already absent record — or adding one
// that is already there — must not be an error; otherwise a single stale entry
// blocks every other change forever.
func (o *openWRT) ApplyDNSRecords(ctx context.Context, remove, add []DNSRecord) (err error) {
	if len(remove) == 0 && len(add) == 0 {
		return nil
	}

	// rpcd keeps what a session staged until it is applied, and no other
	// session sees it: dropping the session on a failure keeps the next change
	// set from applying half of this one along with its own.
	defer func() {
		if err != nil {
			o.ubus.Reset()
		}
	}()

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
		o.log.Debug("nothing changed, skipping apply")
		return nil
	}

	// Commits what the session staged, and procd reloads dnsmasq for it: a
	// changed CNAME restarts it, a changed A record only has the hostfile
	// re-read. A rollback would revert the change unless confirmed in time.
	err = o.ubus.Call(ctx, "uci", "apply", map[string]bool{"rollback": false}, nil)
	if errors.Is(err, ubus.ErrNoData) {
		o.log.Debug("nothing staged, nothing to apply")
		return nil
	}
	return err
}

// removeRecord deletes every owned section matching the record. Sections that
// belong to someone else are never touched.
func (o *openWRT) removeRecord(ctx context.Context, index sectionIndex, record DNSRecord) (int, error) {
	sections := index.owned(record.Key(), o.ownershipID)
	if len(sections) == 0 {
		o.log.Info("record already absent, nothing to delete", recordFields(record)...)
		return 0, nil
	}

	for _, section := range sections {
		if err := o.ubus.Call(ctx, "uci", "delete", map[string]string{"config": uciConfig, "section": section}, nil); err != nil {
			return 0, fmt.Errorf("delete %s (%s): %w", record.Name, section, err)
		}
		index.drop(record.Key(), section)
	}

	o.log.Info("deleted record", recordFields(record)...)
	return len(sections), nil
}

// addRecord creates the record, adopts a matching unowned section, or does
// nothing when it is already owned and present.
func (o *openWRT) addRecord(ctx context.Context, index sectionIndex, record DNSRecord) (int, error) {
	if err := record.Validate(); err != nil {
		return 0, err
	}

	key := record.Key()

	if len(index.owned(key, o.ownershipID)) > 0 {
		o.log.Info("record already present, nothing to add", recordFields(record)...)
		return 0, nil
	}

	// Migration path: the record is already on the router but unmarked, so
	// stamp it instead of adding a second identical section.
	if o.ownershipEnabled() {
		if section, ok := index.firstUnowned(key); ok {
			if err := o.setOwner(ctx, section); err != nil {
				return 0, fmt.Errorf("adopt %s (%s): %w", record.Name, section, err)
			}
			index.markOwned(key, section, o.ownershipID)
			o.log.Info("adopted existing record",
				append(recordFields(record), slog.String("section", section))...)
			return 1, nil
		}
	}

	// Checked after adoption, which writes no second section. dnsmasq refuses
	// to start on two CNAMEs for one alias, even with the same target, and on a
	// CNAME loop, taking DNS and DHCP for the LAN down with it. Skipped rather
	// than failed: an error would block every other change on each retry.
	if record.Type == RecordTypeCNAME {
		if taken := index.named(RecordTypeCNAME, record.Name); len(taken) > 0 {
			o.log.Warn("skipping CNAME, its alias already has one and dnsmasq does not start with two",
				append(recordFields(record), sectionFields(taken[0])...)...)
			return 0, nil
		}
		if index.closesCNAMELoop(record.Name, record.Value) {
			o.log.Warn("skipping CNAME, it would close a CNAME loop and dnsmasq does not start with one",
				recordFields(record)...)
			return 0, nil
		}
	}

	// A section this provider does not own is invisible to the plan, so nothing
	// will ever reconcile the two addresses. It also fires, once, on a section
	// a later target of the same endpoint is about to adopt.
	if record.Type == RecordTypeA && o.ownershipEnabled() {
		for _, other := range index.named(RecordTypeA, record.Name) {
			if other.record.Owner != o.ownershipID && other.record.Value != record.Value {
				o.log.Warn("adding record next to another address it does not own, dnsmasq will answer both",
					append(recordFields(record), sectionFields(other)...)...)
				break
			}
		}
	}

	section, err := o.createSection(ctx, record)
	if err != nil {
		return 0, fmt.Errorf("add %s: %w", record.Name, err)
	}

	index.add(record, section, o.ownershipID)
	o.log.Info("added record", recordFields(record)...)
	return 1, nil
}

func (o *openWRT) createSection(ctx context.Context, record DNSRecord) (string, error) {
	// What goes onto the router is the canonical spelling, so a later lookup
	// finds it whatever case the annotation that asked for it used.
	record = record.canonical()

	sectionType, nameOption, valueOption := sectionTypeDomain, optionName, optionIP
	if record.Type == RecordTypeCNAME {
		sectionType, nameOption, valueOption = sectionTypeCName, optionCName, optionTarget
	}
	values := map[string]string{nameOption: record.Name, valueOption: record.Value}

	if o.ownershipEnabled() {
		values[ownershipOption] = o.ownershipID
	}

	var data struct {
		Section string `json:"section"`
	}
	args := map[string]any{"config": uciConfig, "type": sectionType, "values": values}
	if err := o.ubus.Call(ctx, "uci", "add", args, &data); err != nil {
		return "", err
	}
	if data.Section == "" {
		return "", errors.New("uci add returned no section")
	}

	return data.Section, nil
}

func (o *openWRT) setOwner(ctx context.Context, section string) error {
	args := map[string]any{"config": uciConfig, "section": section, "values": map[string]string{ownershipOption: o.ownershipID}}
	return o.ubus.Call(ctx, "uci", "set", args, nil)
}

func recordFields(record DNSRecord) []any {
	return []any{
		slog.String("name", record.Name),
		slog.String("type", record.Type),
		slog.String("value", record.Value),
	}
}

// sectionFields describes the section in the way of a record.
func sectionFields(section indexedSection) []any {
	return []any{
		slog.String("section", section.name),
		slog.String("section_value", section.record.Value),
		slog.String("section_owner", section.record.Owner),
	}
}
