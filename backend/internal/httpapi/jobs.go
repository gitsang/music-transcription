package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"gorm.io/gorm"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

// unfinished are the statuses that count against a user's quota.
var unfinished = []string{"queued", "running"}

// authorizedJob loads a visible job. Non-admins only ever see their own rows;
// a foreign job is reported as 404 so IDs cannot be probed.
func (s *Server) authorizedJob(ctx context.Context, jobID string, user *authUser, lock bool) (*core.Job, error) {
	query := s.store.DB.WithContext(ctx).
		Where("id = ? AND deleted_at IS NULL AND delete_requested = ?", jobID, false)
	if !user.isAdmin() {
		query = query.Where("user_id = ?", user.User.ID)
	}
	if lock {
		query = query.Clauses(lockUpdate())
	}
	var job core.Job
	err := query.First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, core.NewError("NOT_FOUND", "任务不存在", http.StatusNotFound)
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// checkQuota locks the user row and counts unfinished jobs, so two concurrent
// uploads cannot both slip past the limit.
func (s *Server) checkQuota(tx *gorm.DB, userID string, settings map[string]any) error {
	var user core.User
	err := tx.Clauses(lockUpdate()).Where("id = ?", userID).First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return core.NewError("AUTH_REQUIRED", "会话已失效", http.StatusUnauthorized)
	}
	if err != nil {
		return err
	}
	if user.Status != "active" {
		return core.NewError("AUTH_REQUIRED", "会话已失效", http.StatusUnauthorized)
	}
	limit := core.Number(settings, "max_unfinished_jobs_per_user")
	if limit <= 0 {
		limit = core.Number(core.DefaultSettings, "max_unfinished_jobs_per_user")
	}
	var count int64
	err = tx.Model(&core.Job{}).
		Where("user_id = ? AND status IN ? AND deleted_at IS NULL", userID, unfinished).
		Count(&count).Error
	if err != nil {
		return err
	}
	if count >= int64(limit) {
		return core.ErrJobLimit
	}
	return nil
}

// clearDerived drops everything produced by a previous attempt but keeps the
// original input artifact, which a retry reuses.
func clearDerived(tx *gorm.DB, jobID string) error {
	if err := tx.Where("job_id = ? AND kind <> ?", jobID, "original").Delete(&core.Artifact{}).Error; err != nil {
		return err
	}
	return tx.Where("job_id = ?", jobID).Delete(&core.Track{}).Error
}

// jobView assembles the full Job payload: owner name plus tracks and artifacts.
func (s *Server) jobView(ctx context.Context, job *core.Job) (map[string]any, error) {
	db := s.store.DB.WithContext(ctx)
	var username *string
	var owner core.User
	err := db.Select("username").Where("id = ?", job.UserID).First(&owner).Error
	if err == nil {
		username = &owner.Username
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	var tracks []core.Track
	if err := db.Where("job_id = ?", job.ID).Find(&tracks).Error; err != nil {
		return nil, err
	}
	var artifacts []core.Artifact
	if err := db.Where("job_id = ?", job.ID).Find(&artifacts).Error; err != nil {
		return nil, err
	}
	return jobJSON(*job, username, tracks, artifacts), nil
}

func (s *Server) writeJob(w http.ResponseWriter, r *http.Request, job *core.Job, status int) {
	view, err := s.jobView(r.Context(), job)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, status, view)
}

// visibleJobs is the shared query for /jobs and /admin/jobs: newest 200.
func (s *Server) visibleJobs(ctx context.Context, userID string) ([]core.Job, error) {
	query := s.store.DB.WithContext(ctx).
		Where("deleted_at IS NULL AND delete_requested = ?", false)
	if userID != "" {
		query = query.Where("user_id = ?", userID)
	}
	var jobs []core.Job
	if err := query.Order("created_at DESC").Limit(200).Find(&jobs).Error; err != nil {
		return nil, err
	}
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].CreatedAt.After(jobs[j].CreatedAt) })
	return jobs, nil
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	user := s.requireUser(w, r)
	if user == nil {
		return
	}
	jobs, err := s.visibleJobs(r.Context(), user.User.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	s.writeJobList(w, r, jobs)
}

func (s *Server) adminJobs(w http.ResponseWriter, r *http.Request) {
	user := s.requireAdmin(w, r)
	if user == nil {
		return
	}
	jobs, err := s.visibleJobs(r.Context(), "")
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		return core.Audit(tx, &user.User.ID, "admin.jobs.list", nil, nil)
	}); err != nil {
		writeError(w, err)
		return
	}
	s.writeJobList(w, r, jobs)
}

