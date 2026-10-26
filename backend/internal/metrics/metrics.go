// Package metrics exposes Prometheus instrumentation. Everything registers
// on the default registry, served by Handler on the separate metrics
// listener (config.MetricsAddr) — never on the public mux.
package metrics

import (
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cloud_http_requests_total",
		Help: "HTTP requests by route and status class.",
	}, []string{"method", "route", "status"})

	httpRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "cloud_http_request_duration_seconds",
		Help: "HTTP request latency by route, until the handler returns.",
		// Up to ~8.5 min: file transfers make long requests normal here,
		// unlike a JSON-only API where DefBuckets' 10s ceiling would do.
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 18),
	}, []string{"method", "route"})
)

// Handler serves the Prometheus exposition format.
func Handler() http.Handler {
	return promhttp.Handler()
}

// UnmatchedRoute labels requests no mux pattern matched (e.g. 405s).
const UnmatchedRoute = "unmatched"

// ObserveHTTP records one finished request. pattern must be the mux
// pattern (http.Request.Pattern, e.g. "GET /api/v1/files/{id}"), never the
// raw URL path: labelling by path would create a series per file/user ID
// and blow up the cardinality. status is reduced to its class ("2xx") for
// the same reason.
func ObserveHTTP(method, pattern string, status int, d time.Duration) {
	route := RouteLabel(pattern)
	httpRequestDuration.WithLabelValues(method, route).Observe(d.Seconds())
	httpRequestsTotal.WithLabelValues(method, route, StatusClass(status)).Inc()
}

// RouteLabel strips the method from a mux pattern ("GET /healthz" ->
// "/healthz"), since method is its own label.
func RouteLabel(pattern string) string {
	if pattern == "" {
		return UnmatchedRoute
	}
	if _, route, found := strings.Cut(pattern, " "); found {
		return route
	}
	return pattern
}

// StatusClass maps 404 -> "4xx" etc.
func StatusClass(status int) string {
	if status < 100 || status > 599 {
		return "unknown"
	}
	return string(rune('0'+status/100)) + "xx"
}
