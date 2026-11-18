// Package config loads application configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
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
	// SessionLifetime is the absolute lifetime of a login session.
	SessionLifetime time.Duration
	// SessionIdleTimeout ends a session that has not been used for this long.
	SessionIdleTimeout time.Duration
	// Upload holds the limits applied to code uploads.
	Upload UploadLimits
}

// UploadLimits bounds what a single upload may contain and what a project may
// hold. Sizes are in bytes and refer to uncompressed content.
type UploadLimits struct {
	// MaxRequestBytes caps the size of an upload request body.
	MaxRequestBytes int64
	// MaxFileBytes caps the size of any single stored file.
	MaxFileBytes int64
	// MaxFiles caps the number of files a project may hold.
	MaxFiles int
	// MaxTotalBytes caps the total size of a project's files, and the total
	// amount of data extracted from one archive (ZIP bomb protection).
	MaxTotalBytes int64
	// MaxCompressionRatio rejects archive entries that expand more than this
	// many times their compressed size (ZIP bomb protection).
	MaxCompressionRatio int
	// Timeout bounds how long reading and processing one upload may take.
	Timeout time.Duration
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

	duration := func(key, def string, dst *time.Duration) {
		d, err := time.ParseDuration(get(key, def))
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		case d <= 0:
			errs = append(errs, fmt.Errorf("%s must be positive", key))
		default:
			*dst = d
		}
	}
	duration("APP_SHUTDOWN_TIMEOUT", "15s", &cfg.ShutdownTimeout)
	duration("APP_SESSION_LIFETIME", "168h", &cfg.SessionLifetime)
	duration("APP_SESSION_IDLE_TIMEOUT", "24h", &cfg.SessionIdleTimeout)
	if cfg.SessionIdleTimeout > cfg.SessionLifetime && cfg.SessionLifetime > 0 {
		errs = append(errs, errors.New("APP_SESSION_IDLE_TIMEOUT must not exceed APP_SESSION_LIFETIME"))
	}

	size := func(key, def string, dst *int64) {
		n, err := parseSize(get(key, def))
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		case n <= 0:
			errs = append(errs, fmt.Errorf("%s must be positive", key))
		default:
			*dst = n
		}
	}
	count := func(key, def string, dst *int) {
		n, err := strconv.Atoi(get(key, def))
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s must be a whole number", key))
		case n <= 0:
			errs = append(errs, fmt.Errorf("%s must be positive", key))
		default:
			*dst = n
		}
	}
	up := &cfg.Upload
	size("APP_UPLOAD_MAX_REQUEST_SIZE", "100MiB", &up.MaxRequestBytes)
	size("APP_UPLOAD_MAX_FILE_SIZE", "2MiB", &up.MaxFileBytes)
	size("APP_UPLOAD_MAX_TOTAL_SIZE", "200MiB", &up.MaxTotalBytes)
	count("APP_UPLOAD_MAX_FILES", "5000", &up.MaxFiles)
	count("APP_UPLOAD_MAX_COMPRESSION_RATIO", "100", &up.MaxCompressionRatio)
	duration("APP_UPLOAD_TIMEOUT", "10m", &up.Timeout)
	if up.MaxFileBytes > up.MaxTotalBytes && up.MaxTotalBytes > 0 {
		errs = append(errs, errors.New("APP_UPLOAD_MAX_FILE_SIZE must not exceed APP_UPLOAD_MAX_TOTAL_SIZE"))
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// parseSize parses a byte size such as "512", "64KiB", "2MiB", "1GiB" or the
// decimal forms "64KB", "2MB", "1GB".
func parseSize(v string) (int64, error) {
	units := []struct {
		suffix string
		mult   int64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30},
		{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000},
		{"B", 1},
	}
	s := strings.TrimSpace(v)
	mult := int64(1)
	for _, u := range units {
		if strings.HasSuffix(strings.ToUpper(s), strings.ToUpper(u.suffix)) {
			s, mult = strings.TrimSpace(s[:len(s)-len(u.suffix)]), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q (use bytes or a KiB/MiB/GiB suffix)", v)
	}
	if n > (1<<62)/mult {
		return 0, fmt.Errorf("size %q is too large", v)
	}
	return n * mult, nil
}
