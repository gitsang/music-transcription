package core

import (
	"encoding/json"
	"math"

	"gorm.io/gorm"
)

// DefaultSettings mirrors the legacy deployment defaults.
var DefaultSettings = map[string]any{
	"max_audio_duration_seconds":   600,
	"max_unfinished_jobs_per_user": 3,
	"retention_days":               nil,
	"max_upload_bytes":             209715200,
	"default_engine":               "baseline",
}

// Settings merges stored settings over defaults. Missing rows and NULL values
// fall back to the defaults, so callers always receive a complete map.
func Settings(tx *gorm.DB) (map[string]any, error) {
	result := make(map[string]any, len(DefaultSettings))
	for key, value := range DefaultSettings {
		result[key] = value
	}
	var rows []Setting
	if err := tx.Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.Value == nil {
			continue
		}
		result[row.Key] = row.Value
	}
	return result, nil
}

// Number reads a numeric setting with a JSON-friendly conversion. Missing or
// non-numeric values return 0.
func Number(values map[string]any, key string) int {
	value, ok := values[key]
	if !ok || value == nil {
		return 0
	}
	switch typed := value.(type) {
	case int:
		return typed
	case int32:
		return int(typed)
	case int64:
		return int(typed)
	case float32:
		if math.IsNaN(float64(typed)) || math.IsInf(float64(typed), 0) {
			return 0
		}
		return int(typed)
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0
		}
		return int(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0
		}
		return int(parsed)
	default:
		return 0
	}
}

// Audit appends an audit row inside the caller's transaction.
func Audit(tx *gorm.DB, actor *string, action string, resource *string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	row := AuditLog{ActorID: actor, Action: action, ResourceID: resource, Details: details}
	return tx.Create(&row).Error
}

// Beat upserts a service heartbeat row for the given role.
func Beat(tx *gorm.DB, role string) error {
	now := Now()
	return tx.Exec(`INSERT INTO service_heartbeats (role, updated_at) VALUES (?, ?)
		ON CONFLICT (role) DO UPDATE SET updated_at = EXCLUDED.updated_at`, role, now).Error
}
