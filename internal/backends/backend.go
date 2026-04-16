package backends

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/engel75/eiroute/internal/config"
	"github.com/engel75/eiroute/internal/metrics"
)

// Sentinel errors for backend selection.
var (
	ErrUnknownModel       = errors.New("unknown model")
	ErrBackendUnavailable = errors.New("no healthy backend for model")
)

// Backend represents a single upstream LLM backend.
type Backend struct {
	Name                     string
	URL                      *url.URL
	Models                   []string
	OwnedBy                  string
	HealthPath               string
	HealthInterval           time.Duration
	MaxConcurrent            int
	Static                   bool
	Deprecated               bool
	Successor                string
	DeprecatedNoticeInterval int
	RetryAfter               time.Duration

	semaphore  chan struct{}
	healthy    atomic.Bool
	failCount  atomic.Int32
	activeReqs atomic.Int32
	lastCheck  atomic.Value // time.Time
	lastError  atomic.Value // string

	healthVersion *atomic.Int64
}

// NewBackend creates a Backend from config.
func NewBackend(cfg config.BackendConfig, healthVersion *atomic.Int64) (*Backend, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parsing backend URL %q: %w", cfg.URL, err)
	}

	var retryAfter time.Duration
	if cfg.RetryAfter != "" && cfg.RetryAfter != "off" {
		retryAfter, err = time.ParseDuration(cfg.RetryAfter)
		if err != nil {
			return nil, fmt.Errorf("parsing retry_after %q: %w", cfg.RetryAfter, err)
		}
	}

	b := &Backend{
		Name:                     cfg.Name,
		URL:                      u,
		Models:                   cfg.Models,
		OwnedBy:                  cfg.OwnedBy,
		HealthPath:               cfg.HealthPath,
		HealthInterval:           cfg.HealthInterval.Duration,
		MaxConcurrent:            cfg.MaxConcurrent,
		Static:                   cfg.Static,
		Deprecated:               cfg.Deprecated,
		Successor:                cfg.Successor,
		DeprecatedNoticeInterval: cfg.DeprecatedNoticeInterval,
		RetryAfter:               retryAfter,
		semaphore:                make(chan struct{}, cfg.MaxConcurrent),
		healthVersion:            healthVersion,
	}
	b.healthy.Store(true) // assume healthy until first check
	return b, nil
}

// IsHealthy returns the backend's current health state.
func (b *Backend) IsHealthy() bool {
	return b.healthy.Load()
}

// ActiveRequestCount returns the number of in-flight requests.
func (b *Backend) ActiveRequestCount() int32 {
	return b.activeReqs.Load()
}

// Acquire tries to get a semaphore slot within the given context deadline.
func (b *Backend) Acquire(ctx context.Context) error {
	select {
	case b.semaphore <- struct{}{}:
		b.activeReqs.Add(1)
		metrics.ActiveRequests.WithLabelValues(b.Name).Inc()
		return nil
	case <-ctx.Done():
		metrics.SemaphoreTimeoutsTotal.WithLabelValues(b.Name).Inc()
		return ctx.Err()
	}
}

// Release frees a semaphore slot.
func (b *Backend) Release() {
	<-b.semaphore
	b.activeReqs.Add(-1)
	metrics.ActiveRequests.WithLabelValues(b.Name).Dec()
}

// SemaphoreUsage returns the current and max semaphore usage.
func (b *Backend) SemaphoreUsage() (used int, capacity int) {
	return len(b.semaphore), cap(b.semaphore)
}

// RecordFailure increments the consecutive failure count. After 3 failures,
// the backend is marked unhealthy.
func (b *Backend) RecordFailure() {
	count := b.failCount.Add(1)
	if count >= 3 && b.healthy.CompareAndSwap(true, false) {
		b.healthVersion.Add(1)
		metrics.BackendHealthy.WithLabelValues(b.Name).Set(0)
	}
}

// RecordSuccess resets the failure count and marks the backend healthy.
func (b *Backend) RecordSuccess() {
	b.failCount.Store(0)
	if b.healthy.CompareAndSwap(false, true) {
		b.healthVersion.Add(1)
		metrics.BackendHealthy.WithLabelValues(b.Name).Set(1)
	}
}

// SetLastCheck records the time and optional error of the last health check.
func (b *Backend) SetLastCheck(t time.Time, err error) {
	b.lastCheck.Store(t)
	if err != nil {
		b.lastError.Store(err.Error())
	} else {
		b.lastError.Store("")
	}
}

// LastCheck returns the last health check time and error string.
func (b *Backend) LastCheck() (time.Time, string) {
	t, _ := b.lastCheck.Load().(time.Time)
	e, _ := b.lastError.Load().(string)
	return t, e
}

// Pool manages a set of backends and model-to-backend mapping.
type Pool struct {
	backends      []*Backend
	modelIndex    map[string][]*Backend
	HealthVersion atomic.Int64
	mu            sync.Mutex
}

