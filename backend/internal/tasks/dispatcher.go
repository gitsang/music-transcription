package tasks

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

// maxRecoveryAttempts bounds automatic retries after lost worker heartbeats,
// matching the legacy dispatcher.
const maxRecoveryAttempts = 3

// beat upserts this process's role heartbeat.
func (r *Runner) beat(ctx context.Context, role string) error {
	return r.store.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return core.Beat(tx, role)
	})
}

// recoverLeases revokes leases that stopped renewing. Clearing the token under
// the row lock is what stops a late process from renewing or publishing; the
// attempt is then re-queued through the durable outbox, or failed when the
// retry budget is exhausted.
func (r *Runner) recoverLeases(ctx context.Context) error {
	now := core.Now()
	return r.store.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var jobs []core.Job
		err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ? AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?", "running", now).
			Find(&jobs).Error
		if err != nil {
			return err
		}
		for index := range jobs {
			job := &jobs[index]
			switch {
			case job.CancelRequested || job.DeleteRequested:
				core.Complete(job, "cancelled")
				if job.Warnings == nil {
					job.Warnings = []string{}
				}
			case job.Attempt < maxRecoveryAttempts:
				job.Attempt++
				job.Status = "queued"
				job.Stage = nil
				job.StageProgress = nil
				job.StartedAt = nil
				job.Warnings = append(append([]string{}, job.Warnings...), "worker心跳中断，已恢复到队列")
				outbox := core.QueueOutbox{JobID: job.ID, Attempt: job.Attempt}
				if err := tx.Create(&outbox).Error; err != nil {
					// A concurrent recovery may already have created this row.
					if !isDuplicateKey(err) {
						return err
					}
				}
			default:
				code, message := "WORKER_LOST", "处理进程多次中断，请联系管理员"
				job.ErrorCode = &code
				job.ErrorMessage = &message
				core.Complete(job, "failed")
			}
			if job.Warnings == nil {
				job.Warnings = []string{}
			}
			// The status columns are written through the model because jsonb
			// columns (warnings) need GORM's `serializer:json` encoder: a map
			// update would send plain text and Postgres would reject it.
			tracked := core.Job{
				Status: job.Status, Attempt: job.Attempt, Stage: job.Stage,
				StageProgress: job.StageProgress, StartedAt: job.StartedAt,
				Warnings: job.Warnings, ErrorCode: job.ErrorCode,
				ErrorMessage: job.ErrorMessage, CompletedAt: job.CompletedAt,
				ExpiresAt: job.ExpiresAt,
			}
			if err := tx.Model(&core.Job{}).Where("id = ?", job.ID).Updates(&tracked).Error; err != nil {
				return err
			}
			// Clearing the token is what revokes the old attempt's ownership.
			if err := tx.Model(&core.Job{}).Where("id = ?", job.ID).Updates(map[string]any{
				"lease_token": nil, "lease_expires_at": nil,
			}).Error; err != nil {
				return err
			}
			if err := core.Audit(tx, nil, "job.lease_recovered", jobPointer(job.ID), map[string]any{
				"attempt": job.Attempt, "status": job.Status,
			}); err != nil {
				return err
			}
			r.logger.Warn("recovered expired lease", slog.String("job", job.ID),
				slog.Int("attempt", job.Attempt), slog.String("status", job.Status))
		}
		return nil
	})
}

