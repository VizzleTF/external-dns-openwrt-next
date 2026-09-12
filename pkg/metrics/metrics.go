// Package metrics implements the optional `/metrics` endpoint of the
// ExternalDNS webhook provider specification.
//
// It is written by hand, in the Prometheus text exposition format, because the
// alternative is linking a client library: prometheus/client_golang pulls in
// its model, common and procfs packages plus protobuf, which is a large part of
// a binary that currently links nothing outside the standard library. The
// format itself is three lines per metric and has been stable for a decade.
//
// A Registry is safe for concurrent use: every value is an atomic, and the
// registry lock is only taken when a series is created or the whole set is
// rendered.
package metrics

import (
	"io"
	"math"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Prefix starts every metric name this binary exposes. Prometheus convention
// is one namespace per process, and `external_dns_` alone would collide with
// the controller's own metrics in the same pod.
const Prefix = "external_dns_openwrt"

// ContentType is the exposition format this package writes. Prometheus, the
// OpenMetrics scrapers and the ServiceMonitor in the ExternalDNS chart all
// accept it.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

type metricType string

const (
	typeCounter metricType = "counter"
	typeGauge   metricType = "gauge"
)

// Registry holds every metric this process exposes.
type Registry struct {
	mu      sync.Mutex
	metrics []*metric
}

func NewRegistry() *Registry {
	return &Registry{}
}

// metric is one name: its help text, its type, and every label combination
// seen so far.
type metric struct {
	name       string
	help       string
	kind       metricType
	labelNames []string

	mu     sync.Mutex
	series map[string]*series
}

// series is one label combination of one metric.
//
// Values are held as bits of a float64 so a gauge can be set to a timestamp or
// a fraction while a counter still increments without a lock.
type series struct {
	labelValues []string
	bits        atomic.Uint64
}

func (s *series) add(delta float64) {
	for {
		old := s.bits.Load()
		updated := math.Float64bits(math.Float64frombits(old) + delta)
		if s.bits.CompareAndSwap(old, updated) {
			return
		}
	}
}

func (s *series) set(value float64) { s.bits.Store(math.Float64bits(value)) }

func (s *series) value() float64 { return math.Float64frombits(s.bits.Load()) }

// Counter is a monotonically increasing metric.
type Counter struct{ m *metric }

// Gauge is a metric that goes up and down.
type Gauge struct{ m *metric }

// Counter registers a counter, or returns the one already registered under
// this name. Re-registering is not an error: a caller that asks twice wants the
// same series, and a panic in a metrics helper is a poor trade for a typo.
func (r *Registry) Counter(name, help string, labelNames ...string) *Counter {
	return &Counter{m: r.register(name, help, typeCounter, labelNames)}
}

// Gauge registers a gauge, or returns the one already registered.
func (r *Registry) Gauge(name, help string, labelNames ...string) *Gauge {
	return &Gauge{m: r.register(name, help, typeGauge, labelNames)}
}

func (r *Registry) register(name, help string, kind metricType, labelNames []string) *metric {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, existing := range r.metrics {
		if existing.name == name {
			return existing
		}
	}

	m := &metric{
		name:       name,
		help:       help,
		kind:       kind,
		labelNames: labelNames,
		series:     make(map[string]*series),
	}
	r.metrics = append(r.metrics, m)
	return m
}

// Inc adds one to the series with these label values, in the order the labels
// were declared.
func (c *Counter) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Add increases the series by delta. A negative delta is ignored: a counter
// that goes backwards would be read as a process restart.
func (c *Counter) Add(delta float64, labelValues ...string) {
	if delta < 0 {
		return
	}
	c.m.seriesFor(labelValues).add(delta)
}

// Set replaces the value of the series with these label values.
func (g *Gauge) Set(value float64, labelValues ...string) {
	g.m.seriesFor(labelValues).set(value)
}

// Add adds delta to the series, which may be negative.
func (g *Gauge) Add(delta float64, labelValues ...string) {
	g.m.seriesFor(labelValues).add(delta)
}

func (m *metric) seriesFor(labelValues []string) *series {
	key := strings.Join(labelValues, "\xff")

	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.series[key]; ok {
		return existing
	}

	// Copied: the caller's slice may be reused, and these values outlive it.
	values := make([]string, len(labelValues))
	copy(values, labelValues)

	created := &series{labelValues: values}
	m.series[key] = created
	return created
}

// WriteTo renders the whole registry. Metrics keep registration order and
// series are sorted by label value, so a scrape — and a test — reads the same
// way every time.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	registered := make([]*metric, len(r.metrics))
	copy(registered, r.metrics)
	r.mu.Unlock()

	var out strings.Builder
	for _, m := range registered {
		m.writeTo(&out)
	}

	n, err := io.WriteString(w, out.String())
	return int64(n), err
}

func (m *metric) writeTo(out *strings.Builder) {
	m.mu.Lock()
	all := make([]*series, 0, len(m.series))
	for _, s := range m.series {
		all = append(all, s)
	}
	m.mu.Unlock()

	if len(all) == 0 {
		return
	}

	sort.Slice(all, func(i, j int) bool {
		return strings.Join(all[i].labelValues, "\xff") < strings.Join(all[j].labelValues, "\xff")
	})

	// A HELP line with nothing after the name is a trailing space, which some
	// parsers read as an empty help string and others complain about.
	if m.help != "" {
		out.WriteString("# HELP " + m.name + " " + m.help + "\n")
	}
	out.WriteString("# TYPE " + m.name + " " + string(m.kind) + "\n")

	for _, s := range all {
		out.WriteString(m.name)
		out.WriteString(formatLabels(m.labelNames, s.labelValues))
		out.WriteString(" " + strconv.FormatFloat(s.value(), 'g', -1, 64) + "\n")
	}
}

func formatLabels(names, values []string) string {
	if len(names) == 0 {
		return ""
	}

	pairs := make([]string, 0, len(names))
	for i, name := range names {
		value := ""
		if i < len(values) {
			value = values[i]
		}
		pairs = append(pairs, name+`="`+escapeLabelValue(value)+`"`)
	}

	return "{" + strings.Join(pairs, ",") + "}"
}

// escapeLabelValue escapes the three characters the exposition format reserves
// inside a label value.
func escapeLabelValue(value string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		"\n", `\n`,
		`"`, `\"`,
	).Replace(value)
}

// Build registers the build_info gauge, the conventional way to expose what is
// running. ko stamps the module version into the build info, so a deployed
// image reports its tag without a linker flag.
func Build(r *Registry) {
	r.Gauge(Prefix+"_build_info",
		"Build information of the running webhook, always 1.",
		"version", "goversion").Set(1, buildVersion(), runtime.Version())
}

// buildVersion is the module version when there is one, and the commit
// otherwise.
//
// `go build` of a checkout reports the main module as "(devel)", which is the
// common case here: ko builds from the source tree rather than from a module
// proxy, so the tag never reaches Main.Version. The VCS stamps it does embed
// carry the revision, which is what identifies a running image anyway.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}

	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}

	var revision, modified string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}

	if revision == "" {
		return "unknown"
	}

	// Short, the way a commit is written everywhere else; a dirty tree says so,
	// because that image is not any commit.
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified == "true" {
		revision += "-dirty"
	}
	return revision
}
