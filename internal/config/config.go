package config

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// ParseLogLevel converts a log level string to slog.Level.
func ParseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// GetLogLevel returns the log level, checking LOG_LEVEL env var first,
// then config file, then defaulting to info.
func GetLogLevel(cfg *Config) slog.Level {
	if level := os.Getenv("LOG_LEVEL"); level != "" {
		return ParseLogLevel(level)
	}
	if cfg.LogLevel != "" {
		return ParseLogLevel(cfg.LogLevel)
	}
	return slog.LevelInfo
}

type Config struct {
	Listen           string          `yaml:"listen"`
	RequestTimeout   Duration        `yaml:"request_timeout"`
	IdleConnTimeout  Duration        `yaml:"idle_conn_timeout"`
	SemaphoreTimeout Duration        `yaml:"semaphore_timeout"`
	ErrorTemplates   string          `yaml:"error_templates"`
	OwnedByOverride  string          `yaml:"owned_by_override"`
	Backends         []BackendConfig `yaml:"backends"`
	LogLevel         string          `yaml:"log_level"`
}

type BackendConfig struct {
	Name           string   `yaml:"name"`
	URL            string   `yaml:"url"`
	MaxConcurrent  int      `yaml:"max_concurrent"`
	HealthPath     string   `yaml:"health_path"`
	HealthInterval Duration `yaml:"health_interval"`
	Models         []string `yaml:"models"`
	OwnedBy        string   `yaml:"owned_by"`
	Static         bool     `yaml:"static"`
}

// Duration wraps time.Duration for YAML string unmarshalling.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = dur
	return nil
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	setDefaults(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

func Reload(path string) (*Config, error) {
	return Load(path)
}

func setDefaults(cfg *Config) {
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.RequestTimeout.Duration == 0 {
		cfg.RequestTimeout.Duration = 10 * time.Minute
	}
	if cfg.IdleConnTimeout.Duration == 0 {
		cfg.IdleConnTimeout.Duration = 90 * time.Second
	}
	if cfg.SemaphoreTimeout.Duration == 0 {
		cfg.SemaphoreTimeout.Duration = 2 * time.Second
	}
	for i := range cfg.Backends {
		if cfg.Backends[i].HealthPath == "" {
			cfg.Backends[i].HealthPath = "/health"
		}
		if cfg.Backends[i].HealthInterval.Duration == 0 {
			cfg.Backends[i].HealthInterval.Duration = 10 * time.Second
		}
		if cfg.Backends[i].MaxConcurrent == 0 {
			cfg.Backends[i].MaxConcurrent = 32
		}
	}
}

func validate(cfg *Config) error {
	if len(cfg.Backends) == 0 {
		return fmt.Errorf("at least one backend is required")
	}

	names := make(map[string]bool, len(cfg.Backends))
	for i, b := range cfg.Backends {
		if b.Name == "" {
			return fmt.Errorf("backend %d: name is required", i)
		}
		if names[b.Name] {
			return fmt.Errorf("duplicate backend name: %q", b.Name)
		}
		names[b.Name] = true

		if b.URL == "" {
			return fmt.Errorf("backend %q: url is required", b.Name)
		}
		if len(b.Models) == 0 {
			return fmt.Errorf("backend %q: at least one model is required", b.Name)
		}
	}
	return nil
}
