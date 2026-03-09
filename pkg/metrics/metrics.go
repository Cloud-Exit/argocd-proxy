package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Server-side metrics.
var (
	ProxyRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "proxy_requests_total",
		Help: "Total number of proxied HTTP requests.",
	}, []string{"cluster", "method", "code"})

	ProxyRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "proxy_request_duration_seconds",
		Help:    "Histogram of proxy request latencies.",
		Buckets: prometheus.DefBuckets,
	}, []string{"cluster"})

	TunnelActiveConnections = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tunnel_active_connections",
		Help: "Number of active tunnel connections per cluster.",
	}, []string{"cluster"})

	AgentsConnected = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "agents_connected",
		Help: "Number of agents currently connected to the server.",
	})

	ProxyRateLimitedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "proxy_rate_limited_total",
		Help: "Total number of proxy requests that were rate limited.",
	})

	AuthFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auth_failures_total",
		Help: "Total authentication failures.",
	}, []string{"endpoint"})
)

// Agent-side metrics.
var (
	AgentConnected = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "agent_connected",
		Help: "Whether the agent is currently connected (1) or not (0).",
	})

	AgentReconnectsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "agent_reconnects_total",
		Help: "Total number of agent reconnection attempts.",
	})

	AgentUpstreamRequestsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "agent_upstream_requests_total",
		Help: "Total number of requests forwarded to the upstream Kubernetes API.",
	})

	AgentUpstreamErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "agent_upstream_errors_total",
		Help: "Total number of errors when forwarding to the upstream Kubernetes API.",
	})
)
