package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

func uploadOK(t *testing.T, c *client) map[string]any {
	t.Helper()
	response := uploadJob(t, c, "musicxml", "song.musicxml", []byte(xmlFixture))
	if response.Code != http.StatusAccepted {
		t.Fatalf("upload: %d %s", response.Code, response.Body)
	}
	return decode(t, response.Body.Bytes())
}

func TestUploadCreatesJobOutboxAndOriginalArtifact(t *testing.T) {
	ts := newTestServer(t)
	user := createUser(t, ts.store, "dave")
	session := loginClient(t, ts, "dave")

	response := uploadJob(t, session, "musicxml", "song.musicxml", []byte(xmlFixture))
	if response.Code != http.StatusAccepted {
		t.Fatalf("upload: %d %s", response.Code, response.Body)
	}
	job := decode(t, response.Body.Bytes())
	if job["status"] != "queued" || job["attempt"] != float64(1) || job["kind"] != "musicxml" {
		t.Fatalf("job = %v", job)
	}
	artifacts := job["artifacts"].([]any)
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %v", artifacts)
	}
	original := artifacts[0].(map[string]any)
	if original["kind"] != "original" || original["status"] != "ready" {
		t.Fatalf("original = %v", original)
	}
	if original["bytes"] != float64(len(xmlFixture)) {
		t.Fatalf("original bytes = %v, want %d", original["bytes"], len(xmlFixture))
	}
	// The storage key and hash stay private.
	if _, leaked := original["storage_key"]; leaked {
		t.Fatalf("artifact leaked storage_key: %v", original)
	}

	var outbox []core.QueueOutbox
	if err := ts.store.DB.Where("job_id = ?", job["id"]).Find(&outbox).Error; err != nil {
		t.Fatalf("load outbox: %v", err)
	}
	if len(outbox) != 1 || outbox[0].Attempt != 1 || outbox[0].DeliveredAt != nil {
		t.Fatalf("outbox = %+v", outbox)
	}

	jobID, _ := job["id"].(string)
	path := filepath.Join(jobDirFor(ts.store, user.ID, jobID), "input", "original.musicxml")
	content, err := os.ReadFile(path)
	if err != nil || string(content) != xmlFixture {
		t.Fatalf("stored input: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("input permissions = %v (%v)", info.Mode(), err)
	}

	listed := session.get(t, Prefix+"/jobs")
	if listed.Code != http.StatusOK {
		t.Fatalf("list: %d", listed.Code)
	}
	var jobs []map[string]any
	if err := json.Unmarshal(listed.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(jobs) != 1 || jobs[0]["id"] != jobID {
		t.Fatalf("jobs = %v", jobs)
	}
	// The owner username is included for the admin table.
	if jobs[0]["username"] != "dave" {
		t.Fatalf("username = %v", jobs[0]["username"])
	}
}

// TestJobListIncludesOwnerAndChildren checks the list payload is complete: the
// frontend renders the same Job shape from both list and detail endpoints.
func TestJobListIncludesOwnerAndChildren(t *testing.T) {
	ts := newTestServer(t)
	createUser(t, ts.store, "olga")
	session := loginClient(t, ts, "olga")
	for index := 0; index < 3; index++ {
		if response := uploadJob(t, session, "musicxml", "song.musicxml", []byte(xmlFixture)); response.Code != http.StatusAccepted {
			t.Fatalf("upload %d: %d", index, response.Code)
		}
	}
	request := httptest.NewRequest(http.MethodGet, Prefix+"/jobs", nil)
	request.Header.Set("Cookie", session.cookie)
	request.RemoteAddr = "127.0.0.1:12345"
	recorder := httptest.NewRecorder()
	ts.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list: %d %s", recorder.Code, recorder.Body)
	}
	var jobs []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	for _, job := range jobs {
		if len(job["artifacts"].([]any)) != 1 {
			t.Fatalf("artifacts = %v", job["artifacts"])
		}
		if len(job["tracks"].([]any)) != 0 {
			t.Fatalf("tracks = %v", job["tracks"])
		}
		if job["username"] != "olga" {
			t.Fatalf("username = %v", job["username"])
		}
	}
}

