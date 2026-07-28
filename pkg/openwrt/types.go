package openwrt

import "fmt"

// Record types as exposed to the provider layer. On the wire (UCI) they are
// section types `domain` and `cname`; GetDNSRecords normalises them.
const (
	RecordTypeA     = "A"
	RecordTypeCNAME = "CNAME"

	sectionTypeDomain = "domain"
	sectionTypeCName  = "cname"
)

// DNSRecord represents a DNS record in LuciRPC
type DNSRecord struct {
	Type   string `json:".type" validate:"required"`
	IP     string `json:"ip,omitempty"`
	Name   string `json:"name,omitempty"`
	CName  string `json:"cname,omitempty"`
	Target string `json:"target,omitempty"`
}

// Key is the full identity of a record: type plus BOTH sides of the mapping.
//
// Matching on the name alone (as this provider used to do) is wrong for
// endpoints that carry several targets — every UCI section sharing the name
// looks identical, so an arbitrary one gets deleted. UCI section order is not
// stable either, since `uci get_all` is unmarshalled into a map.
func (r DNSRecord) Key() string {
	switch r.Type {
	case RecordTypeA:
		return fmt.Sprintf("%s|%s|%s", RecordTypeA, r.Name, r.IP)
	case RecordTypeCNAME:
		return fmt.Sprintf("%s|%s|%s", RecordTypeCNAME, r.CName, r.Target)
	default:
		return fmt.Sprintf("%s|%s|%s|%s|%s", r.Type, r.Name, r.IP, r.CName, r.Target)
	}
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
		if r.Name == "" {
			return fmt.Errorf("name is required for an %s record", RecordTypeA)
		}
		if r.IP == "" {
			return fmt.Errorf("ip is required for an %s record", RecordTypeA)
		}
	case RecordTypeCNAME:
		if r.CName == "" {
			return fmt.Errorf("cname is required for a %s record", RecordTypeCNAME)
		}
		if r.Target == "" {
			return fmt.Errorf("target is required for a %s record", RecordTypeCNAME)
		}
	default:
		return fmt.Errorf("invalid record type: %s", r.Type)
	}

	return nil
}
