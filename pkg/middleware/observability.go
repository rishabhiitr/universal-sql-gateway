package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type Metrics struct {
	RequestCount      *prometheus.CounterVec
	RequestDuration   *prometheus.HistogramVec
	ConnectorDuration *prometheus.HistogramVec
	RateLimitHits     *prometheus.CounterVec
	CacheHits         *prometheus.CounterVec
}

func NewMetrics(registerer prometheus.Registerer) *Metrics {
	m := &Metrics{
		RequestCount: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "query_gateway_requests_total",
				Help: "Total number of gateway HTTP requests.",
			},
			[]string{"method", "path", "status"},
		),
		RequestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "query_gateway_request_duration_seconds",
				Help:    "Latency of gateway HTTP requests.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"method", "path"},
		),
		ConnectorDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "query_connector_duration_seconds",
				Help:    "Latency of connector fetch calls.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"connector"},
		),
		RateLimitHits: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "query_rate_limit_hits_total",
				Help: "Number of rate-limited connector requests.",
			},
			[]string{"connector"},
		),
		CacheHits: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "query_cache_hits_total",
				Help: "Number of query source cache hits.",
			},
			[]string{"connector"},
		),
	}

	registerer.MustRegister(
		m.RequestCount,
		m.RequestDuration,
		m.ConnectorDuration,
		m.RateLimitHits,
		m.CacheHits,
	)
	return m
}

func (m *Metrics) HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(recorder, r)

		duration := time.Since(start).Seconds()
		path := r.URL.Path
		m.RequestDuration.WithLabelValues(r.Method, path).Observe(duration)
		m.RequestCount.WithLabelValues(r.Method, path, strconv.Itoa(recorder.status)).Inc()
	})
}

func (m *Metrics) ObserveConnector(connector string, d time.Duration) {
	m.ConnectorDuration.WithLabelValues(connector).Observe(d.Seconds())
}

func (m *Metrics) IncRateLimit(connector string) {
	m.RateLimitHits.WithLabelValues(connector).Inc()
}

func (m *Metrics) IncCacheHit(connector string) {
	m.CacheHits.WithLabelValues(connector).Inc()
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