// deliverOutbox republishes queued attempts. Sending is retried even after an
// earlier success: losing Redis, or a worker dying before claiming, must not
// strand a durable database task. The producer never trims the stream.
func (r *Runner) deliverOutbox(ctx context.Context) error {
	now := core.Now()
	var rows []core.QueueOutbox
	err := r.store.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED", Table: clause.Table{Name: "queue_outbox"}}).
			Model(&core.QueueOutbox{}).
			Joins("JOIN jobs ON jobs.id = queue_outbox.job_id").
			Where("queue_outbox.attempt = jobs.attempt AND jobs.status = ? AND jobs.cancel_requested = ? AND jobs.delete_requested = ? AND jobs.deleted_at IS NULL AND queue_outbox.next_retry_at <= ?",
				"queued", false, false, now).
			Order("queue_outbox.created_at").
			Limit(100)
		if err := query.Find(&rows).Error; err != nil {
			return err
		}
		for index := range rows {
			row := &rows[index]
			row.Tries++
			updates := map[string]any{"tries": row.Tries}
			if err := r.producer.publish(ctx, jobMessage{JobID: row.JobID, Attempt: row.Attempt}); err != nil {
				r.logger.Warn("outbox delivery failed", slog.String("job", row.JobID), slog.Any("err", err))
				retry := now.Add(backoff(row.Tries))
				updates["next_retry_at"] = retry
			} else {
				updates["delivered_at"] = now
				updates["next_retry_at"] = now.Add(time.Minute)
			}
			if err := tx.Model(&core.QueueOutbox{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

// backoff is the bounded exponential retry used for broker failures.
func backoff(tries int) time.Duration {
	if tries > 8 {
		tries = 8
	}
	seconds := math.Pow(2, float64(tries))
	if seconds > 300 {
		seconds = 300
	}
	return time.Duration(seconds) * time.Second
}

// cleanup performs the retention sweep. Files are only ever removed for
// terminal jobs and only after the execution lock is free, so a winding-down
// attempt can never see its published files disappear underneath it.
func (r *Runner) cleanup(ctx context.Context) error {
	if err := r.store.InitializeStorage(); err != nil {
		return err
	}
	now := core.Now()
	// Collect candidates, then purge each in its own transaction: a busy lock
	// must not roll back the entire sweep.
	type candidate struct {
		ID     string
		UserID string
	}
	var candidates []candidate
	if err := r.store.DB.WithContext(ctx).Model(&core.Job{}).
		Select("id", "user_id", "deleted_at IS NOT NULL AS deleted").
		Where("status NOT IN ?", []string{"queued", "running"}).
		Where("(delete_requested = ? OR expires_at <= ?) AND deleted_at IS NULL", true, now).
		Limit(200).
		Scan(&candidates).Error; err != nil {
		return err
	}
	for _, item := range candidates {
		if err := r.purgeJob(ctx, item.ID); err != nil {
			r.logger.Warn("purge failed", slog.String("job", item.ID), slog.Any("err", err))
		}
	}

	// Sweep stale tombstones' leftover directories and expired sessions.
	if err := r.store.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM sessions WHERE expires_at < ?", now.Add(-24*time.Hour)).Error; err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM rate_limits WHERE reset_at < ?", now).Error; err != nil {
			return err
		}
		// Only a live queued attempt may keep an outbox row. Superseded
		// attempts, terminal jobs and cancelled/deleted jobs would otherwise
		// keep a row that deliverOutbox refuses to send forever.
		return tx.Exec(`DELETE FROM queue_outbox WHERE id IN (
			SELECT queue_outbox.id FROM queue_outbox JOIN jobs ON jobs.id = queue_outbox.job_id
			WHERE queue_outbox.attempt <> jobs.attempt OR jobs.status NOT IN ('queued','running')
			OR jobs.cancel_requested OR jobs.delete_requested OR jobs.deleted_at IS NOT NULL)`).Error
	}); err != nil {
		return err
	}
	return r.sweepStorage(ctx)
}

// purgeJob fences and removes one terminal job, leaving a tombstone row.
func (r *Runner) purgeJob(ctx context.Context, jobID string) error {
	return r.store.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		job, err := lockedJob(tx, jobID)
		if err != nil {
			if errors.Is(err, errJobStopped) {
				return nil
			}
			return err
		}
		if job.DeletedAt != nil {
			return nil
		}
		if job.Status == "queued" || job.Status == "running" {
			return nil
		}
		// A terminal row should already be fenced; clear any leftover token so a
		// late worker cannot write back after this sweep.
		if job.LeaseToken != nil || job.LeaseExpiresAt != nil {
			job.LeaseToken, job.LeaseExpiresAt = nil, nil
			if err := tx.Model(&core.Job{}).Where("id = ?", jobID).Updates(map[string]any{
				"lease_token": nil, "lease_expires_at": nil,
			}).Error; err != nil {
				return err
			}
		}
		purged, err := r.store.Purge(tx, job)
		if err != nil {
			return err
		}
		if !purged {
			// An old process still holds the execution lock: retry next tick.
			return nil
		}
		// jsonb columns (config_snapshot, warnings) are written through the
		// model so GORM applies the JSON serializer.
		if err := tx.Model(&core.Job{}).Where("id = ?", jobID).Updates(&core.Job{
			DeletedAt:       job.DeletedAt,
			DeleteRequested: true,
			ConfigSnapshot:  map[string]any{},
			Warnings:        []string{},
			Title:           job.Title,
			ErrorMessage:    nil,
		}).Error; err != nil {
			return err
		}
		if err := tx.Model(&core.Job{}).Where("id = ?", jobID).Updates(map[string]any{
			"lease_token": nil, "lease_expires_at": nil,
		}).Error; err != nil {
			return err
		}
		return core.Audit(tx, nil, "job.purged", jobPointer(jobID), nil)
	})
}

