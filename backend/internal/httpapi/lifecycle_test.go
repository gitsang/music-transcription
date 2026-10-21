package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

func jobStatus(t *testing.T, store *core.Store, jobID string) core.Job {
	t.Helper()
	var job core.Job
	if err := store.DB.Where("id = ?", jobID).First(&job).Error; err != nil {
		t.Fatalf("load job %s: %v", jobID, err)
	}
	return job
}

func TestQueuedCancelThenRetryRotatesAttempt(t *testing.T) {
	ts := newTestServer(t)
	user := createUser(t, ts.store, "karl")
	session := loginClient(t, ts, "karl")
	job := uploadOK(t, session)
	jobID := job["id"].(string)
	directory := jobDirFor(ts.store, user.ID, jobID)
	if _, err := os.Stat(directory); err != nil {
		t.Fatalf("job dir missing: %v", err)
	}

	cancelled := session.postJSON(t, Prefix+"/jobs/"+jobID+"/cancel", "")
	if cancelled.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", cancelled.Code, cancelled.Body)
	}
	body := decode(t, cancelled.Body.Bytes())
	if body["status"] != "cancelled" {
		t.Fatalf("cancel body = %v", body)
	}
	// Cancelled jobs stay visible until explicitly deleted.
	listed := session.get(t, Prefix+"/jobs")
	var jobs []map[string]any
	if err := json.Unmarshal(listed.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("decode jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0]["status"] != "cancelled" {
		t.Fatalf("jobs = %v", jobs)
	}

	retried := session.postJSON(t, Prefix+"/jobs/"+jobID+"/retry", "")
	if retried.Code != http.StatusAccepted {
		t.Fatalf("retry: %d %s", retried.Code, retried.Body)
	}
	if body := decode(t, retried.Body.Bytes()); body["attempt"] != float64(2) {
		t.Fatalf("retry attempt = %v", body["attempt"])
	}
	var attempts []int
	if err := ts.store.DB.Model(&core.QueueOutbox{}).Where("job_id = ?", jobID).
		Order("attempt").Pluck("attempt", &attempts).Error; err != nil {
		t.Fatalf("load outbox: %v", err)
	}
	if len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
		t.Fatalf("outbox attempts = %v", attempts)
	}
	// Retrying must not duplicate or drop the original input artifact.
	var artifacts []core.Artifact
	if err := ts.store.DB.Where("job_id = ?", jobID).Find(&artifacts).Error; err != nil {
		t.Fatalf("load artifacts: %v", err)
	}
	if len(artifacts) != 1 || artifacts[0].Kind != "original" {
		t.Fatalf("artifacts after retry = %+v", artifacts)
	}
	stored := jobStatus(t, ts.store, jobID)
	if stored.Status != "queued" || stored.Attempt != 2 || stored.CancelRequested {
		t.Fatalf("job after retry = %+v", stored)
	}
	if listed := session.get(t, Prefix+"/jobs/"+jobID); listed.Code != http.StatusOK {
		t.Fatalf("job detail: %d", listed.Code)
	}
}

func TestDeletePurgesFilesAndHidesJob(t *testing.T) {
	ts := newTestServer(t)
	user := createUser(t, ts.store, "karl")
	session := loginClient(t, ts, "karl")
	job := uploadOK(t, session)
	jobID := job["id"].(string)
	directory := jobDirFor(ts.store, user.ID, jobID)

	deleted := session.do(t, http.MethodDelete, Prefix+"/jobs/"+jobID, nil)
	if deleted.Code != http.StatusAccepted {
		t.Fatalf("delete: %d %s", deleted.Code, deleted.Body)
	}
	if body := decode(t, deleted.Body.Bytes()); body["status"] != "deleted" {
		t.Fatalf("delete body = %v", body)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("job dir still present: %v", err)
	}
	if jobs := session.get(t, Prefix+"/jobs"); string(jobs.Body.Bytes()) != "[]\n" && string(jobs.Body.Bytes()) != "[]" {
		t.Fatalf("jobs after delete = %s", jobs.Body)
	}
	if detail := session.get(t, Prefix+"/jobs/"+jobID); detail.Code != http.StatusNotFound {
		t.Fatalf("deleted job detail: %d", detail.Code)
	}
	stored := jobStatus(t, ts.store, jobID)
	if stored.DeletedAt == nil || !stored.DeleteRequested {
		t.Fatalf("tombstone missing: %+v", stored)
	}
	if stored.Title != "已删除任务" || len(stored.Warnings) != 0 || len(stored.ConfigSnapshot) != 0 {
		t.Fatalf("tombstone not sanitized: %+v", stored)
	}
	var artifacts, tracks, outbox int64
	ts.store.DB.Model(&core.Artifact{}).Where("job_id = ?", jobID).Count(&artifacts)
	ts.store.DB.Model(&core.Track{}).Where("job_id = ?", jobID).Count(&tracks)
	ts.store.DB.Model(&core.QueueOutbox{}).Where("job_id = ?", jobID).Count(&outbox)
	if artifacts != 0 || tracks != 0 || outbox != 0 {
		t.Fatalf("derived rows remain: artifacts=%d tracks=%d outbox=%d", artifacts, tracks, outbox)
	}
}

