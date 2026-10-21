package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	goredis "github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

// Sentinel outcomes of a lease mutation. errJobStopped means this attempt no
// longer owns the row (a peer or the dispatcher took over): no further writes
// are allowed. errJobCancelled keeps ownership but must stop the pipeline.
var (
	errJobStopped   = errors.New("execution lease was lost")
	errJobCancelled = errors.New("job cancelled")
)

// Runner owns the Redis connections, the pipeline subprocess factory and the
// database lease protocol for one worker or dispatcher process.
type Runner struct {
	store       *core.Store
	logger      *slog.Logger
	client      *goredis.Client
	producer    *producer
	pipeline    *pythonRunner
	env         []string
	lease       time.Duration
	heartbeat   time.Duration
	concurrency int
	// consumeTimeout is the deadline handed to each queue callback. A job's own
	// budget is clamped to it so the pipeline child always ends before the
	// broker deadline, never after.
	consumeTimeout time.Duration
	consumers      []*consumer
	closeOnce      sync.Once
}

// New connects to Redis and prepares a Runner. Concurrency defaults to one
// pipeline at a time; each slot gets its own consumer identity.
func New(store *core.Store) (*Runner, error) {
	if store == nil || store.DB == nil {
		return nil, errors.New("tasks: store is required")
	}
	logger := slog.Default()
	client := goredis.NewClient(&goredis.Options{
		Addr:         core.RedisAddr(store.Config.RedisURL),
		Password:     core.RedisPassword(store.Config.RedisURL),
		DB:           core.RedisDB(store.Config.RedisURL),
		DialTimeout:  5 * time.Second,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	})
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("tasks: connect broker: %w", err)
	}
	lease := time.Duration(store.Config.LeaseSeconds) * time.Second
	if lease <= 0 {
		lease = 120 * time.Second
	}
	config := core.Config(store.Config)
	concurrency := config.WorkerConcurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	runner := &Runner{
		store:       store,
		logger:      logger,
		client:      client,
		producer:    newProducer(client),
		lease:       lease,
		heartbeat:   heartbeatInterval(lease),
		concurrency: concurrency,
		env:         pipelineEnv(config),
	}
	runner.pipeline = newPythonRunner(config.PipelinePython, runner.env,
		pipelineTimeout(nil, config.StageTimeoutSeconds), logger)
	if err := store.InitializeStorage(); err != nil {
		_ = client.Close()
		return nil, err
	}
	if err := runner.producer.connect(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("tasks: connect producer: %w", err)
	}
	return runner, nil
}

// heartbeatInterval keeps roughly three renewals per lease window, matching the
// legacy worker: a slow renewal must never be mistaken for a lost lease.
func heartbeatInterval(lease time.Duration) time.Duration {
	interval := lease / 3
	if interval < time.Second {
		interval = time.Second
	}
	if interval > 20*time.Second {
		interval = 20 * time.Second
	}
	return interval
}

// pipelineEnv builds the child environment. It intentionally does not inherit
// a shell: the command is exec'd directly and only deployment paths are passed.
func pipelineEnv(config core.Config) []string {
	env := make([]string, 0, len(os.Environ())+8)
	seen := map[string]bool{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		seen[key] = true
		env = append(env, entry)
	}
	set := func(key, value string) {
		if seen[key] || strings.TrimSpace(value) == "" {
			return
		}
		seen[key] = true
		env = append(env, key+"="+value)
	}
	set("PYTHONPATH", "/app")
	set("MODEL_PYTHON", config.ModelPython)
	set("DEMUCS_REPO", config.DemucsRepo)
	set("BASIC_PITCH_MODEL_PATH", config.BasicPitchModelPath)
	set("DATA_DIR", config.DataDir)
	set("PYTHONUNBUFFERED", "1")
	return env
}

// Close releases the broker connections. It never touches jobs; the database
// remains authoritative if the process dies.
func (r *Runner) Close() error {
	var err error
	r.closeOnce.Do(func() {
		for _, consumer := range r.consumers {
			if stopErr := consumer.stop(10 * time.Second); stopErr != nil {
				err = stopErr
			}
		}
		if closeErr := r.client.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	})
	return err
}

