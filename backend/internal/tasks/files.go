package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Artifact kinds accepted by the API and the exporter. Anything else is a
// pipeline bug and must never reach the database.
var artifactKinds = map[string]bool{
	"original": true, "score": true, "part": true, "midi": true, "stem": true,
	"stem_preview": true, "synth": true, "synth_preview": true, "pdf": true,
	"part_pdf": true, "timeline": true, "notes": true, "manifest": true,
}

// Pipeline-only kinds additionally allowed in a pipeline result. The original
// input artifact is registered by the API, never by the pipeline.
var pipelineKinds = map[string]bool{
	"score": true, "part": true, "midi": true, "stem": true, "stem_preview": true,
	"synth": true, "synth_preview": true, "pdf": true, "part_pdf": true,
	"timeline": true, "notes": true, "manifest": true, "original_preview": true,
}

var labelSources = map[string]bool{
	"model": true, "separator_category": true, "unknown": true, "imported": true,
}

// JobStages mirrors the Python pipeline stages. A progress line for any other
// stage is rejected so a buggy exporter cannot drive the public UI.
var JobStages = map[string]bool{
	"validating": true, "preparing": true, "analyzing": true, "separating": true,
	"transcribing": true, "scoring": true, "exporting": true, "finalizing": true,
}

const (
	maxLabelRunes   = 255
	maxErrorRunes   = 500
	maxResultBytes  = 64 << 20
	maxProgressLine = 1 << 20
)

type pipelineTrack struct {
	ID            string   `json:"id"`
	Label         string   `json:"label"`
	Program       *int     `json:"program"`
	Confidence    *float64 `json:"confidence"`
	LabelSource   string   `json:"label_source"`
	IsDrum        bool     `json:"is_drum"`
	SourceStemIDs []string `json:"source_stem_ids"`
}

type pipelineArtifact struct {
	TrackID *string `json:"track_id"`
	Kind    string  `json:"kind"`
	Label   string  `json:"label"`
	Mime    string  `json:"mime"`
	Status  string  `json:"status"`
	Error   string  `json:"error"`
	Path    string  `json:"path"`
}

type pipelineResult struct {
	Tracks       []pipelineTrack    `json:"tracks"`
	Artifacts    []pipelineArtifact `json:"artifacts"`
	Warnings     []string           `json:"warnings"`
	Engine       string             `json:"engine"`
	ActualDevice string             `json:"actual_device"`
}

// preparedTrack is a validated track ready to be inserted under the fence.
type preparedTrack struct {
	ID            string
	Label         string
	Program       *int
	Confidence    *float64
	LabelSource   string
	IsDrum        bool
	SourceStemIDs []string
}

// preparedArtifact is a validated artifact. SourcePath is only set for ready
// items; the bytes are copied and hashed while publishing.
type preparedArtifact struct {
	ID         string
	TrackID    *string
	Kind       string
	Label      string
	Mime       string
	Status     string
	Error      *string
	SourcePath string
	Path       string // path as reported by the pipeline, kept for diagnostics
	Suffix     string
}

// parsePipelineResult decodes and validates the terminal result message.
func parsePipelineResult(raw json.RawMessage) (pipelineResult, error) {
	var result pipelineResult
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if err := decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("invalid pipeline result: %w", err)
	}
	return result, nil
}

