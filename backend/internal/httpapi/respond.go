package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

// errorEnvelope matches FastAPI's {"error": {"code", "message"}} contract.
type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if value == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("write response", "error", err)
	}
}

func writeError(w http.ResponseWriter, err error) {
	status, code, message := http.StatusInternalServerError, "INTERNAL_ERROR", "服务暂时不可用"
	var domain *core.Error
	if errors.As(err, &domain) {
		status, code, message = domain.Status, domain.Code, domain.Message
	} else if err != nil {
		slog.Error("api request failed", "error", err)
	}
	body := errorEnvelope{}
	body.Error.Code, body.Error.Message = code, message
	writeJSON(w, status, body)
}

func writeStatus(w http.ResponseWriter, status int, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	writeJSON(w, status, payload)
}

// recoverPanic converts an unexpected panic into the generic error envelope so
// one bad request cannot take down the process.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("api panic", "path", r.URL.Path, "recovered", recovered, "stack", string(debug.Stack()))
				writeError(w, errors.New("internal error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
