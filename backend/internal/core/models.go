package core

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// NewID returns a random UUIDv4 string used for every VARCHAR(36) primary key.
func NewID() string { return uuid.NewString() }

// Now returns the current UTC time. Database timestamps are timezone-naive
// UTC, matching the legacy SQLAlchemy schema.
func Now() time.Time { return time.Now().UTC() }

// User is an account row. PasswordHash is never serialized to API responses.
type User struct {
	ID           string     `gorm:"column:id;type:varchar(36);primaryKey" json:"id"`
	Username     string     `gorm:"column:username;type:varchar(64);not null;uniqueIndex" json:"username"`
	PasswordHash string     `gorm:"column:password_hash;type:text;not null" json:"-"`
	Role         string     `gorm:"column:role;type:varchar(16);not null;default:user" json:"role"`
	Status       string     `gorm:"column:status;type:varchar(16);not null;default:pending" json:"status"`
	ReviewedBy   *string    `gorm:"column:reviewed_by;type:varchar(36)" json:"reviewed_by,omitempty"`
	ReviewedAt   *time.Time `gorm:"column:reviewed_at;type:timestamp" json:"reviewed_at,omitempty"`
	CreatedAt    time.Time  `gorm:"column:created_at;type:timestamp;not null;autoCreateTime:false" json:"created_at"`
}

func (User) TableName() string { return "users" }

func (u *User) BeforeCreate(*gorm.DB) error {
	if u.ID == "" {
		u.ID = NewID()
	}
	if u.Role == "" {
		u.Role = "user"
	}
	if u.Status == "" {
		u.Status = "pending"
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = Now()
	}
	return nil
}

// Session is a server-side session; only the SHA256 token digest is stored.
type Session struct {
	ID        string     `gorm:"column:id;type:varchar(36);primaryKey" json:"id"`
	TokenHash string     `gorm:"column:token_hash;type:varchar(64);not null;uniqueIndex" json:"token_hash"`
	UserID    string     `gorm:"column:user_id;type:varchar(36);not null;index:ix_sessions_user_id" json:"user_id"`
	ExpiresAt time.Time  `gorm:"column:expires_at;type:timestamp;not null" json:"expires_at"`
	RevokedAt *time.Time `gorm:"column:revoked_at;type:timestamp" json:"revoked_at,omitempty"`
	CreatedAt time.Time  `gorm:"column:created_at;type:timestamp;not null;autoCreateTime:false" json:"created_at"`
}

func (Session) TableName() string { return "sessions" }

func (s *Session) BeforeCreate(*gorm.DB) error {
	if s.ID == "" {
		s.ID = NewID()
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = Now()
	}
	return nil
}

// Job is the durable task record and is authoritative across broker loss.
// DeletedAt is a plain nullable timestamp (tombstone), not a GORM soft delete.
type Job struct {
	ID              string         `gorm:"column:id;type:varchar(36);primaryKey" json:"id"`
	UserID          string         `gorm:"column:user_id;type:varchar(36);not null;index:ix_jobs_user_id;index:ix_jobs_owner_status,priority:1" json:"user_id"`
	Title           string         `gorm:"column:title;type:varchar(255);not null" json:"title"`
	Kind            string         `gorm:"column:kind;type:varchar(16);not null" json:"kind"`
	Status          string         `gorm:"column:status;type:varchar(16);not null;default:queued;index:ix_jobs_status;index:ix_jobs_owner_status,priority:2" json:"status"`
	Stage           *string        `gorm:"column:stage;type:varchar(32)" json:"stage"`
	StageProgress   *float64       `gorm:"column:stage_progress;type:double precision" json:"stage_progress"`
	Engine          string         `gorm:"column:engine;type:varchar(32);not null" json:"engine"`
	ActualDevice    *string        `gorm:"column:actual_device;type:varchar(32)" json:"actual_device"`
	ConfigSnapshot  map[string]any `gorm:"column:config_snapshot;type:json;serializer:json;not null" json:"config_snapshot"`
	Warnings        []string       `gorm:"column:warnings;type:json;serializer:json;not null" json:"warnings"`
	ErrorCode       *string        `gorm:"column:error_code;type:varchar(64)" json:"error_code"`
	ErrorMessage    *string        `gorm:"column:error_message;type:text" json:"error_message"`
	Attempt         int            `gorm:"column:attempt;type:integer;not null;default:1" json:"attempt"`
	LeaseToken      *string        `gorm:"column:lease_token;type:varchar(36)" json:"-"`
	LeaseExpiresAt  *time.Time     `gorm:"column:lease_expires_at;type:timestamp;index:ix_jobs_lease_expires_at" json:"-"`
	HeartbeatAt     *time.Time     `gorm:"column:heartbeat_at;type:timestamp" json:"-"`
	CancelRequested bool           `gorm:"column:cancel_requested;type:boolean;not null;default:false" json:"-"`
	DeleteRequested bool           `gorm:"column:delete_requested;type:boolean;not null;default:false" json:"-"`
	DeletedAt       *time.Time     `gorm:"column:deleted_at;type:timestamp" json:"-"`
	CreatedAt       time.Time      `gorm:"column:created_at;type:timestamp;not null;autoCreateTime:false" json:"created_at"`
	StartedAt       *time.Time     `gorm:"column:started_at;type:timestamp" json:"started_at"`
	CompletedAt     *time.Time     `gorm:"column:completed_at;type:timestamp" json:"completed_at"`
	ExpiresAt       *time.Time     `gorm:"column:expires_at;type:timestamp;index:ix_jobs_expires_at" json:"expires_at"`
}

