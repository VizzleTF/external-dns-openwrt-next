package metrics

import (
	"strings"
	"sync"
	"testing"
)

// The exposition format is the contract with every scraper, so the expected
// output is written out in full rather than asserted on piece by piece.
func TestRegistryRendersTheExpositionFormat(t *testing.T) {
	registry := NewRegistry()

	requests := registry.Counter("test_requests_total", "Requests served.", "route", "status")
	requests.Inc("/records", "200")
	requests.Inc("/records", "200")
	requests.Inc("/records", "500")

	records := registry.Gauge("test_records", "Records on the router.")
	records.Set(12)

	want := `# HELP test_requests_total Requests served.
# TYPE test_requests_total counter
test_requests_total{route="/records",status="200"} 2
test_requests_total{route="/records",status="500"} 1
# HELP test_records Records on the router.
# TYPE test_records gauge
test_records 12
`

	if got := render(t, registry); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestAMetricWithNoSeriesIsNotRendered(t *testing.T) {
	registry := NewRegistry()
	registry.Counter("test_never_incremented_total", "Nothing happened.")

	// A HELP and TYPE header with no sample underneath is legal but noise: the
	// series appears as soon as something is counted.
	if got := render(t, registry); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestAMetricWithoutHelpSkipsTheHelpLine(t *testing.T) {
	registry := NewRegistry()
	registry.Gauge("test_undocumented", "").Set(1)

	want := "# TYPE test_undocumented gauge\ntest_undocumented 1\n"
	if got := render(t, registry); got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestLabelValuesAreEscaped(t *testing.T) {
	registry := NewRegistry()
	registry.Counter("test_escaping_total", "Escaping.", "value").
		Inc(`a "quoted" \ value` + "\nwith a newline")

	want := `test_escaping_total{value="a \"quoted\" \\ value\nwith a newline"} 1` + "\n"
	if got := render(t, registry); !strings.Contains(got, want) {
		t.Errorf("got:\n%s\nwant it to contain:\n%s", got, want)
	}
}

func TestGaugesAreSetAndCountersOnlyGoUp(t *testing.T) {
	registry := NewRegistry()

	gauge := registry.Gauge("test_gauge", "A gauge.")
	gauge.Set(5)
	gauge.Set(2)

	counter := registry.Counter("test_counter_total", "A counter.")
	counter.Inc()
	// Refused: a counter that goes backwards reads as a process restart.
	counter.Add(-10)

	out := render(t, registry)
	if !strings.Contains(out, "test_gauge 2\n") {
		t.Errorf("gauge: got:\n%s", out)
	}
	if !strings.Contains(out, "test_counter_total 1\n") {
		t.Errorf("counter: got:\n%s", out)
	}
}

func TestRegisteringTheSameNameTwiceReturnsTheSameMetric(t *testing.T) {
	registry := NewRegistry()

	registry.Counter("test_shared_total", "Shared.", "label").Inc("a")
	registry.Counter("test_shared_total", "Shared.", "label").Inc("a")

	out := render(t, registry)
	if !strings.Contains(out, `test_shared_total{label="a"} 2`) {
		t.Errorf("got:\n%s", out)
	}
	if strings.Count(out, "# TYPE test_shared_total") != 1 {
		t.Errorf("the metric was registered twice:\n%s", out)
	}
}

func TestConcurrentUpdatesAreNotLost(t *testing.T) {
	registry := NewRegistry()
	counter := registry.Counter("test_concurrent_total", "Concurrency.", "route")

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				counter.Inc("/records")
			}
		}()
	}
	wg.Wait()

	if got := render(t, registry); !strings.Contains(got, `test_concurrent_total{route="/records"} 5000`) {
		t.Errorf("got:\n%s", got)
	}
}

func TestBuildInfoIsAlwaysOne(t *testing.T) {
	registry := NewRegistry()
	Build(registry)

	out := render(t, registry)
	if !strings.Contains(out, Prefix+"_build_info{version=") {
		t.Fatalf("build info missing:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), " 1") {
		t.Errorf("build info must be 1:\n%s", out)
	}
}

func render(t *testing.T, registry *Registry) string {
	t.Helper()

	var out strings.Builder
	if _, err := registry.WriteTo(&out); err != nil {
		t.Fatalf("write: %v", err)
	}
	return out.String()
}