func TestUploadRejectsInvalidInput(t *testing.T) {
	ts := newTestServer(t)
	createUser(t, ts.store, "erin")
	session := loginClient(t, ts, "erin")

	cases := []struct {
		kind     string
		filename string
		content  []byte
		code     int
		error    string
	}{
		{"musicxml", "song.txt", []byte(xmlFixture), http.StatusBadRequest, "INVALID_INPUT"},
		{"audio", "song.musicxml", []byte(xmlFixture), http.StatusBadRequest, "INVALID_INPUT"},
		{"video", "song.mp4", []byte(xmlFixture), http.StatusBadRequest, "INVALID_INPUT"},
		{"musicxml", "song.musicxml", []byte{}, http.StatusBadRequest, "INVALID_INPUT"},
		// Audio is unavailable because the model venv is absent in tests.
		{"audio", "song.wav", []byte("RIFF"), http.StatusServiceUnavailable, "MODEL_NOT_READY"},
	}
	for _, item := range cases {
		response := uploadJob(t, session, item.kind, item.filename, item.content)
		if response.Code != item.code {
			t.Fatalf("%s/%s: status %d body %s", item.kind, item.filename, response.Code, response.Body)
		}
		if code := errorCode(t, response.Body.Bytes()); code != item.error {
			t.Fatalf("%s/%s: code %s, want %s", item.kind, item.filename, code, item.error)
		}
	}
	if response := uploadJob(t, session, "musicxml", "song.musicxml", []byte(xmlFixture)); response.Code != http.StatusAccepted {
		t.Fatalf("valid upload after rejections: %d %s", response.Code, response.Body)
	}
	// A rejected upload must not leave a temp file behind.
	uploads, err := os.ReadDir(filepath.Join(ts.store.Config.DataDir, "uploads"))
	if err != nil {
		t.Fatalf("read uploads dir: %v", err)
	}
	if len(uploads) != 0 {
		t.Fatalf("temp upload files remain: %v", uploads)
	}
	var count int64
	if err := ts.store.DB.Model(&core.Job{}).Count(&count).Error; err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("jobs = %d, want 1", count)
	}
}

func TestUploadQuotaIsEnforcedPerUser(t *testing.T) {
	ts := newTestServer(t)
	createUser(t, ts.store, "frank")
	session := loginClient(t, ts, "frank")
	for index := 0; index < 3; index++ {
		if response := uploadJob(t, session, "musicxml", "song.musicxml", []byte(xmlFixture)); response.Code != http.StatusAccepted {
			t.Fatalf("upload %d: %d %s", index, response.Code, response.Body)
		}
	}
	limited := uploadJob(t, session, "musicxml", "song.musicxml", []byte(xmlFixture))
	if limited.Code != http.StatusConflict || errorCode(t, limited.Body.Bytes()) != "JOB_LIMIT_REACHED" {
		t.Fatalf("quota: %d %s", limited.Code, limited.Body)
	}
	// The rejected job must not leave a directory on disk.
	entries, err := os.ReadDir(filepath.Join(ts.store.Config.DataDir, "users"))
	if err != nil {
		t.Fatalf("read users dir: %v", err)
	}
	for _, entry := range entries {
		jobs, err := os.ReadDir(filepath.Join(ts.store.Config.DataDir, "users", entry.Name(), "jobs"))
		if err != nil {
			continue
		}
		if len(jobs) != 3 {
			t.Fatalf("job dirs = %d, want 3", len(jobs))
		}
	}

	// Cancelling a queued job frees a slot immediately.
	listed := session.get(t, Prefix+"/jobs")
	var jobs []map[string]any
	if err := json.Unmarshal(listed.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("decode jobs: %v", err)
	}
	cancelled := session.postJSON(t, Prefix+"/jobs/"+jobs[0]["id"].(string)+"/cancel", "")
	if cancelled.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", cancelled.Code, cancelled.Body)
	}
	if body := decode(t, cancelled.Body.Bytes()); body["status"] != "cancelled" {
		t.Fatalf("cancel body = %v", body)
	}
	if response := uploadJob(t, session, "musicxml", "song.musicxml", []byte(xmlFixture)); response.Code != http.StatusAccepted {
		t.Fatalf("upload after cancel: %d %s", response.Code, response.Body)
	}
}

func TestUploadRespectsAdminSizeSetting(t *testing.T) {
	ts := newTestServer(t)
	makeAdmin(t, ts.store, "root")
	admin := loginClient(t, ts, "root")
	patched := admin.do(t, http.MethodPatch, Prefix+"/admin/settings", []byte(`{"max_upload_bytes":1024}`))
	if patched.Code != http.StatusOK {
		t.Fatalf("patch settings: %d %s", patched.Code, patched.Body)
	}
	createUser(t, ts.store, "mia")
	session := loginClient(t, ts, "mia")

	oversized := uploadJob(t, session, "musicxml", "song.musicxml", []byte(make([]byte, 2048)))
	if oversized.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized: %d %s", oversized.Code, oversized.Body)
	}
	if code := errorCode(t, oversized.Body.Bytes()); code != "UPLOAD_TOO_LARGE" {
		t.Fatalf("oversized code = %s", code)
	}
	// Exactly at the limit is accepted.
	if response := uploadJob(t, session, "musicxml", "song.musicxml", []byte(make([]byte, 1024))); response.Code != http.StatusAccepted {
		t.Fatalf("at limit: %d %s", response.Code, response.Body)
	}
}

