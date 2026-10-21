package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

// runJob executes one attempt through the same claim/execute path the consumer
// uses and returns the resulting job row.
func runJob(t *testing.T, runner *Runner, jobID string, attempt int) (*core.Job, error) {
	t.Helper()
	err := runner.processJob(context.Background(), jobID, attempt)
	return reloadJob(t, runner.store, jobID), err
}

func jobByStatus(t *testing.T, store *core.Store, jobID string) *core.Job {
	t.Helper()
	return reloadJob(t, store, jobID)
}

func TestProcessJobPublishesResultAndClearsLease(t *testing.T) {
	store := requireDatabase(t)
	job := seededJob(t, store, nil)
	runner := newTestRunner(t, store, "python3")

	completed, err := runJob(t, runner, job.ID, 1)
	if err != nil {
		t.Fatalf("processJob: %v", err)
	}
	if completed.Status != "succeeded" {
		t.Fatalf("status = %q (error: %v/%v)", completed.Status, completed.ErrorCode, completed.ErrorMessage)
	}
	if completed.Stage == nil || *completed.Stage != "finalizing" {
		t.Fatalf("stage = %v", completed.Stage)
	}
	if completed.LeaseToken != nil || completed.LeaseExpiresAt != nil {
		t.Fatalf("lease not cleared: %v / %v", completed.LeaseToken, completed.LeaseExpiresAt)
	}
	if completed.CompletedAt == nil {
		t.Fatal("completed_at must be set")
	}
	if completed.ActualDevice == nil || *completed.ActualDevice != "cpu" {
		t.Fatalf("actual_device = %v", completed.ActualDevice)
	}
	if len(completed.Warnings) != 1 {
		t.Fatalf("warnings = %v", completed.Warnings)
	}

	var tracks []core.Track
	if err := store.DB.Where("job_id = ?", job.ID).Find(&tracks).Error; err != nil {
		t.Fatalf("load tracks: %v", err)
	}
	if len(tracks) != 1 || tracks[0].Label != "Piano" {
		t.Fatalf("tracks = %+v", tracks)
	}

	var artifacts []core.Artifact
	if err := store.DB.Where("job_id = ?", job.ID).Order("kind").Find(&artifacts).Error; err != nil {
		t.Fatalf("load artifacts: %v", err)
	}
	if len(artifacts) != 2 {
		t.Fatalf("artifacts = %+v", artifacts)
	}
	var score *core.Artifact
	for index := range artifacts {
		if artifacts[index].Kind == "score" {
			score = &artifacts[index]
		}
	}
	if score == nil || score.Status != "ready" {
		t.Fatalf("score artifact = %+v", score)
	}
	if score.StorageKey == nil || score.SHA256 == nil || score.Bytes == nil {
		t.Fatalf("score artifact metadata incomplete: %+v", score)
	}
	if score.Attempt != 1 {
		t.Fatalf("artifact attempt = %d", score.Attempt)
	}
	if *score.SHA256 != "4d3d03aab5b1e0e2f0a57d0e9e2e3b1b9e1b5be4b4fbfbf46b9a2f4d0b3b6c3c" &&
		len(*score.SHA256) != 64 {
		t.Fatalf("sha256 = %q", *score.SHA256)
	}
	published, err := store.PrivatePath(*score.StorageKey)
	if err != nil {
		t.Fatalf("resolve published artifact: %v", err)
	}
	content, err := os.ReadFile(published)
	if err != nil {
		t.Fatalf("read published artifact: %v", err)
	}
	if string(content) != "<score/>" {
		t.Fatalf("published content = %q", content)
	}
	// The attempt work tree is removed after publication.
	workDir := filepath.Join(store.JobDir(job.UserID, job.ID), "work")
	if entries, err := os.ReadDir(workDir); err == nil && len(entries) > 0 {
		t.Fatalf("work tree not cleaned: %v", entries)
	}

	var audit core.AuditLog
	if err := store.DB.Where("resource_id = ? AND action = ?", job.ID, "job.completed").Take(&audit).Error; err != nil {
		t.Fatalf("load completion audit: %v", err)
	}
}

