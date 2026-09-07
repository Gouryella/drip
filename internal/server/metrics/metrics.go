package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Tunnel metrics
	TunnelCount = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "drip_tunnel_count",
		Help: "Current number of active tunnels",
	})

	TunnelRegistrations = promauto.NewCounter(prometheus.CounterOpts{
		Name: "drip_tunnel_registrations_total",
		Help: "Total number of tunnel registrations",
	})

	TunnelRegistrationFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "drip_tunnel_registration_failures_total",
		Help: "Total number of failed tunnel registrations",
	}, []string{"reason"})

	TunnelsByIP = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "drip_tunnels_by_ip",
		Help: "Number of tunnels per client IP",
	}, []string{"ip"})

	// Connection metrics
	ActiveConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "drip_active_connections",
		Help: "Current number of active TCP connections",
	})

	TotalConnections = promauto.NewCounter(prometheus.CounterOpts{
		Name: "drip_connections_total",
		Help: "Total number of connections handled",
	})

	// Traffic metrics
	BytesReceived = promauto.NewCounter(prometheus.CounterOpts{
		Name: "drip_bytes_received_total",
		Help: "Total bytes received",
	})

	BytesSent = promauto.NewCounter(prometheus.CounterOpts{
		Name: "drip_bytes_sent_total",
		Help: "Total bytes sent",
	})

	RequestsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "drip_requests_total",
		Help: "Total number of HTTP requests handled",
	})

	// Per-tunnel metrics
	TunnelBytesReceived = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "drip_tunnel_bytes_received_total",
		Help: "Total bytes received per tunnel",
	}, []string{"tunnel_id", "subdomain", "type"})

	TunnelBytesSent = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "drip_tunnel_bytes_sent_total",
		Help: "Total bytes sent per tunnel",
	}, []string{"tunnel_id", "subdomain", "type"})

	TunnelActiveConnections = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "drip_tunnel_active_connections",
		Help: "Current number of active connections per tunnel",
	}, []string{"tunnel_id", "subdomain", "type"})

	// Rate limiting metrics
	RateLimitRejections = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "drip_rate_limit_rejections_total",
		Help: "Total number of rate limit rejections",
	}, []string{"type", "ip"})

	// System metrics
	PanicTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "drip_panic_total",
		Help: "Total number of panics recovered",
	})

	WorkerPoolSize = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "drip_worker_pool_size",
		Help: "Current worker pool size",
	})

	WorkerPoolActiveWorkers = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "drip_worker_pool_active_workers",
		Help: "Current number of active workers",
	})

	// HTTP proxy metrics
	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "drip_http_request_duration_seconds",
		Help:    "HTTP request duration in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "status"})

	HTTPRequestsInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "drip_http_requests_in_flight",
		Help: "Current number of HTTP requests being processed",
	})

	ProxyAuthEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "drip_proxy_auth_events_total",
		Help: "Total number of proxy authentication failures and lockouts",
	}, []string{"auth_type", "outcome", "reason"})

	ProxyTransferResults = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "drip_proxy_transfer_results_total",
		Help: "Total number of proxy response copy and pipe outcomes",
	}, []string{"operation", "result"})
)
