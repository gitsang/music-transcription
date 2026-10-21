package httpapi

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

// xmlFixture is a minimal but structurally valid MusicXML document.
const xmlFixture = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE score-partwise PUBLIC "-//Recordare//DTD MusicXML 3.1 Partwise//EN" "http://www.musicxml.org/dtds/partwise.dtd">
<score-partwise version="3.1"><part-list><score-part id="P1"><part-name>Piano</part-name></score-part></part-list><part id="P1"><measure number="1"/></part></score-partwise>`

// multipartBody builds the exact body shape the browser sends for POST /jobs.
func multipartBody(t *testing.T, kind, filename string, content []byte, engine string) ([]byte, string) {
	t.Helper()
	buffer := &bytes.Buffer{}
	writer := multipart.NewWriter(buffer)
	if err := writer.WriteField("kind", kind); err != nil {
		t.Fatalf("write kind: %v", err)
	}
	if engine != "" {
		if err := writer.WriteField("engine", engine); err != nil {
			t.Fatalf("write engine: %v", err)
		}
	}
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buffer.Bytes(), writer.FormDataContentType()
}

func uploadJob(t *testing.T, c *client, kind, filename string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := multipartBody(t, kind, filename, content, "")
	return c.server.request(t, http.MethodPost, Prefix+"/jobs", body, map[string]string{
		"Content-Type": contentType,
		"Cookie":       c.cookie,
		"Origin":       c.server.origin,
	})
}

func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode json: %v (%s)", err, string(body))
	}
	return value
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	value := decode(t, body)
	envelope, ok := value["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error envelope: %s", string(body))
	}
	code, _ := envelope["code"].(string)
	return code
}

// makeAdmin creates an administrator row and returns its ID.
func makeAdmin(t *testing.T, store *core.Store, username string) core.User {
	t.Helper()
	return createUser(t, store, username, func(user *core.User) {
		user.Role = "admin"
		user.Status = "active"
	})
}

// holdLock takes an exclusive flock on path and returns the release function,
// mirroring the worker's execution lock so purge can be observed deferring.
func holdLock(t *testing.T, path string) func() {
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
