package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	goredis "github.com/go-redis/redis/v8"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Store owns the database handle and deployment configuration.
type Store struct {
	DB     *gorm.DB
	Config Config
}

// Open connects to PostgreSQL and returns a Store. It does not mutate schema.
func Open(cfg Config) (*Store, error) {
	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	db, err := gorm.Open(postgres.Open(cfg.DatabaseURL), &gorm.Config{
		Logger: logger.New(log.New(os.Stderr, "\r\n", log.LstdFlags), logger.Config{
			SlowThreshold:             200 * time.Millisecond,
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: true,
			Colorful:                  false,
		}),
		SkipDefaultTransaction: true,
		NowFunc:                func() time.Time { return Now() },
	})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("database handle: %w", err)
	}
	sqlDB.SetMaxOpenConns(20)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(time.Hour)
	store := &Store{DB: db, Config: cfg}
	if err := store.initializeStorage(); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

// Close releases the database connection pool.
func (s *Store) Close() error {
	sqlDB, err := s.DB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// Migrate creates missing tables on a fresh database and only adds missing
// columns/indexes on an existing one. It never drops, renames or retypes
// existing data, so the legacy SQLAlchemy schema keeps working.
func (s *Store) Migrate(ctx context.Context) error {
	db := s.DB.WithContext(ctx)
	for _, model := range Models() {
		stmt := &gorm.Statement{DB: db}
		if err := stmt.Parse(model); err != nil {
			return fmt.Errorf("parse model: %w", err)
		}
		table := stmt.Schema.Table
		exists, err := tableExists(db, table)
		if err != nil {
			return err
		}
		if !exists {
			if err := db.Migrator().AutoMigrate(model); err != nil {
				return fmt.Errorf("create table %s: %w", table, err)
			}
			continue
		}
		if err := addMissingColumns(db, stmt); err != nil {
			return fmt.Errorf("upgrade table %s: %w", table, err)
		}
	}
	return ensureIndexes(db)
}

func tableExists(db *gorm.DB, table string) (bool, error) {
	var count int64
	err := db.Raw(`SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name = ?`, table).Scan(&count).Error
	return count > 0, err
}

// addMissingColumns is strictly additive: existing columns are left untouched.
func addMissingColumns(db *gorm.DB, stmt *gorm.Statement) error {
	existing := map[string]bool{}
	rows, err := db.Raw(`SELECT column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = ?`, stmt.Schema.Table).Rows()
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		existing[name] = true
	}
	rows.Close()
	for _, field := range stmt.Schema.Fields {
		if field.DBName == "" || field.IgnoreMigration {
			continue
		}
		if existing[field.DBName] {
			continue
		}
		if err := db.Migrator().AddColumn(stmt.Schema.ModelType, field.DBName); err != nil {
			return fmt.Errorf("add column %s: %w", field.DBName, err)
		}
	}
	return nil
}

type indexSpec struct {
	name    string
	table   string
	columns string
	unique  bool
}

func declaredIndexes() []indexSpec {
	return []indexSpec{
		{"ix_jobs_owner_status", "jobs", "user_id, status", false},
		{"ix_jobs_status", "jobs", "status", false},
		{"ix_jobs_user_id", "jobs", "user_id", false},
		{"ix_jobs_lease_expires_at", "jobs", "lease_expires_at", false},
		{"ix_jobs_expires_at", "jobs", "expires_at", false},
		{"ix_sessions_user_id", "sessions", "user_id", false},
		{"ix_tracks_job_id", "tracks", "job_id", false},
		{"ix_artifacts_job_id", "artifacts", "job_id", false},
		{"ix_queue_outbox_job_id", "queue_outbox", "job_id", false},
		{"ix_queue_outbox_next_retry_at", "queue_outbox", "next_retry_at", false},
		{"ix_audit_logs_actor_id", "audit_logs", "actor_id", false},
		{"ix_rate_limits_reset_at", "rate_limits", "reset_at", false},
		{"users_username_key", "users", "username", true},
		{"sessions_token_hash_key", "sessions", "token_hash", true},
		{"queue_outbox_job_id_attempt_key", "queue_outbox", "job_id, attempt", true},
	}
}

// ensureIndexes creates only indexes that do not exist yet. Index names match
// the legacy schema so an upgraded database gains nothing new.
func ensureIndexes(db *gorm.DB) error {
	for _, spec := range declaredIndexes() {
		var count int64
		err := db.Raw(`SELECT count(*) FROM pg_indexes
			WHERE schemaname = current_schema() AND indexname = ?`, spec.name).Scan(&count).Error
		if err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		existing, err := tableExists(db, spec.table)
		if err != nil {
			return err
		}
		if !existing {
			continue
		}
		unique := ""
		if spec.unique {
			unique = "UNIQUE "
		}
		statement := fmt.Sprintf("CREATE %sINDEX IF NOT EXISTS %s ON %s (%s)",
			unique, spec.name, spec.table, spec.columns)
		if err := db.Exec(statement).Error; err != nil {
			return fmt.Errorf("create index %s: %w", spec.name, err)
		}
	}
	return nil
}

