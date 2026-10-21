package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is deployment configuration. Database-backed settings live in the
// settings table and are snapshotted per job at creation time.
type Config struct {
	DatabaseURL         string
	RedisURL            string
	DataDir             string
	PublicOrigin        string
	ModelPython         string
	PipelinePython      string
	DemucsRepo          string
	BasicPitchModelPath string
	ListenAddr          string
	CookieSecure        bool
	SessionSeconds      int
	LeaseSeconds        int
	StageTimeoutSeconds int
	WorkerConcurrency   int
	DiskMinFreeBytes    uint64
}

// LoadConfig reads deployment configuration from the environment. It accepts
// both the legacy SQLAlchemy URL prefix (postgresql+psycopg://) and plain
// postgresql:// URLs, normalizing to the latter.
func LoadConfig() (Config, error) {
	dataDir := env("DATA_DIR", "/data")
	abs, err := filepath.Abs(dataDir)
	if err == nil {
		dataDir = abs
	}
	if resolved, err := filepath.EvalSymlinks(dataDir); err == nil {
		dataDir = resolved
	}

	cfg := Config{
		DatabaseURL:         normalizeDatabaseURL(env("DATABASE_URL", "postgresql://music:music@postgres/music")),
		RedisURL:            env("REDIS_URL", "redis://redis:6379/0"),
		DataDir:             filepath.Clean(dataDir),
		PublicOrigin:        strings.TrimRight(env("PUBLIC_ORIGIN", "http://localhost:8080"), "/"),
		ModelPython:         env("MODEL_PYTHON", "/opt/models-venv/bin/python"),
		PipelinePython:      env("PIPELINE_PYTHON", "python3"),
		DemucsRepo:          env("DEMUCS_REPO", "/models/demucs"),
		BasicPitchModelPath: env("BASIC_PITCH_MODEL_PATH", "/models/basic-pitch/nmp.onnx"),
		ListenAddr:          env("LISTEN_ADDR", ":8080"),
		CookieSecure:        strings.EqualFold(env("COOKIE_SECURE", "true"), "true"),
	}

	cfg.SessionSeconds, err = envInt("SESSION_SECONDS", 86400)
	if err != nil {
		return Config{}, err
	}
	cfg.LeaseSeconds, err = envInt("LEASE_SECONDS", 120)
	if err != nil {
		return Config{}, err
	}
	cfg.StageTimeoutSeconds, err = envInt("STAGE_TIMEOUT_SECONDS", 3600)
	if err != nil {
		return Config{}, err
	}
	cfg.WorkerConcurrency, err = envInt("WORKER_CONCURRENCY", 1)
	if err != nil {
		return Config{}, err
	}
	free, err := envInt("DISK_MIN_FREE_BYTES", 1073741824)
	if err != nil {
		return Config{}, err
	}
	if free < 0 {
		free = 0
	}
	cfg.DiskMinFreeBytes = uint64(free)
	return cfg, nil
}

func env(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}
	return value, nil
}

func normalizeDatabaseURL(url string) string {
	if strings.HasPrefix(url, "postgresql+psycopg://") {
		return "postgresql://" + strings.TrimPrefix(url, "postgresql+psycopg://")
	}
	if strings.HasPrefix(url, "postgresql+asyncpg://") {
		return "postgresql://" + strings.TrimPrefix(url, "postgresql+asyncpg://")
	}
	return url
}
