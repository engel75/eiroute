package models

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/engel75/eiroute/internal/config"
	"github.com/engel75/eiroute/internal/backends"
	"github.com/prometheus/client_golang/prometheus"
)

func init() {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()
}

func testAggregator(t *testing.T, override string, cfgs ...config.BackendConfig) (*Aggregator, *backends.Pool) {
	t.Helper()
	pool, err := backends.NewPool(cfgs)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return NewAggregator(pool, override), pool
}

func getModels(t *testing.T, agg *Aggregator) ModelList {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	agg.ServeHTTP(w, r)
	var ml ModelList
	json.Unmarshal(w.Body.Bytes(), &ml)
	return ml
}

func TestAggregator_Basic(t *testing.T) {
	agg, _ := testAggregator(t, "acme",
		config.BackendConfig{Name: "a", URL: "http://a:8000", MaxConcurrent: 4, Models: []string{"model-1", "model-2"}},
	)
	ml := getModels(t, agg)

	if ml.Object != "list" {
		t.Errorf("object = %q, want %q", ml.Object, "list")
	}
	if len(ml.Data) != 2 {
		t.Fatalf("data count = %d, want 2", len(ml.Data))
	}
	if ml.Data[0].OwnedBy != "acme" {
		t.Errorf("owned_by = %q, want %q", ml.Data[0].OwnedBy, "acme")
	}
	if ml.Data[0].Object != "model" {
		t.Errorf("model object = %q, want %q", ml.Data[0].Object, "model")
	}
}

func TestAggregator_Dedup(t *testing.T) {
	agg, _ := testAggregator(t, "",
		config.BackendConfig{Name: "a", URL: "http://a:8000", MaxConcurrent: 4, Models: []string{"shared-model"}, OwnedBy: "team-a"},
		config.BackendConfig{Name: "b", URL: "http://b:8000", MaxConcurrent: 4, Models: []string{"shared-model"}, OwnedBy: "team-b"},
	)
	ml := getModels(t, agg)

	if len(ml.Data) != 1 {
		t.Errorf("data count = %d, want 1 (deduped)", len(ml.Data))
	}
}

func TestAggregator_OwnedByResolution(t *testing.T) {
	tests := []struct {
		name     string
		override string
		backend  string
		want     string
	}{
		{"global override wins", "global", "per-backend", "global"},
		{"per-backend fallback", "", "per-backend", "per-backend"},
		{"default fallback", "", "", "self-hosted"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agg, _ := testAggregator(t, tt.override,
				config.BackendConfig{Name: "a", URL: "http://a:8000", MaxConcurrent: 4, Models: []string{"m"}, OwnedBy: tt.backend},
			)
			ml := getModels(t, agg)
			if ml.Data[0].OwnedBy != tt.want {
				t.Errorf("owned_by = %q, want %q", ml.Data[0].OwnedBy, tt.want)
			}
		})
	}
}

func TestAggregator_SkipsUnhealthy(t *testing.T) {
	agg, pool := testAggregator(t, "",
		config.BackendConfig{Name: "a", URL: "http://a:8000", MaxConcurrent: 4, Models: []string{"m-a"}},
		config.BackendConfig{Name: "b", URL: "http://b:8000", MaxConcurrent: 4, Models: []string{"m-b"}},
	)

	// Mark backend "a" as unhealthy.
	for i := 0; i < 3; i++ {
		pool.Backends()[0].RecordFailure()
	}

	ml := getModels(t, agg)
	if len(ml.Data) != 1 {
		t.Fatalf("data count = %d, want 1", len(ml.Data))
	}
	if ml.Data[0].ID != "m-b" {
		t.Errorf("model id = %q, want %q", ml.Data[0].ID, "m-b")
	}
}

func TestAggregator_CacheInvalidation(t *testing.T) {
	agg, pool := testAggregator(t, "",
		config.BackendConfig{Name: "a", URL: "http://a:8000", MaxConcurrent: 4, Models: []string{"m-a"}},
	)

	ml1 := getModels(t, agg)
	if len(ml1.Data) != 1 {
		t.Fatalf("first call: data count = %d, want 1", len(ml1.Data))
	}

	// Make unhealthy → bumps healthVersion → cache should be invalidated.
	for i := 0; i < 3; i++ {
		pool.Backends()[0].RecordFailure()
	}

	ml2 := getModels(t, agg)
	if len(ml2.Data) != 0 {
		t.Errorf("after unhealthy: data count = %d, want 0", len(ml2.Data))
	}
}
