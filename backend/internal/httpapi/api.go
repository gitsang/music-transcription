// Package httpapi serves the JSON API described in docs/implementation-contract.md.
// It is a thin authorization and persistence layer over internal/core; all
// expensive work happens in internal/tasks.
package httpapi

import (
	"net/http"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

// Prefix is the public API mount point, matching the legacy FastAPI service.
const Prefix = "/api/v1"

// Server owns the store and the deployment configuration used by handlers.
type Server struct {
	store *core.Store
	cfg   core.Config
}

// New returns the complete API handler, including security middleware.
func New(store *core.Store) http.Handler {
	server := &Server{store: store}
	if store != nil {
		server.cfg = store.Config
	}
	mux := http.NewServeMux()
	server.routes(mux)
	return server.recoverPanic(server.security(mux))
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+Prefix+"/auth/register", s.register)
	mux.HandleFunc("POST "+Prefix+"/auth/login", s.login)
	mux.HandleFunc("GET "+Prefix+"/auth/me", s.me)
	mux.HandleFunc("POST "+Prefix+"/auth/logout", s.logout)

	mux.HandleFunc("GET "+Prefix+"/capabilities", s.capabilities)

	mux.HandleFunc("POST "+Prefix+"/jobs", s.uploadJob)
	mux.HandleFunc("GET "+Prefix+"/jobs", s.listJobs)
	mux.HandleFunc("GET "+Prefix+"/jobs/{id}", s.getJob)
	mux.HandleFunc("GET "+Prefix+"/jobs/{id}/tracks", s.jobTracks)
	mux.HandleFunc("GET "+Prefix+"/jobs/{id}/artifacts", s.jobArtifacts)
	mux.HandleFunc("POST "+Prefix+"/jobs/{id}/cancel", s.cancelJob)
	mux.HandleFunc("POST "+Prefix+"/jobs/{id}/retry", s.retryJob)
	mux.HandleFunc("DELETE "+Prefix+"/jobs/{id}", s.deleteJob)

	mux.HandleFunc("GET "+Prefix+"/artifacts/{id}/content", s.artifactContent)
	mux.HandleFunc("GET "+Prefix+"/artifacts/{id}/download", s.artifactDownload)

	mux.HandleFunc("GET "+Prefix+"/admin/users", s.adminUsers)
	mux.HandleFunc("POST "+Prefix+"/admin/users/{id}/approve", s.adminModerate("approve"))
	mux.HandleFunc("POST "+Prefix+"/admin/users/{id}/reject", s.adminModerate("reject"))
	mux.HandleFunc("POST "+Prefix+"/admin/users/{id}/disable", s.adminModerate("disable"))
	mux.HandleFunc("GET "+Prefix+"/admin/jobs", s.adminJobs)
	mux.HandleFunc("GET "+Prefix+"/admin/settings", s.adminSettings)
	mux.HandleFunc("PATCH "+Prefix+"/admin/settings", s.patchSettings)

	mux.HandleFunc("GET "+Prefix+"/health/live", s.healthLive)
	mux.HandleFunc("GET "+Prefix+"/health/ready", s.healthReady)

	// Unknown paths must still answer with the shared JSON error envelope.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, core.ErrNotFound)
	})
}
