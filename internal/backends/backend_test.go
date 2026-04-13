package backends

import (
	"context"
	"testing"
	"time"

	"github.com/engel75/eiroute/internal/config"
	"github.com/prometheus/client_golang/prometheus"
)

func init() {
	// Reset default registry for tests to avoid duplicate registration panics.
	prometheus.DefaultRegisterer = prometheus.NewRegistry()
}

func testPool(t *testing.T, cfgs ...config.BackendConfig) *Pool {
	t.Helper()
	p, err := NewPool(cfgs)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return p
}

func TestSelectBackend_UnknownModel(t *testing.T) {
	p := testPool(t, config.BackendConfig{
		Name: "a", URL: "http://localhost:8000", MaxConcurrent: 4, Models: []string{"model-a"},
	})

	_, err := p.SelectBackend("nonexistent")
	if err != ErrUnknownModel {
		t.Errorf("err = %v, want ErrUnknownModel", err)
	}
}

func TestSelectBackend_AllUnhealthy(t *testing.T) {
	p := testPool(t, config.BackendConfig{
		Name: "a", URL: "http://localhost:8000", MaxConcurrent: 4, Models: []string{"model-a"},
	})

	// Force unhealthy.
	for i := 0; i < 3; i++ {
		p.backends[0].RecordFailure()
	}

	_, err := p.SelectBackend("model-a")
	if err != ErrBackendUnavailable {
		t.Errorf("err = %v, want ErrBackendUnavailable", err)
	}
}

func TestSelectBackend_LeastConnections(t *testing.T) {
	p := testPool(t,
		config.BackendConfig{Name: "a", URL: "http://a:8000", MaxConcurrent: 10, Models: []string{"m"}},
		config.BackendConfig{Name: "b", URL: "http://b:8000", MaxConcurrent: 10, Models: []string{"m"}},
	)

	// Simulate 3 active requests on backend "a".
	for i := 0; i < 3; i++ {
		p.backends[0].Acquire(context.Background())
	}

	// All selections should go to "b" (0 active vs 3).
	for i := 0; i < 20; i++ {
		b, err := p.SelectBackend("m")
		if err != nil {
			t.Fatalf("SelectBackend: %v", err)
		}
		if b.Name != "b" {
			t.Errorf("iteration %d: selected %q, want %q", i, b.Name, "b")
		}
	}
}

func TestSelectBackend_TieBreak(t *testing.T) {
	p := testPool(t,
		config.BackendConfig{Name: "a", URL: "http://a:8000", MaxConcurrent: 10, Models: []string{"m"}},
		config.BackendConfig{Name: "b", URL: "http://b:8000", MaxConcurrent: 10, Models: []string{"m"}},
	)

	counts := map[string]int{}
	for i := 0; i < 100; i++ {
		b, err := p.SelectBackend("m")
		if err != nil {
			t.Fatalf("SelectBackend: %v", err)
		}
		counts[b.Name]++
	}

	// With 100 iterations, both should be picked at least once (statistical).
	if counts["a"] == 0 || counts["b"] == 0 {
		t.Errorf("expected both backends picked, got a=%d b=%d", counts["a"], counts["b"])
	}
}

func TestSemaphore_AcquireRelease(t *testing.T) {
	p := testPool(t, config.BackendConfig{
		Name: "a", URL: "http://localhost:8000", MaxConcurrent: 2, Models: []string{"m"},
	})
	b := p.backends[0]

	// Acquire 2 slots.
	b.Acquire(context.Background())
	b.Acquire(context.Background())

	if b.ActiveRequestCount() != 2 {
		t.Errorf("activeReqs = %d, want 2", b.ActiveRequestCount())
	}

	// Third acquire should timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := b.Acquire(ctx)
	if err == nil {
		t.Error("expected timeout error, got nil")
	}

	// Release one, then acquire should succeed.
	b.Release()
	if err := b.Acquire(context.Background()); err != nil {
		t.Errorf("acquire after release: %v", err)
	}
}

func TestHealthThresholds(t *testing.T) {
	p := testPool(t, config.BackendConfig{
		Name: "a", URL: "http://localhost:8000", MaxConcurrent: 4, Models: []string{"m"},
	})
	b := p.backends[0]

	// Initially healthy.
	if !b.IsHealthy() {
		t.Fatal("expected healthy initially")
	}

	// 2 failures: still healthy.
	b.RecordFailure()
	b.RecordFailure()
	if !b.IsHealthy() {
		t.Error("expected healthy after 2 failures")
	}

	// 3rd failure: unhealthy.
	b.RecordFailure()
	if b.IsHealthy() {
		t.Error("expected unhealthy after 3 failures")
	}

	// 1 success: healthy again.
	b.RecordSuccess()
	if !b.IsHealthy() {
		t.Error("expected healthy after 1 success")
	}
}

func TestHealthVersion_Increments(t *testing.T) {
	p := testPool(t, config.BackendConfig{
		Name: "a", URL: "http://localhost:8000", MaxConcurrent: 4, Models: []string{"m"},
	})
	b := p.backends[0]

	v0 := p.HealthVersion.Load()

	// 3 failures → state flip → version bump.
	b.RecordFailure()
	b.RecordFailure()
	b.RecordFailure()

	v1 := p.HealthVersion.Load()
	if v1 <= v0 {
		t.Errorf("version not bumped after becoming unhealthy: %d -> %d", v0, v1)
	}

	// 1 success → state flip → version bump.
	b.RecordSuccess()
	v2 := p.HealthVersion.Load()
	if v2 <= v1 {
		t.Errorf("version not bumped after becoming healthy: %d -> %d", v1, v2)
	}
}
