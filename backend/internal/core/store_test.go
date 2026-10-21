package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const testDSNEnv = "MUSIC_TEST_DATABASE_URL"

// openTestSchema creates a throwaway PostgreSQL schema so core tests never see
// the deployment database or each other's rows.
func openTestSchema(t *testing.T) (*Store, *gorm.DB, string) {
	t.Helper()
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set; skipping PostgreSQL test", testDSNEnv)
	}
	if !containsTestMarker(dsn) {
		t.Fatalf("%s must name a dedicated test database", testDSNEnv)
	}
	schema := "core_test_" + NewID()[:8]
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_ = admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	separator := "?"
	if containsRune(dsn, '?') {
		separator = "&"
	}
	store, err := Open(Config{
		DatabaseURL: dsn + separator + "search_path=" + schema,
		DataDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, admin, schema
}

func containsTestMarker(dsn string) bool { return containsSubstring(dsn, "test") }

func containsSubstring(value, part string) bool {
	for index := 0; index+len(part) <= len(value); index++ {
		if value[index:index+len(part)] == part {
			return true
		}
	}
	return false
}

func containsRune(value string, target rune) bool {
	for _, r := range value {
		if r == target {
			return true
		}
	}
	return false
}

// TestMigrateIsAdditiveOnLegacySchema builds the SQLAlchemy/Alembic schema by
// hand (types and nullability included), inserts production-shaped rows, then
// runs Migrate twice and asserts nothing was dropped, renamed or rewritten.
func TestMigrateIsAdditiveOnLegacySchema(t *testing.T) {
	store, admin, schema := openTestSchema(t)
	// Run the legacy DDL inside the throwaway schema, not in public. One
	// transaction pins a single connection so SET search_path applies to all
	// fixture statements.
	err := admin.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL search_path TO " + schema).Error; err != nil {
			return err
		}
		for _, statement := range splitStatements(legacySchemaSQL) {
			if err := tx.Exec(statement).Error; err != nil {
				return fmt.Errorf("legacy DDL %q: %w", statement, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("legacy fixture: %v", err)
	}
	// A deliberate production-shaped row with a Python argon2id hash.
	pythonHash := "$argon2id$v=19$m=65536,t=3,p=4$RrYQpmxj7QFUBWwHNMJLuQ$Qg+uhNYSzZBuWtEgXOUiiRQ/YgGaDlwCFYNgU4KERh8"
	err = store.DB.Exec(`INSERT INTO users (id, username, password_hash, role, status, created_at)
		VALUES ('legacy-user', 'root', ?, 'admin', 'active', '2026-01-01 00:00:00')`, pythonHash).Error
	if err != nil {
		t.Fatalf("insert legacy user: %v", err)
	}
	err = store.DB.Exec(`INSERT INTO jobs (id, user_id, title, kind, status, engine, config_snapshot,
		warnings, attempt, cancel_requested, delete_requested, created_at)
		VALUES ('legacy-job', 'legacy-user', 'old', 'musicxml', 'succeeded', 'baseline',
		'{"engine":"baseline","retention_days":7}', '["note"]', 1, false, false,
		'2026-01-01 00:00:00')`).Error
	if err != nil {
		t.Fatalf("insert legacy job: %v", err)
	}

	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second migrate must be a no-op: %v", err)
	}

	// The row and its JSON payload must be byte-identical after migration.
	var user User
	if err := store.DB.First(&user, "id = ?", "legacy-user").Error; err != nil {
		t.Fatalf("legacy user lost: %v", err)
	}
	if user.PasswordHash != pythonHash || user.Role != "admin" || user.Status != "active" {
		t.Fatalf("legacy user changed: %+v", user)
	}
	if !VerifyPassword("correct-horse-battery", user.PasswordHash) {
		t.Fatal("python argon2id hash no longer verifies")
	}
	var job Job
	if err := store.DB.First(&job, "id = ?", "legacy-job").Error; err != nil {
		t.Fatalf("legacy job lost: %v", err)
	}
	if job.ConfigSnapshot["retention_days"] != float64(7) || len(job.Warnings) != 1 {
		t.Fatalf("legacy job payload changed: %+v", job)
	}

	// Every declared table must exist and none of the legacy columns may be gone.
	for _, model := range Models() {
		stmt := &gorm.Statement{DB: store.DB}
		if err := stmt.Parse(model); err != nil {
			t.Fatalf("parse model: %v", err)
		}
		var count int64
		if err := admin.Raw(`SELECT count(*) FROM information_schema.columns
			WHERE table_schema = ? AND table_name = ?`, schema, stmt.Schema.Table).Scan(&count).Error; err != nil {
			t.Fatalf("count columns: %v", err)
		}
		if count == 0 {
			t.Fatalf("table %s missing after migrate", stmt.Schema.Table)
		}
	}
	assertColumns(t, admin, schema, "jobs", []string{
		"id", "user_id", "title", "kind", "status", "stage", "stage_progress", "engine",
		"actual_device", "config_snapshot", "warnings", "error_code", "error_message",
		"attempt", "lease_token", "lease_expires_at", "heartbeat_at", "cancel_requested",
		"delete_requested", "deleted_at", "created_at", "started_at", "completed_at", "expires_at",
	})
	assertColumns(t, admin, schema, "artifacts", []string{
		"id", "job_id", "track_id", "kind", "label", "storage_key", "mime", "bytes",
		"sha256", "status", "error", "attempt", "created_at",
	})
	// Legacy index names must exist so query plans do not regress.
	for _, index := range []string{"ix_jobs_owner_status", "ix_jobs_expires_at", "ix_artifacts_job_id"} {
		var count int64
		if err := admin.Raw(`SELECT count(*) FROM pg_indexes WHERE schemaname = ? AND indexname = ?`,
			schema, index).Scan(&count).Error; err != nil {
			t.Fatalf("count index: %v", err)
		}
		if count != 1 {
			t.Fatalf("index %s missing", index)
		}
	}
}

func assertColumns(t *testing.T, db *gorm.DB, schema, table string, want []string) {
	t.Helper()
	var have []string
	err := db.Raw(`SELECT column_name FROM information_schema.columns
		WHERE table_schema = ? AND table_name = ?`, schema, table).Scan(&have).Error
	if err != nil {
		t.Fatalf("read columns for %s: %v", table, err)
	}
	present := make(map[string]bool, len(have))
	for _, name := range have {
		present[name] = true
	}
	for _, name := range want {
		if !present[name] {
			t.Fatalf("column %s.%s missing", table, name)
		}
	}
}

// splitStatements breaks the fixture DDL on semicolons; the fixture contains no
// semicolons inside literals.
func splitStatements(sql string) []string {
	var result []string
	current := make([]rune, 0, len(sql))
	for _, r := range sql {
		if r == ';' {
			if len(current) > 0 {
				result = append(result, string(current))
			}
			current = current[:0]
			continue
		}
		if r == '\n' && len(current) == 0 {
			continue
		}
		current = append(current, r)
	}
	if len(current) > 0 {
		result = append(result, string(current))
	}
	return result
}

// legacySchemaSQL mirrors alembic revision 4a03afadaf7c exactly.
const legacySchemaSQL = `
CREATE TABLE users (
    id VARCHAR(36) NOT NULL,
    username VARCHAR(64) NOT NULL,
    password_hash TEXT NOT NULL,
    role VARCHAR(16) NOT NULL,
    status VARCHAR(16) NOT NULL,
    reviewed_by VARCHAR(36),
    reviewed_at TIMESTAMP WITHOUT TIME ZONE,
    created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    PRIMARY KEY (id),
    UNIQUE (username)
);
CREATE TABLE sessions (
    id VARCHAR(36) NOT NULL,
    token_hash VARCHAR(64) NOT NULL,
    user_id VARCHAR(36) NOT NULL REFERENCES users(id),
    expires_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    revoked_at TIMESTAMP WITHOUT TIME ZONE,
    created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    PRIMARY KEY (id),
    UNIQUE (token_hash)
);
CREATE INDEX ix_sessions_user_id ON sessions (user_id);
CREATE TABLE jobs (
    id VARCHAR(36) NOT NULL,
    user_id VARCHAR(36) NOT NULL REFERENCES users(id),
    title VARCHAR(255) NOT NULL,
    kind VARCHAR(16) NOT NULL,
    status VARCHAR(16) NOT NULL,
    stage VARCHAR(32),
    stage_progress DOUBLE PRECISION,
    engine VARCHAR(32) NOT NULL,
    actual_device VARCHAR(32),
    config_snapshot JSON NOT NULL,
    warnings JSON NOT NULL,
    error_code VARCHAR(64),
    error_message TEXT,
    attempt INTEGER NOT NULL,
    lease_token VARCHAR(36),
    lease_expires_at TIMESTAMP WITHOUT TIME ZONE,
    heartbeat_at TIMESTAMP WITHOUT TIME ZONE,
    cancel_requested BOOLEAN NOT NULL,
    delete_requested BOOLEAN NOT NULL,
    deleted_at TIMESTAMP WITHOUT TIME ZONE,
    created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    started_at TIMESTAMP WITHOUT TIME ZONE,
    completed_at TIMESTAMP WITHOUT TIME ZONE,
    expires_at TIMESTAMP WITHOUT TIME ZONE,
    PRIMARY KEY (id)
);
CREATE INDEX ix_jobs_expires_at ON jobs (expires_at);
CREATE INDEX ix_jobs_lease_expires_at ON jobs (lease_expires_at);
CREATE INDEX ix_jobs_owner_status ON jobs (user_id, status);
CREATE INDEX ix_jobs_status ON jobs (status);
CREATE INDEX ix_jobs_user_id ON jobs (user_id);
CREATE TABLE tracks (
    id VARCHAR(36) NOT NULL,
    job_id VARCHAR(36) NOT NULL REFERENCES jobs(id),
    label VARCHAR(255) NOT NULL,
    program INTEGER,
    confidence DOUBLE PRECISION,
    label_source VARCHAR(32) NOT NULL,
    is_drum BOOLEAN NOT NULL,
    source_stem_ids JSON NOT NULL,
    PRIMARY KEY (id)
);
CREATE INDEX ix_tracks_job_id ON tracks (job_id);
CREATE TABLE artifacts (
    id VARCHAR(36) NOT NULL,
    job_id VARCHAR(36) NOT NULL REFERENCES jobs(id),
    track_id VARCHAR(36),
    kind VARCHAR(32) NOT NULL,
    label VARCHAR(255) NOT NULL,
    storage_key TEXT,
    mime VARCHAR(128) NOT NULL,
    bytes INTEGER,
    sha256 VARCHAR(64),
    status VARCHAR(16) NOT NULL,
    error TEXT,
    attempt INTEGER NOT NULL,
    created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    PRIMARY KEY (id)
);
CREATE INDEX ix_artifacts_job_id ON artifacts (job_id);
CREATE TABLE settings (
    key VARCHAR(64) NOT NULL,
    value JSON NOT NULL,
    updated_by VARCHAR(36) REFERENCES users(id),
    updated_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    PRIMARY KEY (key)
);
CREATE TABLE audit_logs (
    id VARCHAR(36) NOT NULL,
    actor_id VARCHAR(36),
    action VARCHAR(64) NOT NULL,
    resource_id VARCHAR(64),
    details JSON NOT NULL,
    created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    PRIMARY KEY (id)
);
CREATE INDEX ix_audit_logs_actor_id ON audit_logs (actor_id);
CREATE TABLE queue_outbox (
    id VARCHAR(36) NOT NULL,
    job_id VARCHAR(36) NOT NULL REFERENCES jobs(id),
    attempt INTEGER NOT NULL,
    delivered_at TIMESTAMP WITHOUT TIME ZONE,
    next_retry_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    tries INTEGER NOT NULL,
    created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    PRIMARY KEY (id),
    UNIQUE (job_id, attempt)
);
CREATE INDEX ix_queue_outbox_job_id ON queue_outbox (job_id);
CREATE INDEX ix_queue_outbox_next_retry_at ON queue_outbox (next_retry_at);
CREATE TABLE rate_limits (
    key VARCHAR(64) NOT NULL,
    count INTEGER NOT NULL,
    reset_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    PRIMARY KEY (key)
);
CREATE INDEX ix_rate_limits_reset_at ON rate_limits (reset_at);
CREATE TABLE service_heartbeats (
    role VARCHAR(16) NOT NULL,
    updated_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    PRIMARY KEY (role)
);
`

// TestMigrateCreatesFreshSchema checks that an empty database becomes usable in
// one call, which is what `musicd migrate` does on a new deployment.
func TestMigrateCreatesFreshSchema(t *testing.T) {
	store, _, _ := openTestSchema(t)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// The app must be able to insert and read back every JSON-bearing row.
	user := User{Username: "fresh", PasswordHash: "x", Role: "user", Status: "active"}
	if err := store.DB.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	job := Job{UserID: user.ID, Title: "t", Kind: "musicxml", Engine: "baseline",
		ConfigSnapshot: map[string]any{"retention_days": 3}, Warnings: []string{}}
	if err := store.DB.Create(&job).Error; err != nil {
		t.Fatalf("create job: %v", err)
	}
	var loaded Job
	if err := store.DB.First(&loaded, "id = ?", job.ID).Error; err != nil {
		t.Fatalf("reload job: %v", err)
	}
	if loaded.ConfigSnapshot["retention_days"] != float64(3) || loaded.Warnings == nil {
		t.Fatalf("job roundtrip: %+v", loaded)
	}
	// Empty JSON collections must survive a map update, not become NULL.
	if err := store.DB.Model(&Job{}).Where("id = ?", job.ID).Updates(map[string]any{
		"warnings": JSONValue([]string{}),
	}).Error; err != nil {
		t.Fatalf("update warnings: %v", err)
	}
	if err := store.DB.First(&loaded, "id = ?", job.ID).Error; err != nil {
		t.Fatalf("reload job: %v", err)
	}
	if loaded.Warnings == nil || len(loaded.Warnings) != 0 {
		t.Fatalf("warnings = %#v", loaded.Warnings)
	}
}

// TestSettingsRoundTripDistinguishesNull checks retention_days semantics: an
// unset value means "keep forever", and that must survive a patch cycle.
func TestSettingsRoundTripDistinguishesNull(t *testing.T) {
	store, _, _ := openTestSchema(t)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	values, err := Settings(store.DB)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if values["retention_days"] != nil || Number(values, "max_upload_bytes") != 209715200 {
		t.Fatalf("defaults = %v", values)
	}
	if err := store.DB.Exec(`INSERT INTO settings (key, value, updated_at)
		VALUES ('retention_days', ?::json, ?)`, JSONValue(nil), Now()).Error; err != nil {
		t.Fatalf("insert null setting: %v", err)
	}
	values, err = Settings(store.DB)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if values["retention_days"] != nil {
		t.Fatalf("null retention = %#v", values["retention_days"])
	}
	// A stored number overrides the default.
	if err := store.DB.Exec(`INSERT INTO settings (key, value, updated_at)
		VALUES ('max_upload_bytes', ?::json, ?)`, JSONValue(4096), Now()).Error; err != nil {
		t.Fatalf("insert number setting: %v", err)
	}
	values, _ = Settings(store.DB)
	if Number(values, "max_upload_bytes") != 4096 {
		t.Fatalf("max_upload_bytes = %v", values["max_upload_bytes"])
	}
}

// TestNumberAcceptsJSONDecodedTypes covers float64/int64 from encoding/json and
// PostgreSQL, plus rejection of junk.
func TestNumberAcceptsJSONDecodedTypes(t *testing.T) {
	cases := []struct {
		value any
		want  int
	}{
		{float64(600), 600},
		{float32(3), 3},
		{int(7), 7},
		{int64(9), 9},
		{"600", 0},
		{nil, 0},
		{true, 0},
	}
	for _, item := range cases {
		got := Number(map[string]any{"k": item.value}, "k")
		if got != item.want {
			t.Fatalf("Number(%#v) = %d, want %d", item.value, got, item.want)
		}
	}
	if Number(map[string]any{}, "k") != 0 {
		t.Fatal("missing key must be 0")
	}
}

// TestPrivatePathRejectsEscapes covers traversal, absolute keys and symlinks.
func TestPrivatePathRejectsEscapes(t *testing.T) {
	store, _, _ := openTestSchema(t)
	store.Config.DataDir = t.TempDir()
	if err := store.InitializeStorage(); err != nil {
		t.Fatalf("initialize storage: %v", err)
	}
	inside := filepath.Join(store.Config.DataDir, "users", "u1", "jobs", "j1", "input", "x.xml")
	if err := os.MkdirAll(filepath.Dir(inside), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(inside, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	resolved, err := store.PrivatePath("users/u1/jobs/j1/input/x.xml")
	if err != nil || resolved != inside {
		t.Fatalf("valid key: %q %v", resolved, err)
	}
	for _, key := range []string{"", "/etc/passwd", "../outside", "users/../../etc/passwd", "."} {
		if _, err := store.PrivatePath(key); err == nil {
			t.Fatalf("key %q escaped", key)
		}
	}

	// A symlink inside DATA_DIR that points outside must be rejected.
	link := filepath.Join(store.Config.DataDir, "users", "escape")
	if err := os.Symlink("/etc", link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := store.PrivatePath("users/escape/passwd"); err == nil {
		t.Fatal("symlink escape allowed")
	}

	// StorageKey is the inverse mapping and must agree.
	key, err := store.StorageKey(inside)
	if err != nil || key != "users/u1/jobs/j1/input/x.xml" {
		t.Fatalf("StorageKey = %q %v", key, err)
	}
	if _, err := store.StorageKey("/etc/passwd"); err == nil {
		t.Fatal("StorageKey allowed an outside path")
	}
}

// TestPurgeHoldsFlockAndLeavesTombstone verifies the deletion boundary that
// protects a still-unwinding worker.
func TestPurgeHoldsFlockAndLeavesTombstone(t *testing.T) {
	store, _, _ := openTestSchema(t)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	user := User{Username: "purge", PasswordHash: "x", Role: "user", Status: "active"}
	if err := store.DB.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	job := Job{UserID: user.ID, Title: "victim", Kind: "musicxml", Engine: "baseline"}
	if err := store.DB.Create(&job).Error; err != nil {
		t.Fatalf("create job: %v", err)
	}
	directory := store.JobDir(user.ID, job.ID)
	if err := os.MkdirAll(filepath.Join(directory, "input"), 0o700); err != nil {
		t.Fatalf("mkdir job dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "input", "original.xml"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if err := store.DB.Create(&Track{JobID: job.ID, Label: "P", LabelSource: "imported"}).Error; err != nil {
		t.Fatalf("create track: %v", err)
	}
	if err := store.DB.Create(&QueueOutbox{JobID: job.ID, Attempt: 1}).Error; err != nil {
		t.Fatalf("create outbox: %v", err)
	}

	// A running lease may not be purged at all.
	running := job
	running.Status = "running"
	token := NewID()
	running.LeaseToken = &token
	if _, err := store.Purge(store.DB, &running); err == nil {
		t.Fatal("purge accepted a running lease")
	}

	cancelled := job
	cancelled.Status = "cancelled"
	// Hold the execution lock: purge must refuse and report false, not error.
	lockPath := filepath.Join(store.Config.DataDir, "locks", job.ID+".lock")
	release := holdFileLock(t, lockPath)
	purged, err := store.Purge(store.DB, &cancelled)
	if err != nil || purged {
		t.Fatalf("purge while locked = %v %v", purged, err)
	}
	if _, err := os.Stat(directory); err != nil {
		t.Fatalf("directory removed while locked: %v", err)
	}
	release()

	purged, err = store.Purge(store.DB, &cancelled)
	if err != nil || !purged {
		t.Fatalf("purge = %v %v", purged, err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("directory survived purge: %v", err)
	}
	var tracks, outbox int64
	store.DB.Model(&Track{}).Where("job_id = ?", job.ID).Count(&tracks)
	store.DB.Model(&QueueOutbox{}).Where("job_id = ?", job.ID).Count(&outbox)
	if tracks != 0 || outbox != 0 {
		t.Fatalf("derived rows remain: tracks=%d outbox=%d", tracks, outbox)
	}
	if cancelled.DeletedAt == nil || !cancelled.DeleteRequested || cancelled.Title != "已删除任务" {
		t.Fatalf("tombstone = %+v", cancelled)
	}
}

// TestCompleteAppliesRetention covers the retention math shared with workers.
func TestCompleteAppliesRetention(t *testing.T) {
	job := Job{ConfigSnapshot: map[string]any{"retention_days": float64(7)}}
	Complete(&job, "succeeded")
	if job.Status != "succeeded" || job.CompletedAt == nil || job.ExpiresAt == nil {
		t.Fatalf("complete = %+v", job)
	}
	delta := job.ExpiresAt.Sub(*job.CompletedAt)
	if delta != 7*24*time.Hour {
		t.Fatalf("retention = %v", delta)
	}
	job.LeaseToken = nil
	job.LeaseExpiresAt = nil

	forever := Job{ConfigSnapshot: map[string]any{"retention_days": nil}}
	Complete(&forever, "failed")
	if forever.ExpiresAt != nil {
		t.Fatalf("null retention produced %v", forever.ExpiresAt)
	}

	zero := Job{}
	Complete(&zero, "cancelled")
	if zero.ExpiresAt != nil {
		t.Fatalf("missing retention produced %v", zero.ExpiresAt)
	}
}

// holdFileLock takes an exclusive flock and returns the release function.
func holdFileLock(t *testing.T, path string) func() {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create lock dir: %v", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("flock: %v", err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}
}
