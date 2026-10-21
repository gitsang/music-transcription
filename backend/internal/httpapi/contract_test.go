package httpapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

// These tests need no database: they pin the pure helpers that define the
// public JSON contract and the Range/upload rules.

func TestRangeBounds(t *testing.T) {
	const size = 100
	cases := []struct {
		header  string
		start   int64
		end     int64
		partial bool
		ok      bool
	}{
		{"", 0, 99, false, true},
		{"bytes=0-8", 0, 8, true, true},
		{"bytes=0-", 0, 99, true, true},
		{"bytes=90-", 90, 99, true, true},
		{"bytes=-10", 90, 99, true, true},
		{"bytes=-200", 0, 99, true, true},
		{"bytes=99-99", 99, 99, true, true},
		{"bytes=0-999", 0, 99, true, true},
		{"bytes=50-20", 0, 0, false, false},
		{"bytes=100-", 0, 0, false, false},
		{"bytes=-0", 0, 0, false, false},
		{"bytes=", 0, 0, false, false},
		{"bytes=abc", 0, 0, false, false},
		{"bytes=1-2-3", 0, 0, false, false},
		{"items=0-1", 0, 0, false, false},
		{"bytes=0-1,3-4", 0, 0, false, false},
	}
	for _, item := range cases {
		start, end, partial, err := rangeBounds(item.header, size)
		if item.ok {
			if err != nil {
				t.Fatalf("%q: unexpected error %v", item.header, err)
			}
			if start != item.start || end != item.end || partial != item.partial {
				t.Fatalf("%q: got %d-%d partial=%v", item.header, start, end, partial)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%q: expected an error, got %d-%d", item.header, start, end)
		}
	}
	// An empty file can never satisfy a range.
	if _, _, _, err := rangeBounds("bytes=0-0", 0); err == nil {
		t.Fatal("range on an empty file must fail")
	}
}

func TestSafeTitleAndExtension(t *testing.T) {
	titles := map[string]string{
		"":                     "upload",
		"song.musicxml":        "song.musicxml",
		"/etc/passwd":          "passwd",
		`C:\Users\me\song.wav`: "song.wav",
		"../../song.xml":       "song.xml",
		"with\x00null\x1f.xml": "withnull.xml",
		"  spaces  .musicxml":  "  spaces  .musicxml",
	}
	for input, want := range titles {
		if got := safeTitle(input); got != want {
			t.Fatalf("safeTitle(%q) = %q, want %q", input, got, want)
		}
	}
	long := ""
	for len([]rune(long)) < 400 {
		long += "音"
	}
	if runes := []rune(safeTitle(long)); len(runes) != 255 {
		t.Fatalf("long title truncated to %d runes", len(runes))
	}

	valid := []struct{ kind, name, suffix string }{
		{"audio", "a.wav", ".wav"},
		{"audio", "a.MP3", ".mp3"},
		{"audio", "a.flac", ".flac"},
		{"audio", "a.m4a", ".m4a"},
		{"audio", "a.ogg", ".ogg"},
		{"musicxml", "a.xml", ".xml"},
		{"musicxml", "a.musicxml", ".musicxml"},
		{"musicxml", "a.mxl", ".mxl"},
	}
	for _, item := range valid {
		suffix, err := extensionFor(item.kind, item.name)
		if err != nil || suffix != item.suffix {
			t.Fatalf("extensionFor(%q,%q) = %q %v", item.kind, item.name, suffix, err)
		}
	}
	invalid := []struct{ kind, name string }{
		{"audio", "a.xml"},
		{"musicxml", "a.wav"},
		{"musicxml", "a.txt"},
		{"video", "a.mp4"},
		{"", "a.wav"},
		{"musicxml", "noextension"},
	}
	for _, item := range invalid {
		if _, err := extensionFor(item.kind, item.name); err == nil {
			t.Fatalf("extensionFor(%q,%q) accepted", item.kind, item.name)
		} else if domain, ok := err.(*core.Error); !ok || domain.Code != "INVALID_INPUT" {
			t.Fatalf("extensionFor(%q,%q) error = %v", item.kind, item.name, err)
		}
	}
}

func TestJobJSONContract(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	progress := 0.5
	username := "alice"
	job := core.Job{
		ID: "job-1", UserID: "user-1", Title: "song", Kind: "musicxml",
		Status: "running", Stage: strptr("scoring"), StageProgress: &progress,
		Engine: "baseline", ConfigSnapshot: map[string]any{"engine": "baseline"},
		Warnings: []string{"w"}, Attempt: 2, CreatedAt: now,
	}
	track := core.Track{ID: "t1", JobID: "job-1", Label: "Piano", LabelSource: "imported"}
	artifact := core.Artifact{ID: "a1", JobID: "job-1", Kind: "score", Label: "总谱",
		Mime: "application/xml", Status: "ready"}

	payload := jobJSON(job, &username, []core.Track{track}, []core.Artifact{artifact})
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	expected := []string{
		"id", "user_id", "username", "title", "kind", "status", "stage", "stage_progress",
		"engine", "actual_device", "config_snapshot", "warnings", "error_code", "error_message",
		"attempt", "created_at", "started_at", "completed_at", "expires_at", "tracks", "artifacts",
	}
	if len(decoded) != len(expected) {
		t.Fatalf("job keys = %d, want %d: %v", len(decoded), len(expected), decoded)
	}
	for _, key := range expected {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("job missing %q", key)
		}
	}
	// Nullable timestamps must be JSON null, never a zero-time string.
	if decoded["started_at"] != nil || decoded["completed_at"] != nil {
		t.Fatalf("nullable timestamps = %v", decoded)
	}
	// Timestamps are UTC-naive to match the legacy Python encoder.
	if decoded["created_at"] != "2026-01-02T03:04:05" {
		t.Fatalf("created_at = %v", decoded["created_at"])
	}
	tracks := decoded["tracks"].([]any)
	trackJSON := tracks[0].(map[string]any)
	for _, key := range []string{"id", "label", "program", "confidence", "label_source", "is_drum", "source_stem_ids"} {
		if _, ok := trackJSON[key]; !ok {
			t.Fatalf("track missing %q: %v", key, trackJSON)
		}
	}
	if stems, ok := trackJSON["source_stem_ids"].([]any); !ok || len(stems) != 0 {
		t.Fatalf("source_stem_ids = %v (must be [] not null)", trackJSON["source_stem_ids"])
	}
	artifacts := decoded["artifacts"].([]any)
	artifactJSON := artifacts[0].(map[string]any)
	if len(artifactJSON) != 8 {
		t.Fatalf("artifact keys = %v", artifactJSON)
	}
	for _, leaked := range []string{"storage_key", "sha256", "job_id", "attempt", "created_at"} {
		if _, ok := artifactJSON[leaked]; ok {
			t.Fatalf("artifact leaked %q", leaked)
		}
	}
}