func TestProcessJobDuplicateMessageIsNoop(t *testing.T) {
	store := requireDatabase(t)
	job := seededJob(t, store, nil)
	runner := newTestRunner(t, store, "python3")

	if _, err := runJob(t, runner, job.ID, 1); err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	// A redelivered message must not re-run a terminal attempt.
	second, err := runJob(t, runner, job.ID, 1)
	if err != nil {
		t.Fatalf("redelivery returned an error (would block the ACK): %v", err)
	}
	if second.Status != "succeeded" || second.Attempt != 1 {
		t.Fatalf("redelivery changed the row: %q attempt %d", second.Status, second.Attempt)
	}

	var count int64
	if err := store.DB.Model(&core.Track{}).Where("job_id = ?", job.ID).Count(&count).Error; err != nil {
		t.Fatalf("count tracks: %v", err)
	}
	if count != 1 {
		t.Fatalf("tracks = %d after redelivery", count)
	}
}

func TestProcessJobStaleAttemptIsNoop(t *testing.T) {
	store := requireDatabase(t)
	job := seededJob(t, store, nil)
	runner := newTestRunner(t, store, "python3")

	if _, err := runJob(t, runner, job.ID, 1); err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	// Attempt 5 never existed: claiming must be a silent no-op.
	stale, err := runJob(t, runner, job.ID, 5)
	if err != nil {
		t.Fatalf("stale attempt error: %v", err)
	}
	if stale.Attempt != 1 || stale.Status != "succeeded" {
		t.Fatalf("stale claim mutated the job: attempt %d status %q", stale.Attempt, stale.Status)
	}
}

func TestProcessJobSupersededAttemptCannotPublish(t *testing.T) {
	store := requireDatabase(t)
	// The row was already re-queued as attempt 2 by the dispatcher.
	job := seededJob(t, store, func(job *core.Job) { job.Attempt = 2 })
	runner := newTestRunner(t, store, "python3")

	result, err := runJob(t, runner, job.ID, 1)
	if err != nil {
		t.Fatalf("superseded attempt error: %v", err)
	}
	if result.Status != "queued" || result.Attempt != 2 {
		t.Fatalf("superseded attempt mutated the job: %q attempt %d", result.Status, result.Attempt)
	}
	var count int64
	if err := store.DB.Model(&core.Artifact{}).Where("job_id = ? AND kind <> ?", job.ID, "original").
		Count(&count).Error; err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	if count != 0 {
		t.Fatalf("superseded attempt published %d derived artifacts", count)
	}
}

func TestProcessJobCancelKeepsCancelled(t *testing.T) {
	store := requireDatabase(t)
	job := seededJob(t, store, nil)
	// The API sets cancel_requested before the worker claims: claim refuses it,
	// and the attempt must stay a no-op rather than run the pipeline.
	if err := store.DB.Model(&core.Job{}).Where("id = ?", job.ID).
		Update("cancel_requested", true).Error; err != nil {
		t.Fatalf("set cancel flag: %v", err)
	}
	runner := newTestRunner(t, store, "python3")

	result, err := runJob(t, runner, job.ID, 1)
	if err != nil {
		t.Fatalf("cancelled claim error: %v", err)
	}
	if result.Status != "queued" {
		t.Fatalf("cancelled-but-queued job changed status to %q", result.Status)
	}
}

func TestProcessJobCancelDuringRunStopsPipeline(t *testing.T) {
	store := requireDatabase(t)
	job := seededJob(t, store, nil)
	runner := newTestRunner(t, store, "python3")
	runner.pipeline.args = []string{testdataPath(t, "fake_pipeline.py")}
	runner.pipeline.env = append(runner.env, "FAKE_MODE=cancel")

	// Cancel shortly after the attempt starts, the way the API does it.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = store.DB.Model(&core.Job{}).Where("id = ?", job.ID).
			Update("cancel_requested", true).Error
	}()

	err := runner.processJob(ctx, job.ID, 1)
	cancel()
	if err != nil {
		t.Fatalf("processJob returned an error (message would stay pending): %v", err)
	}
	result := reloadJob(t, store, job.ID)
	if result.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", result.Status)
	}
	if result.CompletedAt == nil {
		t.Fatal("completed_at must be set on cancellation")
	}
	if result.LeaseToken != nil {
		t.Fatal("lease must be cleared on cancellation")
	}
}

