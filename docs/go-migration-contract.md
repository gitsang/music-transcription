# Go migration implementation contract

Go module in `backend/go.mod`: `github.com/gitsang/music-transcription/backend` (Go 1.24+). No private module paths in dependencies. Public dependencies: GORM postgres driver, go-redis/redis/v8 v8.11.5, google/uuid, x/crypto/argon2, x/text normalization. HTTP standard net/http ServeMux. Python retained only backend/pipeline.

## Shared core API

Package `internal/core`:
- `Config` fields `DatabaseURL, RedisURL, DataDir, PublicOrigin, ModelPython, PipelinePython, DemucsRepo, BasicPitchModelPath, ListenAddr string`; `CookieSecure bool`; `SessionSeconds, LeaseSeconds, StageTimeoutSeconds, WorkerConcurrency int`; `DiskMinFreeBytes uint64`.
- `LoadConfig() (Config,error)` accepts existing postgresql+psycopg URL (strip +psycopg) and postgresql://.
- `Store {DB *gorm.DB; Config Config}`; `Open(Config)(*Store,error)`; `(*Store).Close() error`; `(*Store).Migrate(ctx context.Context) error`; `(*Store).Health(ctx,role) map[string]any`.
- `User` fields ID Username PasswordHash Role Status strings, ReviewedBy *string, ReviewedAt *time.Time, CreatedAt time.Time. JSON hides hash; responses explicitly id/username/role/status.
- `Session`: ID TokenHash UserID strings, ExpiresAt time.Time, RevokedAt *time.Time, CreatedAt time.Time.
- `Job`: ID UserID Title Kind Status Engine strings; Stage *string; StageProgress *float64; ActualDevice *string; ConfigSnapshot map[string]any (GORM serializer:json type:json); Warnings []string (serializer:json); ErrorCode ErrorMessage *string; Attempt int; LeaseToken *string; LeaseExpiresAt HeartbeatAt *time.Time; CancelRequested DeleteRequested bool; DeletedAt *time.Time (plain pointer, NOT gorm.DeletedAt); CreatedAt time.Time; StartedAt CompletedAt ExpiresAt *time.Time.
- `Track`: ID JobID Label LabelSource strings; Program *int; Confidence *float64; IsDrum bool; SourceStemIDs []string serializer:json.
- `Artifact`: ID JobID Kind Label Mime Status strings; TrackID StorageKey SHA256 Error *string; Bytes *int64; Attempt int; CreatedAt time.Time. TrackID NO FK (stem identity can be independent).
- `Setting`: Key string PK; Value any serializer:json; UpdatedBy *string; UpdatedAt time.Time.
- `AuditLog`: ID string ActorID ResourceID *string; Action string; Details map[string]any serializer:json; CreatedAt time.Time.
- `QueueOutbox`: ID JobID strings; Attempt Tries int; DeliveredAt *time.Time; NextRetryAt CreatedAt time.Time. unique(job_id,attempt).
- `RateLimit`: Key string PK; Count int; ResetAt time.Time.
- `ServiceHeartbeat`: Role string PK; UpdatedAt time.Time.
- All model TableName explicit matching Python names; existing DB primary keys VARCHAR(36), timestamps WITHOUT TIME ZONE; no dropped/renamed columns. UUID values populated using `NewID() string` (google/uuid), `Now() time.Time` UTC.
- `Settings(tx *gorm.DB)(map[string]any,error)` merge defaults; `Number(map,key) int` JSON numeric cast helper.
- `Audit(tx *gorm.DB, actor *string, action string, resource *string, details map[string]any) error`.
- `(*Store).InitializeStorage() error`; `(*Store).EnsureSpace() error`; `(*Store).JobDir(userID,jobID string) string`; `(*Store).PrivatePath(key string)(string,error)` resolves symlinks and containment; `(*Store).StorageKey(path string)(string,error)`.
- `Complete(job *Job,status string)` terminal timestamp/retention/clear lease.
- `Purge(tx *gorm.DB,job *Job) (bool,error)` uses job nonblocking flock in DATA_DIR/locks to avoid deleting old process files; deletes artifacts/tracks/outbox and leaves tombstone; requires terminal/lease clear; store needed path config -> method `(*Store).Purge`.
- `Capabilities(cfg Config) []map[string]any`: shared files SHA256 cached, baseline/multi_instrument. No Python exec needed API.
- `Beat(tx *gorm.DB,role string) error` heartbeat upsert.
- Errors `Error {Code,Message string; Status int}` implements error for shared quota/storage/auth failures.

API package `internal/httpapi`: `New(store *core.Store) http.Handler`. Auth implementation can add core/auth.go: `HashPassword(string)(string,error)`, `VerifyPassword(password,encoded string) bool`, `NormalizeUsername(string)(string,error)`, `ValidatePassword(string) error`, `Digest(string) string`, `InitAdmin(store *Store,username,password string)(string,error)`. Compatible Python argon2id hashes + cookie music_session SHA256 tokens; existing user/session data retained. Match docs/implementation-contract.md routes/JSON.

Tasks package `internal/tasks`: `New(store *core.Store) (*Runner,error)`, `(*Runner).Close() error`, `(*Runner).Dispatch(ctx) error`, `(*Runner).Work(ctx) error`. Own redis clients/local MQ. Preserve outbox/fencing/cancel/retention, database task authoritative. MQ payload `{job_id,attempt}`. Use local `pkg/mq/redismq/producer`+consumer APIs from the local pkg/mq. Topic `transcription.jobs`, namespace `music-transcription`, group `transcription-workers`, cluster mode, consume timeout >= job execution timeout, no fixed 10k trim. callback runs task and waits; ACK after durable terminal outcome, infrastructure failures return error. Duplicate/stale job message no-op. WorkerConcurrency consumers unique IDs. Heartbeat service separate from task lease.

Python subprocess protocol: `PIPELINE_PYTHON` default `python3`, execute `-m pipeline.cli`, stdin one JSON request `{input_path,work_dir,kind,config}`. stdout JSON Lines `{type:"progress",stage,stage_progress}` and exactly one `{type:"result",result:{...existing process result...}}` or `{type:"error",code,message}`. stderr diagnostics only. Child and its subprocesses cleaned on SIGTERM; Go cancellation sends TERM to Python process group, waits bounded then KILL; runner adds signal handling. Progress scanner generous bounded line size, result cap 64MiB. Config only DB validated paths. Pipeline overall timeout configurable by job snapshot/deployment, per-stage env retained. Env PYTHONPATH=/app (worker); stderr log outside public output.

CLI `cmd/musicd`: api, worker, dispatcher, migrate, init-admin --username --password-stdin (or no-echo prompt), health --role. manual wiring in internal/app. Existing compose DB volumes and DATA_PATH retained. Old Celery keys ignored; queued outbox replayed onto new stream. Must stop old workers before rollout; back up DB privately before migration; no reset user data. No Git commit/push requested yet.
