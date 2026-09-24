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
	Type string
	// Name is the owner name: `name` of a domain section, `cname` of a cname
	// section.
	Name string
	// Value is the right-hand side: `ip` of a domain section, `target` of a
	// cname section.
	Value string

	// Owner is the value of the ownership option on the section, empty when the
	// section carries no marker. Only meaningful on records read back from the
	// router — writes take the ID from the provider config.
	Owner string
}

// CanonicalName reduces a DNS name to what a comparison should see: trimmed,
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
func CanonicalName(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

// Key is the full identity of a record: type plus BOTH sides of the mapping.
//
// Matching on the name alone (as this provider used to do) is wrong for
// endpoints that carry several targets — every UCI section sharing the name
// looks identical, so an arbitrary one gets deleted. UCI section order is not
// stable either, since `uci get_all` is unmarshalled into a map.
func (r DNSRecord) Key() string {
	c := r.canonical()
	return c.Type + "|" + c.Name + "|" + c.Value
}

// canonical returns the record as it should be written to UCI, so the router
// holds one spelling of a name rather than whichever one an annotation used.
// Names are canonicalised, values as written: an IP is a literal, and a CNAME
// target is a name.
func (r DNSRecord) canonical() DNSRecord {
	r.Name = CanonicalName(r.Name)
	if r.Type == RecordTypeCNAME {
		r.Value = CanonicalName(r.Value)
	}
	return r
}

// Validate reports whether the record can be written to UCI.
//
// Canonically empty, not literally: a name of "." or " " carries no more
// information than "" and must not reach the router either.
func (r DNSRecord) Validate() error {
	c := r.canonical()
	if c.Name == "" {
		return fmt.Errorf("name is required for a %s record", r.Type)
	}
	if c.Value == "" {
		return fmt.Errorf("value is required for a %s record", r.Type)
	}
	return nil
}