func TestProcessJobLostLeaseAbortsWithoutTerminalWrite(t *testing.T) {
	store := requireDatabase(t)
	job := seededJob(t, store, nil)
	runner := newTestRunner(t, store, "python3")
	runner.pipeline.args = []string{testdataPath(t, "fake_pipeline.py")}
	runner.pipeline.env = append(runner.env, "FAKE_MODE=cancel")

	// Simulate the dispatcher revoking the lease mid-run by clearing the token
	// and expiring the lease: the worker must stop and must not write a verdict.
	go func() {
		time.Sleep(300 * time.Millisecond)
		past := core.Now().Add(-time.Minute)
		_ = store.DB.Model(&core.Job{}).Where("id = ?", job.ID).
			Updates(map[string]any{"lease_token": nil, "lease_expires_at": past}).Error
	}()

	if err := runner.processJob(context.Background(), job.ID, 1); err != nil {
		t.Fatalf("processJob returned an error: %v", err)
	}
	result := reloadJob(t, store, job.ID)
	if result.Status != "running" {
		t.Fatalf("status = %q; a process without the lease must not write a verdict", result.Status)
	}
	if result.CompletedAt != nil {
		t.Fatal("completed_at set by a process that lost its lease")
	}
}

func TestProcessJobPipelineErrorFailsJob(t *testing.T) {
	store := requireDatabase(t)
	job := seededJob(t, store, nil)
	runner := newTestRunner(t, store, "python3")
	runner.pipeline.env = append(runner.env, "FAKE_MODE=error")

	completed, err := runJob(t, runner, job.ID, 1)
	if err != nil {
		t.Fatalf("processJob: %v", err)
	}
	if completed.Status != "failed" {
		t.Fatalf("status = %q", completed.Status)
	}
	if completed.ErrorCode == nil || *completed.ErrorCode != "UNSUPPORTED_AUDIO" {
		t.Fatalf("error_code = %v", completed.ErrorCode)
	}
	if completed.ErrorMessage == nil || *completed.ErrorMessage != "无法解码该音频" {
		t.Fatalf("error_message = %v", completed.ErrorMessage)
	}
	if completed.LeaseToken != nil {
		t.Fatal("lease not cleared after failure")
	}
}

func TestProcessJobProtocolViolationIsSanitized(t *testing.T) {
	store := requireDatabase(t)
	job := seededJob(t, store, nil)
	runner := newTestRunner(t, store, "python3")
	runner.pipeline.env = append(runner.env, "FAKE_MODE=garbage")

	completed, err := runJob(t, runner, job.ID, 1)
	if err != nil {
		t.Fatalf("processJob: %v", err)
	}
	if completed.Status != "failed" || completed.ErrorCode == nil ||
		*completed.ErrorCode != "PROCESSING_FAILED" {
		t.Fatalf("status/code = %q/%v", completed.Status, completed.ErrorCode)
	}
	if completed.ErrorMessage != nil && strings.Contains(*completed.ErrorMessage, "not json") {
		t.Fatalf("raw protocol output leaked: %q", *completed.ErrorMessage)
	}
}

func TestProcessJobPathEscapeIsRejected(t *testing.T) {
	store := requireDatabase(t)
	job := seededJob(t, store, nil)
	runner := newTestRunner(t, store, "python3")
	runner.pipeline.env = append(runner.env, "FAKE_MODE=escape_path")

	completed, err := runJob(t, runner, job.ID, 1)
	if err != nil {
		t.Fatalf("processJob: %v", err)
	}
	if completed.Status != "failed" {
		t.Fatalf("status = %q, want failed", completed.Status)
	}
	var count int64
	if err := store.DB.Model(&core.Artifact{}).Where("job_id = ? AND kind = ?", job.ID, "midi").
		Count(&count).Error; err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	if count != 0 {
		t.Fatal("an artifact outside the attempt directory was published")
	}
	// The escaped file must not have been copied into results.
	results := filepath.Join(store.JobDir(job.UserID, job.ID), "results")
	_ = filepath.Walk(results, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if strings.Contains(path, "escaped") {
			t.Fatalf("escaped artifact was copied: %s", path)
		}
		return nil
	})
}