func TestUserJSONNeverExposesHash(t *testing.T) {
	encoded, err := json.Marshal(userJSON(core.User{
		ID: "u", Username: "alice", PasswordHash: "$argon2id$secret", Role: "admin", Status: "active",
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded) != 4 {
		t.Fatalf("user keys = %v", decoded)
	}
	if _, ok := decoded["password_hash"]; ok {
		t.Fatal("password hash leaked")
	}
}

func TestSettingsJSONAlwaysComplete(t *testing.T) {
	payload := settingsJSON(map[string]any{"max_upload_bytes": 4096, "retention_days": nil})
	for key := range core.DefaultSettings {
		if _, ok := payload[key]; !ok {
			t.Fatalf("settings missing %q: %v", key, payload)
		}
	}
	if payload["max_upload_bytes"] != 4096 || payload["retention_days"] != nil {
		t.Fatalf("settings = %v", payload)
	}
	if payload["default_engine"] != "baseline" {
		t.Fatalf("default_engine = %v", payload["default_engine"])
	}
}

func TestSessionTokenAndDigestAreOpaque(t *testing.T) {
	token, err := core.SessionToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if len(token) < 60 {
		t.Fatalf("token too short: %d", len(token))
	}
	digest := core.Digest(token)
	if len(digest) != 64 {
		t.Fatalf("digest = %q", digest)
	}
	if digest == core.Digest(token+"x") {
		t.Fatal("digest collision")
	}
	other, _ := core.SessionToken()
	if other == token {
		t.Fatal("session tokens must differ")
	}
}
