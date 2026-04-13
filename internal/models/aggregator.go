package models

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/engel75/eiroute/internal/backends"
)

const cacheTTL = 10 * time.Second

// Model matches the OpenAI model object schema.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelList is the OpenAI /v1/models response.
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// Aggregator serves GET /v1/models by synthesizing the response from config.
type Aggregator struct {
	pool            *backends.Pool
	ownedByOverride string
	startTime       int64

	mu        sync.Mutex
	cached    []byte
	cachedAt  time.Time
	cachedVer int64
}

// NewAggregator creates a new model aggregator.
func NewAggregator(pool *backends.Pool, ownedByOverride string) *Aggregator {
	return &Aggregator{
		pool:            pool,
		ownedByOverride: ownedByOverride,
		startTime:       time.Now().Unix(),
	}
}

func (a *Aggregator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data := a.getOrRebuild()
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (a *Aggregator) getOrRebuild() []byte {
	currentVer := a.pool.HealthVersion.Load()

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cached != nil && time.Since(a.cachedAt) < cacheTTL && a.cachedVer == currentVer {
		return a.cached
	}

	a.cached = a.build()
	a.cachedAt = time.Now()
	a.cachedVer = currentVer
	return a.cached
}

func (a *Aggregator) build() []byte {
	seen := make(map[string]bool)
	var models []Model

	for _, b := range a.pool.Backends() {
		if !b.IsHealthy() && !b.Static {
			continue
		}
		for _, m := range b.Models {
			if seen[m] {
				continue
			}
			seen[m] = true

			ownedBy := a.resolveOwnedBy(b)
			models = append(models, Model{
				ID:      m,
				Object:  "model",
				Created: a.startTime,
				OwnedBy: ownedBy,
			})
		}
	}

	if models == nil {
		models = []Model{}
	}

	resp := ModelList{Object: "list", Data: models}
	data, _ := json.Marshal(resp)
	return data
}

func (a *Aggregator) resolveOwnedBy(b *backends.Backend) string {
	if a.ownedByOverride != "" {
		return a.ownedByOverride
	}
	if b.OwnedBy != "" {
		return b.OwnedBy
	}
	return "self-hosted"
}

// Reload clears the cached model list so the next request rebuilds it.
func (a *Aggregator) Reload() {
	a.mu.Lock()
	a.cached = nil
	a.mu.Unlock()
}