func TestProcessJobPartialWhenOptionalArtifactFails(t *testing.T) {
	store := requireDatabase(t)
	job := seededJob(t, store, nil)
	runner := newTestRunner(t, store, "python3")
	runner.pipeline.env = append(runner.env, "FAKE_MODE=fail_artifact")

	completed, err := runJob(t, runner, job.ID, 1)
	if err != nil {
		t.Fatalf("processJob: %v", err)
	}
	if completed.Status != "partial" {
		t.Fatalf("status = %q, want partial", completed.Status)
	}
	var failed core.Artifact
	if err := store.DB.Where("job_id = ? AND status = ?", job.ID, "failed").Take(&failed).Error; err != nil {
		t.Fatalf("load failed artifact: %v", err)
	}
	if failed.StorageKey != nil {
		t.Fatal("a failed artifact must not have a storage key")
	}
	if failed.Error == nil || *failed.Error != "musescore 不可用" {
		t.Fatalf("failed artifact error = %v", failed.Error)
	}
}

func TestProcessJobRetryReplacesDerivedRows(t *testing.T) {
	store := requireDatabase(t)
	job := seededJob(t, store, nil)
	runner := newTestRunner(t, store, "python3")
	runner.pipeline.env = append(runner.env, "FAKE_MODE=fail_artifact")

	if _, err := runJob(t, runner, job.ID, 1); err != nil {
		t.Fatalf("attempt 1: %v", err)
	}
	// The API re-queues a partial job as the next attempt.
	if err := store.DB.Model(&core.Job{}).Where("id = ?", job.ID).
		Updates(map[string]any{"status": "queued", "attempt": 2, "stage": nil}).Error; err != nil {
		t.Fatalf("requeue: %v", err)
	}
	runner.pipeline.env = append(runner.env, "FAKE_MODE=success")

	completed, err := runJob(t, runner, job.ID, 2)
	if err != nil {
		t.Fatalf("attempt 2: %v", err)
	}
	if completed.Status != "succeeded" {
		t.Fatalf("status = %q", completed.Status)
	}
	var tracks, artifacts int64
	store.DB.Model(&core.Track{}).Where("job_id = ?", job.ID).Count(&tracks)
	store.DB.Model(&core.Artifact{}).Where("job_id = ? AND kind <> ?", job.ID, "original").Count(&artifacts)
	if tracks != 1 || artifacts != 1 {
		t.Fatalf("derived rows = %d tracks / %d artifacts, want 1/1", tracks, artifacts)
	}
	var attempt int
	if err := store.DB.Model(&core.Artifact{}).Where("job_id = ? AND kind = ?", job.ID, "score").
		Pluck("attempt", &attempt).Error; err != nil {
		t.Fatalf("load artifact attempt: %v", err)
	}
	if attempt != 2 {
		t.Fatalf("artifact attempt = %d, want 2", attempt)
	}
}

func TestProcessJobHeartbeatKeepsLeaseAlive(t *testing.T) {
	store := requireDatabase(t)
	// A three second lease with a one second heartbeat: a job that runs longer
	// than the lease must still own it.
	store.Config.LeaseSeconds = 3
	job := seededJob(t, store, nil)
	runner := newTestRunner(t, store, "python3")
	runner.pipeline.args = []string{testdataPath(t, "fake_pipeline.py")}
	runner.pipeline.env = append(runner.env, "FAKE_SLEEP_GRACE=0")

	started := core.Now()
	completed, err := runJob(t, runner, job.ID, 1)
	if err != nil {
		t.Fatalf("processJob: %v", err)
	}
	if completed.Status != "succeeded" {
		t.Fatalf("status = %q", completed.Status)
	}
	if completed.HeartbeatAt == nil || completed.HeartbeatAt.Before(started) {
		t.Fatalf("heartbeat_at = %v", completed.HeartbeatAt)
	}
}

func TestAcquireLockSerializesAttempts(t *testing.T) {
	store := requireDatabase(t)
	job := seededJob(t, store, nil)
	runner := newTestRunner(t, store, "python3")

	lock, err := runner.acquireLock(context.Background(), job.ID, func(string, *float64) error { return nil })
	if err != nil {
		t.Fatalf("first acquireLock: %v", err)
	}
	// A second attempt must block until the first releases, then proceed.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	acquired := make(chan error, 1)
	go func() {
		second, secondErr := runner.acquireLock(ctx, job.ID, func(string, *float64) error { return nil })
		if secondErr == nil {
			_ = second.Close()
		}
		acquired <- secondErr
	}()
	select {
	case err := <-acquired:
		t.Fatalf("second acquireLock returned immediately: %v", err)
	case <-time.After(400 * time.Millisecond):
	}
	_ = lock.Close()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("second acquireLock: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second acquireLock never progressed after the lock was freed")
	}
}

