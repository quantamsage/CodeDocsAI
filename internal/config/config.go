// Package config loads application configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Environment names.
const (
	EnvDevelopment = "development"
	EnvProduction  = "production"
)

// Config holds all runtime configuration for the application.
type Config struct {
	// Env is the deployment environment ("development" or "production").
	Env string
	// Addr is the TCP address the HTTP server listens on.
	Addr string
	// DatabasePath is the filesystem path of the SQLite database file.
	DatabasePath string
	// DataDir is the root directory for application data (e.g. uploads).
	DataDir string
	// LogLevel is the minimum level that is logged.
	LogLevel slog.Level
	// LogFormat is "text" or "json".
	LogFormat string
	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout time.Duration
}

// IsProduction reports whether the app runs in production mode.
func (c Config) IsProduction() bool { return c.Env == EnvProduction }

// Load reads configuration from the process environment.
func Load() (Config, error) {
	return LoadFrom(os.Getenv)
}

// LoadFrom reads configuration using the supplied lookup function, which makes
// configuration loading testable without touching the real environment.
func LoadFrom(getenv func(string) string) (Config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}

	cfg := Config{
		Env:       strings.ToLower(get("APP_ENV", EnvDevelopment)),
		Addr:      get("APP_ADDR", ":8080"),
		DataDir:   get("APP_DATA_DIR", "data"),
		LogFormat: strings.ToLower(get("APP_LOG_FORMAT", "text")),
	}
	cfg.DatabasePath = get("APP_DATABASE_PATH", cfg.DataDir+"/app.db")

	var errs []error

	if cfg.Env != EnvDevelopment && cfg.Env != EnvProduction {
		errs = append(errs, fmt.Errorf("APP_ENV must be %q or %q, got %q", EnvDevelopment, EnvProduction, cfg.Env))
	}
	if cfg.LogFormat != "text" && cfg.LogFormat != "json" {
		errs = append(errs, fmt.Errorf("APP_LOG_FORMAT must be \"text\" or \"json\", got %q", cfg.LogFormat))
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(get("APP_LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("APP_LOG_LEVEL: %w", err))
	}

	timeout, err := time.ParseDuration(get("APP_SHUTDOWN_TIMEOUT", "15s"))
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("APP_SHUTDOWN_TIMEOUT: %w", err))
	case timeout <= 0:
		errs = append(errs, errors.New("APP_SHUTDOWN_TIMEOUT must be positive"))
	default:
		cfg.ShutdownTimeout = timeout
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}
