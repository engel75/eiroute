package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(`
listen: ":9090"
request_timeout: "5m"
idle_conn_timeout: "60s"
semaphore_timeout: "3s"
error_templates: "./errors.json"
owned_by_override: "acme"
backends:
  - name: "backend-a"
    url: "http://localhost:8001"
    max_concurrent: 16
    models: ["model-a", "model-b"]
    owned_by: "team-a"
  - name: "backend-b"
    url: "http://localhost:8002"
    models: ["model-c"]
`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Listen != ":9090" {
		t.Errorf("listen = %q, want %q", cfg.Listen, ":9090")
	}
	if cfg.RequestTimeout.Duration != 5*time.Minute {
		t.Errorf("request_timeout = %v, want 5m", cfg.RequestTimeout.Duration)
	}
	if cfg.IdleConnTimeout.Duration != 60*time.Second {
		t.Errorf("idle_conn_timeout = %v, want 60s", cfg.IdleConnTimeout.Duration)
	}
	if cfg.SemaphoreTimeout.Duration != 3*time.Second {
		t.Errorf("semaphore_timeout = %v, want 3s", cfg.SemaphoreTimeout.Duration)
	}
	if cfg.OwnedByOverride != "acme" {
		t.Errorf("owned_by_override = %q, want %q", cfg.OwnedByOverride, "acme")
	}
	if len(cfg.Backends) != 2 {
		t.Fatalf("backends count = %d, want 2", len(cfg.Backends))
	}

	b := cfg.Backends[1]
	if b.HealthPath != "/health" {
		t.Errorf("default health_path = %q, want %q", b.HealthPath, "/health")
	}
	if b.MaxConcurrent != 32 {
		t.Errorf("default max_concurrent = %d, want 32", b.MaxConcurrent)
	}
}

func TestLoad_Defaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(`
backends:
  - name: "b"
    url: "http://localhost:8000"
    models: ["m"]
`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("default listen = %q, want %q", cfg.Listen, ":8080")
	}
	if cfg.RequestTimeout.Duration != 10*time.Minute {
		t.Errorf("default request_timeout = %v, want 10m", cfg.RequestTimeout.Duration)
	}
}

func TestLoad_Validation(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"no backends", `backends: []`},
		{"empty name", "backends:\n  - name: \"\"\n    url: \"http://x\"\n    models: [\"m\"]"},
		{"duplicate name", "backends:\n  - name: \"a\"\n    url: \"http://x\"\n    models: [\"m\"]\n  - name: \"a\"\n    url: \"http://y\"\n    models: [\"n\"]"},
		{"empty url", "backends:\n  - name: \"a\"\n    url: \"\"\n    models: [\"m\"]"},
		{"empty models", "backends:\n  - name: \"a\"\n    url: \"http://x\"\n    models: []"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			os.WriteFile(path, []byte(tt.yaml), 0644)

			_, err := Load(path)
			if err == nil {
				t.Error("expected validation error, got nil")
			}
		})
	}
}