func TestPurgeLeavesTombstoneAndRemovesFiles(t *testing.T) {
	store := requireDatabase(t)
	job := seededJob(t, store, nil)
	runner := newTestRunner(t, store, "python3")

	completed, err := runJob(t, runner, job.ID, 1)
	if err != nil {
		t.Fatalf("processJob: %v", err)
	}
	if completed.Status != "succeeded" {
		t.Fatalf("status = %q", completed.Status)
	}
	jobDir := store.JobDir(job.UserID, job.ID)

	// Retention expiry triggers the sweep.
	if err := store.DB.Model(&core.Job{}).Where("id = ?", job.ID).
		Updates(map[string]any{"expires_at": core.Now().Add(-time.Minute)}).Error; err != nil {
		t.Fatalf("expire job: %v", err)
	}
	if err := runner.cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	tombstone := reloadJob(t, store, job.ID)
	if tombstone.DeletedAt == nil {
		t.Fatal("purge must leave a deleted_at tombstone")
	}
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Fatalf("job directory still present: %v", err)
	}
	var artifacts int64
	if err := store.DB.Model(&core.Artifact{}).Where("job_id = ?", job.ID).Count(&artifacts).Error; err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	if artifacts != 0 {
		t.Fatalf("artifacts = %d after purge", artifacts)
	}
	var audit core.AuditLog
	if err := store.DB.Where("resource_id = ? AND action = ?", job.ID, "job.purged").Take(&audit).Error; err != nil {
		t.Fatalf("purge audit: %v", err)
	}
}

func TestPurgeSkipsRunningAndLockedJobs(t *testing.T) {
	store := requireDatabase(t)
	job := runningJob(t, store, "token", nil)
	runner := newTestRunner(t, store, "python3")

	if err := runner.purgeJob(context.Background(), job.ID); err != nil {
		t.Fatalf("purgeJob: %v", err)
	}
	if reloadJob(t, store, job.ID).DeletedAt != nil {
		t.Fatal("a running job must never be purged")
	}

	// Terminal but a stale process still holds the execution lock.
	terminal := seededJob(t, store, nil)
	if _, err := runJob(t, runner, terminal.ID, 1); err != nil {
		t.Fatalf("processJob: %v", err)
	}
	lock, err := runner.acquireLock(context.Background(), terminal.ID, func(string, *float64) error { return nil })
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	defer lock.Close()
	if err := runner.purgeJob(context.Background(), terminal.ID); err != nil {
		t.Fatalf("purgeJob while locked: %v", err)
	}
	if reloadJob(t, store, terminal.ID).DeletedAt != nil {
		t.Fatal("a job whose execution lock is held must be purged later, not now")
	}
}

func TestRecoverLeasesRequeuesAndExhausts(t *testing.T) {
	store := requireDatabase(t)
	runner := newTestRunner(t, store, "")

	expired := runningJob(t, store, "old-token", func(job *core.Job) {
		past := core.Now().Add(-time.Minute)
		job.LeaseExpiresAt = &past
	})
	if err := runner.recoverLeases(context.Background()); err != nil {
		t.Fatalf("recoverLeases: %v", err)
	}
	recovered := reloadJob(t, store, expired.ID)
	if recovered.Status != "queued" || recovered.Attempt != 2 {
		t.Fatalf("recovered job = %q attempt %d", recovered.Status, recovered.Attempt)
	}
	if recovered.LeaseToken != nil || recovered.LeaseExpiresAt != nil {
		t.Fatal("recovery must clear the fencing token")
	}
	if len(recovered.Warnings) != 1 || !strings.Contains(recovered.Warnings[0], "心跳中断") {
		t.Fatalf("warnings = %v", recovered.Warnings)
	}
	var outbox core.QueueOutbox
	if err := store.DB.Where("job_id = ? AND attempt = ?", expired.ID, 2).Take(&outbox).Error; err != nil {
		t.Fatalf("outbox row for the re-queued attempt: %v", err)
	}

	// The retry budget is bounded: attempt 3 is the last recovery.
	lastChance := runningJob(t, store, "old-token", func(job *core.Job) {
		past := core.Now().Add(-time.Minute)
		job.LeaseExpiresAt = &past
		job.Attempt = maxRecoveryAttempts
	})
	if err := runner.recoverLeases(context.Background()); err != nil {
		t.Fatalf("recoverLeases: %v", err)
	}
	exhausted := reloadJob(t, store, lastChance.ID)
	if exhausted.Status != "failed" {
		t.Fatalf("status = %q, want failed", exhausted.Status)
	}
	if exhausted.ErrorCode == nil || *exhausted.ErrorCode != "WORKER_LOST" {
		t.Fatalf("error_code = %v", exhausted.ErrorCode)
	}
	if exhausted.CompletedAt == nil {
		t.Fatal("completed_at must be set")
	}
}

