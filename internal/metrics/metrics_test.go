package metrics_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edersonangelo/charon/internal/metrics"
)

type snapshot struct {
	taken metrics.Snapshot
	err   error
}

func (s snapshot) Snapshot(context.Context) (metrics.Snapshot, error) {
	return s.taken, s.err
}

func scrape(t *testing.T, source metrics.Source) (int, string) {
	t.Helper()

	mux := http.NewServeMux()
	metrics.New(source, "1.2.3", nil).Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	return rec.Code, rec.Body.String()
}

func TestAScrapeReadsAsPrometheusExpects(t *testing.T) {
	status, body := scrape(t, snapshot{taken: metrics.Snapshot{
		Events: []metrics.Measure{
			{Tenant: "acme", Kind: "valid", Value: 12},
			{Tenant: "acme", Kind: "invalid", Value: 3},
		},
		Deliveries:    []metrics.Measure{{Tenant: "acme", Kind: "pending", Value: 4}},
		Attempts:      []metrics.Measure{{Tenant: "acme", Value: 40}},
		OldestPending: []metrics.Measure{{Tenant: "acme", Value: 91.5}},
		Tenants:       2,
		Operators:     5,
	}})

	if status != http.StatusOK {
		t.Fatalf("got %d, want 200", status)
	}

	for _, want := range []string{
		"# TYPE charon_events_total gauge",
		`charon_events_total{tenant="acme",signature="valid"} 12`,
		`charon_events_total{tenant="acme",signature="invalid"} 3`,
		`charon_deliveries_total{tenant="acme",state="pending"} 4`,
		"# TYPE charon_delivery_attempts_total counter",
		`charon_delivery_attempts_total{tenant="acme"} 40`,
		`charon_oldest_pending_seconds{tenant="acme"} 91.5`,
		"charon_tenants 2",
		"charon_operators 5",
		`charon_build_info{version="1.2.3"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scrape is missing %q\n%s", want, body)
		}
	}
}

// Two scrapes of the same numbers have to read the same, or a diff of them is
// noise instead of a change.
func TestAScrapeIsStable(t *testing.T) {
	source := snapshot{taken: metrics.Snapshot{
		Events: []metrics.Measure{
			{Tenant: "zeta", Kind: "valid", Value: 1},
			{Tenant: "acme", Kind: "valid", Value: 2},
			{Tenant: "acme", Kind: "missing", Value: 3},
		},
	}}

	_, first := scrape(t, source)
	_, again := scrape(t, source)
	if first != again {
		t.Errorf("two scrapes differ:\n%s\n---\n%s", first, again)
	}
}

// A tenant slug cannot break the format, whatever it is called.
func TestALabelIsEscaped(t *testing.T) {
	_, body := scrape(t, snapshot{taken: metrics.Snapshot{
		Attempts: []metrics.Measure{{Tenant: `we"ird\one`, Value: 1}},
	}})

	if !strings.Contains(body, `tenant="we\"ird\\one"`) {
		t.Errorf("the label is not escaped:\n%s", body)
	}
}

// A metric with nothing in it is left out rather than reported as zero, which
// would be a claim nobody made.
func TestNothingMeasuredIsNotReportedAsZero(t *testing.T) {
	_, body := scrape(t, snapshot{taken: metrics.Snapshot{Tenants: 1}})

	if strings.Contains(body, "charon_events_total") {
		t.Errorf("a metric with no measures was reported:\n%s", body)
	}
	if !strings.Contains(body, "charon_tenants 1") {
		t.Errorf("the measures that exist are missing:\n%s", body)
	}
}

// Prometheus refuses a scrape that names the same series twice, so a bug that
// counts one tenant under every tenant's name takes the whole endpoint down
// rather than showing a wrong number.
func TestNoSeriesIsReportedTwice(t *testing.T) {
	_, body := scrape(t, snapshot{taken: metrics.Snapshot{
		Events: []metrics.Measure{
			{Tenant: "acme", Kind: "valid", Value: 1},
			{Tenant: "globex", Kind: "valid", Value: 2},
		},
		Attempts: []metrics.Measure{
			{Tenant: "acme", Value: 3},
			{Tenant: "globex", Value: 4},
		},
	}})

	seen := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		series, _, _ := strings.Cut(line, " ")
		if seen[series] {
			t.Errorf("%s is reported twice:\n%s", series, body)
		}
		seen[series] = true
	}
}

func TestAScrapeThatCannotBeReadSaysSo(t *testing.T) {
	status, _ := scrape(t, snapshot{err: errors.New("the database is away")})

	if status != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503", status)
	}
}