func (Job) TableName() string { return "jobs" }

func (j *Job) BeforeCreate(*gorm.DB) error {
	if j.ID == "" {
		j.ID = NewID()
	}
	if j.Status == "" {
		j.Status = "queued"
	}
	if j.Attempt == 0 {
		j.Attempt = 1
	}
	if j.ConfigSnapshot == nil {
		j.ConfigSnapshot = map[string]any{}
	}
	if j.Warnings == nil {
		j.Warnings = []string{}
	}
	if j.CreatedAt.IsZero() {
		j.CreatedAt = Now()
	}
	return nil
}

// Track is a score part or a separated stem identity.
type Track struct {
	ID            string   `gorm:"column:id;type:varchar(36);primaryKey" json:"id"`
	JobID         string   `gorm:"column:job_id;type:varchar(36);not null;index:ix_tracks_job_id" json:"job_id"`
	Label         string   `gorm:"column:label;type:varchar(255);not null" json:"label"`
	Program       *int     `gorm:"column:program;type:integer" json:"program"`
	Confidence    *float64 `gorm:"column:confidence;type:double precision" json:"confidence"`
	LabelSource   string   `gorm:"column:label_source;type:varchar(32);not null" json:"label_source"`
	IsDrum        bool     `gorm:"column:is_drum;type:boolean;not null;default:false" json:"is_drum"`
	SourceStemIDs []string `gorm:"column:source_stem_ids;type:json;serializer:json;not null" json:"source_stem_ids"`
}

func (Track) TableName() string { return "tracks" }

func (t *Track) BeforeCreate(*gorm.DB) error {
	if t.ID == "" {
		t.ID = NewID()
	}
	if t.SourceStemIDs == nil {
		t.SourceStemIDs = []string{}
	}
	return nil
}

// Artifact is a private output file. TrackID has no FK: stem artifacts use an
// independent separator stem identity rather than a score track ID.
type Artifact struct {
	ID         string    `gorm:"column:id;type:varchar(36);primaryKey" json:"id"`
	JobID      string    `gorm:"column:job_id;type:varchar(36);not null;index:ix_artifacts_job_id" json:"job_id"`
	TrackID    *string   `gorm:"column:track_id;type:varchar(36)" json:"track_id"`
	Kind       string    `gorm:"column:kind;type:varchar(32);not null" json:"kind"`
	Label      string    `gorm:"column:label;type:varchar(255);not null" json:"label"`
	StorageKey *string   `gorm:"column:storage_key;type:text" json:"-"`
	Mime       string    `gorm:"column:mime;type:varchar(128);not null" json:"mime"`
	Bytes      *int64    `gorm:"column:bytes;type:integer" json:"bytes"`
	SHA256     *string   `gorm:"column:sha256;type:varchar(64)" json:"-"`
	Status     string    `gorm:"column:status;type:varchar(16);not null" json:"status"`
	Error      *string   `gorm:"column:error;type:text" json:"error"`
	Attempt    int       `gorm:"column:attempt;type:integer;not null;default:1" json:"attempt"`
	CreatedAt  time.Time `gorm:"column:created_at;type:timestamp;not null;autoCreateTime:false" json:"created_at"`
}

func (Artifact) TableName() string { return "artifacts" }

func (a *Artifact) BeforeCreate(*gorm.DB) error {
	if a.ID == "" {
		a.ID = NewID()
	}
	if a.Attempt == 0 {
		a.Attempt = 1
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = Now()
	}
	return nil
}

