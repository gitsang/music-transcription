package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

func TestArtifactContentAndRange(t *testing.T) {
	ts := newTestServer(t)
	createUser(t, ts.store, "judy")
	session := loginClient(t, ts, "judy")
	job := uploadOK(t, session)
	artifactID := job["artifacts"].([]any)[0].(map[string]any)["id"].(string)
	path := Prefix + "/artifacts/" + artifactID + "/content"

	full := session.get(t, path)
	if full.Code != http.StatusOK {
		t.Fatalf("full: %d %s", full.Code, full.Body)
	}
	if full.Body.String() != xmlFixture {
		t.Fatalf("body mismatch: %q", full.Body.String())
	}
	headers := full.Result().Header
	if headers.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("accept-ranges = %q", headers.Get("Accept-Ranges"))
	}
	if headers.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("cache-control = %q", headers.Get("Cache-Control"))
	}
	if headers.Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
		t.Fatalf("csp = %q", headers.Get("Content-Security-Policy"))
	}
	if headers.Get("Content-Type") != "application/vnd.recordare.musicxml+xml" {
		t.Fatalf("content-type = %q", headers.Get("Content-Type"))
	}
	if headers.Get("Content-Disposition") == "" {
		t.Fatal("missing content-disposition")
	}

	partial := ts.request(t, http.MethodGet, path, nil, map[string]string{
		"Cookie": session.cookie, "Range": "bytes=0-8",
	})
	if partial.Code != http.StatusPartialContent {
		t.Fatalf("partial: %d", partial.Code)
	}
	if partial.Body.String() != xmlFixture[:9] {
		t.Fatalf("partial body = %q", partial.Body.String())
	}
	want := "bytes 0-8/" + itoa(len(xmlFixture))
	if got := partial.Result().Header.Get("Content-Range"); got != want {
		t.Fatalf("content-range = %q, want %q", got, want)
	}

	suffix := ts.request(t, http.MethodGet, path, nil, map[string]string{
		"Cookie": session.cookie, "Range": "bytes=-5",
	})
	if suffix.Code != http.StatusPartialContent || suffix.Body.String() != xmlFixture[len(xmlFixture)-5:] {
		t.Fatalf("suffix: %d %q", suffix.Code, suffix.Body.String())
	}

	for _, value := range []string{"bytes=999999-", "bytes=5-2", "bytes=-0", "bytes=abc", "items=0-1"} {
		response := ts.request(t, http.MethodGet, path, nil, map[string]string{
			"Cookie": session.cookie, "Range": value,
		})
		if response.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("range %q: status %d", value, response.Code)
		}
		if code := errorCode(t, response.Body.Bytes()); code != "INVALID_RANGE" {
			t.Fatalf("range %q: code %s", value, code)
		}
		if got := response.Result().Header.Get("Content-Range"); got != "bytes */"+itoa(len(xmlFixture)) {
			t.Fatalf("range %q: content-range %q", value, got)
		}
	}

	download := session.get(t, Prefix+"/artifacts/"+artifactID+"/download")
	if download.Code != http.StatusOK {
		t.Fatalf("download: %d", download.Code)
	}
	if disposition := download.Result().Header.Get("Content-Disposition"); len(disposition) < 10 || disposition[:10] != "attachment" {
		t.Fatalf("download disposition = %q", disposition)
	}
}

func TestArtifactRequiresSession(t *testing.T) {
	ts := newTestServer(t)
	createUser(t, ts.store, "karl")
	session := loginClient(t, ts, "karl")
	job := uploadOK(t, session)
	artifactID := job["artifacts"].([]any)[0].(map[string]any)["id"].(string)
	if response := ts.client().get(t, Prefix+"/artifacts/"+artifactID+"/content"); response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous artifact: %d", response.Code)
	}
}

func TestFailedArtifactHasNoDownload(t *testing.T) {
	ts := newTestServer(t)
	user := createUser(t, ts.store, "liam")
	session := loginClient(t, ts, "liam")
	job := uploadOK(t, session)
	jobID := job["id"].(string)

	failed := core.Artifact{
		JobID: jobID, Kind: "pdf", Label: "PDF", Mime: "application/pdf",
		Status: "failed", Error: strptr("本地工具不可用"),
	}
	if err := ts.store.DB.Create(&failed).Error; err != nil {
		t.Fatalf("create failed artifact: %v", err)
	}
	response := session.get(t, Prefix+"/artifacts/"+failed.ID+"/content")
	if response.Code != http.StatusConflict {
		t.Fatalf("failed artifact: %d %s", response.Code, response.Body)
	}
	if code := errorCode(t, response.Body.Bytes()); code != "ARTIFACT_NOT_AVAILABLE" {
		t.Fatalf("failed artifact code = %s", code)
	}
	// Artifacts list must expose status/error and never the storage key.
	list := session.get(t, Prefix+"/jobs/"+jobID+"/artifacts")
	var artifacts []map[string]any
	if err := json.Unmarshal(list.Body.Bytes(), &artifacts); err != nil {
		t.Fatalf("decode artifacts: %v", err)
	}
	if len(artifacts) != 2 {
		t.Fatalf("artifacts = %v", artifacts)
	}
	for _, artifact := range artifacts {
		if _, leaked := artifact["storage_key"]; leaked {
			t.Fatalf("storage key leaked: %v", artifact)
		}
		if _, leaked := artifact["sha256"]; leaked {
			t.Fatalf("sha256 leaked: %v", artifact)
		}
	}
	_ = user
}

func TestMissingArtifactFileReturnsUnavailable(t *testing.T) {
	ts := newTestServer(t)
	user := createUser(t, ts.store, "nina")
	session := loginClient(t, ts, "nina")
	job := uploadOK(t, session)
	jobID := job["id"].(string)
	// Delete the bytes on disk while leaving the row: the API must degrade.
	target := filepath.Join(jobDirFor(ts.store, user.ID, jobID), "input", "original.musicxml")
	if err := os.Remove(target); err != nil {
		t.Fatalf("remove original: %v", err)
	}
	artifactID := job["artifacts"].([]any)[0].(map[string]any)["id"].(string)
	response := session.get(t, Prefix+"/artifacts/"+artifactID+"/content")
	if response.Code != http.StatusNotFound || errorCode(t, response.Body.Bytes()) != "ARTIFACT_NOT_AVAILABLE" {
		t.Fatalf("missing file: %d %s", response.Code, response.Body)
	}
}

func strptr(value string) *string { return &value }

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 12)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
