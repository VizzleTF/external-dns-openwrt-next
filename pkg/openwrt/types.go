package openwrt

import (
	"fmt"
	"strings"
)

// Record types as exposed to the provider layer. On the wire (UCI) they are
// section types `domain` and `cname`; GetDNSRecords normalises them.
const (
	RecordTypeA     = "A"
	RecordTypeCNAME = "CNAME"

	sectionTypeDomain = "domain"
	sectionTypeCName  = "cname"

	optionSectionType = ".type"
	optionName        = "name"
	optionIP          = "ip"
	optionCName       = "cname"
	optionTarget      = "target"
)

// DNSRecord is a single DNS record, one UCI section.
//
// A `domain` section maps to an A record and a `cname` section to a CNAME; each
// holds exactly one value, so an endpoint with several targets spans several
// sections.
type DNSRecord struct {
	Type   string
	IP     string
	Name   string
	CName  string
	Target string

	// Owner is the value of the ownership option on the section, empty when the
	// section carries no marker. Only meaningful on records read back from the
	// router — writes take the ID from the provider config.
	Owner string
}

// canonicalName reduces a DNS name to what a comparison should see: trimmed,
// lower-cased, without the trailing dot.
//
// ExternalDNS compares names exactly that way — `internal/idna.NormalizeDNSName`,
// which the plan has used for every name since v0.18 — while UCI stores whatever
// it was handed. Without this, a `NAS.lan` typed into LuCI and an endpoint
// asking for `nas.lan` are two different records: adoption misses the existing
// section and writes a second one for a name dnsmasq already answers.
//
// Non-ASCII names are passed through as they are. ExternalDNS maps those to
// punycode via golang.org/x/net/idna, and this binary links no third-party
// packages; such a name would not resolve through dnsmasq either.
func canonicalName(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

// CanonicalName is canonicalName for the provider layer, which has to group
// sections the same way this package matches them.
func CanonicalName(name string) string { return canonicalName(name) }

// Key is the full identity of a record: type plus BOTH sides of the mapping.
//
// Matching on the name alone (as this provider used to do) is wrong for
// endpoints that carry several targets — every UCI section sharing the name
// looks identical, so an arbitrary one gets deleted. UCI section order is not
// stable either, since `uci get_all` is unmarshalled into a map.
//
// Names are compared canonically, values as written: an IP is a literal, and a
// CNAME target is a name.
func (r DNSRecord) Key() string {
	switch r.Type {
	case RecordTypeA:
		return fmt.Sprintf("%s|%s|%s", RecordTypeA, canonicalName(r.Name), r.IP)
	case RecordTypeCNAME:
		return fmt.Sprintf("%s|%s|%s", RecordTypeCNAME, canonicalName(r.CName), canonicalName(r.Target))
	default:
		return fmt.Sprintf("%s|%s|%s|%s|%s", r.Type,
			canonicalName(r.Name), r.IP, canonicalName(r.CName), canonicalName(r.Target))
	}
}

// canonical returns the record as it should be written to UCI, so the router
// holds one spelling of a name rather than whichever one an annotation used.
func (r DNSRecord) canonical() DNSRecord {
	r.Name = canonicalName(r.Name)
	r.CName = canonicalName(r.CName)
	r.Target = canonicalName(r.Target)
	return r
}

// DNSName returns the owner name of the record regardless of its type.
func (r DNSRecord) DNSName() string {
	if r.Type == RecordTypeCNAME {
		return r.CName
	}
	return r.Name
}

// Value returns the right-hand side of the record regardless of its type.
func (r DNSRecord) Value() string {
	if r.Type == RecordTypeCNAME {
		return r.Target
	}
	return r.IP
}

// Validate reports whether the record can be written to UCI.
func (r DNSRecord) Validate() error {
	switch r.Type {
	case RecordTypeA:
		// Canonically empty, not literally: a name of "." or " " carries no
		// more information than "" and must not reach the router either.
		if canonicalName(r.Name) == "" {
			return fmt.Errorf("name is required for an %s record", RecordTypeA)
		}
		if r.IP == "" {
			return fmt.Errorf("ip is required for an %s record", RecordTypeA)
		}
	case RecordTypeCNAME:
		if canonicalName(r.CName) == "" {
			return fmt.Errorf("cname is required for a %s record", RecordTypeCNAME)
		}
		if canonicalName(r.Target) == "" {
			return fmt.Errorf("target is required for a %s record", RecordTypeCNAME)
		}
	default:
		return fmt.Errorf("invalid record type: %s", r.Type)
	}

	return nil
}