func TestDeleteIsDeferredWhileRunning(t *testing.T) {
	ts := newTestServer(t)
	user := createUser(t, ts.store, "karl")
	session := loginClient(t, ts, "karl")
	job := uploadOK(t, session)
	jobID := job["id"].(string)

	// Simulate a worker holding the lease and the flock.
	lockPath := ts.store.Config.DataDir + "/locks/" + jobID + ".lock"
	release := holdLock(t, lockPath)
	now := core.Now()
	expires := now.Add(120 * 1e9)
	if err := ts.store.DB.Model(&core.Job{}).Where("id = ?", jobID).Updates(map[string]any{
		"status": "running", "lease_token": core.NewID(), "lease_expires_at": expires, "started_at": now,
	}).Error; err != nil {
		t.Fatalf("mark running: %v", err)
	}

	response := session.do(t, http.MethodDelete, Prefix+"/jobs/"+jobID, nil)
	if response.Code != http.StatusAccepted {
		t.Fatalf("delete running: %d %s", response.Code, response.Body)
	}
	if body := decode(t, response.Body.Bytes()); body["status"] != "deleting" {
		t.Fatalf("delete running body = %v", body)
	}
	if _, err := os.Stat(jobDirFor(ts.store, user.ID, jobID)); err != nil {
		t.Fatalf("job dir removed while held: %v", err)
	}
	stored := jobStatus(t, ts.store, jobID)
	if stored.DeletedAt != nil || !stored.DeleteRequested || !stored.CancelRequested {
		t.Fatalf("running job state = %+v", stored)
	}

	// Once the lease is fenced and the flock released, a retry of delete
	// completes. The dispatcher performs the same call.
	release()
	if err := ts.store.DB.Model(&core.Job{}).Where("id = ?", jobID).Updates(map[string]any{
		"status": "cancelled", "lease_token": nil, "lease_expires_at": nil,
	}).Error; err != nil {
		t.Fatalf("fence job: %v", err)
	}
	// delete_requested hides the job, so this path goes through the store.
	var jobRow core.Job
	if err := ts.store.DB.Where("id = ?", jobID).First(&jobRow).Error; err != nil {
		t.Fatalf("load job: %v", err)
	}
	purged, err := ts.store.Purge(ts.store.DB, &jobRow)
	if err != nil || !purged {
		t.Fatalf("purge: %v %v", purged, err)
	}
	if _, err := os.Stat(jobDirFor(ts.store, user.ID, jobID)); !os.IsNotExist(err) {
		t.Fatalf("job dir not purged: %v", err)
	}
}

func TestRetryRejectsRunningAndMissingInput(t *testing.T) {
	ts := newTestServer(t)
	user := createUser(t, ts.store, "karl")
	session := loginClient(t, ts, "karl")
	job := uploadOK(t, session)
	jobID := job["id"].(string)

	running := session.postJSON(t, Prefix+"/jobs/"+jobID+"/retry", "")
	if running.Code != http.StatusConflict {
		t.Fatalf("retry queued: %d %s", running.Code, running.Body)
	}
	if code := errorCode(t, running.Body.Bytes()); code != "JOB_NOT_RETRYABLE" {
		t.Fatalf("retry queued code = %s", code)
	}

	// Mark it cancelled, then delete the original bytes.
	if err := ts.store.DB.Model(&core.Job{}).Where("id = ?", jobID).
		Update("status", "cancelled").Error; err != nil {
		t.Fatalf("mark cancelled: %v", err)
	}
	if err := os.Remove(jobDirFor(ts.store, user.ID, jobID) + "/input/original.musicxml"); err != nil {
		t.Fatalf("remove input: %v", err)
	}
	missing := session.postJSON(t, Prefix+"/jobs/"+jobID+"/retry", "")
	if missing.Code != http.StatusConflict || errorCode(t, missing.Body.Bytes()) != "INPUT_NOT_AVAILABLE" {
		t.Fatalf("retry missing input: %d %s", missing.Code, missing.Body)
	}
}

func TestCancelRunningJobLeavesLeaseToCallback(t *testing.T) {
	ts := newTestServer(t)
	createUser(t, ts.store, "karl")
	session := loginClient(t, ts, "karl")
	job := uploadOK(t, session)
	jobID := job["id"].(string)
	expires := core.Now().Add(120 * 1e9)
	if err := ts.store.DB.Model(&core.Job{}).Where("id = ?", jobID).Updates(map[string]any{
		"status": "running", "lease_token": core.NewID(), "lease_expires_at": expires,
	}).Error; err != nil {
		t.Fatalf("mark running: %v", err)
	}
	response := session.postJSON(t, Prefix+"/jobs/"+jobID+"/cancel", "")
	if response.Code != http.StatusOK {
		t.Fatalf("cancel running: %d %s", response.Code, response.Body)
	}
	stored := jobStatus(t, ts.store, jobID)
	if stored.Status != "running" || !stored.CancelRequested || stored.LeaseToken == nil {
		t.Fatalf("running cancel state = %+v", stored)
	}
	// A second cancel is idempotent.
	if second := session.postJSON(t, Prefix+"/jobs/"+jobID+"/cancel", ""); second.Code != http.StatusOK {
		t.Fatalf("second cancel: %d", second.Code)
	}
}
