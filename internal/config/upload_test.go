package config

import (
	"strings"
	"testing"
	"time"
)

func TestUploadLimitDefaults(t *testing.T) {
	cfg, err := LoadFrom(envMap(nil))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	want := UploadLimits{
		MaxRequestBytes:     100 << 20,
		MaxFileBytes:        2 << 20,
		MaxFiles:            5000,
		MaxTotalBytes:       200 << 20,
		MaxCompressionRatio: 100,
		Timeout:             10 * time.Minute,
	}
	if cfg.Upload != want {
		t.Errorf("Upload = %+v, want %+v", cfg.Upload, want)
	}
}

func TestUploadLimitOverrides(t *testing.T) {
	cfg, err := LoadFrom(envMap(map[string]string{
		"APP_UPLOAD_MAX_REQUEST_SIZE":      "10MB",
		"APP_UPLOAD_MAX_FILE_SIZE":         "512KiB",
		"APP_UPLOAD_MAX_TOTAL_SIZE":        "1GiB",
		"APP_UPLOAD_MAX_FILES":             "42",
		"APP_UPLOAD_MAX_COMPRESSION_RATIO": "20",
		"APP_UPLOAD_TIMEOUT":               "90s",
	}))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	want := UploadLimits{
		MaxRequestBytes:     10_000_000,
		MaxFileBytes:        512 << 10,
		MaxFiles:            42,
		MaxTotalBytes:       1 << 30,
		MaxCompressionRatio: 20,
		Timeout:             90 * time.Second,
	}
	if cfg.Upload != want {
		t.Errorf("Upload = %+v, want %+v", cfg.Upload, want)
	}
}

func TestUploadLimitValidation(t *testing.T) {
	cases := map[string]map[string]string{
		"APP_UPLOAD_MAX_FILE_SIZE":         {"APP_UPLOAD_MAX_FILE_SIZE": "lots"},
		"APP_UPLOAD_MAX_REQUEST_SIZE":      {"APP_UPLOAD_MAX_REQUEST_SIZE": "0"},
		"APP_UPLOAD_MAX_TOTAL_SIZE":        {"APP_UPLOAD_MAX_TOTAL_SIZE": "-1MiB"},
		"APP_UPLOAD_MAX_FILES":             {"APP_UPLOAD_MAX_FILES": "1.5"},
		"APP_UPLOAD_MAX_COMPRESSION_RATIO": {"APP_UPLOAD_MAX_COMPRESSION_RATIO": "0"},
		"APP_UPLOAD_TIMEOUT":               {"APP_UPLOAD_TIMEOUT": "forever"},
		"must not exceed APP_UPLOAD_MAX_TOTAL_SIZE": {
			"APP_UPLOAD_MAX_FILE_SIZE": "10MiB", "APP_UPLOAD_MAX_TOTAL_SIZE": "1MiB",
		},
	}
	for want, env := range cases {
		_, err := LoadFrom(envMap(env))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("env %v: err = %v, want mention of %q", env, err, want)
		}
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"512": 512, "512B": 512, "64KiB": 64 << 10, "64kib": 64 << 10, "2 MiB": 2 << 20,
		"1GiB": 1 << 30, "64KB": 64_000, "2MB": 2_000_000, "1GB": 1_000_000_000,
	}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "MiB", "1.5MiB", "ten", "99999999999999GiB"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) succeeded, want error", bad)
		}
	}
}