func (s *Server) writeJobList(w http.ResponseWriter, r *http.Request, jobs []core.Job) {
	views, err := s.jobViews(r.Context(), jobs)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

// jobViews builds many job payloads with a constant number of queries. The
// frontend polls the list, so an N+1 per job would cost hundreds of round
// trips on a full page of 200 jobs.
func (s *Server) jobViews(ctx context.Context, jobs []core.Job) ([]map[string]any, error) {
	if len(jobs) == 0 {
		return []map[string]any{}, nil
	}
	db := s.store.DB.WithContext(ctx)
	jobIDs := make([]string, 0, len(jobs))
	userIDs := make([]string, 0, len(jobs))
	seenUser := make(map[string]bool, len(jobs))
	for _, job := range jobs {
		jobIDs = append(jobIDs, job.ID)
		if !seenUser[job.UserID] {
			seenUser[job.UserID] = true
			userIDs = append(userIDs, job.UserID)
		}
	}
	var owners []core.User
	if err := db.Select("id, username").Where("id IN ?", userIDs).Find(&owners).Error; err != nil {
		return nil, err
	}
	usernames := make(map[string]string, len(owners))
	for _, owner := range owners {
		usernames[owner.ID] = owner.Username
	}
	var tracks []core.Track
	if err := db.Where("job_id IN ?", jobIDs).Find(&tracks).Error; err != nil {
		return nil, err
	}
	tracksByJob := make(map[string][]core.Track, len(jobs))
	for _, track := range tracks {
		tracksByJob[track.JobID] = append(tracksByJob[track.JobID], track)
	}
	var artifacts []core.Artifact
	if err := db.Where("job_id IN ?", jobIDs).Find(&artifacts).Error; err != nil {
		return nil, err
	}
	artifactsByJob := make(map[string][]core.Artifact, len(jobs))
	for _, artifact := range artifacts {
		artifactsByJob[artifact.JobID] = append(artifactsByJob[artifact.JobID], artifact)
	}

	views := make([]map[string]any, 0, len(jobs))
	for index := range jobs {
		job := &jobs[index]
		var username *string
		if value, ok := usernames[job.UserID]; ok {
			username = &value
		}
		views = append(views, jobJSON(*job, username, tracksByJob[job.ID], artifactsByJob[job.ID]))
	}
	return views, nil
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	user := s.requireUser(w, r)
	if user == nil {
		return
	}
	job, err := s.authorizedJob(r.Context(), r.PathValue("id"), user, false)
	if err != nil {
		writeError(w, err)
		return
	}
	if user.isAdmin() {
		if err := s.auditUser(r, user, "admin.job.read", job.ID, nil); err != nil {
			writeError(w, err)
			return
		}
	}
	s.writeJob(w, r, job, http.StatusOK)
}

func (s *Server) jobTracks(w http.ResponseWriter, r *http.Request) {
	user := s.requireUser(w, r)
	if user == nil {
		return
	}
	job, err := s.authorizedJob(r.Context(), r.PathValue("id"), user, false)
	if err != nil {
		writeError(w, err)
		return
	}
	var tracks []core.Track
	if err := s.store.DB.WithContext(r.Context()).Where("job_id = ?", job.ID).Find(&tracks).Error; err != nil {
		writeError(w, err)
		return
	}
	result := make([]map[string]any, 0, len(tracks))
	for _, track := range tracks {
		result = append(result, trackJSON(track))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) jobArtifacts(w http.ResponseWriter, r *http.Request) {
	user := s.requireUser(w, r)
	if user == nil {
		return
	}
	job, err := s.authorizedJob(r.Context(), r.PathValue("id"), user, false)
	if err != nil {
		writeError(w, err)
		return
	}
	var artifacts []core.Artifact
	if err := s.store.DB.WithContext(r.Context()).Where("job_id = ?", job.ID).Find(&artifacts).Error; err != nil {
		writeError(w, err)
		return
	}
	result := make([]map[string]any, 0, len(artifacts))
	for _, artifact := range artifacts {
		result = append(result, artifactJSON(artifact))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) auditUser(r *http.Request, user *authUser, action, resource string, details map[string]any) error {
	return s.store.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		return core.Audit(tx, &user.User.ID, action, &resource, details)
	})
}

// safeTitle strips directories and control characters from an upload name.
func safeTitle(filename string) string {
	filename = strings.ReplaceAll(filename, "\\", "/")
	if index := strings.LastIndex(filename, "/"); index >= 0 {
		filename = filename[index+1:]
	}
	var builder strings.Builder
	for _, r := range filename {
		if r < 0x20 || r == 0x7f {
			continue
		}
		builder.WriteRune(r)
	}
	title := builder.String()
	runes := []rune(title)
	if len(runes) > 255 {
		title = string(runes[:255])
	}
	if title == "" {
		return "upload"
	}
	return title
}
