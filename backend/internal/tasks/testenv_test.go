package tasks

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/gitsang/music-transcription/backend/internal/core"
	"github.com/gitsang/music-transcription/backend/pkg/mq"
)

// The integration tests never touch a deployment database. They are skipped
// unless MUSIC_TEST_DATABASE_URL points at a throwaway instance; every test
// creates its own schema so a parallel run cannot collide.
const (
	envTestDatabaseURL = "MUSIC_TEST_DATABASE_URL"
	envTestRedisURL    = "MUSIC_TEST_REDIS_URL"
	envTestFakePython  = "MUSIC_TEST_FAKE_PYTHON"
	// envTestPipelinePython selects an interpreter that can import the retained
	// pipeline package, for the opt-in real-CLI protocol test.
	envTestPipelinePython = "MUSIC_TEST_PIPELINE_PYTHON"
)

// packageDir returns backend/internal/tasks, used to locate testdata scripts.
func packageDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return dir
}

// workspaceDir is an absolute, symlink-resolved scratch directory. core resolves
// DATA_DIR symlinks, so /tmp -> /private/tmp style hosts need the real path.
func workspaceDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return dir
}

// testConfig returns a Config rooted in a fresh scratch directory.
func testConfig(t *testing.T) core.Config {
	t.Helper()
	redisURL := os.Getenv(envTestRedisURL)
	if redisURL == "" {
		redisURL = "redis://127.0.0.1:6379/0"
	}
	return core.Config{
		RedisURL:            redisURL,
		DataDir:             workspaceDir(t),
		PipelinePython:      "python3",
		LeaseSeconds:        5,
		StageTimeoutSeconds: 60,
		WorkerConcurrency:   1,
		DiskMinFreeBytes:    0,
	}
}

// requireDatabase connects to the throwaway test database and creates a private
// schema for this test. It skips the test when no test database is configured.
func requireDatabase(t *testing.T) *core.Store {
	t.Helper()
	databaseURL := os.Getenv(envTestDatabaseURL)
	if databaseURL == "" {
		t.Skipf("%s is not set; skipping Postgres integration test", envTestDatabaseURL)
	}
	admin, err := gorm.Open(postgres.Open(databaseURL), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), SkipDefaultTransaction: true,
	})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	schema := "tasks_test_" + uuid.NewString()[:8]
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_ = admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	// search_path is applied per connection; pgx forwards it as a server
	// runtime parameter, so current_schema() resolves to the private schema.
	scoped, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse test database url: %v", err)
	}
	query := scoped.Query()
	query.Set("search_path", schema)
	scoped.RawQuery = query.Encode()
	db, err := gorm.Open(postgres.Open(scoped.String()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), SkipDefaultTransaction: true,
		NowFunc: func() time.Time { return core.Now() },
	})
	if err != nil {
		t.Fatalf("open scoped database: %v", err)
	}
	store := &core.Store{DB: db, Config: testConfig(t)}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate test schema: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// seededJob inserts the rows a claimable attempt needs: a job and its original
// artifact whose bytes live under DATA_DIR.
func seededJob(t *testing.T, store *core.Store, mutate func(*core.Job)) *core.Job {
	t.Helper()
	db := store.DB
	user := &core.User{Username: "u" + uuid.NewString()[:8], PasswordHash: "x", Role: "user", Status: "active"}
	if err := db.Create(user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	job := &core.Job{
		UserID: user.ID, Title: "fixture", Kind: "audio", Engine: "baseline",
		Status: "queued", Attempt: 1, ConfigSnapshot: map[string]any{},
	}
	if mutate != nil {
		mutate(job)
	}
	if err := db.Create(job).Error; err != nil {
		t.Fatalf("create job: %v", err)
	}

	inputDir := filepath.Join(store.JobDir(user.ID, job.ID), "inputs")
	if err := os.MkdirAll(inputDir, 0o700); err != nil {
		t.Fatalf("create input dir: %v", err)
	}
	inputPath := filepath.Join(inputDir, "source.wav")
	if err := os.WriteFile(inputPath, []byte("fake audio bytes"), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	key, err := store.StorageKey(inputPath)
	if err != nil {
		t.Fatalf("storage key: %v", err)
	}
	original := &core.Artifact{
		JobID: job.ID, Kind: "original", Label: "source.wav", Mime: "audio/wav",
		Status: "ready", StorageKey: &key, Attempt: 1,
	}
	if err := db.Create(original).Error; err != nil {
		t.Fatalf("create original artifact: %v", err)
	}
	return job
}

// reloadJob fetches the current job row.
func reloadJob(t *testing.T, store *core.Store, jobID string) *core.Job {
	t.Helper()
	var job core.Job
	if err := store.DB.Where("id = ?", jobID).Take(&job).Error; err != nil {
		t.Fatalf("reload job: %v", err)
	}
	return &job
}

// runningJob seeds a job already fenced by a live lease for the given token.
func runningJob(t *testing.T, store *core.Store, token string, mutate func(*core.Job)) *core.Job {
	t.Helper()
	job := seededJob(t, store, func(job *core.Job) {
		now := core.Now()
		job.Status = "running"
		job.Stage = jobPointer("validating")
		job.LeaseToken = &token
		expires := now.Add(60 * time.Second)
		job.LeaseExpiresAt = &expires
		job.HeartbeatAt = &now
		job.StartedAt = &now
		if mutate != nil {
			mutate(job)
		}
	})
	return job
}

// writeFile writes a file under dir, creating parents.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create dir for %s: %v", name, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// testdataPath resolves a fixture root relative to the package directory.
func testdataPath(t *testing.T, parts ...string) string {
	t.Helper()
	return filepath.Join(append([]string{packageDir(t), "testdata"}, parts...)...)
}

// newTestRunner builds a Runner without touching Redis or a network broker, so
// database lease/fencing/publication behaviour can be exercised deterministically.
// The pipeline is wired to the fake Python CLI unless python is "".
func newTestRunner(t *testing.T, store *core.Store, python string) *Runner {
	t.Helper()
	config := store.Config
	lease := time.Duration(config.LeaseSeconds) * time.Second
	if lease <= 0 {
		lease = 120 * time.Second
	}
	runner := &Runner{
		store:       store,
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		producer:    &producer{inner: &fakeProducer{}},
		lease:       lease,
		heartbeat:   heartbeatInterval(lease),
		concurrency: 1,
		// Same clamp the worker applies, so a test snapshot cannot outlive the
		// queue callback deadline.
		consumeTimeout: 10 * time.Minute,
		env:            pipelineEnv(config),
	}
	if python != "" {
		script := testdataPath(t, "fake_pipeline.py")
		runner.pipeline = newPythonRunner(python, append(runner.env, "PYTHONPATH="+filepath.Dir(script)),
			2*time.Minute, runner.logger)
		runner.pipeline.args = []string{script}
	} else {
		runner.pipeline = newPythonRunner("python3", runner.env, time.Minute, runner.logger)
	}
	if err := store.InitializeStorage(); err != nil {
		t.Fatalf("initialize storage: %v", err)
	}
	t.Cleanup(func() { _ = runner.pipeline.close() })
	return runner
}

// fakeProducer captures publications so outbox tests need no Redis.
type fakeProducer struct {
	mu       sync.Mutex
	messages []string
	err      error
}

func (p *fakeProducer) Connect() error { return nil }

func (p *fakeProducer) Publish(_ context.Context, messages ...*mq.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	for _, message := range messages {
		p.messages = append(p.messages, string(message.Content))
	}
	return nil
}

func (p *fakeProducer) Close() error { return nil }

func (p *fakeProducer) published() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.messages...)
}