// validateResult enforces the publication contract before any file is copied:
// UUID identities, provenance, MIDI programs, confidences and artifact kinds.
func validateResult(result pipelineResult) ([]preparedTrack, []preparedArtifact, error) {
	if result.Tracks == nil && result.Artifacts == nil {
		return nil, nil, errors.New("invalid pipeline result")
	}
	tracks := make([]preparedTrack, 0, len(result.Tracks))
	seen := make(map[string]bool, len(result.Tracks))
	for _, source := range result.Tracks {
		id, err := uuid.Parse(source.ID)
		if err != nil || id.String() != strings.ToLower(source.ID) {
			return nil, nil, fmt.Errorf("invalid track ID %q", source.ID)
		}
		if seen[id.String()] {
			return nil, nil, errors.New("duplicate track ID")
		}
		seen[id.String()] = true
		if source.Program != nil && (*source.Program < 0 || *source.Program > 127) {
			return nil, nil, errors.New("invalid MIDI program")
		}
		if source.Confidence != nil {
			value := *source.Confidence
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
				return nil, nil, errors.New("invalid confidence")
			}
		}
		if !labelSources[source.LabelSource] {
			return nil, nil, fmt.Errorf("invalid instrument provenance %q", source.LabelSource)
		}
		track := preparedTrack{
			ID: id.String(), Label: truncate(source.Label, maxLabelRunes),
			Program: source.Program, Confidence: source.Confidence,
			LabelSource: source.LabelSource, IsDrum: source.IsDrum,
			SourceStemIDs: source.SourceStemIDs,
		}
		if track.SourceStemIDs == nil {
			track.SourceStemIDs = []string{}
		}
		tracks = append(tracks, track)
	}

	// Stem identities are independent of score tracks: an artifact may point at
	// a stem ID that never became a track.
	stems := make(map[string]bool)
	for _, source := range result.Artifacts {
		if source.Kind == "stem" || source.Kind == "stem_preview" {
			if source.TrackID != nil && *source.TrackID != "" {
				stems[*source.TrackID] = true
			}
		}
	}

	artifacts := make([]preparedArtifact, 0, len(result.Artifacts))
	for _, source := range result.Artifacts {
		if !pipelineKinds[source.Kind] {
			return nil, nil, fmt.Errorf("invalid artifact kind %q", source.Kind)
		}
		if source.Status != "ready" && source.Status != "failed" {
			return nil, nil, fmt.Errorf("invalid artifact status %q", source.Status)
		}
		var trackID *string
		if source.TrackID != nil && *source.TrackID != "" {
			parsed, err := uuid.Parse(*source.TrackID)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid artifact track ID %q", *source.TrackID)
			}
			normalized := parsed.String()
			if !seen[normalized] && !stems[normalized] && source.Kind != "stem" && source.Kind != "stem_preview" {
				return nil, nil, errors.New("unknown artifact track")
			}
			trackID = &normalized
		}
		item := preparedArtifact{
			ID: uuid.NewString(), TrackID: trackID, Kind: source.Kind,
			Label: truncate(source.Label, maxLabelRunes), Mime: truncate(source.Mime, 128),
			Status: source.Status, Path: source.Path,
		}
		if source.Status == "ready" {
			if strings.TrimSpace(source.Path) == "" {
				return nil, nil, errors.New("missing artifact path")
			}
			item.Suffix = strings.ToLower(filepath.Ext(source.Path))
		} else {
			message := truncate(strings.TrimSpace(source.Error), maxErrorRunes)
			if message == "" {
				message = "该产物导出失败"
			}
			item.Error = &message
		}
		artifacts = append(artifacts, item)
	}

	hasScore := false
	for _, artifact := range artifacts {
		if artifact.Kind == "score" && artifact.Status == "ready" {
			hasScore = true
			break
		}
	}
	if !hasScore {
		return nil, nil, errors.New("missing core score artifact")
	}
	return tracks, artifacts, nil
}

// resolveArtifactPath accepts only paths inside the attempt workspace. The
// resolved parent and target are compared so a symlink cannot escape the work
// directory even if the pipeline was compromised.
func resolveArtifactPath(workDir, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || strings.HasPrefix(relative, "/") {
		return "", errors.New("pipeline artifact outside attempt directory")
	}
	clean := filepath.Clean(relative)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("pipeline artifact outside attempt directory")
	}
	base, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return "", fmt.Errorf("resolve work directory: %w", err)
	}
	candidate := filepath.Join(base, clean)
	parent, err := filepath.EvalSymlinks(filepath.Dir(candidate))
	if err != nil {
		return "", fmt.Errorf("resolve artifact directory: %w", err)
	}
	if !within(base, parent) {
		return "", errors.New("pipeline artifact outside attempt directory")
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve artifact: %w", err)
	}
	if !within(base, resolved) {
		return "", errors.New("pipeline artifact outside attempt directory")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat artifact: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("pipeline artifact is not a regular file")
	}
	return resolved, nil
}

// within reports whether path is base or below it, after resolution.
func within(base, path string) bool {
	base = filepath.Clean(base)
	path = filepath.Clean(path)
	if path == base {
		return true
	}
	return strings.HasPrefix(path, base+string(filepath.Separator))
}

// copyArtifact streams source into a private temp file, fsyncs it and renames it
// atomically into the attempt results directory. It returns the SHA256 and size
// of the bytes that actually landed on disk.
func copyArtifact(source, targetDir, id, suffix string) (string, int64, error) {
	temporary := filepath.Join(targetDir, id+".tmp")
	final := filepath.Join(targetDir, id+suffix)
	input, err := os.Open(source)
	if err != nil {
		return "", 0, err
	}
	defer input.Close()
	output, err := os.OpenFile(temporary, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", 0, err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(output, hasher), io.LimitReader(input, maxResultBytes+1))
	if copyErr == nil && written > maxResultBytes {
		copyErr = errors.New("artifact exceeds size limit")
	}
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(temporary)
		return "", 0, copyErr
	}
	if err := os.Rename(temporary, final); err != nil {
		_ = os.Remove(temporary)
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), written, nil
}

// syncDir fsyncs a directory so a rename survives a crash.
func syncDir(path string) error {
	handle, err := os.Open(path)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

func truncate(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit])
}