func TestRecoverLeasesHonoursCancellation(t *testing.T) {
	store := requireDatabase(t)
	runner := newTestRunner(t, store, "")
	job := runningJob(t, store, "old-token", func(job *core.Job) {
		past := core.Now().Add(-time.Minute)
		job.LeaseExpiresAt = &past
		job.CancelRequested = true
	})
	if err := runner.recoverLeases(context.Background()); err != nil {
		t.Fatalf("recoverLeases: %v", err)
	}
	recovered := reloadJob(t, store, job.ID)
	if recovered.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", recovered.Status)
	}
	if recovered.Attempt != 1 {
		t.Fatalf("attempt = %d; a cancelled job must not be retried", recovered.Attempt)
	}
}

func TestRecoverLeasesIgnoresLiveLease(t *testing.T) {
	store := requireDatabase(t)
	runner := newTestRunner(t, store, "")
	job := runningJob(t, store, "live-token", nil)
	if err := runner.recoverLeases(context.Background()); err != nil {
		t.Fatalf("recoverLeases: %v", err)
	}
	if current := reloadJob(t, store, job.ID); current.Status != "running" || current.Attempt != 1 {
		t.Fatalf("a live lease was recovered: %q attempt %d", current.Status, current.Attempt)
	}
}

func TestDeliverOutboxPublishesAndBacksOff(t *testing.T) {
	store := requireDatabase(t)
	runner := newTestRunner(t, store, "")
	fake := runner.producer.inner.(*fakeProducer)

	job := seededJob(t, store, nil)
	outbox := &core.QueueOutbox{JobID: job.ID, Attempt: 1}
	if err := store.DB.Create(outbox).Error; err != nil {
		t.Fatalf("create outbox: %v", err)
	}
	if err := runner.deliverOutbox(context.Background()); err != nil {
		t.Fatalf("deliverOutbox: %v", err)
	}
	published := fake.published()
	if len(published) != 1 {
		t.Fatalf("published %d messages", len(published))
	}
	var message jobMessage
	if err := json.Unmarshal([]byte(published[0]), &message); err != nil {
		t.Fatalf("decode published message: %v", err)
	}
	if message.JobID != job.ID || message.Attempt != 1 {
		t.Fatalf("published payload = %+v", message)
	}
	var delivered core.QueueOutbox
	if err := store.DB.Where("id = ?", outbox.ID).Take(&delivered).Error; err != nil {
		t.Fatalf("reload outbox: %v", err)
	}
	if delivered.Tries != 1 || delivered.DeliveredAt == nil {
		t.Fatalf("outbox after delivery = %+v", delivered)
	}
	if !delivered.NextRetryAt.After(core.Now()) {
		t.Fatal("delivery must schedule a later replay")
	}

	// Broker failure: keep the row deliverable with a backoff.
	fake.err = errors.New("redis down")
	if err := store.DB.Model(&core.QueueOutbox{}).Where("id = ?", outbox.ID).
		Update("next_retry_at", core.Now().Add(-time.Second)).Error; err != nil {
		t.Fatalf("reset outbox: %v", err)
	}
	if err := runner.deliverOutbox(context.Background()); err != nil {
		t.Fatalf("deliverOutbox with broker failure: %v", err)
	}
	var retried core.QueueOutbox
	if err := store.DB.Where("id = ?", outbox.ID).Take(&retried).Error; err != nil {
		t.Fatalf("reload outbox: %v", err)
	}
	if retried.Tries != 2 {
		t.Fatalf("tries = %d", retried.Tries)
	}
	if !retried.NextRetryAt.After(core.Now()) {
		t.Fatalf("next_retry_at = %v", retried.NextRetryAt)
	}
	// A failed send keeps the row due for another replay with backoff instead
	// of waiting the full post-success minute.
	if window := time.Until(retried.NextRetryAt); window < 2*time.Second || window > backoff(retried.Tries)+time.Second {
		t.Fatalf("retry window = %s, want the exponential backoff", window)
	}
}

