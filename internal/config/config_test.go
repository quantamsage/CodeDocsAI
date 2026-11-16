package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := LoadFrom(envMap(nil))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Env != EnvDevelopment || cfg.IsProduction() {
		t.Errorf("Env = %q, want development", cfg.Env)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q", cfg.Addr)
	}
	if cfg.DataDir != "data" || cfg.DatabasePath != "data/app.db" {
		t.Errorf("DataDir/DatabasePath = %q/%q", cfg.DataDir, cfg.DatabasePath)
	}
	if cfg.LogLevel != slog.LevelInfo || cfg.LogFormat != "text" {
		t.Errorf("logging = %v/%q", cfg.LogLevel, cfg.LogFormat)
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout = %v", cfg.ShutdownTimeout)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := LoadFrom(envMap(map[string]string{
		"APP_ENV":              "production",
		"APP_ADDR":             "127.0.0.1:9000",
		"APP_DATA_DIR":         "/srv/data",
		"APP_LOG_LEVEL":        "debug",
		"APP_LOG_FORMAT":       "JSON",
		"APP_SHUTDOWN_TIMEOUT": "3s",
	}))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !cfg.IsProduction() || cfg.Addr != "127.0.0.1:9000" {
		t.Errorf("unexpected cfg %+v", cfg)
	}
	if cfg.DatabasePath != "/srv/data/app.db" {
		t.Errorf("DatabasePath should derive from DataDir, got %q", cfg.DatabasePath)
	}
	if cfg.LogLevel != slog.LevelDebug || cfg.LogFormat != "json" {
		t.Errorf("logging = %v/%q", cfg.LogLevel, cfg.LogFormat)
	}
	if cfg.ShutdownTimeout != 3*time.Second {
		t.Errorf("ShutdownTimeout = %v", cfg.ShutdownTimeout)
	}
}

func TestLoadExplicitDatabasePath(t *testing.T) {
	cfg, err := LoadFrom(envMap(map[string]string{"APP_DATABASE_PATH": "x/y.db"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabasePath != "x/y.db" {
		t.Errorf("DatabasePath = %q", cfg.DatabasePath)
	}
}

func TestLoadInvalid(t *testing.T) {
	_, err := LoadFrom(envMap(map[string]string{
		"APP_ENV":              "staging",
		"APP_LOG_LEVEL":        "loud",
		"APP_LOG_FORMAT":       "xml",
		"APP_SHUTDOWN_TIMEOUT": "-1s",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"APP_ENV", "APP_LOG_LEVEL", "APP_LOG_FORMAT", "APP_SHUTDOWN_TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}