// Dispatch runs one dispatcher maintenance tick: service heartbeat, expired
// lease recovery, outbox delivery and retention/orphan cleanup.
func (r *Runner) Dispatch(ctx context.Context) error {
	if err := r.beat(ctx, "dispatcher"); err != nil {
		r.logger.Error("dispatcher heartbeat failed", slog.Any("err", err))
	}
	if err := r.recoverLeases(ctx); err != nil {
		return fmt.Errorf("recover leases: %w", err)
	}
	if err := r.deliverOutbox(ctx); err != nil {
		return fmt.Errorf("dispatch outbox: %w", err)
	}
	if err := r.cleanup(ctx); err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	return nil
}

// Work consumes the job stream until the context is cancelled. It registers a
// separate worker service heartbeat, independent of per-task leases.
func (r *Runner) Work(ctx context.Context) error {
	stopHeartbeat := make(chan struct{})
	var heartbeatWG sync.WaitGroup
	heartbeatWG.Add(1)
	go func() {
		defer heartbeatWG.Done()
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			if err := r.beat(ctx, "worker"); err != nil && ctx.Err() == nil {
				r.logger.Error("worker service heartbeat failed", slog.Any("err", err))
			}
			select {
			case <-ctx.Done():
				return
			case <-stopHeartbeat:
				return
			case <-ticker.C:
			}
		}
	}()

	host, _ := os.Hostname()
	if host == "" {
		host = "worker"
	}
	consumeTimeout := r.pipeline.timeout + 30*time.Minute
	r.consumeTimeout = consumeTimeout
	r.consumers = make([]*consumer, 0, r.concurrency)
	for index := 0; index < r.concurrency; index++ {
		uniqueID := fmt.Sprintf("%s-%s-%d", host, uuid.NewString()[:8], index)
		worker := newConsumer(r.client, uniqueID, r.handleMessage, consumeTimeout)
		if err := worker.start(ctx); err != nil {
			for _, started := range r.consumers {
				_ = started.stop(10 * time.Second)
			}
			close(stopHeartbeat)
			heartbeatWG.Wait()
			return fmt.Errorf("start worker %d: %w", index, err)
		}
		r.consumers = append(r.consumers, worker)
	}
	r.logger.Info("worker started", slog.Int("consumers", r.concurrency),
		slog.String("stream", streamKey()), slog.String("group", Group))

	<-ctx.Done()
	close(stopHeartbeat)
	for _, consumer := range r.consumers {
		if err := consumer.stop(time.Duration(consumeTimeout)); err != nil {
			r.logger.Warn("consumer shutdown incomplete", slog.Any("err", err))
		}
	}
	heartbeatWG.Wait()
	return nil
}

// handleMessage decodes and processes one queue message. A returned error keeps
// the message pending for redelivery (infrastructure failure).
func (r *Runner) handleMessage(ctx context.Context, content []byte) error {
	message, err := decodeJobMessage(content)
	if err != nil {
		// A malformed message cannot become correct by retrying: log and drop.
		r.logger.Error("invalid queue message", slog.Any("err", err))
		return nil
	}
	return r.processJob(ctx, message.JobID, message.Attempt)
}

func decodeJobMessage(content []byte) (jobMessage, error) {
	var message jobMessage
	if err := json.Unmarshal(content, &message); err != nil {
		return message, err
	}
	// UUIDs are canonicalized by the API; accepting a non-canonical spelling
	// would make two different keys address the same job.
	if id, err := uuid.Parse(message.JobID); err != nil || id.String() != message.JobID {
		return message, errors.New("invalid job id")
	}
	if message.Attempt < 1 {
		return message, errors.New("invalid attempt")
	}
	return message, nil
}

