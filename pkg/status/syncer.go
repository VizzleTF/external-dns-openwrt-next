package status

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/VizzleTF/external-dns-openwrt-next/pkg/metrics"
)

// Syncer writes the latest snapshot of records to the API server from one
// background goroutine, so a slow or unreachable API server never holds up a
// request from ExternalDNS.
type Syncer struct {
	client *Client
	log    *slog.Logger
	errors *metrics.Counter
	now    func() time.Time

	// kick has room for one: snapshots published while a sync runs collapse
	// into a single follow-up that writes the newest.
	kick   chan struct{}
	mu     sync.Mutex
	latest []Record

	// written is what the API server holds, as far as this process knows, so
	// that only what changed is written. listed says it has been read once.
	written map[string]writtenRecord
	listed  bool
}

type writtenRecord struct {
	record Record
	since  string
}

func NewSyncer(client *Client, log *slog.Logger, registry *metrics.Registry) *Syncer {
	return &Syncer{
		client: client,
		log:    log,
		errors: registry.Counter(metrics.Prefix+"_status_errors_total",
			"Failed writes of OpenWrtRecord objects, by operation.", "operation"),
		now:     time.Now,
		kick:    make(chan struct{}, 1),
		written: map[string]writtenRecord{},
	}
}

// Publish replaces the snapshot to write. It never blocks.
func (s *Syncer) Publish(records []Record) {
	s.mu.Lock()
	s.latest = records
	s.mu.Unlock()

	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run writes snapshots until ctx is done. A failed write is retried with the
// next snapshot, which ExternalDNS triggers on every run.
func (s *Syncer) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.kick:
		}

		s.mu.Lock()
		records := s.latest
		s.mu.Unlock()

		s.sync(ctx, records)
	}
}

func (s *Syncer) sync(ctx context.Context, records []Record) {
	// What a previous process wrote: kept where it still holds, so its
	// transition times survive a restart, and deleted where it does not.
	if !s.listed {
		objects, err := s.client.list(ctx)
		if err != nil {
			s.fail("list", "", err)
			return
		}
		for _, obj := range objects {
			s.written[obj.Metadata.Name] = writtenRecord{
				record: Record{
					DNSName:    obj.Spec.DNSName,
					RecordType: obj.Spec.RecordType,
					Targets:    obj.Spec.Targets,
					State:      obj.Status.State,
					Reason:     obj.Status.Reason,
				},
				since: obj.Status.LastTransitionTime,
			}
		}
		s.listed = true
	}

	wanted := make(map[string]bool, len(records))
	for _, record := range records {
		name := objectName(record)
		wanted[name] = true

		previous, ok := s.written[name]
		if ok && previous.record.equal(record) {
			continue
		}
		since := previous.since
		if !ok || previous.record.State != record.State || since == "" {
			since = s.now().UTC().Format(time.RFC3339)
		}

		obj := &object{}
		obj.Metadata.Name = name
		obj.Spec.DNSName, obj.Spec.RecordType, obj.Spec.Targets = record.DNSName, record.RecordType, record.Targets
		obj.Status.State, obj.Status.Reason, obj.Status.LastTransitionTime = record.State, record.Reason, since
		if err := s.client.apply(ctx, obj); err != nil {
			s.fail("apply", name, err)
			continue
		}
		s.written[name] = writtenRecord{record: record, since: since}
	}

	for name := range s.written {
		if wanted[name] {
			continue
		}
		if err := s.client.remove(ctx, name); err != nil {
			s.fail("delete", name, err)
			continue
		}
		delete(s.written, name)
	}
}

func (s *Syncer) fail(operation, name string, err error) {
	s.errors.Inc(operation)
	s.log.Warn("could not update record status",
		slog.String("operation", operation), slog.String("object", name), slog.Any("error", err))
}

// dns1123 is a Kubernetes object name: a DNS-1123 subdomain.
var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// objectName is `<type>.<name>`, which reads well in `kubectl get`. A name
// Kubernetes would not accept — one AdjustEndpoints rejected, mostly — is
// hashed instead.
func objectName(record Record) string {
	name := strings.ToLower(record.RecordType + "." + record.DNSName)
	if len(name) <= 253 && dns1123.MatchString(name) {
		return name
	}
	sum := sha256.Sum256([]byte(record.RecordType + "|" + record.DNSName))
	return strings.ToLower(record.RecordType) + "-" + hex.EncodeToString(sum[:])[:16]
}
