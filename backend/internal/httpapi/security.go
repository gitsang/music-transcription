package httpapi

import (
	"net/http"
	"strings"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

// defaultBodyLimit bounds non-upload writes. Uploads are capped separately in
// the upload handler using the shared DB setting, as in the legacy service.
const defaultBodyLimit = 16384

// uploadOverhead covers multipart boundaries and the small text fields so a
// Content-Length pre-check does not reject a file exactly at the limit.
const uploadOverhead = 1024 * 1024

// security applies the CSRF/origin guard and a hard request body cap. A missing
// Origin header is accepted (CLI clients); a mismatching one is rejected.
func (s *Server) security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWrite(r.Method) {
			origin := r.Header.Get("Origin")
			if (origin != "" && origin != s.cfg.PublicOrigin) ||
				strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
				writeError(w, core.NewError("ORIGIN_FORBIDDEN", "拒绝跨站写入请求", http.StatusForbidden))
				return
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !isUpload(r) {
			r.Body = http.MaxBytesReader(w, r.Body, defaultBodyLimit)
		}
		next.ServeHTTP(w, r)
	})
}

func isUpload(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == Prefix+"/jobs"
}

func isWrite(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}
