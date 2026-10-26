package metrics

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// poolStater is the subset of *pgxpool.Pool this collector needs.
type poolStater interface {
	Stat() *pgxpool.Stat
}

// dbPoolCollector reads pgxpool.Pool.Stat() on every scrape rather than
// polling on a timer — Stat() is a cheap atomic-counter read, and this way
// the exposed value is never stale between scrapes.
type dbPoolCollector struct {
	pool poolStater

	total    *prometheus.Desc
	idle     *prometheus.Desc
	acquired *prometheus.Desc
	maxConns *prometheus.Desc
}

// RegisterDBPool wires pool's live stats into the default registry under
// cloud_db_pool_*. Call once from main.go after the pool is created.
func RegisterDBPool(pool poolStater) {
	c := &dbPoolCollector{
		pool:     pool,
		total:    prometheus.NewDesc("cloud_db_pool_conns_total", "Total DB connections currently held by the pool (idle + in use).", nil, nil),
		idle:     prometheus.NewDesc("cloud_db_pool_conns_idle", "Idle DB connections in the pool.", nil, nil),
		acquired: prometheus.NewDesc("cloud_db_pool_conns_acquired", "DB connections currently checked out by a query.", nil, nil),
		maxConns: prometheus.NewDesc("cloud_db_pool_conns_max", "Configured maximum pool size.", nil, nil),
	}
	prometheus.MustRegister(c)
}

func (c *dbPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.total
	ch <- c.idle
	ch <- c.acquired
	ch <- c.maxConns
}

func (c *dbPoolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	ch <- prometheus.MustNewConstMetric(c.total, prometheus.GaugeValue, float64(s.TotalConns()))
	ch <- prometheus.MustNewConstMetric(c.idle, prometheus.GaugeValue, float64(s.IdleConns()))
	ch <- prometheus.MustNewConstMetric(c.acquired, prometheus.GaugeValue, float64(s.AcquiredConns()))
	ch <- prometheus.MustNewConstMetric(c.maxConns, prometheus.GaugeValue, float64(s.MaxConns()))
}
