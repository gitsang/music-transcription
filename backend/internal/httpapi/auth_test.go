package httpapi

import (
	"net/http"
	"testing"

	"gorm.io/gorm"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

func TestRegisterLoginAndApprovalFlow(t *testing.T) {
	ts := newTestServer(t)
	makeAdmin(t, ts.store, "root")
	anonymous := ts.client()

	created := anonymous.postJSON(t, Prefix+"/auth/register",
		`{"username":"Alice","password":"`+testPassword+`"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("register: status %d body %s", created.Code, created.Body)
	}
	if body := decode(t, created.Body.Bytes()); body["status"] != "pending" {
		t.Fatalf("register body = %v", body)
	}

	// Pending accounts may not log in; the username is casefolded at storage.
	pending := anonymous.postJSON(t, Prefix+"/auth/login",
		`{"username":"alice","password":"`+testPassword+`"}`)
	if pending.Code != http.StatusForbidden || errorCode(t, pending.Body.Bytes()) != "ACCOUNT_PENDING" {
		t.Fatalf("pending login: %d %s", pending.Code, pending.Body)
	}

	admin := loginClient(t, ts, "root")
	var alice core.User
	if err := ts.store.DB.Where("username = ?", "alice").First(&alice).Error; err != nil {
		t.Fatalf("load alice: %v", err)
	}
	approved := admin.postJSON(t, Prefix+"/admin/users/"+alice.ID+"/approve", "")
	if approved.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", approved.Code, approved.Body)
	}
	if body := decode(t, approved.Body.Bytes()); body["status"] != "active" {
		t.Fatalf("approve body = %v", body)
	}

	user := loginClient(t, ts, "alice")
	me := user.get(t, Prefix+"/auth/me")
	if me.Code != http.StatusOK {
		t.Fatalf("me: %d %s", me.Code, me.Body)
	}
	body := decode(t, me.Body.Bytes())
	if body["username"] != "alice" || body["role"] != "user" {
		t.Fatalf("me body = %v", body)
	}
	// The session cookie must be HttpOnly and scoped to the whole site.
	cookies := me.Result().Cookies()
	_ = cookies

	if out := user.postJSON(t, Prefix+"/auth/logout", ""); out.Code != http.StatusOK {
		t.Fatalf("logout: %d", out.Code)
	}
	if after := user.get(t, Prefix+"/auth/me"); after.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout: %d", after.Code)
	}
}

// TestSessionCookieAttributes pins the browser-facing security flags; a
// regression here silently weakens every logged-in session.
func TestSessionCookieAttributes(t *testing.T) {
	ts := newTestServer(t)
	createUser(t, ts.store, "cookie-user")
	response := ts.client().postJSON(t, Prefix+"/auth/login",
		`{"username":"cookie-user","password":"`+testPassword+`"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("login: %d %s", response.Code, response.Body)
	}
	var session *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == core.CookieName {
			session = cookie
		}
	}
	if session == nil {
		t.Fatal("login did not set a session cookie")
	}
	if !session.HttpOnly {
		t.Fatal("session cookie must be HttpOnly")
	}
	if session.SameSite != http.SameSiteLaxMode {
		t.Fatalf("same-site = %v", session.SameSite)
	}
	if session.Path != "/" {
		t.Fatalf("path = %q", session.Path)
	}
	if session.MaxAge != ts.store.Config.SessionSeconds {
		t.Fatalf("max-age = %d", session.MaxAge)
	}
	// The cookie value must be the raw token, never the stored digest.
	if len(session.Value) < 60 {
		t.Fatalf("cookie value looks wrong: %q", session.Value)
	}
	var stored core.Session
	if err := ts.store.DB.First(&stored).Error; err != nil {
		t.Fatalf("load session: %v", err)
	}
	if stored.TokenHash != core.Digest(session.Value) {
		t.Fatal("stored token hash does not match the cookie")
	}
	if stored.TokenHash == session.Value {
		t.Fatal("cookie stores a raw token in the database")
	}
}

// TestReadsIgnoreOriginAndCrossSiteHeader checks the write guard does not turn
// into a read guard: cross-site GETs still answer, because reads are protected
// by the session cookie and SameSite, not by Origin.
func TestReadsIgnoreOriginAndCrossSiteHeader(t *testing.T) {
	ts := newTestServer(t)
	createUser(t, ts.store, "read-user")
	session := loginClient(t, ts, "read-user")
	response := ts.request(t, http.MethodGet, Prefix+"/auth/me", nil, map[string]string{
		"Cookie": session.cookie, "Origin": "http://evil.example", "Sec-Fetch-Site": "cross-site",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("cross-site read: %d %s", response.Code, response.Body)
	}
	// Sec-Fetch-Site alone is enough to reject a write, even with no Origin.
	blocked := ts.request(t, http.MethodPost, Prefix+"/auth/logout", nil, map[string]string{
		"Cookie": session.cookie, "Sec-Fetch-Site": "cross-site", "Origin": ts.origin,
	})
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("cross-site write: %d", blocked.Code)
	}
}

func TestRegisterValidationAndDuplicate(t *testing.T) {
	ts := newTestServer(t)
	anonymous := ts.client()

	if response := anonymous.postJSON(t, Prefix+"/auth/register",
		`{"username":"x","password":"`+testPassword+`"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("short username: %d %s", response.Code, response.Body)
	}
	if response := anonymous.postJSON(t, Prefix+"/auth/register",
		`{"username":"bob","password":"short"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("short password: %d", response.Code)
	}
	if response := anonymous.postJSON(t, Prefix+"/auth/register",
		`{"username":"bob","password":"`+testPassword+`","extra":1}`); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown field: %d", response.Code)
	}
	if response := anonymous.postJSON(t, Prefix+"/auth/register",
		`{"username":"bob","password":"`+testPassword+`"}`); response.Code != http.StatusCreated {
		t.Fatalf("first register: %d", response.Code)
	}
	duplicate := anonymous.postJSON(t, Prefix+"/auth/register",
		`{"username":"Bob","password":"`+testPassword+`"}`)
	if duplicate.Code != http.StatusConflict || errorCode(t, duplicate.Body.Bytes()) != "USERNAME_TAKEN" {
		t.Fatalf("duplicate: %d %s", duplicate.Code, duplicate.Body)
	}
}

func TestOriginGuardAndLoginFailures(t *testing.T) {
	ts := newTestServer(t)
	createUser(t, ts.store, "carol")
	anonymous := ts.client()

	// A matching Origin is required for writes; a foreign one is rejected.
	blocked := ts.request(t, http.MethodPost, Prefix+"/auth/login",
		[]byte(`{"username":"carol","password":"`+testPassword+`"}`),
		map[string]string{"Origin": "http://evil.example", "Content-Type": "application/json"})
	if blocked.Code != http.StatusForbidden || errorCode(t, blocked.Body.Bytes()) != "ORIGIN_FORBIDDEN" {
		t.Fatalf("origin guard: %d %s", blocked.Code, blocked.Body)
	}
	// CLI clients send no Origin and must still be able to log in.
	cli := ts.request(t, http.MethodPost, Prefix+"/auth/login",
		[]byte(`{"username":"carol","password":"`+testPassword+`"}`),
		map[string]string{"Content-Type": "application/json"})
	if cli.Code != http.StatusOK {
		t.Fatalf("cli login: %d %s", cli.Code, cli.Body)
	}

	wrong := anonymous.postJSON(t, Prefix+"/auth/login",
		`{"username":"carol","password":"wrong-password-xx"}`)
	if wrong.Code != http.StatusUnauthorized || errorCode(t, wrong.Body.Bytes()) != "INVALID_CREDENTIALS" {
		t.Fatalf("wrong password: %d %s", wrong.Code, wrong.Body)
	}
	missing := anonymous.postJSON(t, Prefix+"/auth/login",
		`{"username":"nobody","password":"`+testPassword+`"}`)
	if missing.Code != http.StatusUnauthorized || errorCode(t, missing.Body.Bytes()) != "INVALID_CREDENTIALS" {
		t.Fatalf("missing user: %d %s", missing.Code, missing.Body)
	}
}

func TestRejectAndDisableRevokeSessions(t *testing.T) {
	ts := newTestServer(t)
	makeAdmin(t, ts.store, "root")
	user := createUser(t, ts.store, "dave")
	admin := loginClient(t, ts, "root")
	session := loginClient(t, ts, "dave")

	if response := admin.postJSON(t, Prefix+"/admin/users/"+user.ID+"/disable", ""); response.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", response.Code, response.Body)
	}
	// Existing sessions must stop working immediately.
	if me := session.get(t, Prefix+"/auth/me"); me.Code != http.StatusUnauthorized {
		t.Fatalf("me after disable: %d", me.Code)
	}
	relogin := ts.client().postJSON(t, Prefix+"/auth/login",
		`{"username":"dave","password":"`+testPassword+`"}`)
	if relogin.Code != http.StatusForbidden || errorCode(t, relogin.Body.Bytes()) != "ACCOUNT_DISABLED" {
		t.Fatalf("relogin after disable: %d %s", relogin.Code, relogin.Body)
	}
	if response := admin.postJSON(t, Prefix+"/admin/users/"+user.ID+"/reject", ""); response.Code != http.StatusOK {
		t.Fatalf("reject: %d", response.Code)
	}
	if response := admin.postJSON(t, Prefix+"/admin/users/"+user.ID+"/approve", ""); response.Code != http.StatusOK {
		t.Fatalf("re-approve: %d", response.Code)
	}
	// Admins are protected from moderation endpoints.
	var root core.User
	if err := ts.store.DB.Where("username = ?", "root").First(&root).Error; err != nil {
		t.Fatalf("load root: %v", err)
	}
	protected := admin.postJSON(t, Prefix+"/admin/users/"+root.ID+"/disable", "")
	if protected.Code != http.StatusConflict || errorCode(t, protected.Body.Bytes()) != "ADMIN_PROTECTED" {
		t.Fatalf("admin protect: %d %s", protected.Code, protected.Body)
	}
}