// Setting is a database-backed global setting with a JSON value.
type Setting struct {
	Key       string    `gorm:"column:key;type:varchar(64);primaryKey" json:"key"`
	Value     any       `gorm:"column:value;type:json;serializer:json;not null" json:"value"`
	UpdatedBy *string   `gorm:"column:updated_by;type:varchar(36)" json:"updated_by,omitempty"`
	UpdatedAt time.Time `gorm:"column:updated_at;type:timestamp;not null;autoCreateTime:false;autoUpdateTime:false" json:"updated_at"`
}

func (Setting) TableName() string { return "settings" }

func (s *Setting) BeforeCreate(*gorm.DB) error {
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = Now()
	}
	return nil
}

func (s *Setting) BeforeUpdate(*gorm.DB) error {
	s.UpdatedAt = Now()
	return nil
}

// AuditLog records a sanitized action trail.
type AuditLog struct {
	ID         string         `gorm:"column:id;type:varchar(36);primaryKey" json:"id"`
	ActorID    *string        `gorm:"column:actor_id;type:varchar(36);index:ix_audit_logs_actor_id" json:"actor_id"`
	Action     string         `gorm:"column:action;type:varchar(64);not null" json:"action"`
	ResourceID *string        `gorm:"column:resource_id;type:varchar(64)" json:"resource_id"`
	Details    map[string]any `gorm:"column:details;type:json;serializer:json;not null" json:"details"`
	CreatedAt  time.Time      `gorm:"column:created_at;type:timestamp;not null;autoCreateTime:false" json:"created_at"`
}

func (AuditLog) TableName() string { return "audit_logs" }

func (a *AuditLog) BeforeCreate(*gorm.DB) error {
	if a.ID == "" {
		a.ID = NewID()
	}
	if a.Details == nil {
		a.Details = map[string]any{}
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = Now()
	}
	return nil
}

// QueueOutbox is the transactional outbox row for one job attempt.
type QueueOutbox struct {
	ID          string     `gorm:"column:id;type:varchar(36);primaryKey" json:"id"`
	JobID       string     `gorm:"column:job_id;type:varchar(36);not null;uniqueIndex:queue_outbox_job_id_attempt_key,priority:1;index:ix_queue_outbox_job_id" json:"job_id"`
	Attempt     int        `gorm:"column:attempt;type:integer;not null;uniqueIndex:queue_outbox_job_id_attempt_key,priority:2" json:"attempt"`
	DeliveredAt *time.Time `gorm:"column:delivered_at;type:timestamp" json:"delivered_at"`
	NextRetryAt time.Time  `gorm:"column:next_retry_at;type:timestamp;not null;index:ix_queue_outbox_next_retry_at" json:"next_retry_at"`
	Tries       int        `gorm:"column:tries;type:integer;not null;default:0" json:"tries"`
	CreatedAt   time.Time  `gorm:"column:created_at;type:timestamp;not null;autoCreateTime:false" json:"created_at"`
}

func (QueueOutbox) TableName() string { return "queue_outbox" }

func (q *QueueOutbox) BeforeCreate(*gorm.DB) error {
	if q.ID == "" {
		q.ID = NewID()
	}
	if q.NextRetryAt.IsZero() {
		q.NextRetryAt = Now()
	}
	if q.CreatedAt.IsZero() {
		q.CreatedAt = Now()
	}
	return nil
}

// RateLimit is a shared fixed-window counter, safe across API processes.
type RateLimit struct {
	Key     string    `gorm:"column:key;type:varchar(64);primaryKey" json:"key"`
	Count   int       `gorm:"column:count;type:integer;not null;default:0" json:"count"`
	ResetAt time.Time `gorm:"column:reset_at;type:timestamp;not null;index:ix_rate_limits_reset_at" json:"reset_at"`
}

func (RateLimit) TableName() string { return "rate_limits" }

// ServiceHeartbeat tracks a long-lived worker/dispatcher process.
type ServiceHeartbeat struct {
	Role      string    `gorm:"column:role;type:varchar(16);primaryKey" json:"role"`
	UpdatedAt time.Time `gorm:"column:updated_at;type:timestamp;not null;autoCreateTime:false;autoUpdateTime:false" json:"updated_at"`
}

func (ServiceHeartbeat) TableName() string { return "service_heartbeats" }

// Models lists every table in creation order.
func Models() []any {
	return []any{
		&User{}, &Session{}, &Job{}, &Track{}, &Artifact{}, &Setting{},
		&AuditLog{}, &QueueOutbox{}, &RateLimit{}, &ServiceHeartbeat{},
	}
}
