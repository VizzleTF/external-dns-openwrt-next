// Package webhookapi defines the ExternalDNS webhook provider wire contract.
//
// These types exist so this webhook does not have to import
// sigs.k8s.io/external-dns. That module is the controller, not a client SDK:
// pulling in its `endpoint`/`plan`/`provider` packages drags k8s.io/apimachinery
// (for CRD types this webhook never touches), klog, part of the AWS SDK and,
// from v0.21, istio and contour — 429 linked packages and a 30 MB binary for
// three structs.
//
// The contract itself is a stable, documented JSON API pinned by the media type
// `application/external.dns.webhook+json;version=1`, so mirroring it here costs
// nothing in compatibility. Field names and JSON tags follow
// kubernetes-sigs/external-dns `api/webhook.yaml` and the `endpoint`/`plan`
// package definitions exactly — see types_test.go, which round-trips a payload
// captured from a live ExternalDNS.
package webhookapi

// Record types. Only A and CNAME can be written to UCI; the rest are listed
// because ExternalDNS may still send them.
const (
	RecordTypeA     = "A"
	RecordTypeAAAA  = "AAAA"
	RecordTypeCNAME = "CNAME"
	RecordTypeTXT   = "TXT"
)

// TTL is a record TTL in seconds. Zero means "not configured", matching
// ExternalDNS semantics.
type TTL int64

// IsConfigured reports whether a TTL was actually requested.
func (t TTL) IsConfigured() bool { return t > 0 }

// Targets are the right-hand side values of a record.
type Targets []string

// Labels carry ExternalDNS registry metadata. Unused here, but they must
// survive a round trip.
type Labels map[string]string

// ProviderSpecificProperty is a provider-scoped key/value annotation.
type ProviderSpecificProperty struct {
	Name  string `json:"name,omitempty"`
	Value string `json:"value,omitempty"`
}

// ProviderSpecific is the collection of provider-scoped annotations.
type ProviderSpecific []ProviderSpecificProperty

// Endpoint is a single desired or observed DNS record.
type Endpoint struct {
	DNSName          string           `json:"dnsName,omitempty"`
	Targets          Targets          `json:"targets,omitempty"`
	RecordType       string           `json:"recordType,omitempty"`
	SetIdentifier    string           `json:"setIdentifier,omitempty"`
	RecordTTL        TTL              `json:"recordTTL,omitempty"`
	Labels           Labels           `json:"labels,omitempty"`
	ProviderSpecific ProviderSpecific `json:"providerSpecific,omitempty"`
}

// Changes is one reconcile step.
//
// The field names are capitalised on the wire: `plan.Changes` in ExternalDNS
// carries no JSON tags, so Go's default marshalling applies.
type Changes struct {
	Create    []*Endpoint `json:"Create,omitempty"`
	UpdateOld []*Endpoint `json:"UpdateOld,omitempty"`
	UpdateNew []*Endpoint `json:"UpdateNew,omitempty"`
	Delete    []*Endpoint `json:"Delete,omitempty"`
}

// Empty reports whether the change set asks for nothing.
func (c *Changes) Empty() bool {
	if c == nil {
		return true
	}
	return len(c.Create)+len(c.UpdateOld)+len(c.UpdateNew)+len(c.Delete) == 0
}

// DomainFilter is the negotiation response served from `GET /`.
type DomainFilter struct {
	Filters []string `json:"filters"`
}