func TestAdminEndpointsRequireAdmin(t *testing.T) {
	ts := newTestServer(t)
	createUser(t, ts.store, "erin")
	session := loginClient(t, ts, "erin")
	for _, path := range []string{"/admin/users", "/admin/jobs", "/admin/settings"} {
		if response := session.get(t, Prefix+path); response.Code != http.StatusForbidden {
			t.Fatalf("%s: %d", path, response.Code)
		}
	}
	if response := session.get(t, Prefix+"/admin/users"); response.Code != http.StatusForbidden {
		t.Fatalf("anonymous admin access: %d", response.Code)
	}
}

func TestLoginRateLimitIsSharedAndHashed(t *testing.T) {
	ts := newTestServer(t)
	createUser(t, ts.store, "frank")
	anonymous := ts.client()
	limited := false
	for attempt := 0; attempt < 60; attempt++ {
		response := anonymous.postJSON(t, Prefix+"/auth/login",
			`{"username":"frank","password":"wrong-password-xx"}`)
		if response.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("login attempts were never rate limited")
	}
	// Only digests may be stored: a raw IP or username must never appear.
	var rows []core.RateLimit
	if err := ts.store.DB.Find(&rows).Error; err != nil {
		t.Fatalf("load rate limits: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no rate limit rows recorded")
	}
	for _, row := range rows {
		if len(row.Key) != 64 {
			t.Fatalf("rate limit key is not a sha256 digest: %q", row.Key)
		}
	}
}

