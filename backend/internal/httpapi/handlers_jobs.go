package httpapi

import (
	"errors"
	"net/http"
	"os"

	"gorm.io/gorm"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

var retryableStatuses = map[string]bool{"failed": true, "partial": true, "cancelled": true}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	user := s.requireUser(w, r)
	if user == nil {
		return
	}
	var job core.Job
	err := s.store.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		locked, err := s.authorizedJobTx(tx, r, user, true)
		if err != nil {
			return err
		}
		job = *locked
		switch job.Status {
		case "queued":
			// Nothing is executing yet, so the cancel is final immediately and
			// the queue row becomes a no-op for the worker.
			job.CancelRequested = true
			core.Complete(&job, "cancelled")
		case "running":
			// The worker observes the flag on its next progress callback.
			job.CancelRequested = true
		}
		if err := tx.Model(&core.Job{}).Where("id = ?", job.ID).Updates(map[string]any{
			"status":           job.Status,
			"cancel_requested": job.CancelRequested,
			"completed_at":     job.CompletedAt,
			"expires_at":       job.ExpiresAt,
			"lease_token":      job.LeaseToken,
			"lease_expires_at": job.LeaseExpiresAt,
		}).Error; err != nil {
			return err
		}
		return core.Audit(tx, &user.User.ID, "job.cancel", &job.ID, nil)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	s.writeJob(w, r, &job, http.StatusOK)
}

func (s *Server) retryJob(w http.ResponseWriter, r *http.Request) {
	user := s.requireUser(w, r)
	if user == nil {
		return
	}
	if err := s.store.EnsureSpace(); err != nil {
		writeError(w, err)
		return
	}
	var job core.Job
	err := s.store.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		initial, err := s.authorizedJobTx(tx, r, user, false)
		if err != nil {
			return err
		}
		settings, err := core.Settings(tx)
		if err != nil {
			return err
		}
		if err := s.checkQuota(tx, initial.UserID, settings); err != nil {
			return err
		}
		locked, err := s.authorizedJobTx(tx, r, user, true)
		if err != nil {
			return err
		}
		job = *locked
		if !retryableStatuses[job.Status] || job.LeaseToken != nil {
			return core.NewError("JOB_NOT_RETRYABLE", "任务当前不可重试", http.StatusConflict)
		}
		if job.Kind == "audio" && !core.BaselineReady(s.cfg) {
			return core.ErrModelNotReady
		}
		var original core.Artifact
		err = tx.Where("job_id = ? AND kind = ?", job.ID, "original").First(&original).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return core.NewError("INPUT_NOT_AVAILABLE", "原始文件已不存在", http.StatusConflict)
		}
		if err != nil {
			return err
		}
		path, err := s.store.PrivatePath(deref(original.StorageKey))
		if err != nil {
			return core.NewError("INPUT_NOT_AVAILABLE", "原始文件已不存在", http.StatusConflict)
		}
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			return core.NewError("INPUT_NOT_AVAILABLE", "原始文件已不存在", http.StatusConflict)
		}
		if err := clearDerived(tx, job.ID); err != nil {
			return err
		}
		job.Attempt++
		job.Status, job.Stage, job.StageProgress = "queued", nil, nil
		job.CancelRequested = false
		job.Warnings = []string{}
		job.ErrorCode, job.ErrorMessage = nil, nil
		job.StartedAt, job.CompletedAt, job.ExpiresAt = nil, nil, nil
		job.ActualDevice = nil
		job.LeaseToken, job.LeaseExpiresAt = nil, nil
		if err := tx.Model(&core.Job{}).Where("id = ?", job.ID).Updates(map[string]any{
			"attempt":          job.Attempt,
			"status":           job.Status,
			"stage":            nil,
			"stage_progress":   nil,
			"cancel_requested": false,
			"warnings":         core.JSONValue(job.Warnings),
			"error_code":       nil,
			"error_message":    nil,
			"started_at":       nil,
			"completed_at":     nil,
			"expires_at":       nil,
			"actual_device":    nil,
			"lease_token":      nil,
			"lease_expires_at": nil,
		}).Error; err != nil {
			return err
		}
		if err := tx.Create(&core.QueueOutbox{JobID: job.ID, Attempt: job.Attempt}).Error; err != nil {
			return err
		}
		return core.Audit(tx, &user.User.ID, "job.retry", &job.ID, map[string]any{"attempt": job.Attempt})
	})
	if err != nil {
		writeError(w, err)
		return
	}
	s.writeJob(w, r, &job, http.StatusAccepted)
}

func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	user := s.requireUser(w, r)
	if user == nil {
		return
	}
	var job core.Job
	purged := false
	err := s.store.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		locked, err := s.authorizedJobTx(tx, r, user, true)
		if err != nil {
			return err
		}
		job = *locked
		job.DeleteRequested = true
		job.CancelRequested = true
		if job.Status == "queued" {
			core.Complete(&job, "cancelled")
		}
		updates := map[string]any{
			"delete_requested": true,
			"cancel_requested": true,
			"status":           job.Status,
			"completed_at":     job.CompletedAt,
			"expires_at":       job.ExpiresAt,
		}
		if err := tx.Model(&core.Job{}).Where("id = ?", job.ID).Updates(updates).Error; err != nil {
			return err
		}
		if job.Status != "running" {
			// Purge takes the job flock; false means a worker is still
			// unwinding and the dispatcher retries on its next tick.
			ok, err := s.store.Purge(tx, &job)
			if err != nil {
				return err
			}
			purged = ok
			if ok {
				if err := tx.Model(&core.Job{}).Where("id = ?", job.ID).Updates(map[string]any{
					"deleted_at":       job.DeletedAt,
					"delete_requested": true,
					"config_snapshot":  core.JSONValue(map[string]any{}),
					"warnings":         core.JSONValue([]string{}),
					"title":            job.Title,
					"error_message":    nil,
					"lease_token":      nil,
					"lease_expires_at": nil,
				}).Error; err != nil {
					return err
				}
			}
		}
		return core.Audit(tx, &user.User.ID, "job.delete", &job.ID, nil)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	status := "deleting"
	if purged || (job.DeletedAt != nil) {
		status = "deleted"
	}
	writeStatus(w, http.StatusAccepted, map[string]any{"status": status})
}

// authorizedJobTx is authorizedJob bound to the caller's transaction.
func (s *Server) authorizedJobTx(tx *gorm.DB, r *http.Request, user *authUser, lock bool) (*core.Job, error) {
	query := tx.Where("id = ? AND deleted_at IS NULL AND delete_requested = ?", r.PathValue("id"), false)
	if !user.isAdmin() {
		query = query.Where("user_id = ?", user.User.ID)
	}
	if lock {
		query = query.Clauses(lockUpdate())
	}
	var job core.Job
	err := query.First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, core.NewError("NOT_FOUND", "任务不存在", http.StatusNotFound)
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
