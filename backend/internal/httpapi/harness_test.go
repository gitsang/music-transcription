package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

// TestDSNEnv names the isolated PostgreSQL used by integration tests. The suite
// refuses to run without it: the contract forbids pointing tests at the
// deployment database.
const TestDSNEnv = "MUSIC_TEST_DATABASE_URL"

const testPassword = "correct-horse-battery"

var schemaCounter atomic.Int64

// testStore opens the shared test database in a fresh schema so parallel runs
// and repeated `go test` invocations never see each other's rows.
func testStore(t *testing.T) *core.Store {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(TestDSNEnv))
	if dsn == "" {
		t.Skipf("%s is not set; skipping PostgreSQL integration test", TestDSNEnv)
	}
	if !strings.Contains(dsn, "test") {
		t.Fatalf("%s must point at a dedicated test database, got %q", TestDSNEnv, dsn)
	}
	schema := fmt.Sprintf("httpapi_test_%d_%d", time.Now().UnixNano(), schemaCounter.Add(1))
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		_ = admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	dataDir := t.TempDir()
	cfg := core.Config{
		DatabaseURL:         dsn + separator + "search_path=" + schema,
		RedisURL:            "redis://127.0.0.1:1/0", // deliberately unreachable
		DataDir:             dataDir,
		PublicOrigin:        "http://localhost:8080",
		SessionSeconds:      3600,
		LeaseSeconds:        120,
		StageTimeoutSeconds: 3600,
		DiskMinFreeBytes:    0,
	}
	store, err := core.Open(cfg)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

// testServer wraps the API in an httptest handler plus a cookie-aware client.
type testServer struct {
	handler http.Handler
	store   *core.Store
	origin  string
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	store := testStore(t)
	return &testServer{handler: New(store), store: store, origin: store.Config.PublicOrigin}
}

// request performs one HTTP call. Writes always carry the configured Origin so
// tests exercise the allowed path unless they override it.
func (ts *testServer) request(t *testing.T, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(string(body)))
	request.RemoteAddr = "127.0.0.1:12345"
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	if isWrite(method) && request.Header.Get("Origin") == "" {
		request.Header.Set("Origin", ts.origin)
	}
	recorder := httptest.NewRecorder()
	ts.handler.ServeHTTP(recorder, request)
	return recorder
}

// client is a minimal cookie jar, enough for the single-session API.
type client struct {
	server *testServer
	cookie string
}

func (ts *testServer) client() *client { return &client{server: ts} }

func (c *client) do(t *testing.T, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{}
	if c.cookie != "" {
		headers["Cookie"] = c.cookie
	}
	recorder := c.server.request(t, method, path, body, headers)
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == core.CookieName {
			if cookie.MaxAge < 0 || cookie.Value == "" {
				c.cookie = ""
			} else {
				c.cookie = cookie.Name + "=" + cookie.Value
			}
		}
	}
	return recorder
}

func (c *client) postJSON(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return c.do(t, http.MethodPost, path, []byte(body))
}

func (c *client) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	return c.do(t, http.MethodGet, path, nil)
}

// createUser inserts an account directly; role/status default to an active user.
func createUser(t *testing.T, store *core.Store, username string, setup ...func(*core.User)) core.User {
	t.Helper()
	hash, err := core.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	user := core.User{
		Username: username, PasswordHash: hash, Role: "user", Status: "active",
		CreatedAt: core.Now(),
	}
	for _, apply := range setup {
		apply(&user)
	}
	if err := store.DB.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	return user
}

// loginClient authenticates and keeps the session cookie.
func loginClient(t *testing.T, ts *testServer, username string) *client {
	t.Helper()
	session := ts.client()
	response := session.postJSON(t, Prefix+"/auth/login",
		fmt.Sprintf(`{"username":%q,"password":%q}`, username, testPassword))
	if response.Code != http.StatusOK {
		t.Fatalf("login %s: status %d body %s", username, response.Code, response.Body.String())
	}
	return session
}

// jobDirFor mirrors the on-disk layout so tests can assert cleanup.
func jobDirFor(store *core.Store, userID, jobID string) string {
	return filepath.Join(store.Config.DataDir, "users", userID, "jobs", jobID)
}
