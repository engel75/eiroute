package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	RequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_router_requests_total",
		Help: "Total requests by backend, model, and status.",
	}, []string{"backend", "model", "status"})

	RequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llm_router_request_duration_seconds",
		Help:    "Request duration in seconds by backend and model.",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600},
	}, []string{"backend", "model"})

	ActiveRequests = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "llm_router_active_requests",
		Help: "Currently active requests per backend.",
	}, []string{"backend"})

	BackendHealthy = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "llm_router_backend_healthy",
		Help: "Whether a backend is healthy (1) or not (0).",
	}, []string{"backend"})

	SemaphoreTimeoutsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_router_semaphore_acquire_timeouts_total",
		Help: "Total semaphore acquire timeouts per backend.",
	}, []string{"backend"})
)

func Register() {
	prometheus.MustRegister(
		RequestsTotal,
		RequestDuration,
		ActiveRequests,
		BackendHealthy,
		SemaphoreTimeoutsTotal,
	)
}
