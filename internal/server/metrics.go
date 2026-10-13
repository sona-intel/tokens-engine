package server

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	Registry *prometheus.Registry
	requests *prometheus.HistogramVec
}

// NewMetrics registers the request histogram, the app adds the gauges of the sources and the index to the same registry
func NewMetrics() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		requests: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "tokens_request_duration_seconds",
			Help:    "Duration of served requests",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"route", "code"}),
	}
	m.Registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), m.requests)
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

func (m *Metrics) observe(route string, status int, seconds float64) {
	m.requests.WithLabelValues(route, strconv.Itoa(status)).Observe(seconds)
}
