package httpapi

import (
	"time"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

// Timestamps are UTC-naive in the legacy schema. Rendering them without an
// offset keeps the JSON byte-compatible with FastAPI's datetime encoder and
// avoids the browser re-interpreting a "Z" suffix.
func formatTime(value time.Time) string {
	utc := value.UTC()
	if utc.Nanosecond() == 0 {
		return utc.Format("2006-01-02T15:04:05")
	}
	// Python's isoformat() always prints exactly six fractional digits.
	return utc.Format("2006-01-02T15:04:05.000000")
}

func formatTimePtr(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}

func floatPtr(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func intPtr(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func int64Ptr(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func stringPtr(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// userJSON is the explicit {id, username, role, status} projection; password
// hashes never leave the store.
func userJSON(user core.User) map[string]any {
	return map[string]any{
		"id":       user.ID,
		"username": user.Username,
		"role":     user.Role,
		"status":   user.Status,
	}
}

func trackJSON(track core.Track) map[string]any {
	stems := track.SourceStemIDs
	if stems == nil {
		stems = []string{}
	}
	return map[string]any{
		"id":              track.ID,
		"label":           track.Label,
		"program":         intPtr(track.Program),
		"confidence":      floatPtr(track.Confidence),
		"label_source":    track.LabelSource,
		"is_drum":         track.IsDrum,
		"source_stem_ids": stems,
	}
}

func artifactJSON(artifact core.Artifact) map[string]any {
	return map[string]any{
		"id":       artifact.ID,
		"track_id": stringPtr(artifact.TrackID),
		"kind":     artifact.Kind,
		"label":    artifact.Label,
		"mime":     artifact.Mime,
		"bytes":    int64Ptr(artifact.Bytes),
		"status":   artifact.Status,
		"error":    stringPtr(artifact.Error),
	}
}

func jobJSON(job core.Job, username *string, tracks []core.Track, artifacts []core.Artifact) map[string]any {
	snapshot := job.ConfigSnapshot
	if snapshot == nil {
		snapshot = map[string]any{}
	}
	warnings := job.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	trackList := make([]map[string]any, 0, len(tracks))
	for _, track := range tracks {
		trackList = append(trackList, trackJSON(track))
	}
	artifactList := make([]map[string]any, 0, len(artifacts))
	for _, artifact := range artifacts {
		artifactList = append(artifactList, artifactJSON(artifact))
	}
	return map[string]any{
		"id":              job.ID,
		"user_id":         job.UserID,
		"username":        stringPtr(username),
		"title":           job.Title,
		"kind":            job.Kind,
		"status":          job.Status,
		"stage":           stringPtr(job.Stage),
		"stage_progress":  floatPtr(job.StageProgress),
		"engine":          job.Engine,
		"actual_device":   stringPtr(job.ActualDevice),
		"config_snapshot": snapshot,
		"warnings":        warnings,
		"error_code":      stringPtr(job.ErrorCode),
		"error_message":   stringPtr(job.ErrorMessage),
		"attempt":         job.Attempt,
		"created_at":      formatTime(job.CreatedAt),
		"started_at":      formatTimePtr(job.StartedAt),
		"completed_at":    formatTimePtr(job.CompletedAt),
		"expires_at":      formatTimePtr(job.ExpiresAt),
		"tracks":          trackList,
		"artifacts":       artifactList,
	}
}

func settingsJSON(values map[string]any) map[string]any {
	result := make(map[string]any, len(core.DefaultSettings))
	for key, fallback := range core.DefaultSettings {
		result[key] = fallback
	}
	for key, value := range values {
		if value == nil && key == "retention_days" {
			result[key] = nil
			continue
		}
		if value != nil {
			result[key] = value
		}
	}
	return result
}