// processJob claims the attempt, executes the pipeline and finalizes. Returning
// an error means "not durably resolved": the message must be retried and never
// acknowledged, because the database may not have reached a terminal state.
func (r *Runner) processJob(ctx context.Context, jobID string, attempt int) error {
	token, err := r.claim(ctx, jobID, attempt)
	if err != nil {
		return err
	}
	if token == "" {
		// Duplicate, stale attempt, already terminal or superseded: no-op.
		return nil
	}
	r.logger.Info("job claimed", slog.String("job", jobID), slog.Int("attempt", attempt))
	return r.execute(ctx, jobID, attempt, token)
}

// claim atomically transitions a queued attempt to running and installs this
// process's fencing token. It returns "" when another owner acted first.
func (r *Runner) claim(ctx context.Context, jobID string, attempt int) (string, error) {
	token := core.NewID()
	now := core.Now()
	result := r.store.DB.WithContext(ctx).Model(&core.Job{}).
		Where("id = ? AND attempt = ? AND status = ? AND cancel_requested = ? AND delete_requested = ? AND deleted_at IS NULL",
			jobID, attempt, "queued", false, false).
		Updates(map[string]any{
			"status":           "running",
			"stage":            "validating",
			"stage_progress":   nil,
			"lease_token":      token,
			"lease_expires_at": now.Add(r.lease),
			"heartbeat_at":     now,
			"started_at":       now,
		})
	if result.Error != nil {
		return "", result.Error
	}
	if result.RowsAffected != 1 {
		return "", nil
	}
	return token, nil
}

// execution carries one attempt's cancellation wiring.
type execution struct {
	ctx     context.Context
	cancel  context.CancelFunc
	reason  error
	mu      sync.Mutex
	stopped chan struct{}
	once    sync.Once
}

func (e *execution) stop(reason error) {
	e.mu.Lock()
	if e.reason == nil {
		e.reason = reason
	}
	e.mu.Unlock()
	e.once.Do(func() {
		e.cancel()
		close(e.stopped)
	})
}

func (e *execution) failure() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reason
}

// execute drives one attempt: filesystem lock, input resolution, pipeline
// subprocess, publication and cleanup. Terminal database outcomes are written
// here; infrastructure errors are returned to the consumer for redelivery.
func (r *Runner) execute(ctx context.Context, jobID string, attempt int, token string) error {
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	current := &execution{ctx: attemptCtx, cancel: cancel, stopped: make(chan struct{})}

	// Heartbeat thread: renews the lease while the pipeline is quiet. A renewal
	// failure revokes this process's ownership and kills the subprocess.
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(r.heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-attemptCtx.Done():
				return
			case <-ticker.C:
			}
			if err := r.renew(attemptCtx, jobID, attempt, token, nil, nil); err != nil {
				current.stop(err)
				return
			}
		}
	}()
	defer func() {
		cancel()
		<-heartbeatDone
	}()

	info, err := r.loadAttempt(ctx, jobID)
	if err != nil {
		if errors.Is(err, errJobStopped) {
			return nil
		}
		// Database read failed before any file was created: retry later.
		return err
	}
	// Fence before creating any file: a lost lease must leave no directory.
	if err := r.renew(attemptCtx, jobID, attempt, token, nil, nil); err != nil {
		return nil
	}

	progress := func(stage string, fraction *float64) error {
		if current.ctx.Err() != nil {
			if reason := current.failure(); reason != nil {
				return reason
			}
			return errJobStopped
		}
		if !JobStages[stage] {
			return fmt.Errorf("invalid stage %q", stage)
		}
		if err := r.renew(attemptCtx, jobID, attempt, token, &stage, fraction); err != nil {
			current.stop(err)
			return err
		}
		return nil
	}

	lockFile, err := r.acquireLock(attemptCtx, jobID, progress)
	if err != nil {
		return r.finishStopped(ctx, jobID, attempt, token, err, current)
	}
	defer func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}()

	workDir := filepath.Join(r.store.JobDir(info.UserID, jobID), "work",
		fmt.Sprintf("%d", attempt), token)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return err
	}
	defer r.cleanWork(workDir)

	request := pipelineRequest{
		InputPath: info.InputPath,
		WorkDir:   workDir,
		Kind:      info.Kind,
		Config:    info.Config,
	}
	result, runErr := r.pipeline.run(attemptCtx, r.pipelineBudget(info.Config), request, progress)
	if runErr != nil {
		// A revoke (lost lease or user cancel) is remembered on the execution,
		// which takes precedence over the raw context error it caused.
		if reason := current.failure(); reason != nil {
			runErr = reason
		}
		return r.finishFailure(ctx, jobID, attempt, token, runErr, current)
	}
	if err := progress("finalizing", nil); err != nil {
		return r.finishStopped(ctx, jobID, attempt, token, err, current)
	}
	if err := r.publish(ctx, jobID, attempt, token, workDir, result); err != nil {
		if errors.Is(err, errJobStopped) || errors.Is(err, errJobCancelled) {
			return r.finishStopped(ctx, jobID, attempt, token, err, current)
		}
		var pipelineFailure *PipelineError
		if errors.As(err, &pipelineFailure) {
			return r.finishFailure(ctx, jobID, attempt, token, pipelineFailure, current)
		}
		return err
	}
	r.logger.Info("job published", slog.String("job", jobID), slog.Int("attempt", attempt))
	return nil
}

