package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"gorm.io/gorm"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// decodeJSON reads a bounded request body and rejects unknown fields, matching
// the Pydantic models' extra='forbid' behaviour. An oversized body is a 413, not
// a validation error, so the frontend shows the upload-limit message.
func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, defaultBodyLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return core.ErrUploadTooLarge
		}
		return core.NewError("INVALID_REQUEST", "请求参数格式错误", http.StatusUnprocessableEntity)
	}
	return nil
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var body credentials
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, err)
		return
	}
	if len(body.Username) < 1 || len(body.Username) > 128 || len(body.Password) < 1 || len(body.Password) > 128 {
		writeError(w, core.NewError("INVALID_REQUEST", "请求参数格式错误", http.StatusUnprocessableEntity))
		return
	}
	name, err := core.NormalizeUsername(body.Username)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.rateLimit(r, "register", ""); err != nil {
		writeError(w, err)
		return
	}
	if err := core.ValidatePassword(body.Password); err != nil {
		writeError(w, err)
		return
	}
	hash, err := core.HashPassword(body.Password)
	if err != nil {
		writeError(w, err)
		return
	}
	user := core.User{Username: name, PasswordHash: hash, Role: "user", Status: "pending"}
	err = s.store.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&user).Error; err != nil {
			return err
		}
		return core.Audit(tx, &user.ID, "account.register", &user.ID, nil)
	})
	if err != nil {
		if isDuplicateKey(err) {
			writeError(w, core.NewError("USERNAME_TAKEN", "用户名已被注册", http.StatusConflict))
			return
		}
		writeError(w, err)
		return
	}
	writeStatus(w, http.StatusCreated, map[string]any{"status": "pending"})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body credentials
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, err)
		return
	}
	if len(body.Username) < 1 || len(body.Username) > 128 || len(body.Password) < 1 || len(body.Password) > 128 {
		writeError(w, core.NewError("INVALID_REQUEST", "请求参数格式错误", http.StatusUnprocessableEntity))
		return
	}
	name, err := core.NormalizeUsername(body.Username)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.rateLimit(r, "login", name); err != nil {
		writeError(w, err)
		return
	}

	var user core.User
	lookupErr := s.store.DB.WithContext(r.Context()).Where("username = ?", name).First(&user).Error
	if errors.Is(lookupErr, gorm.ErrRecordNotFound) {
		// Verify against a dummy hash so a missing account costs the same time
		// as a wrong password.
		core.VerifyPassword(body.Password, core.DummyHash())
		writeError(w, core.NewError("INVALID_CREDENTIALS", "用户名或密码错误", http.StatusUnauthorized))
		return
	}
	if lookupErr != nil {
		writeError(w, lookupErr)
		return
	}
	if !core.VerifyPassword(body.Password, user.PasswordHash) {
		writeError(w, core.NewError("INVALID_CREDENTIALS", "用户名或密码错误", http.StatusUnauthorized))
		return
	}

	var token string
	var result map[string]any
	err = s.store.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		// Re-read under the row lock: a concurrent rejection must win.
		var locked core.User
		if err := tx.Clauses(lockUpdate()).Where("id = ?", user.ID).First(&locked).Error; err != nil {
			return err
		}
		if locked.Status != "active" {
			return statusError(locked.Status)
		}
		var tokenErr error
		token, tokenErr = core.SessionToken()
		if tokenErr != nil {
			return tokenErr
		}
		session := core.Session{
			TokenHash: core.Digest(token),
			UserID:    locked.ID,
			ExpiresAt: core.Now().Add(time.Duration(s.cfg.SessionSeconds) * time.Second),
		}
		if err := tx.Create(&session).Error; err != nil {
			return err
		}
		if err := core.Audit(tx, &locked.ID, "account.login", &locked.ID, nil); err != nil {
			return err
		}
		result = userJSON(locked)
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	s.setSessionCookie(w, token)
	writeJSON(w, http.StatusOK, result)
}

// statusError maps a non-active account status to the code the frontend explains.
func statusError(status string) error {
	switch status {
	case "pending":
		return core.NewError("ACCOUNT_PENDING", "账号尚未获准登录", http.StatusForbidden)
	case "rejected":
		return core.NewError("ACCOUNT_REJECTED", "账号尚未获准登录", http.StatusForbidden)
	default:
		return core.NewError("ACCOUNT_DISABLED", "账号尚未获准登录", http.StatusForbidden)
	}
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	user := s.requireUser(w, r)
	if user == nil {
		return
	}
	writeJSON(w, http.StatusOK, userJSON(user.User))
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	token := sessionToken(r)
	if token != "" {
		now := core.Now()
		err := s.store.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
			return tx.Model(&core.Session{}).
				Where("token_hash = ? AND revoked_at IS NULL", core.Digest(token)).
				Update("revoked_at", now).Error
		})
		if err != nil {
			writeError(w, err)
			return
		}
	}
	s.clearSessionCookie(w)
	writeStatus(w, http.StatusOK, map[string]any{"status": "logged_out"})
}