// Health reports database, broker, storage and service heartbeat readiness.
func (s *Store) Health(ctx context.Context, role string) map[string]any {
	checks := map[string]any{}
	var one int
	if err := s.DB.WithContext(ctx).Raw("SELECT 1").Scan(&one).Error; err != nil {
		checks["database"] = false
	} else {
		checks["database"] = true
		if role == "worker" || role == "dispatcher" {
			healthy := false
			var beat ServiceHeartbeat
			if err := s.DB.WithContext(ctx).Where("role = ?", role).First(&beat).Error; err == nil {
				healthy = Now().Sub(beat.UpdatedAt.UTC()).Seconds() < 90
			}
			checks["heartbeat"] = healthy
		}
	}
	broker := false
	if s.Config.RedisURL != "" {
		client := goredis.NewClient(&goredis.Options{
			Addr:         RedisAddr(s.Config.RedisURL),
			Password:     RedisPassword(s.Config.RedisURL),
			DB:           RedisDB(s.Config.RedisURL),
			DialTimeout:  2 * time.Second,
			ReadTimeout:  2 * time.Second,
			WriteTimeout: 2 * time.Second,
		})
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if err := client.Ping(pingCtx).Err(); err == nil {
			broker = true
		}
		cancel()
		_ = client.Close()
	}
	checks["broker"] = broker
	checks["storage"] = s.storageReady()
	status := "ready"
	for _, value := range checks {
		if ready, ok := value.(bool); !ok || !ready {
			status = "not_ready"
			break
		}
	}
	return map[string]any{
		"status":      status,
		"checks":      checks,
		"audio_ready": baselineReady(s.Config),
	}
}

func (s *Store) storageReady() bool {
	info, err := os.Stat(s.Config.DataDir)
	if err != nil || !info.IsDir() {
		return false
	}
	probe := filepath.Join(s.Config.DataDir, ".ready-probe")
	file, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return false
	}
	_ = file.Close()
	_ = os.Remove(probe)
	return true
}

// InitializeStorage creates the private data root and fixed subdirectories.
func (s *Store) InitializeStorage() error { return s.initializeStorage() }

func (s *Store) initializeStorage() error {
	root := s.Config.DataDir
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	for _, name := range []string{"users", "uploads", "locks"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			return fmt.Errorf("create %s dir: %w", name, err)
		}
	}
	return nil
}

// EnsureSpace refuses new jobs when free disk space is below the configured
// low-water mark.
func (s *Store) EnsureSpace() error {
	if err := s.initializeStorage(); err != nil {
		return err
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(s.Config.DataDir, &stat); err != nil {
		return fmt.Errorf("stat data dir: %w", err)
	}
	free := stat.Bavail * uint64(stat.Bsize)
	if free < s.Config.DiskMinFreeBytes {
		return ErrDiskLow
	}
	return nil
}

// JobDir returns the per-job private directory.
func (s *Store) JobDir(userID, jobID string) string {
	return filepath.Join(s.Config.DataDir, "users", userID, "jobs", jobID)
}

// PrivatePath resolves a storage key to an absolute path, rejecting traversal
// and symlink escapes outside DATA_DIR.
func (s *Store) PrivatePath(key string) (string, error) {
	if key == "" || filepath.IsAbs(key) {
		return "", errors.New("invalid storage key")
	}
	clean := filepath.Clean(key)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("invalid storage key")
	}
	root, err := filepath.EvalSymlinks(s.Config.DataDir)
	if err != nil {
		return "", fmt.Errorf("resolve data dir: %w", err)
	}
	candidate := filepath.Join(root, clean)
	resolved, err := resolveExisting(candidate)
	if err != nil {
		return "", err
	}
	if !within(root, resolved) {
		return "", errors.New("storage key escapes data dir")
	}
	return resolved, nil
}

// resolveExisting resolves symlinks in the longest existing prefix of path and
// appends the not-yet-created remainder. Missing leaf files are allowed.
func resolveExisting(path string) (string, error) {
	current := path
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// StorageKey maps an absolute path under DATA_DIR to its forward-slash key.
func (s *Store) StorageKey(path string) (string, error) {
	root, err := filepath.EvalSymlinks(s.Config.DataDir)
	if err != nil {
		return "", fmt.Errorf("resolve data dir: %w", err)
	}
	resolved, err := resolveExisting(path)
	if err != nil {
		return "", err
	}
	if !within(root, resolved) {
		return "", errors.New("path escapes data dir")
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// Purge removes a terminal job's files and derived rows, keeping a tombstone.
// It takes a non-blocking flock so an old process still unwinding from an
// expired lease is never raced; false means "try again later".
func (s *Store) Purge(tx *gorm.DB, job *Job) (bool, error) {
	if job.Status == "queued" || job.Status == "running" || job.LeaseToken != nil {
		return false, errors.New("cannot purge an executing lease")
	}
	lockPath := filepath.Join(s.Config.DataDir, "locks", job.ID+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return false, err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck

	if err := os.RemoveAll(s.JobDir(job.UserID, job.ID)); err != nil {
		return false, err
	}
	if err := tx.Where("job_id = ?", job.ID).Delete(&Artifact{}).Error; err != nil {
		return false, err
	}
	if err := tx.Where("job_id = ?", job.ID).Delete(&Track{}).Error; err != nil {
		return false, err
	}
	if err := tx.Where("job_id = ?", job.ID).Delete(&QueueOutbox{}).Error; err != nil {
		return false, err
	}
	now := Now()
	job.DeletedAt = &now
	job.DeleteRequested = true
	job.ConfigSnapshot = map[string]any{}
	job.Warnings = []string{}
	job.Title = "已删除任务"
	job.ErrorMessage = nil
	return true, nil
}

// Complete sets a terminal status, retention deadline and clears the lease.
func Complete(job *Job, status string) {
	now := Now()
	job.Status = status
	job.CompletedAt = &now
	if days := Number(job.ConfigSnapshot, "retention_days"); days > 0 {
		expires := now.Add(time.Duration(days) * 24 * time.Hour)
		job.ExpiresAt = &expires
	} else {
		job.ExpiresAt = nil
	}
	job.LeaseToken = nil
	job.LeaseExpiresAt = nil
}

// SortJobs orders jobs newest first, matching API expectations.
func SortJobs(jobs []Job) {
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].CreatedAt.After(jobs[j].CreatedAt) })
}