// finishStopped discards the attempt when it lost ownership, or records the
// terminal cancellation when the lease is still ours.
func (r *Runner) finishStopped(ctx context.Context, jobID string, attempt int, token string, cause error, current *execution) error {
	if errors.Is(cause, errJobCancelled) || (current != nil && errors.Is(current.failure(), errJobCancelled)) {
		if err := r.fail(ctx, jobID, attempt, token, nil); err != nil {
			return err
		}
	}
	return nil
}

// finishFailure records a terminal pipeline error (sanitized) or treats a lost
// lease as a no-op owned by another attempt.
func (r *Runner) finishFailure(ctx context.Context, jobID string, attempt int, token string, cause error, current *execution) error {
	var pipelineFailure *PipelineError
	if errors.As(cause, &pipelineFailure) {
		if err := r.fail(ctx, jobID, attempt, token, pipelineFailure); err != nil {
			return err
		}
		return nil
	}
	if errors.Is(cause, errJobStopped) || errors.Is(cause, errJobCancelled) {
		return r.finishStopped(ctx, jobID, attempt, token, cause, current)
	}
	if ctx.Err() != nil {
		// Shutdown: leave the row running; the dispatcher recovers it after the
		// lease expires and re-queues the attempt.
		return nil
	}
	r.logger.Error("pipeline execution failed", slog.String("job", jobID),
		slog.Int("attempt", attempt), slog.Any("err", cause))
	generic := &PipelineError{Code: "PROCESSING_FAILED", Message: "处理失败，请联系管理员或重试"}
	if err := r.fail(ctx, jobID, attempt, token, generic); err != nil {
		return err
	}
	return nil
}

// pipelineBudget resolves the execution budget for one attempt and keeps it
// below the queue callback deadline, so a slow job is killed by the pipeline
// timeout (a sanitized STAGE_TIMEOUT failure) instead of being silently
// cancelled by the consumer deadline.
func (r *Runner) pipelineBudget(snapshot map[string]any) time.Duration {
	budget := pipelineTimeout(snapshot, r.store.Config.StageTimeoutSeconds)
	if r.consumeTimeout > time.Minute && budget > r.consumeTimeout-time.Minute {
		budget = r.consumeTimeout - time.Minute
	}
	return budget
}

// attemptInfo is everything the worker needs from the database for one run.
type attemptInfo struct {
	UserID    string
	Kind      string
	Config    map[string]any
	InputPath string
}

