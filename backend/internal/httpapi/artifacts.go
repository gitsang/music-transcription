package httpapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

const copyBuffer = 1024 * 1024

func (s *Server) artifactContent(w http.ResponseWriter, r *http.Request) {
	s.serveArtifact(w, r, false)
}

func (s *Server) artifactDownload(w http.ResponseWriter, r *http.Request) {
	s.serveArtifact(w, r, true)
}

// serveArtifact authorizes every request, including Range probes, then streams
// the private file. Only ready artifacts with a storage key are downloadable.
func (s *Server) serveArtifact(w http.ResponseWriter, r *http.Request, download bool) {
	user := s.requireUser(w, r)
	if user == nil {
		return
	}
	var artifact core.Artifact
	err := s.store.DB.WithContext(r.Context()).Where("id = ?", r.PathValue("id")).First(&artifact).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeError(w, core.NewError("NOT_FOUND", "文件不存在", http.StatusNotFound))
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	job, err := s.authorizedJob(r.Context(), artifact.JobID, user, false)
	if err != nil {
		writeError(w, err)
		return
	}
	if artifact.Status != "ready" || artifact.StorageKey == nil {
		writeError(w, core.NewError("ARTIFACT_NOT_AVAILABLE", "该产物未能生成", http.StatusConflict))
		return
	}
	path, err := s.store.PrivatePath(*artifact.StorageKey)
	if err != nil {
		writeError(w, core.ErrArtifactMissing)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		writeError(w, core.ErrArtifactMissing)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, core.ErrArtifactMissing)
		return
	}
	size := info.Size()

	if user.isAdmin() {
		action := "admin.artifact.read"
		if download {
			action = "admin.artifact.download"
		}
		if err := s.auditUser(r, user, action, artifact.ID, map[string]any{"job_id": job.ID}); err != nil {
			writeError(w, err)
			return
		}
	}

	start, end, partial, rangeErr := rangeBounds(r.Header.Get("Range"), size)
	if rangeErr != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		writeError(w, rangeErr)
		return
	}

	disposition := "inline"
	if download {
		disposition = "attachment"
	}
	header := w.Header()
	header.Set("Accept-Ranges", "bytes")
	header.Set("Content-Type", artifact.Mime)
	header.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	header.Set("Cache-Control", "private, no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	header.Set("Content-Disposition", fmt.Sprintf("%s; filename=\"artifact\"; filename*=UTF-8''%s",
		disposition, url.PathEscape(artifactFilename(artifact, path))))
	if partial {
		header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	}

	status := http.StatusOK
	if partial {
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return
	}
	_, _ = io.CopyN(w, file, end-start+1)
}

// artifactFilename mirrors the legacy naming: the label, plus the stored
// extension when the label does not already carry it.
func artifactFilename(artifact core.Artifact, path string) string {
	name := safeTitle(artifact.Label)
	suffix := strings.ToLower(filepathExt(path))
	if artifact.Kind != "original" && suffix != "" && !strings.HasSuffix(strings.ToLower(name), suffix) {
		name += suffix
	}
	return name
}

func filepathExt(path string) string {
	base := path
	if index := strings.LastIndexAny(base, `/\`); index >= 0 {
		base = base[index+1:]
	}
	if index := strings.LastIndex(base, "."); index > 0 {
		return base[index:]
	}
	return ""
}

// rangeBounds parses a single-range request, matching jobs.range_bounds().
// An empty header means "send everything".
func rangeBounds(value string, size int64) (int64, int64, bool, error) {
	if value == "" {
		return 0, size - 1, false, nil
	}
	if !strings.HasPrefix(value, "bytes=") {
		return 0, 0, false, core.ErrInvalidRange
	}
	spec := strings.TrimSpace(strings.TrimPrefix(value, "bytes="))
	if strings.Contains(spec, ",") {
		return 0, 0, false, core.ErrInvalidRange
	}
	dash := strings.Index(spec, "-")
	if dash < 0 {
		return 0, 0, false, core.ErrInvalidRange
	}
	first, last := strings.TrimSpace(spec[:dash]), strings.TrimSpace(spec[dash+1:])
	if first == "" && last == "" {
		return 0, 0, false, core.ErrInvalidRange
	}
	if size == 0 {
		return 0, 0, false, core.ErrInvalidRange
	}
	var start, end int64
	if first != "" {
		parsed, err := strconv.ParseInt(first, 10, 64)
		if err != nil || parsed < 0 {
			return 0, 0, false, core.ErrInvalidRange
		}
		start = parsed
		end = size - 1
		if last != "" {
			parsedEnd, err := strconv.ParseInt(last, 10, 64)
			if err != nil || parsedEnd < 0 {
				return 0, 0, false, core.ErrInvalidRange
			}
			if parsedEnd < end {
				end = parsedEnd
			}
		}
	} else {
		suffix, err := strconv.ParseInt(last, 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false, core.ErrInvalidRange
		}
		start = size - suffix
		if start < 0 {
			start = 0
		}
		end = size - 1
	}
	if start >= size || start > end {
		return 0, 0, false, core.ErrInvalidRange
	}
	return start, end, true, nil
}