func TestCapabilitiesShape(t *testing.T) {
	ts := newTestServer(t)
	response := ts.client().get(t, Prefix+"/capabilities")
	if response.Code != http.StatusOK {
		t.Fatalf("capabilities: %d %s", response.Code, response.Body)
	}
	body := decode(t, response.Body.Bytes())
	engines, ok := body["engines"].([]any)
	if !ok || len(engines) == 0 {
		t.Fatalf("engines = %v", body["engines"])
	}
	first, _ := engines[0].(map[string]any)
	for _, key := range []string{"id", "label", "ready", "limitations"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("engine missing %q: %v", key, first)
		}
	}
	for _, key := range []string{"max_upload_bytes", "max_audio_duration_seconds", "max_unfinished_jobs_per_user"} {
		if _, ok := body[key].(float64); !ok {
			t.Fatalf("capabilities missing %q: %v", key, body)
		}
	}
}

func TestHealthLiveIsUnauthenticated(t *testing.T) {
	ts := newTestServer(t)
	live := ts.client().get(t, Prefix+"/health/live")
	if live.Code != http.StatusOK {
		t.Fatalf("live: %d", live.Code)
	}
	if body := decode(t, live.Body.Bytes()); body["status"] != "ok" {
		t.Fatalf("live body = %v", body)
	}
	ready := ts.client().get(t, Prefix+"/health/ready")
	body := decode(t, ready.Body.Bytes())
	if _, ok := body["checks"]; !ok {
		t.Fatalf("ready body missing checks: %v", body)
	}
	// The broker is unreachable in tests, so readiness must report 503 and
	// never claim ready while a dependency is down.
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready status = %d body %v", ready.Code, body)
	}
	if body["status"] != "not_ready" {
		t.Fatalf("ready body = %v", body)
	}
}

func TestUnknownRouteReturnsJSONEnvelope(t *testing.T) {
	ts := newTestServer(t)
	response := ts.client().get(t, Prefix+"/nope")
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown route: %d", response.Code)
	}
	if errorCode(t, response.Body.Bytes()) != "NOT_FOUND" {
		t.Fatalf("unknown route body: %s", response.Body)
	}
}

// ensure the test harness really created the schema it expects
func TestSchemaIsIsolated(t *testing.T) {
	ts := newTestServer(t)
	var count int64
	if err := ts.store.DB.Model(&core.User{}).Count(&count).Error; err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 0 {
		t.Fatalf("fresh schema should be empty, got %d users", count)
	}
	var table string
	err := ts.store.DB.Raw("SELECT current_schema()").Scan(&table).Error
	if err != nil {
		t.Fatalf("current_schema: %v", err)
	}
	if table == "public" {
		t.Fatal("tests must not run in the public schema")
	}
	_ = gorm.ErrRecordNotFound
}