func (r *Runner) loadAttempt(ctx context.Context, jobID string) (attemptInfo, error) {
	var info attemptInfo
	db := r.store.DB.WithContext(ctx)
	var job core.Job
	if err := db.Where("id = ?", jobID).Take(&job).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return info, errJobStopped
		}
		return info, err
	}
	if job.DeletedAt != nil {
		return info, errJobStopped
	}
	var original core.Artifact
	err := db.Where("job_id = ? AND kind = ?", jobID, "original").
		Order("created_at DESC").Take(&original).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return info, &PipelineError{Code: "PROCESSING_FAILED", Message: "处理失败，请联系管理员或重试"}
		}
		return info, err
	}
	if original.StorageKey == nil || *original.StorageKey == "" {
		return info, &PipelineError{Code: "PROCESSING_FAILED", Message: "处理失败，请联系管理员或重试"}
	}
	inputPath, err := r.store.PrivatePath(*original.StorageKey)
	if err != nil {
		return info, &PipelineError{Code: "PROCESSING_FAILED", Message: "处理失败，请联系管理员或重试"}
	}
	if stat, statErr := os.Stat(inputPath); statErr != nil || !stat.Mode().IsRegular() {
		return info, &PipelineError{Code: "PROCESSING_FAILED", Message: "处理失败，请联系管理员或重试"}
	}
	info = attemptInfo{
		UserID: job.UserID, Kind: job.Kind, Config: job.ConfigSnapshot,
		InputPath: inputPath,
	}
	if info.Config == nil {
		info.Config = map[string]any{}
	}
	return info, nil
}

// acquireLock waits for the per-job execution lock. Older attempts keep the
// lock until they have fully unwound, so two attempts never share files.
func (r *Runner) acquireLock(ctx context.Context, jobID string, progress progressFunc) (*os.File, error) {
	lockDir := filepath.Join(r.store.Config.DataDir, "locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(lockDir, jobID+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = lock.Close()
			return nil, err
		}
		if ctx.Err() != nil {
			_ = lock.Close()
			return nil, ctx.Err()
		}
		// Keep the lease alive while a stale attempt finishes its cleanup.
		if err := progress("validating", nil); err != nil {
			_ = lock.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = lock.Close()
			return nil, ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
}

// renew refreshes the lease under a row lock and optionally records progress.
// It is the single fence used by heartbeats, progress and publication.
func (r *Runner) renew(ctx context.Context, jobID string, attempt int, token string, stage *string, fraction *float64) error {
	if stage != nil {
		if !JobStages[*stage] {
			return fmt.Errorf("invalid pipeline stage %q", *stage)
		}
		if fraction != nil {
			value := *fraction
			if value != value || value < 0 || value > 1 {
				return errors.New("invalid pipeline progress")
			}
		}
	}
	return r.store.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		job, err := lockedJob(tx, jobID)
		if err != nil {
			return err
		}
		if job.Attempt != attempt || job.Status != "running" || job.LeaseToken == nil ||
			*job.LeaseToken != token || job.LeaseExpiresAt == nil ||
			!job.LeaseExpiresAt.After(core.Now()) || job.DeletedAt != nil {
			return errJobStopped
		}
		if job.CancelRequested || job.DeleteRequested {
			return errJobCancelled
		}
		now := core.Now()
		updates := map[string]any{
			"heartbeat_at":     now,
			"lease_expires_at": now.Add(r.lease),
		}
		if stage != nil {
			updates["stage"] = *stage
			updates["stage_progress"] = fraction
		}
		return tx.Model(&core.Job{}).Where("id = ?", jobID).Updates(updates).Error
	})
}

func lockedJob(tx *gorm.DB, jobID string) (*core.Job, error) {
	var job core.Job
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", jobID).Take(&job).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errJobStopped
		}
		return nil, err
	}
	return &job, nil
}

// publication is the prepared, fenced write set. Files are copied before the
// transaction so a long copy can never hold the row lock past the lease.
type publication struct {
	tracks    []core.Track
	artifacts []core.Artifact
	warnings  []string
	engine    string
	device    *string
	partial   bool
}

