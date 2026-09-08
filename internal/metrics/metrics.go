package metrics

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
)

// Measure is one number with the labels that tell it apart.
type Measure struct {
	Tenant string
	// Kind is the second label when a metric has one: the signature state of
	// an event, or the state of a delivery. Empty when it does not.
	Kind  string
	Value float64
}

// Snapshot is everything one scrape reports, read together so the numbers in
// it belong to the same moment.
type Snapshot struct {
	Events        []Measure
	Deliveries    []Measure
	Attempts      []Measure
	OldestPending []Measure
	Tenants       float64
	Operators     float64
}

// Source is where a snapshot comes from. Declared here, by the side that
// consumes it, so this package depends on no storage.
type Source interface {
	Snapshot(ctx context.Context) (Snapshot, error)
}

type Handler struct {
	source  Source
	version string
	logger  *slog.Logger
}

func New(source Source, version string, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Handler{source: source, version: version, logger: logger}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /metrics", h.scrape)
}

func (h *Handler) scrape(w http.ResponseWriter, r *http.Request) {
	snapshot, err := h.source.Snapshot(r.Context())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "could not read the metrics", "error", err)
		http.Error(w, "the metrics could not be read", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	Write(w, snapshot, h.version)
}

// Write renders the exposition format Prometheus reads. It is a few lines of
// text, which is why nothing is imported to produce it.
func Write(w io.Writer, snapshot Snapshot, version string) {
	metric(w, "charon_build_info", "gauge",
		"the version this process was built from",
		[]Measure{{Kind: version, Value: 1}}, "version")

	metric(w, "charon_events_total", "gauge",
		"events recorded, by how their signature checked out",
		snapshot.Events, "signature")
	metric(w, "charon_deliveries_total", "gauge",
		"deliveries, by the state they are in",
		snapshot.Deliveries, "state")
	metric(w, "charon_delivery_attempts_total", "counter",
		"attempts made to hand a delivery over",
		snapshot.Attempts, "")
	metric(w, "charon_oldest_pending_seconds", "gauge",
		"how long the delivery that has been due longest has been waiting",
		snapshot.OldestPending, "")

	metric(w, "charon_tenants", "gauge", "tenants configured",
		[]Measure{{Value: snapshot.Tenants}}, "")
	metric(w, "charon_operators", "gauge", "operators who can sign in",
		[]Measure{{Value: snapshot.Operators}}, "")
}

func metric(w io.Writer, name, kind, help string, measures []Measure, second string) {
	if len(measures) == 0 {
		return
	}

	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)

	lines := make([]string, 0, len(measures))
	for _, measure := range measures {
		var labels []string
		if measure.Tenant != "" {
			labels = append(labels, `tenant="`+escape(measure.Tenant)+`"`)
		}
		if second != "" && measure.Kind != "" {
			labels = append(labels, second+`="`+escape(measure.Kind)+`"`)
		}

		if len(labels) == 0 {
			lines = append(lines, fmt.Sprintf("%s %g", name, measure.Value))
			continue
		}
		lines = append(lines,
			fmt.Sprintf("%s{%s} %g", name, strings.Join(labels, ","), measure.Value))
	}

	// Sorted so two scrapes of the same numbers read the same, which is what
	// makes a diff of them worth looking at.
	sort.Strings(lines)
	for _, line := range lines {
		_, _ = fmt.Fprintln(w, line)
	}
}

var escaping = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escape(value string) string { return escaping.Replace(value) }
