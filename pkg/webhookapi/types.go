// Package webhookapi defines the ExternalDNS webhook provider wire contract.
//
// These types exist so this webhook does not have to import
// sigs.k8s.io/external-dns. That module is the controller, not a client SDK:
// pulling in its `endpoint`/`plan`/`provider` packages drags k8s.io/apimachinery
// (for CRD types this webhook never touches), klog, part of the AWS SDK and,
// from v0.21, istio and contour — 429 linked packages and a 30 MB binary for
// three structs.
//
// The contract itself is a stable JSON API pinned by the media type
// `application/external.dns.webhook+json;version=1`, so mirroring it here costs
// nothing in compatibility. Field names and JSON tags follow the
// `endpoint`/`plan` package definitions, which are what the controller actually
// encodes and decodes; `api/webhook.yaml` documents the same API but has drifted
// from the code in places, and where the two disagree the code wins — see
// DomainFilter below. types_test.go pins every field against the wire.
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
// `plan.Changes` carries lower-camel JSON tags since ExternalDNS PR #5355
// (`fix(webhook): api json object plan.Changes case`); before that it had no
// tags at all and Go's default marshalling put `Create`/`UpdateOld`/… on the
// wire. Both spellings decode into these fields — encoding/json matches keys
// case-insensitively — so an older controller is still understood.
type Changes struct {
	Create    []*Endpoint `json:"create,omitempty"`
	UpdateOld []*Endpoint `json:"updateOld,omitempty"`
	UpdateNew []*Endpoint `json:"updateNew,omitempty"`
	Delete    []*Endpoint `json:"delete,omitempty"`
}

// Empty reports whether the change set asks for nothing.
func (c *Changes) Empty() bool {
	if c == nil {
		return true
	}
	return len(c.Create)+len(c.UpdateOld)+len(c.UpdateNew)+len(c.Delete) == 0
}

// DomainFilter is the negotiation response served from `GET /`.
//
// The wire format is the one ExternalDNS parses, not the one `api/webhook.yaml`
// documents. That document still shows `{"filters": [...]}`, but the controller
// decodes this body into `endpoint.DomainFilter`, whose UnmarshalJSON reads
// `include`/`exclude` (or `regexInclude`/`regexExclude`) and knows no `filters`
// key at all — a response using it is silently discarded and the controller
// ends up with an empty filter. That has been the shape since v0.14, when the
// webhook provider was introduced.
//
// The regex variant is deliberately not mirrored: this provider serves no
// filter of its own, and the two forms are mutually exclusive upstream.
type DomainFilter struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}