// publish validates the pipeline result, copies artifacts atomically and, in a
// short fenced transaction, replaces derived rows and completes the job.
func (r *Runner) publish(ctx context.Context, jobID string, attempt int, token, workDir string, result pipelineResult) error {
	tracks, artifacts, err := validateResult(result)
	if err != nil {
		r.logger.Error("invalid pipeline result", slog.String("job", jobID), slog.Any("err", err))
		return &PipelineError{Code: "PROCESSING_FAILED", Message: "处理失败，请联系管理员或重试"}
	}

	var userID string
	if err := r.store.DB.WithContext(ctx).Model(&core.Job{}).Select("user_id").
		Where("id = ?", jobID).Scan(&userID).Error; err != nil {
		return err
	}
	if userID == "" {
		return errJobStopped
	}
	outputDir := filepath.Join(r.store.JobDir(userID, jobID), "results", fmt.Sprintf("%d", attempt))
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return err
	}

	publication := publication{engine: result.Engine}
	if strings.TrimSpace(result.ActualDevice) != "" {
		device := truncate(result.ActualDevice, 32)
		publication.device = &device
	}
	for _, warning := range result.Warnings {
		publication.warnings = append(publication.warnings, truncate(warning, 500))
	}

	for _, track := range tracks {
		publication.tracks = append(publication.tracks, core.Track{
			ID: track.ID, JobID: jobID, Label: track.Label, Program: track.Program,
			Confidence: track.Confidence, LabelSource: track.LabelSource,
			IsDrum: track.IsDrum, SourceStemIDs: track.SourceStemIDs,
		})
	}
	for _, artifact := range artifacts {
		row := core.Artifact{
			ID: artifact.ID, JobID: jobID, TrackID: artifact.TrackID, Kind: artifact.Kind,
			Label: artifact.Label, Mime: artifact.Mime, Status: artifact.Status,
			Error: artifact.Error, Attempt: attempt, CreatedAt: core.Now(),
		}
		if artifact.Status == "ready" {
			// Resolve, hash and install the file before touching the database.
			source, err := resolveArtifactPath(workDir, artifact.Path)
			if err != nil {
				r.logger.Error("artifact path rejected", slog.String("job", jobID),
					slog.String("path", artifact.Path), slog.Any("err", err))
				return &PipelineError{Code: "PROCESSING_FAILED", Message: "处理失败，请联系管理员或重试"}
			}
			digest, size, err := copyArtifact(source, outputDir, artifact.ID, artifact.Suffix)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return &PipelineError{Code: "PROCESSING_FAILED", Message: "处理失败，请联系管理员或重试"}
				}
				return err
			}
			key, err := r.store.StorageKey(filepath.Join(outputDir, artifact.ID+artifact.Suffix))
			if err != nil {
				return err
			}
			row.StorageKey = &key
			row.Bytes = &size
			row.SHA256 = &digest
			row.CreatedAt = core.Now()
		} else {
			publication.partial = true
		}
		publication.artifacts = append(publication.artifacts, row)
	}
	if err := syncDir(outputDir); err != nil {
		return err
	}

	// Short fenced transaction: the token decides publication, not the file
	// names. A peer attempt can never publish into another attempt's rows.
	return r.store.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		job, err := lockedJob(tx, jobID)
		if err != nil {
			return err
		}
		if job.Attempt != attempt || job.Status != "running" || job.LeaseToken == nil ||
			*job.LeaseToken != token || job.DeletedAt != nil {
			return errJobStopped
		}
		if job.CancelRequested || job.DeleteRequested {
			return errJobCancelled
		}
		if err := tx.Where("job_id = ? AND kind <> ?", jobID, "original").
			Delete(&core.Artifact{}).Error; err != nil {
			return err
		}
		if err := tx.Where("job_id = ?", jobID).Delete(&core.Track{}).Error; err != nil {
			return err
		}
		if len(publication.tracks) > 0 {
			if err := tx.Create(&publication.tracks).Error; err != nil {
				return err
			}
		}
		if len(publication.artifacts) > 0 {
			if err := tx.Create(&publication.artifacts).Error; err != nil {
				return err
			}
		}
		// The copy above may have exhausted the lease: never publish afterwards.
		if job.LeaseExpiresAt == nil || !job.LeaseExpiresAt.After(core.Now()) {
			return errJobStopped
		}
		finalizing := "finalizing"
		progress := 1.0
		// jsonColumns is a struct update on purpose: GORM only applies the
		// `serializer:json` encoder for model/struct updates, so a map update
		// would send a plain text value and Postgres would reject it.
		fields := core.Job{
			Warnings:       publication.warnings,
			Stage:          &finalizing,
			StageProgress:  &progress,
			HeartbeatAt:    timestamp(),
			LeaseExpiresAt: timestamp(r.lease),
		}
		if publication.device != nil {
			fields.ActualDevice = publication.device
		}
		if strings.TrimSpace(publication.engine) != "" {
			engine := truncate(publication.engine, 32)
			fields.Engine = engine
		}
		if err := tx.Model(&core.Job{}).Where("id = ?", jobID).Updates(&fields).Error; err != nil {
			return err
		}
		if err := tx.Where("id = ?", jobID).Take(job).Error; err != nil {
			return err
		}
		if publication.partial {
			core.Complete(job, "partial")
		} else {
			core.Complete(job, "succeeded")
		}
		if err := tx.Model(&core.Job{}).Where("id = ?", jobID).Updates(map[string]any{
			"status":           job.Status,
			"completed_at":     job.CompletedAt,
			"expires_at":       job.ExpiresAt,
			"lease_token":      nil,
			"lease_expires_at": nil,
		}).Error; err != nil {
			return err
		}
		return core.Audit(tx, nil, "job.completed", jobPointer(jobID), map[string]any{
			"status": job.Status, "attempt": attempt,
		})
	})
}

