package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"gorm.io/gorm"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

// acceptedSuffixes mirrors jobs.extension(): only known containers are allowed.
var acceptedSuffixes = map[string]map[string]bool{
	"audio":    {".wav": true, ".mp3": true, ".flac": true, ".m4a": true, ".ogg": true},
	"musicxml": {".xml": true, ".musicxml": true, ".mxl": true},
}

func extensionFor(kind, title string) (string, error) {
	suffix := strings.ToLower(filepath.Ext(title))
	allowed, ok := acceptedSuffixes[kind]
	if !ok || !allowed[suffix] {
		return "", core.NewError("INVALID_INPUT", "不支持的上传类型", http.StatusBadRequest)
	}
	return suffix, nil
}

// mimeFor returns the MusicXML media type explicitly so the artifact row does
// not depend on the host mime.types database.
func mimeFor(suffix string) string {
	switch suffix {
	case ".xml", ".musicxml", ".mxl":
		return "application/vnd.recordare.musicxml+xml"
	case ".wav":
		return "audio/wav"
	case ".mp3":
		return "audio/mpeg"
	case ".flac":
		return "audio/flac"
	case ".m4a":
		return "audio/mp4"
	case ".ogg":
		return "audio/ogg"
	default:
		return "application/octet-stream"
	}
}

// uploadResult carries the durable bytes of the received file.
type uploadResult struct {
	temp   string
	size   int64
	sha256 string
}

func (s *Server) uploadJob(w http.ResponseWriter, r *http.Request) {
	user := s.requireUser(w, r)
	if user == nil {
		return
	}
	if err := s.store.EnsureSpace(); err != nil {
		writeError(w, err)
		return
	}
	snapshot, err := core.Settings(s.store.DB.WithContext(r.Context()))
	if err != nil {
		writeError(w, err)
		return
	}
	limit := core.Number(snapshot, "max_upload_bytes")
	if limit <= 0 {
		limit = core.Number(core.DefaultSettings, "max_upload_bytes")
	}
	// Reject oversized Content-Length before multipart spooling starts.
	if r.ContentLength > int64(limit)+uploadOverhead {
		writeError(w, core.ErrUploadTooLarge)
		return
	}
	// Chunked uploads carry no Content-Length; cap the body itself so neither a
	// huge file part nor an ignored part can stream unbounded bytes.
	r.Body = http.MaxBytesReader(w, r.Body, int64(limit)+uploadOverhead)

	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, core.NewError("INVALID_REQUEST", "请求参数格式错误", http.StatusUnprocessableEntity))
		return
	}
	form, err := readUploadForm(reader, int64(limit), filepath.Join(s.cfg.DataDir, "uploads"))
	if err != nil {
		writeError(w, err)
		return
	}
	defer os.Remove(form.upload.temp)

	kind, title := form.kind, safeTitle(form.filename)
	suffix, err := extensionFor(kind, title)
	if err != nil {
		writeError(w, err)
		return
	}
	selected := form.engine
	if selected == "" {
		selected = defaultEngine(snapshot)
	}
	if selected != "baseline" {
		writeError(w, core.ErrEngineNotReady)
		return
	}
	if kind == "audio" && !core.BaselineReady(s.cfg) {
		writeError(w, core.ErrModelNotReady)
		return
	}
	snapshot["engine"] = selected
	snapshot["stage_timeout_seconds"] = s.cfg.StageTimeoutSeconds

	jobID := core.NewID()
	directory := s.store.JobDir(user.User.ID, jobID)
	inputDir := filepath.Join(directory, "input")
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(directory)
		}
	}()

	err = s.store.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		if err := s.checkQuota(tx, user.User.ID, snapshot); err != nil {
			return err
		}
		job := core.Job{
			ID: jobID, UserID: user.User.ID, Title: title, Kind: kind,
			Engine: selected, ConfigSnapshot: snapshot, Warnings: []string{},
		}
		if err := tx.Create(&job).Error; err != nil {
			return err
		}
		// Install the bytes only after the quota check passed, so a rejected
		// upload never leaves a directory behind.
		if err := os.MkdirAll(inputDir, 0o700); err != nil {
			return err
		}
		target := filepath.Join(inputDir, "original"+suffix)
		if err := os.Rename(form.upload.temp, target); err != nil {
			return err
		}
		if err := syncDir(inputDir); err != nil {
			return err
		}
		key, err := s.store.StorageKey(target)
		if err != nil {
			return err
		}
		hash := form.upload.sha256
		bytes := form.upload.size
		artifact := core.Artifact{
			JobID: job.ID, Kind: "original", Label: title, StorageKey: &key,
			Mime: mimeFor(suffix), Bytes: &bytes, SHA256: &hash, Status: "ready",
		}
		if err := tx.Create(&artifact).Error; err != nil {
			return err
		}
		if err := tx.Create(&core.QueueOutbox{JobID: job.ID, Attempt: 1}).Error; err != nil {
			return err
		}
		return core.Audit(tx, &user.User.ID, "job.create", &job.ID, map[string]any{"kind": kind})
	})
	if err != nil {
		writeError(w, err)
		return
	}
	committed = true

	var job core.Job
	if err := s.store.DB.WithContext(r.Context()).Where("id = ?", jobID).First(&job).Error; err != nil {
		writeError(w, err)
		return
	}
	s.writeJob(w, r, &job, http.StatusAccepted)
}