func TestDeliverOutboxSkipsSupersededAndTerminalJobs(t *testing.T) {
	store := requireDatabase(t)
	runner := newTestRunner(t, store, "")
	fake := runner.producer.inner.(*fakeProducer)

	stale := seededJob(t, store, func(job *core.Job) { job.Attempt = 2 })
	done := seededJob(t, store, func(job *core.Job) { job.Status = "succeeded" })
	cancelled := seededJob(t, store, func(job *core.Job) { job.CancelRequested = true })
	for _, job := range []*core.Job{stale, done, cancelled} {
		if err := store.DB.Create(&core.QueueOutbox{JobID: job.ID, Attempt: 1}).Error; err != nil {
			t.Fatalf("create outbox: %v", err)
		}
	}
	if err := runner.deliverOutbox(context.Background()); err != nil {
		t.Fatalf("deliverOutbox: %v", err)
	}
	if published := fake.published(); len(published) != 0 {
		t.Fatalf("published stale messages: %v", published)
	}

	// Cleanup removes the undeliverable rows.
	if err := runner.cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	var count int64
	if err := store.DB.Model(&core.QueueOutbox{}).Count(&count).Error; err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if count != 0 {
		t.Fatalf("outbox rows = %d after cleanup", count)
	}
}

func TestBackoffIsBounded(t *testing.T) {
	if got := backoff(1); got != 2*time.Second {
		t.Fatalf("backoff(1) = %s", got)
	}
	if got := backoff(0); got != time.Second {
		t.Fatalf("backoff(0) = %s", got)
	}
	// The exponent is capped at 8 (256s) and the result never exceeds 300s.
	if got := backoff(50); got != 256*time.Second {
		t.Fatalf("backoff(50) = %s", got)
	}
	if got := backoff(9); got != 256*time.Second {
		t.Fatalf("backoff(9) = %s", got)
	}
}

func TestHeartbeatIntervalTracksLease(t *testing.T) {
	cases := map[time.Duration]time.Duration{
		300 * time.Second: 20 * time.Second,
		30 * time.Second:  10 * time.Second,
		3 * time.Second:   time.Second,
		0:                 time.Second,
	}
	for lease, want := range cases {
		if got := heartbeatInterval(lease); got != want {
			t.Fatalf("heartbeatInterval(%s) = %s, want %s", lease, got, want)
		}
	}
}