// TestUploadRejectsOversizeChunkedBody covers clients that omit Content-Length
// (chunked transfer): the body cap must still apply.
func TestUploadRejectsOversizeChunkedBody(t *testing.T) {
	ts := newTestServer(t)
	makeAdmin(t, ts.store, "root")
	admin := loginClient(t, ts, "root")
	if response := admin.do(t, http.MethodPatch, Prefix+"/admin/settings",
		[]byte(`{"max_upload_bytes":2048}`)); response.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", response.Code, response.Body)
	}
	createUser(t, ts.store, "nora")
	session := loginClient(t, ts, "nora")

	body, contentType := multipartBody(t, "musicxml", "song.musicxml", []byte(make([]byte, 8192)), "")
	request := httptest.NewRequest(http.MethodPost, Prefix+"/jobs", strings.NewReader(string(body)))
	// A negative ContentLength models a chunked request with no length header.
	request.ContentLength = -1
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Cookie", session.cookie)
	request.Header.Set("Origin", ts.origin)
	recorder := httptest.NewRecorder()
	ts.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked oversize: %d %s", recorder.Code, recorder.Body)
	}
	// Nothing durable may remain from the rejected upload.
	var jobs int64
	ts.store.DB.Model(&core.Job{}).Count(&jobs)
	if jobs != 0 {
		t.Fatalf("jobs created = %d", jobs)
	}
	entries, _ := os.ReadDir(filepath.Join(ts.store.Config.DataDir, "uploads"))
	if len(entries) != 0 {
		t.Fatalf("temp uploads remain: %v", entries)
	}
}

func TestCrossUserAccessHidesExistence(t *testing.T) {
	ts := newTestServer(t)
	createUser(t, ts.store, "grace")
	grace := loginClient(t, ts, "grace")
	job := uploadOK(t, grace)
	jobID := job["id"].(string)
	artifactID := job["artifacts"].([]any)[0].(map[string]any)["id"].(string)

	createUser(t, ts.store, "heidi")
	heidi := loginClient(t, ts, "heidi")
	for _, path := range []string{
		"/jobs/" + jobID,
		"/jobs/" + jobID + "/tracks",
		"/jobs/" + jobID + "/artifacts",
		"/artifacts/" + artifactID + "/content",
		"/artifacts/" + artifactID + "/download",
	} {
		response := heidi.get(t, Prefix+path)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d body %s", path, response.Code, response.Body)
		}
	}
	// A Range probe must be authorized too, otherwise the file leaks its size.
	ranged := ts.request(t, http.MethodGet, Prefix+"/artifacts/"+artifactID+"/content", nil, map[string]string{
		"Cookie": heidi.cookie, "Range": "bytes=0-3",
	})
	if ranged.Code != http.StatusNotFound {
		t.Fatalf("ranged foreign access: %d", ranged.Code)
	}
	// Mutations are equally hidden.
	if response := heidi.postJSON(t, Prefix+"/jobs/"+jobID+"/cancel", ""); response.Code != http.StatusNotFound {
		t.Fatalf("foreign cancel: %d", response.Code)
	}
	if response := heidi.do(t, http.MethodDelete, Prefix+"/jobs/"+jobID, nil); response.Code != http.StatusNotFound {
		t.Fatalf("foreign delete: %d", response.Code)
	}
	if response := heidi.postJSON(t, Prefix+"/jobs/"+jobID+"/retry", ""); response.Code != http.StatusNotFound {
		t.Fatalf("foreign retry: %d", response.Code)
	}
}

func TestAdminCanReadAnyJob(t *testing.T) {
	ts := newTestServer(t)
	createUser(t, ts.store, "ivan")
	ivan := loginClient(t, ts, "ivan")
	job := uploadOK(t, ivan)
	jobID := job["id"].(string)

	makeAdmin(t, ts.store, "root")
	root := loginClient(t, ts, "root")
	if response := root.get(t, Prefix+"/jobs/"+jobID); response.Code != http.StatusOK {
		t.Fatalf("admin read: %d %s", response.Code, response.Body)
	}
	var jobs []map[string]any
	response := root.get(t, Prefix+"/admin/jobs")
	if err := json.Unmarshal(response.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("decode admin jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0]["id"] != jobID {
		t.Fatalf("admin jobs = %v", jobs)
	}
	// Reading and listing as admin must be audited.
	var actions []string
	if err := ts.store.DB.Model(&core.AuditLog{}).Where("actor_id = ?", rootID(t, ts)).Pluck("action", &actions).Error; err != nil {
		t.Fatalf("load audit: %v", err)
	}
	if !contains(actions, "admin.job.read") || !contains(actions, "admin.jobs.list") {
		t.Fatalf("audit actions = %v", actions)
	}
}

func rootID(t *testing.T, ts *testServer) string {
	t.Helper()
	var user core.User
	if err := ts.store.DB.Where("username = ?", "root").First(&user).Error; err != nil {
		t.Fatalf("load root: %v", err)
	}
	return user.ID
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
