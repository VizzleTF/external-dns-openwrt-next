// Package status mirrors what this provider knows about each record into
// OpenWrtRecord objects, so `kubectl get openwrtrecords` shows whether a record
// made it onto the router and, if not, why.
//
// The mirror is read-only and best effort. Nothing reads it back: the router
// stays the only source of truth, and an API server that is down or refuses a
// write costs a warning, never a change set.
package status

import "slices"

// The states a record can be in, as the State column prints them.
const (
	// Ready: the router holds the record as ExternalDNS asks for it.
	Ready = "Ready"
	// Pending: ExternalDNS asked for the record and the router does not hold
	// it, or holds other targets, yet.
	Pending = "Pending"
	// Conflict: the provider left the record out so that dnsmasq keeps
	// starting, and will do so on every run until the conflict is resolved.
	Conflict = "Conflict"
	// Error: the last change set carrying the record failed.
	Error = "Error"
	// Rejected: AdjustEndpoints dropped the record, so it is never planned.
	Rejected = "Rejected"
	// Orphan: the router holds a record of this provider that ExternalDNS no
	// longer asks for. policy=sync deletes it, upsert-only leaves it.
	Orphan = "Orphan"
)

// Record is one row of the mirror.
type Record struct {
	DNSName    string
	RecordType string
	// Targets are sorted, so that two snapshots of one record compare equal.
	Targets []string
	State   string
	Reason  string
}

func (r Record) equal(other Record) bool {
	return r.DNSName == other.DNSName && r.RecordType == other.RecordType &&
		r.State == other.State && r.Reason == other.Reason && slices.Equal(r.Targets, other.Targets)
}

type Config struct {
	// Enabled turns the mirror on. It needs the OpenWrtRecord CRD and RBAC
	// to write it, so it is off by default.
	Enabled bool `mapstructure:"enabled"`
	// Namespace the objects are written to. Empty means the pod's own.
	Namespace string `mapstructure:"namespace"`
}

func DefaultConfig() *Config {
	return &Config{}
}