// sweepStorage removes only files no database row references: stale upload
// temporaries, abandoned unpublished results and orphaned job directories.
// Committed artifacts are never touched by this sweep.
func (r *Runner) sweepStorage(ctx context.Context) error {
	root := r.store.Config.DataDir
	cutoff := time.Now().Add(-time.Hour)
	uploads, err := os.ReadDir(filepath.Join(root, "uploads"))
	if err == nil {
		for _, entry := range uploads {
			info, statErr := entry.Info()
			if statErr != nil || !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
				continue
			}
			if removeErr := os.Remove(filepath.Join(root, "uploads", entry.Name())); removeErr != nil {
				r.logger.Warn("stale upload removal failed", slog.String("file", entry.Name()))
			}
		}
	}

	active := map[string]string{}
	rows, err := r.store.DB.WithContext(ctx).Model(&core.Job{}).
		Select("id", "status").Where("deleted_at IS NULL").Rows()
	if err == nil {
		for rows.Next() {
			var id, status string
			if scanErr := rows.Scan(&id, &status); scanErr == nil {
				active[id] = status
			}
		}
		_ = rows.Close()
	}

	referenced := map[string]bool{}
	var keys []string
	if err := r.store.DB.WithContext(ctx).Model(&core.Artifact{}).
		Where("storage_key IS NOT NULL").Pluck("storage_key", &keys).Error; err == nil {
		for _, key := range keys {
			referenced[filepath.ToSlash(key)] = true
		}
	}

	users, err := os.ReadDir(filepath.Join(root, "users"))
	if err != nil {
		return nil
	}
	for _, user := range users {
		if !user.IsDir() {
			continue
		}
		jobsRoot := filepath.Join(root, "users", user.Name(), "jobs")
		entries, readErr := os.ReadDir(jobsRoot)
		if readErr != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			jobDir := filepath.Join(jobsRoot, entry.Name())
			status, known := active[entry.Name()]
			if !known {
				// Unreferenced directory from a crash: remove only when old.
				if info, statErr := entry.Info(); statErr == nil && info.ModTime().Before(cutoff) {
					if removeErr := os.RemoveAll(jobDir); removeErr != nil {
						r.logger.Warn("orphan job directory removal failed", slog.String("dir", jobDir))
					}
				}
				continue
			}
			if status == "queued" || status == "running" {
				continue
			}
			// Terminal job: drop abandoned work trees and unpublished results,
			// never a file referenced by an artifact row.
			work := filepath.Join(jobDir, "work")
			if info, statErr := os.Stat(work); statErr == nil && info.ModTime().Before(cutoff) {
				_ = os.RemoveAll(work)
			}
			results := filepath.Join(jobDir, "results")
			_ = filepath.Walk(results, func(path string, info os.FileInfo, walkErr error) error {
				if walkErr != nil || info == nil || info.IsDir() || info.ModTime().After(cutoff) {
					return nil
				}
				key, keyErr := r.store.StorageKey(path)
				if keyErr != nil || referenced[filepath.ToSlash(key)] {
					return nil
				}
				if removeErr := os.Remove(path); removeErr != nil {
					r.logger.Warn("unreferenced result removal failed", slog.String("file", path))
				}
				return nil
			})
		}
	}
	return nil
}

// isDuplicateKey detects a concurrent insert of the same outbox row.
func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate key") ||
		strings.Contains(message, "unique constraint") ||
		strings.Contains(message, "sqlstate 23505")
}
