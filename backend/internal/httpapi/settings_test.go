package httpapi

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

func TestAdminSettingsDefaultsAndPatch(t *testing.T) {
	ts := newTestServer(t)
	makeAdmin(t, ts.store, "root")
	admin := loginClient(t, ts, "root")

	before := admin.get(t, Prefix+"/admin/settings")
	if before.Code != http.StatusOK {
		t.Fatalf("get settings: %d %s", before.Code, before.Body)
	}
	defaults := decode(t, before.Body.Bytes())
	want := map[string]any{
		"max_audio_duration_seconds":   float64(600),
		"max_unfinished_jobs_per_user": float64(3),
		"retention_days":               nil,
		"max_upload_bytes":             float64(209715200),
		"default_engine":               "baseline",
	}
	if !reflect.DeepEqual(defaults, want) {
		t.Fatalf("defaults = %v", defaults)
	}

	patched := admin.do(t, http.MethodPatch, Prefix+"/admin/settings",
		[]byte(`{"max_unfinished_jobs_per_user":1,"retention_days":7}`))
	if patched.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", patched.Code, patched.Body)
	}
	updated := decode(t, patched.Body.Bytes())
	if updated["max_unfinished_jobs_per_user"] != float64(1) || updated["retention_days"] != float64(7) {
		t.Fatalf("patched = %v", updated)
	}
	// Integer settings must round-trip as integers, not 600.0 strings.
	if updated["max_audio_duration_seconds"] != float64(600) {
		t.Fatalf("unrelated setting changed: %v", updated)
	}

	// Retention is applied to newly completed jobs, so a fresh job snapshots it.
	createUser(t, ts.store, "liam")
	liam := loginClient(t, ts, "liam")
	job := uploadOK(t, liam)
	snapshot := job["config_snapshot"].(map[string]any)
	if snapshot["retention_days"] != float64(7) {
		t.Fatalf("snapshot retention = %v", snapshot)
	}
	if snapshot["max_unfinished_jobs_per_user"] != float64(1) {
		t.Fatalf("snapshot quota = %v", snapshot)
	}
	if snapshot["engine"] != "baseline" {
		t.Fatalf("snapshot engine = %v", snapshot)
	}
	if _, ok := snapshot["stage_timeout_seconds"]; !ok {
		t.Fatalf("snapshot missing stage timeout: %v", snapshot)
	}
	// With a quota of one, the second upload is refused.
	if second := uploadJob(t, liam, "musicxml", "song.musicxml", []byte(xmlFixture)); second.Code != http.StatusConflict {
		t.Fatalf("second upload: %d %s", second.Code, second.Body)
	}

	// Settings changes are audited with the changed keys.
	var entries []core.AuditLog
	if err := ts.store.DB.Where("action = ?", "admin.settings.update").Find(&entries).Error; err != nil {
		t.Fatalf("load audit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("settings audits = %d", len(entries))
	}
	changes, _ := entries[0].Details["changes"].(map[string]any)
	if changes["retention_days"] != float64(7) {
		t.Fatalf("audit changes = %v", entries[0].Details)
	}
}

func TestSettingsPatchValidation(t *testing.T) {
	ts := newTestServer(t)
	makeAdmin(t, ts.store, "root")
	admin := loginClient(t, ts, "root")

	invalid := []string{
		`{"max_unfinished_jobs_per_user":0}`,
		`{"max_unfinished_jobs_per_user":101}`,
		`{"max_audio_duration_seconds":0}`,
		`{"retention_days":0}`,
		`{"retention_days":-1}`,
		`{"max_upload_bytes":512}`,
		`{"default_engine":"multi_instrument"}`,
		`{"max_upload_bytes":"big"}`,
		`{"max_upload_bytes":1.5}`,
		`{"unknown_key":1}`,
	}
	for _, body := range invalid {
		response := admin.do(t, http.MethodPatch, Prefix+"/admin/settings", []byte(body))
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status %d body %s", body, response.Code, response.Body)
		}
		if code := errorCode(t, response.Body.Bytes()); code != "INVALID_REQUEST" {
			t.Fatalf("%s: code %s", body, code)
		}
	}
	// Clearing retention (null) is allowed.
	if response := admin.do(t, http.MethodPatch, Prefix+"/admin/settings",
		[]byte(`{"retention_days":null}`)); response.Code != http.StatusOK {
		t.Fatalf("null retention: %d %s", response.Code, response.Body)
	}
}

func TestAdminUsersListShape(t *testing.T) {
	ts := newTestServer(t)
	makeAdmin(t, ts.store, "root")
	createUser(t, ts.store, "pending-user", func(user *core.User) { user.Status = "pending" })
	admin := loginClient(t, ts, "root")

	response := admin.get(t, Prefix+"/admin/users")
	if response.Code != http.StatusOK {
		t.Fatalf("users: %d %s", response.Code, response.Body)
	}
	var users []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &users); err != nil {
		t.Fatalf("decode users: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("users = %v", users)
	}
	for _, user := range users {
		if len(user) != 4 {
			t.Fatalf("user must expose exactly id/username/role/status: %v", user)
		}
		for _, key := range []string{"id", "username", "role", "status"} {
			if _, ok := user[key]; !ok {
				t.Fatalf("user missing %q: %v", key, user)
			}
		}
	}
}

func TestCapabilitiesReflectSettings(t *testing.T) {
	ts := newTestServer(t)
	makeAdmin(t, ts.store, "root")
	admin := loginClient(t, ts, "root")
	if response := admin.do(t, http.MethodPatch, Prefix+"/admin/settings",
		[]byte(`{"max_upload_bytes":4096,"max_audio_duration_seconds":30,"max_unfinished_jobs_per_user":2}`)); response.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", response.Code, response.Body)
	}
	body := decode(t, ts.client().get(t, Prefix+"/capabilities").Body.Bytes())
	if body["max_upload_bytes"] != float64(4096) ||
		body["max_audio_duration_seconds"] != float64(30) ||
		body["max_unfinished_jobs_per_user"] != float64(2) {
		t.Fatalf("capabilities = %v", body)
	}
}
