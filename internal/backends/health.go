package backends

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/engel75/eiroute/internal/metrics"
)

const (
	healthCheckTimeout = 3 * time.Second
)

// StartHealthChecks launches a goroutine per backend that periodically checks
// its health endpoint. Cancel the context to stop all checks.
func StartHealthChecks(ctx context.Context, backends []*Backend, transport http.RoundTripper, logger *slog.Logger) {
	client := &http.Client{
		Timeout:   healthCheckTimeout,
		Transport: transport,
	}

	for _, b := range backends {
		go runHealthCheck(ctx, b, client, logger)
	}
}

func runHealthCheck(ctx context.Context, b *Backend, client *http.Client, logger *slog.Logger) {
	ticker := time.NewTicker(b.HealthInterval)
	defer ticker.Stop()

	// Run one check immediately at startup.
	checkOnce(ctx, b, client, logger)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkOnce(ctx, b, client, logger)
		}
	}
}

func checkOnce(ctx context.Context, b *Backend, client *http.Client, logger *slog.Logger) {
	wasHealthy := b.IsHealthy()

	url := b.URL.JoinPath(b.HealthPath).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		logger.Debug("health check failed", "backend", b.Name, "url", url, "error", err)
		b.RecordFailure()
		b.SetLastCheck(time.Now(), err)
		return
	}

	start := time.Now()
	resp, err := client.Do(req)
	metrics.HealthCheckDuration.WithLabelValues(b.Name).Observe(time.Since(start).Seconds())
	if err != nil {
		logger.Debug("health check failed", "backend", b.Name, "url", url, "error", err)
		b.RecordFailure()
		b.SetLastCheck(time.Now(), err)
	} else {
		resp.Body.Close()
		if resp.StatusCode < 400 {
			logger.Debug("health check OK", "backend", b.Name, "url", url)
			b.RecordSuccess()
			b.SetLastCheck(time.Now(), nil)
		} else {
			logger.Debug("health check failed", "backend", b.Name, "url", url, "status", resp.StatusCode)
			b.RecordFailure()
			b.SetLastCheck(time.Now(), &httpError{statusCode: resp.StatusCode})
		}
	}

	nowHealthy := b.IsHealthy()
	if wasHealthy && !nowHealthy {
		logger.Warn("backend became unhealthy", "backend", b.Name)
	} else if !wasHealthy && nowHealthy {
		logger.Info("backend became healthy", "backend", b.Name)
	}
}

type httpError struct {
	statusCode int
}

func (e *httpError) Error() string {
	return http.StatusText(e.statusCode)
}
