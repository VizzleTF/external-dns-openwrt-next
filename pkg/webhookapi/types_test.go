package webhookapi

import (
	"encoding/json"
	"testing"
)

// These payloads are the ExternalDNS wire format, taken from
// kubernetes-sigs/external-dns `api/webhook.yaml` and the JSON tags on
// `endpoint.Endpoint` / `plan.Changes`. They are the contract this package
// mirrors, so a change in either direction has to fail here first.

const changesPayload = `{
  "Create": [
    {"dnsName":"new.example.com","targets":["1.2.3.4"],"recordType":"A","recordTTL":300}
  ],
  "UpdateOld": [
    {"dnsName":"moved.example.com","targets":["1.1.1.1"],"recordType":"A"}
  ],
  "UpdateNew": [
    {"dnsName":"moved.example.com","targets":["2.2.2.2"],"recordType":"A"}
  ],
  "Delete": [
    {"dnsName":"gone.example.com","targets":["9.9.9.9"],"recordType":"A",
     "labels":{"owner":"default"},
     "providerSpecific":[{"name":"webhook/foo","value":"bar"}],
     "setIdentifier":"id-1"}
  ]
}`

func TestChangesDecodesTheExternalDNSPayload(t *testing.T) {
	var changes Changes
	if err := json.Unmarshal([]byte(changesPayload), &changes); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got := len(changes.Create); got != 1 {
		t.Fatalf("Create: got %d, want 1", got)
	}
	if got := changes.Create[0].DNSName; got != "new.example.com" {
		t.Errorf("dnsName: got %q", got)
	}
	if got := changes.Create[0].Targets[0]; got != "1.2.3.4" {
		t.Errorf("targets: got %q", got)
	}
	if got := changes.Create[0].RecordType; got != RecordTypeA {
		t.Errorf("recordType: got %q", got)
	}
	if got := changes.Create[0].RecordTTL; got != 300 || !got.IsConfigured() {
		t.Errorf("recordTTL: got %d", got)
	}

	if got := changes.UpdateOld[0].Targets[0]; got != "1.1.1.1" {
		t.Errorf("UpdateOld target: got %q", got)
	}
	if got := changes.UpdateNew[0].Targets[0]; got != "2.2.2.2" {
		t.Errorf("UpdateNew target: got %q", got)
	}

	deleted := changes.Delete[0]
	if got := deleted.Labels["owner"]; got != "default" {
		t.Errorf("labels: got %q", got)
	}
	if got := deleted.ProviderSpecific[0].Name; got != "webhook/foo" {
		t.Errorf("providerSpecific name: got %q", got)
	}
	if got := deleted.ProviderSpecific[0].Value; got != "bar" {
		t.Errorf("providerSpecific value: got %q", got)
	}
	if got := deleted.SetIdentifier; got != "id-1" {
		t.Errorf("setIdentifier: got %q", got)
	}
}

func TestEndpointRoundTripKeepsFieldNames(t *testing.T) {
	in := &Endpoint{
		DNSName:          "a.example.com",
		Targets:          Targets{"1.2.3.4"},
		RecordType:       RecordTypeA,
		SetIdentifier:    "id",
		RecordTTL:        60,
		Labels:           Labels{"k": "v"},
		ProviderSpecific: ProviderSpecific{{Name: "n", Value: "v"}},
	}

	encoded, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var generic map[string]any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, key := range []string{
		"dnsName", "targets", "recordType", "setIdentifier",
		"recordTTL", "labels", "providerSpecific",
	} {
		if _, ok := generic[key]; !ok {
			t.Errorf("missing wire field %q in %s", key, encoded)
		}
	}
}

func TestUnsetTTLIsOmittedAndNotConfigured(t *testing.T) {
	encoded, err := json.Marshal(&Endpoint{DNSName: "a.example.com", RecordType: RecordTypeA})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var generic map[string]any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if _, ok := generic["recordTTL"]; ok {
		t.Errorf("an unset TTL must not be serialised: %s", encoded)
	}
	if TTL(0).IsConfigured() {
		t.Error("TTL(0) must not count as configured")
	}
}

func TestDomainFilterAlwaysSerialisesFilters(t *testing.T) {
	// The negotiation response must carry the key even when empty, otherwise
	// ExternalDNS cannot deserialise the filter.
	encoded, err := json.Marshal(DomainFilter{Filters: []string{}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	if string(encoded) != `{"filters":[]}` {
		t.Errorf("got %s, want {\"filters\":[]}", encoded)
	}
}

func TestChangesEmpty(t *testing.T) {
	var nilChanges *Changes
	if !nilChanges.Empty() {
		t.Error("nil Changes must be empty")
	}
	if !(&Changes{}).Empty() {
		t.Error("zero Changes must be empty")
	}
	if (&Changes{Delete: []*Endpoint{{DNSName: "a"}}}).Empty() {
		t.Error("Changes with a deletion is not empty")
	}
}