func TestSweepStorageKeepsReferencedFiles(t *testing.T) {
	store := requireDatabase(t)
	job := seededJob(t, store, nil)
	runner := newTestRunner(t, store, "python3")
	if _, err := runJob(t, runner, job.ID, 1); err != nil {
		t.Fatalf("processJob: %v", err)
	}

	jobDir := store.JobDir(job.UserID, job.ID)
	// A published artifact must survive; an unreferenced stray file must not.
	var score core.Artifact
	if err := store.DB.Where("job_id = ? AND kind = ?", job.ID, "score").Take(&score).Error; err != nil {
		t.Fatalf("load score: %v", err)
	}
	referenced, err := store.PrivatePath(*score.StorageKey)
	if err != nil {
		t.Fatalf("resolve score: %v", err)
	}
	stray := filepath.Join(filepath.Dir(referenced), "stray.bin")
	if err := os.WriteFile(stray, []byte("stray"), 0o600); err != nil {
		t.Fatalf("write stray: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(stray, old, old)
	_ = os.Chtimes(referenced, old, old)
	staleUpload := filepath.Join(store.Config.DataDir, "uploads", "tmp-upload")
	if err := os.WriteFile(staleUpload, []byte("x"), 0o600); err != nil {
		t.Fatalf("write stale upload: %v", err)
	}
	_ = os.Chtimes(staleUpload, old, old)

	if err := runner.sweepStorage(context.Background()); err != nil {
		t.Fatalf("sweepStorage: %v", err)
	}
	if _, err := os.Stat(referenced); err != nil {
		t.Fatalf("referenced artifact was swept: %v", err)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("unreferenced result file survived the sweep")
	}
	if _, err := os.Stat(staleUpload); !os.IsNotExist(err) {
		t.Fatal("stale upload survived the sweep")
	}
	_ = jobDir
}

func TestDispatchRecordsServiceHeartbeat(t *testing.T) {
	store := requireDatabase(t)
	runner := newTestRunner(t, store, "")
	if err := runner.Dispatch(context.Background()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	var heartbeat core.ServiceHeartbeat
	if err := store.DB.Where("role = ?", "dispatcher").Take(&heartbeat).Error; err != nil {
		t.Fatalf("dispatcher heartbeat: %v", err)
	}
	if core.Now().Sub(heartbeat.UpdatedAt.UTC()) > time.Minute {
		t.Fatalf("heartbeat is stale: %v", heartbeat.UpdatedAt)
	}
}

// TestConcurrentWorkersPublishOnce verifies the fencing contract: two workers
// racing the same attempt produce exactly one publication.
func TestConcurrentWorkersPublishOnce(t *testing.T) {
	store := requireDatabase(t)
	job := seededJob(t, store, nil)

	first := newTestRunner(t, store, "python3")
	second := newTestRunner(t, store, "python3")

	var wait sync.WaitGroup
	errorsFound := make([]error, 2)
	wait.Add(2)
	for index, runner := range []*Runner{first, second} {
		go func(index int, runner *Runner) {
			defer wait.Done()
			errorsFound[index] = runner.processJob(context.Background(), job.ID, 1)
		}(index, runner)
	}
	wait.Wait()
	for _, err := range errorsFound {
		if err != nil {
			t.Fatalf("processJob: %v", err)
		}
	}

	var tracks int64
	if err := store.DB.Model(&core.Track{}).Where("job_id = ?", job.ID).Count(&tracks).Error; err != nil {
		t.Fatalf("count tracks: %v", err)
	}
	if tracks != 1 {
		t.Fatalf("tracks = %d; concurrent attempts published more than once", tracks)
	}
	current := reloadJob(t, store, job.ID)
	if current.Status != "succeeded" {
		t.Fatalf("status = %q", current.Status)
	}
}

// TestPurgeWhileLockedRetriesLater covers the dispatcher's "try again" path.
func TestPurgeWhileLockedRetriesLater(t *testing.T) {
	store := requireDatabase(t)
	job := seededJob(t, store, nil)
	runner := newTestRunner(t, store, "python3")
	if _, err := runJob(t, runner, job.ID, 1); err != nil {
		t.Fatalf("processJob: %v", err)
	}
	if err := store.DB.Model(&core.Job{}).Where("id = ?", job.ID).
		Updates(map[string]any{"delete_requested": true}).Error; err != nil {
		t.Fatalf("request delete: %v", err)
	}

	lock, err := runner.acquireLock(context.Background(), job.ID, func(string, *float64) error { return nil })
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	if err := runner.cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if reloadJob(t, store, job.ID).DeletedAt != nil {
		t.Fatal("purged while a process still held the lock")
	}
	_ = lock.Close()

	if err := runner.cleanup(context.Background()); err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
	if reloadJob(t, store, job.ID).DeletedAt == nil {
		t.Fatal("purge never completed after the lock was released")
	}
}

// guard against accidental signature drift in helpers used by the tests.
var _ = gorm.ErrRecordNotFound

func TestPipelineBudgetStaysInsideConsumeDeadline(t *testing.T) {
	store := requireDatabase(t)
	runner := newTestRunner(t, store, "")
	runner.consumeTimeout = 10 * time.Minute

	if got := runner.pipelineBudget(nil); got != 9*time.Minute {
		t.Fatalf("budget = %s, want the consume deadline minus one minute", got)
	}
	// A snapshot asking for more than the callback deadline is clamped, so the
	// pipeline reports STAGE_TIMEOUT instead of being killed by the broker.
	runner.consumeTimeout = 30 * time.Minute
	if got := runner.pipelineBudget(map[string]any{"job_timeout_seconds": float64(7200)}); got != 29*time.Minute {
		t.Fatalf("budget = %s", got)
	}
	// A shorter snapshot budget is honoured as is.
	if got := runner.pipelineBudget(map[string]any{"job_timeout_seconds": float64(60)}); got != time.Minute {
		t.Fatalf("budget = %s", got)
	}
}
