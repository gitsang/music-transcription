package httpapi

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

// authUser is the authenticated principal for one request.
type authUser struct {
	User  core.User
	Token string
}

func (a *authUser) isAdmin() bool { return a.User.Role == "admin" }

// sessionToken extracts the session cookie, rejecting oversized values.
func sessionToken(r *http.Request) string {
	cookie, err := r.Cookie(core.CookieName)
	if err != nil || cookie.Value == "" || len(cookie.Value) > 256 {
		return ""
	}
	return cookie.Value
}

// currentUser resolves the session cookie to an active user. Sessions are
// server-side rows, so disabling an account revokes access immediately.
func (s *Server) currentUser(r *http.Request) (*authUser, error) {
	token := sessionToken(r)
	if token == "" {
		return nil, core.ErrAuthRequired
	}
	var user core.User
	err := s.store.DB.WithContext(r.Context()).
		Joins("JOIN sessions ON sessions.user_id = users.id").
		Where("sessions.token_hash = ? AND sessions.revoked_at IS NULL AND sessions.expires_at > ? AND users.status = ?",
			core.Digest(token), core.Now(), "active").
		First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, core.NewError("AUTH_REQUIRED", "会话已失效，请重新登录", http.StatusUnauthorized)
	}
	if err != nil {
		return nil, err
	}
	return &authUser{User: user, Token: token}, nil
}

// requireUser is the inline equivalent of a FastAPI dependency.
func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) *authUser {
	user, err := s.currentUser(r)
	if err != nil {
		writeError(w, err)
		return nil
	}
	return user
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) *authUser {
	user := s.requireUser(w, r)
	if user == nil {
		return nil
	}
	if !user.isAdmin() {
		writeError(w, core.ErrForbidden)
		return nil
	}
	return user
}

// setSessionCookie mirrors the Python cookie flags: HttpOnly, SameSite=Lax,
// path=/, and a max-age equal to the configured session lifetime.
func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     core.CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   s.cfg.SessionSeconds,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     core.CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// clientHost is the direct peer address. Forwarding headers are deliberately
// ignored: nginx does not pass X-Forwarded-For and trusting it would let a
// client rotate rate-limit keys at will.
func clientHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "local"
	}
	return host
}

// rateLimit enforces the shared fixed window counter. Keys are SHA256 digests
// of action, IP and account so the table never stores raw addresses.
func (s *Server) rateLimit(r *http.Request, action, username string) error {
	type window struct {
		key   string
		limit int
	}
	windows := []window{{
		key:   core.Digest(action + ":ip:" + clientHost(r)),
		limit: 10,
	}}
	if action == "login" {
		windows[0].limit = 40
	}
	if username != "" {
		windows = append(windows, window{
			key:   core.Digest(action + ":account:" + username),
			limit: 10,
		})
	}
	for _, item := range windows {
		ok, err := s.consumeRateLimit(r, item.key, item.limit)
		if err != nil {
			return err
		}
		if !ok {
			return core.NewError("RATE_LIMITED", "请求过于频繁，请稍后重试", http.StatusTooManyRequests)
		}
	}
	return nil
}

// consumeRateLimit increments a window under a row lock. The insert path races
// with peers on the primary key; a duplicate is retried rather than counted.
func (s *Server) consumeRateLimit(r *http.Request, key string, limit int) (bool, error) {
	var allowed bool
	for attempt := 0; attempt < 3; attempt++ {
		err := s.store.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
			var row core.RateLimit
			err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("key = ?", key).First(&row).Error
			switch {
			case errors.Is(err, gorm.ErrRecordNotFound):
				row = core.RateLimit{Key: key, ResetAt: core.Now().Add(15 * time.Minute)}
			case err != nil:
				return err
			default:
				if !row.ResetAt.After(core.Now()) {
					row.Count, row.ResetAt = 0, core.Now().Add(15*time.Minute)
				}
			}
			if row.Count >= limit {
				allowed = false
				return nil
			}
			row.Count++
			allowed = true
			return tx.Save(&row).Error
		})
		if err == nil {
			return allowed, nil
		}
		if !isDuplicateKey(err) {
			return false, err
		}
	}
	return false, core.NewError("RATE_LIMITED", "请求过于频繁，请稍后重试", http.StatusTooManyRequests)
}

func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate key") || strings.Contains(message, "unique constraint")
}