// NewPool creates a Pool from backend configs.
func NewPool(cfgs []config.BackendConfig) (*Pool, error) {
	p := &Pool{
		modelIndex: make(map[string][]*Backend),
	}

	for _, cfg := range cfgs {
		b, err := NewBackend(cfg, &p.HealthVersion)
		if err != nil {
			return nil, err
		}
		p.backends = append(p.backends, b)
		for _, model := range cfg.Models {
			p.modelIndex[model] = append(p.modelIndex[model], b)
		}
	}

	// Set initial healthy gauge for all backends.
	for _, b := range p.backends {
		metrics.BackendHealthy.WithLabelValues(b.Name).Set(1)
	}

	return p, nil
}

// ReloadPool updates backends from new configs. Existing backends are
// kept to handle in-flight requests.
func (p *Pool) ReloadPool(cfgs []config.BackendConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	existing := make(map[string]*Backend)
	for _, b := range p.backends {
		existing[b.Name] = b
	}

	newNames := make(map[string]bool)
	for _, cfg := range cfgs {
		newNames[cfg.Name] = true
	}

	var newBackends []*Backend
	newModelIndex := make(map[string][]*Backend)

	for _, cfg := range cfgs {
		if b, ok := existing[cfg.Name]; ok {
			u, err := url.Parse(cfg.URL)
			if err != nil {
				return fmt.Errorf("parsing backend URL %q: %w", cfg.URL, err)
			}
			b.URL = u
			if b.MaxConcurrent != cfg.MaxConcurrent {
				b.semaphore = make(chan struct{}, cfg.MaxConcurrent)
			}
			b.MaxConcurrent = cfg.MaxConcurrent
			b.Models = cfg.Models
			b.OwnedBy = cfg.OwnedBy
			b.Static = cfg.Static
			b.HealthPath = cfg.HealthPath
			b.HealthInterval = cfg.HealthInterval.Duration
			b.Deprecated = cfg.Deprecated
			b.Successor = cfg.Successor
			b.DeprecatedNoticeInterval = cfg.DeprecatedNoticeInterval
			if cfg.RetryAfter != "" && cfg.RetryAfter != "off" {
				b.RetryAfter, err = time.ParseDuration(cfg.RetryAfter)
				if err != nil {
					return fmt.Errorf("parsing retry_after %q: %w", cfg.RetryAfter, err)
				}
			} else {
				b.RetryAfter = 0
			}
			newBackends = append(newBackends, b)
		} else {
			b, err := NewBackend(cfg, &p.HealthVersion)
			if err != nil {
				return err
			}
			newBackends = append(newBackends, b)
			metrics.BackendHealthy.WithLabelValues(b.Name).Set(1)
		}
		for _, model := range cfg.Models {
			newModelIndex[model] = append(newModelIndex[model], newBackends[len(newBackends)-1])
		}
	}

	p.backends = newBackends
	p.modelIndex = newModelIndex
	p.HealthVersion.Add(1)

	return nil
}

// Backends returns all backends in the pool.
func (p *Pool) Backends() []*Backend {
	return p.backends
}

// AllModels returns a deduplicated list of all configured model names.
func (p *Pool) AllModels() []string {
	models := make([]string, 0, len(p.modelIndex))
	for m := range p.modelIndex {
		models = append(models, m)
	}
	return models
}

// SelectAnyBackend picks any healthy backend (used for non-model-specific endpoints).
func (p *Pool) SelectAnyBackend() (*Backend, error) {
	backends := p.Backends()
	if len(backends) == 0 {
		return nil, ErrBackendUnavailable
	}

	var best *Backend
	minReqs := int32(1<<31 - 1)
	for _, b := range backends {
		if !b.IsHealthy() {
			continue
		}
		if r := b.ActiveRequestCount(); r < minReqs {
			minReqs = r
			best = b
		}
	}
	if best == nil {
		return nil, ErrBackendUnavailable
	}

	var tied []*Backend
	for _, b := range backends {
		if b.IsHealthy() && b.ActiveRequestCount() == minReqs {
			tied = append(tied, b)
		}
	}
	return tied[rand.IntN(len(tied))], nil
}

// SelectBackend picks the healthiest, least-loaded backend for the given model.
func (p *Pool) SelectBackend(model string) (*Backend, error) {
	candidates, ok := p.modelIndex[model]
	if !ok || len(candidates) == 0 {
		return nil, ErrUnknownModel
	}

	if len(candidates) == 1 {
		if candidates[0].IsHealthy() {
			return candidates[0], nil
		}
		return nil, ErrBackendUnavailable
	}

	var best *Backend
	minReqs := int32(1<<31 - 1)
	for _, b := range candidates {
		if !b.IsHealthy() {
			continue
		}
		if r := b.ActiveRequestCount(); r < minReqs {
			minReqs = r
			best = b
		}
	}
	if best == nil {
		return nil, ErrBackendUnavailable
	}

	var tied []*Backend
	for _, b := range candidates {
		if b.IsHealthy() && b.ActiveRequestCount() == minReqs {
			tied = append(tied, b)
		}
	}
	return tied[rand.IntN(len(tied))], nil
}
