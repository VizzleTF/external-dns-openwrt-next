package provider

import (
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/VizzleTF/external-dns-openwrt-next/pkg/openwrt"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/status"
	"github.com/VizzleTF/external-dns-openwrt-next/pkg/webhookapi"
)

type recordKey struct {
	name       string
	recordType string
}

func keyOf(name, recordType string) recordKey {
	return recordKey{name: openwrt.CanonicalName(name), recordType: recordType}
}

type rejection struct {
	targets []string
	reason  string
}

// tracker turns what one ExternalDNS run shows this provider into a status per
// record. A run is Records(), then AdjustEndpoints() with every endpoint the
// sources produce, then ApplyChanges() if the plan has any: together they say
// what the router holds, what is asked for, and what became of the change.
//
// A nil tracker does nothing, which is how the mirror is switched off.
type tracker struct {
	publish func([]status.Record)

	mu       sync.Mutex
	actual   map[recordKey][]string
	desired  map[recordKey][]string
	rejected map[recordKey]rejection
	// attempted holds every record a change set of this process carried.
	// AdjustEndpoints sees endpoints --domain-filter later drops, and those
	// are never planned: a record that is neither on the router nor ever
	// attempted is most likely one of them, and is not shown.
	attempted map[recordKey]bool
	failed    map[recordKey]string
	conflict  map[recordKey]string
}

func newTracker(publish func([]status.Record)) *tracker {
	if publish == nil {
		return nil
	}
	return &tracker{
		publish:   publish,
		attempted: map[recordKey]bool{},
		failed:    map[recordKey]string{},
		conflict:  map[recordKey]string{},
	}
}

func (t *tracker) observeRecords(endpoints []*webhookapi.Endpoint) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	t.actual = make(map[recordKey][]string, len(endpoints))
	for _, ep := range endpoints {
		t.actual[keyOf(ep.DNSName, ep.RecordType)] = sorted(ep.Targets)
	}
}

func (t *tracker) observeDesired(adjusted []*webhookapi.Endpoint, rejected map[recordKey]rejection) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	t.desired = make(map[recordKey][]string, len(adjusted))
	for _, ep := range adjusted {
		t.desired[keyOf(ep.DNSName, ep.RecordType)] = sorted(ep.Targets)
	}
	t.rejected = rejected
	t.publishLocked()
}

func (t *tracker) observeApply(changes *webhookapi.Changes, skipped []openwrt.Skip, err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	wanted := slices.Concat(changes.Create, changes.UpdateNew)
	for _, ep := range wanted {
		k := keyOf(ep.DNSName, ep.RecordType)
		t.attempted[k] = true
		delete(t.failed, k)
		delete(t.conflict, k)
		if err != nil {
			t.failed[k] = err.Error()
		}
	}

	if err == nil {
		for _, skip := range skipped {
			t.conflict[keyOf(skip.Record.Name, skip.Record.Type)] = skip.Reason
		}
		// What the router holds now, without waiting for the next Records().
		if t.actual != nil {
			for _, ep := range slices.Concat(changes.Delete, changes.UpdateOld) {
				delete(t.actual, keyOf(ep.DNSName, ep.RecordType))
			}
			for _, ep := range wanted {
				if k := keyOf(ep.DNSName, ep.RecordType); t.conflict[k] == "" {
					t.actual[k] = sorted(ep.Targets)
				}
			}
		}
	}
	t.publishLocked()
}

// publishLocked hands a snapshot to the mirror once a run has shown both the
// router and the desired state: one without the other would show every record
// as an orphan, or none at all.
func (t *tracker) publishLocked() {
	if t.actual == nil || t.desired == nil {
		return
	}
	t.publish(t.snapshotLocked())
}

func (t *tracker) snapshotLocked() []status.Record {
	keys := map[recordKey]bool{}
	for _, m := range []map[recordKey][]string{t.actual, t.desired} {
		for k := range m {
			keys[k] = true
		}
	}
	for k := range t.rejected {
		keys[k] = true
	}

	records := make([]status.Record, 0, len(keys))
	for _, k := range slices.SortedFunc(maps.Keys(keys), func(a, b recordKey) int {
		if c := strings.Compare(a.name, b.name); c != 0 {
			return c
		}
		return strings.Compare(a.recordType, b.recordType)
	}) {
		record := status.Record{DNSName: k.name, RecordType: k.recordType}
		have, onRouter := t.actual[k]
		want, isDesired := t.desired[k]

		if rej, ok := t.rejected[k]; ok {
			record.Targets, record.State, record.Reason = rej.targets, status.Rejected, rej.reason
		} else if !isDesired {
			record.Targets, record.State, record.Reason = have, status.Orphan, "not requested by ExternalDNS"
		} else {
			record.Targets = want
			switch {
			case onRouter && slices.Equal(have, want):
				record.State = status.Ready
			case t.conflict[k] != "":
				record.State, record.Reason = status.Conflict, t.conflict[k]
			case t.failed[k] != "":
				record.State, record.Reason = status.Error, t.failed[k]
			case onRouter:
				record.State, record.Reason = status.Pending, "router has "+strings.Join(have, ", ")
			case t.attempted[k]:
				record.State = status.Pending
			default:
				continue
			}
		}
		records = append(records, record)
	}
	return records
}

func sorted(targets []string) []string {
	return slices.Sorted(slices.Values(targets))
}
