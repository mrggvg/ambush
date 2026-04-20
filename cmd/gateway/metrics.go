package main

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds all Prometheus instruments for the gateway.
// All methods are nil-safe — pass nil to disable metrics without branching at call sites.
type Metrics struct {
	exitnodesActive   prometheus.Gauge
	streamsActive     *prometheus.GaugeVec   // label: exitnode_id
	dialsTotal        *prometheus.CounterVec // label: result
	streamErrorsTotal *prometheus.CounterVec // label: exitnode_id
	credLimitExceeded prometheus.Counter
}

// NewMetrics registers all instruments against reg and returns a Metrics instance.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	f := promauto.With(reg)
	return &Metrics{
		exitnodesActive: f.NewGauge(prometheus.GaugeOpts{
			Name: "ambush_exitnodes_active",
			Help: "Number of exit nodes currently connected.",
		}),
		streamsActive: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ambush_streams_active",
			Help: "Number of proxy streams currently open, by exit node.",
		}, []string{"exitnode_id"}),
		dialsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "ambush_dials_total",
			Help: "Total dial attempts by result (success | no_exitnodes | stream_error | rate_limited).",
		}, []string{"result"}),
		streamErrorsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "ambush_stream_errors_total",
			Help: "Total stream open failures (yamux session died between selection and open), by exit node.",
		}, []string{"exitnode_id"}),
		credLimitExceeded: f.NewCounter(prometheus.CounterOpts{
			Name: "ambush_credential_limit_exceeded_total",
			Help: "Total times a credential hit its concurrent stream limit.",
		}),
	}
}

// MetricsHandler serves the Prometheus exposition format for reg.
func MetricsHandler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

// nil-safe instrumentation — all methods are no-ops when m is nil.

func (m *Metrics) incExitnodes()                        { if m != nil { m.exitnodesActive.Inc() } }
func (m *Metrics) decExitnodes()                        { if m != nil { m.exitnodesActive.Dec() } }
func (m *Metrics) incStreams(id string)                 { if m != nil { m.streamsActive.WithLabelValues(id).Inc() } }
func (m *Metrics) decStreams(id string)                 { if m != nil { m.streamsActive.WithLabelValues(id).Dec() } }
func (m *Metrics) deleteExitnodeStreams(id string)      { if m != nil { m.streamsActive.DeleteLabelValues(id) } }
func (m *Metrics) incStreamErrors(id string)            { if m != nil { m.streamErrorsTotal.WithLabelValues(id).Inc() } }
func (m *Metrics) incCredLimitExceeded()                { if m != nil { m.credLimitExceeded.Inc() } }
func (m *Metrics) incDials(result string) { if m != nil { m.dialsTotal.WithLabelValues(result).Inc() } }
