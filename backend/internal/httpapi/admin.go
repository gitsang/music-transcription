package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"gorm.io/gorm"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request) {
	user := s.requireAdmin(w, r)
	if user == nil {
		return
	}
	var users []core.User
	err := s.store.DB.WithContext(r.Context()).Order("created_at DESC").Limit(200).Find(&users).Error
	if err != nil {
		writeError(w, err)
		return
	}
	result := make([]map[string]any, 0, len(users))
	for _, item := range users {
		result = append(result, userJSON(item))
	}
	if err := s.auditUser(r, user, "admin.users.list", "", nil); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

var moderationStatuses = map[string]string{
	"approve": "active",
	"reject":  "rejected",
	"disable": "disabled",
}

func (s *Server) adminModerate(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := s.requireAdmin(w, r)
		if admin == nil {
			return
		}
		target := r.PathValue("id")
		var result map[string]any
		err := s.store.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
			var user core.User
			err := tx.Clauses(lockUpdate()).Where("id = ?", target).First(&user).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return core.NewError("NOT_FOUND", "账号不存在", http.StatusNotFound)
			}
			if err != nil {
				return err
			}
			if user.Role == "admin" {
				return core.NewError("ADMIN_PROTECTED", "管理员账号只能通过运维流程管理", http.StatusConflict)
			}
			now := core.Now()
			user.Status = moderationStatuses[action]
			user.ReviewedAt = &now
			user.ReviewedBy = &admin.User.ID
			if err := tx.Model(&core.User{}).Where("id = ?", user.ID).Updates(map[string]any{
				"status":      user.Status,
				"reviewed_at": now,
				"reviewed_by": admin.User.ID,
			}).Error; err != nil {
				return err
			}
			if user.Status != "active" {
				if err := tx.Model(&core.Session{}).
					Where("user_id = ? AND revoked_at IS NULL", user.ID).
					Update("revoked_at", now).Error; err != nil {
					return err
				}
			}
			if err := core.Audit(tx, &admin.User.ID, "admin.user."+action, &user.ID, nil); err != nil {
				return err
			}
			result = userJSON(user)
			return nil
		})
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	values, err := core.Settings(s.store.DB.WithContext(r.Context()))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsJSON(values))
}

// settingsLimits mirror SettingsPatch validation. Unknown keys are rejected by
// decodeJSON's DisallowUnknownFields.
type settingsLimits struct {
	key string
	min int
	max int
}

var numericSettings = []settingsLimits{
	{"max_audio_duration_seconds", 1, 86400},
	{"max_unfinished_jobs_per_user", 1, 100},
	{"retention_days", 1, 36500},
	{"max_upload_bytes", 1024, 2147483647},
}

func (s *Server) patchSettings(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	patch, err := decodeSettings(r)
	if err != nil {
		writeError(w, err)
		return
	}
	values, err := s.applySettings(r, admin, patch)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsJSON(values))
}

func decodeSettings(r *http.Request) (map[string]any, error) {
	var raw map[string]any
	if err := decodeJSON(r, &raw); err != nil {
		return nil, err
	}
	invalid := core.NewError("INVALID_REQUEST", "请求参数格式错误", http.StatusUnprocessableEntity)
	for key, value := range raw {
		switch key {
		case "default_engine":
			engine, ok := value.(string)
			if !ok || engine != "baseline" {
				return nil, invalid
			}
		case "retention_days":
			if value == nil {
				continue
			}
			number, ok := value.(float64)
			if !ok || number < 1 || number > 36500 || number != float64(int(number)) {
				return nil, invalid
			}
		default:
			limit, ok := numericLimit(key)
			if !ok {
				return nil, invalid
			}
			number, ok := value.(float64)
			if !ok || number < float64(limit.min) || number > float64(limit.max) ||
				number != float64(int(number)) {
				return nil, invalid
			}
		}
	}
	return raw, nil
}

func numericLimit(key string) (settingsLimits, bool) {
	for _, item := range numericSettings {
		if item.key == key {
			return item, true
		}
	}
	return settingsLimits{}, false
}

func (s *Server) applySettings(r *http.Request, admin *authUser, patch map[string]any) (map[string]any, error) {
	var values map[string]any
	err := s.store.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		// Serialize settings writes per administrator, matching lock_user().
		var adminRow core.User
		if err := tx.Clauses(lockUpdate()).Where("id = ?", admin.User.ID).First(&adminRow).Error; err != nil {
			return err
		}
		now := core.Now()
		for key, value := range patch {
			// A raw upsert is required: GORM cannot express a JSON `null` through
			// the serializer, and settings must distinguish it from "unset".
			if err := tx.Exec(`INSERT INTO settings (key, value, updated_by, updated_at)
				VALUES (?, ?::json, ?, ?)
				ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value,
					updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at`,
				key, core.JSONValue(normalizeSetting(value)), admin.User.ID, now).Error; err != nil {
				return err
			}
		}
		changes := make(map[string]any, len(patch))
		for key, value := range patch {
			changes[key] = normalizeSetting(value)
		}
		if err := core.Audit(tx, &admin.User.ID, "admin.settings.update", nil, map[string]any{"changes": changes}); err != nil {
			return err
		}
		current, err := core.Settings(tx)
		if err != nil {
			return err
		}
		values = current
		return nil
	})
	return values, err
}

// normalizeSetting stores integral JSON numbers as ints so the JSON column and
// API responses do not drift into 600.0.
func normalizeSetting(value any) any {
	if number, ok := value.(float64); ok && number == float64(int64(number)) {
		return int64(number)
	}
	return value
}

func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	values, err := core.Settings(s.store.DB.WithContext(r.Context()))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"engines":                      core.Capabilities(s.cfg),
		"max_upload_bytes":             settingInt(values, "max_upload_bytes"),
		"max_audio_duration_seconds":   settingInt(values, "max_audio_duration_seconds"),
		"max_unfinished_jobs_per_user": settingInt(values, "max_unfinished_jobs_per_user"),
	})
}

func settingInt(values map[string]any, key string) int {
	number := core.Number(values, key)
	if number == 0 {
		return core.Number(core.DefaultSettings, key)
	}
	return number
}

func (s *Server) healthLive(w http.ResponseWriter, _ *http.Request) {
	writeStatus(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) healthReady(w http.ResponseWriter, r *http.Request) {
	report := s.store.Health(r.Context(), "api")
	status := http.StatusOK
	if value, ok := report["status"].(string); !ok || strings.ToLower(value) != "ready" {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, report)
}