// fail records a terminal failed/cancelled outcome for this attempt only.
func (r *Runner) fail(ctx context.Context, jobID string, attempt int, token string, cause *PipelineError) error {
	return r.store.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		job, err := lockedJob(tx, jobID)
		if err != nil {
			if errors.Is(err, errJobStopped) {
				return nil
			}
			return err
		}
		if job.Attempt != attempt || job.Status != "running" || job.LeaseToken == nil || *job.LeaseToken != token {
			// A peer attempt or the dispatcher owns the row now: no writes.
			return nil
		}
		status := "failed"
		if job.CancelRequested || job.DeleteRequested || cause == nil {
			status = "cancelled"
		}
		updates := map[string]any{}
		if status == "failed" {
			code, message := "PROCESSING_FAILED", "处理失败，请联系管理员或重试"
			if cause != nil {
				if strings.TrimSpace(cause.Code) != "" && len(cause.Code) <= 64 {
					code = cause.Code
				}
				if strings.TrimSpace(cause.Message) != "" {
					message = truncate(cause.Message, maxErrorRunes)
				}
			}
			updates["error_code"] = code
			updates["error_message"] = message
		}
		core.Complete(job, status)
		updates["status"] = job.Status
		updates["completed_at"] = job.CompletedAt
		updates["expires_at"] = job.ExpiresAt
		updates["lease_token"] = nil
		updates["lease_expires_at"] = nil
		if err := tx.Model(&core.Job{}).Where("id = ?", jobID).Updates(updates).Error; err != nil {
			return err
		}
		return core.Audit(tx, nil, "job.stopped", jobPointer(jobID), map[string]any{
			"status": job.Status, "attempt": attempt,
		})
	})
}

// cleanWork removes the attempt directory after the heartbeat has stopped, so a
// slow rmtree can never be mistaken for a lost lease by a peer. The attempt
// directory (<work>/<attempt>) is owned by this execution, so it is removed with
// its token subdirectory instead of leaving empty parents behind.
func (r *Runner) cleanWork(workDir string) {
	if err := os.RemoveAll(filepath.Dir(workDir)); err != nil {
		r.logger.Warn("work directory cleanup failed", slog.String("dir", workDir), slog.Any("err", err))
	}
}

func jobPointer(id string) *string { return &id }

// timestamp returns now (or now+offset) as a pointer for struct updates.
func timestamp(offset ...time.Duration) *time.Time {
	now := core.Now()
	for _, delta := range offset {
		now = now.Add(delta)
	}
	return &now
}