func defaultEngine(settings map[string]any) string {
	if value, ok := settings["default_engine"].(string); ok && value != "" {
		return value
	}
	return "baseline"
}

// uploadForm is the parsed multipart request. The file has already been
// streamed to a private temp file with its size and digest recorded.
type uploadForm struct {
	kind     string
	engine   string
	filename string
	upload   uploadResult
}

// readUploadForm consumes the multipart parts. The file part is streamed to a
// temp file in uploadDir with a hard byte cap, so a chunked upload cannot
// exceed the admin-configured limit.
func readUploadForm(reader *multipart.Reader, limit int64, uploadDir string) (uploadForm, error) {
	var form uploadForm
	seenFile := false
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var maxBytes *http.MaxBytesError
			if errors.As(err, &maxBytes) {
				return form, core.ErrUploadTooLarge
			}
			return form, core.NewError("UPLOAD_TOO_LARGE", "请求体超过大小限制", http.StatusRequestEntityTooLarge)
		}
		switch part.FormName() {
		case "file":
			if seenFile {
				_ = part.Close()
				return form, core.NewError("INVALID_REQUEST", "请求参数格式错误", http.StatusUnprocessableEntity)
			}
			seenFile = true
			form.filename = part.FileName()
			result, err := streamUpload(part, limit, uploadDir)
			_ = part.Close()
			if err != nil {
				return form, err
			}
			form.upload = result
		case "kind":
			value, err := readField(part, 64)
			if err != nil {
				return form, err
			}
			form.kind = value
		case "engine":
			value, err := readField(part, 64)
			if err != nil {
				return form, err
			}
			form.engine = value
		default:
			_ = part.Close()
		}
	}
	if !seenFile {
		return form, core.NewError("INVALID_REQUEST", "请求参数格式错误", http.StatusUnprocessableEntity)
	}
	if form.kind == "" {
		return form, core.NewError("INVALID_REQUEST", "请求参数格式错误", http.StatusUnprocessableEntity)
	}
	if form.upload.size == 0 {
		os.Remove(form.upload.temp)
		return form, core.NewError("INVALID_INPUT", "上传文件为空", http.StatusBadRequest)
	}
	return form, nil
}

func readField(part *multipart.Part, maximum int64) (string, error) {
	var builder strings.Builder
	if _, err := io.Copy(&builder, io.LimitReader(part, maximum+1)); err != nil {
		return "", err
	}
	if int64(builder.Len()) > maximum {
		return "", core.NewError("INVALID_REQUEST", "请求参数格式错误", http.StatusUnprocessableEntity)
	}
	return builder.String(), nil
}

// streamUpload copies the uploaded bytes into a fresh private temp file,
// fsyncing before the rename that makes them durable.
func streamUpload(part *multipart.Part, limit int64, uploadDir string) (uploadResult, error) {
	if err := os.MkdirAll(uploadDir, 0o700); err != nil {
		return uploadResult{}, err
	}
	temp := filepath.Join(uploadDir, core.NewID()+".upload")
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return uploadResult{}, err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(file, io.LimitReader(part, limit+1))
	if copyErr != nil {
		_ = file.Close()
		_ = os.Remove(temp)
		return uploadResult{}, copyErr
	}
	if written > limit {
		_ = file.Close()
		_ = os.Remove(temp)
		return uploadResult{}, core.NewError("UPLOAD_TOO_LARGE", "文件超过大小限制", http.StatusRequestEntityTooLarge)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(temp)
		return uploadResult{}, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temp)
		return uploadResult{}, err
	}
	return uploadResult{temp: temp, size: written, sha256: hex.EncodeToString(hasher.Sum(nil))}, nil
}

// syncDir fsyncs a directory so the rename survives a crash.
func syncDir(path string) error {
	handle, err := os.Open(path)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}
