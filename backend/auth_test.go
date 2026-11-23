package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

const testAdminUser = "test-admin"
const testInitialPassword = "test-initial-password-123"

func testApp(t *testing.T, base, user, password string) *App {
	t.Helper()
	app, err := newApp(filepath.Join(base, "state"), filepath.Join(base, "backups"), user, password)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func requestWithOrigin(method, path, body string, cookie *http.Cookie) *http.Request {
	req := httptest.NewRequest(method, "http://example.test"+path, strings.NewReader(body))
	req.Header.Set("Origin", "http://example.test")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return req
}

func loginForTest(t *testing.T, h http.Handler, username, password string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, requestWithOrigin(http.MethodPost, "/api/auth/login", `{"username":"`+username+`","password":"`+password+`"}`, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not set a cookie")
	}
	return cookies[0]
}

func TestAdministratorInitializationAndEnvironmentDoesNotOverwrite(t *testing.T) {
	base := t.TempDir()
	app := testApp(t, base, testAdminUser, testInitialPassword)
	var storedUser, passwordHash string
	if err := app.db.QueryRow(`SELECT username, password_hash FROM administrators WHERE id=1`).Scan(&storedUser, &passwordHash); err != nil {
		t.Fatal(err)
	}
	if storedUser != testAdminUser {
		t.Fatalf("stored username=%q", storedUser)
	}
	if passwordHash == testInitialPassword || bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(testInitialPassword)) != nil {
		t.Fatal("administrator password was not stored as a valid bcrypt hash")
	}
	app.close()

	// Existing installations remain usable with empty or changed environment values.
	for _, env := range [][2]string{{"", ""}, {"different-user", "different-password"}} {
		reopened := testApp(t, base, env[0], env[1])
		if reopened.adminUser != testAdminUser {
			t.Fatalf("environment changed persisted username: got %q", reopened.adminUser)
		}
		var reopenedHash string
		if err := reopened.db.QueryRow(`SELECT password_hash FROM administrators WHERE id=1`).Scan(&reopenedHash); err != nil {
			t.Fatal(err)
		}
		if reopenedHash != passwordHash {
			t.Fatal("environment overwrote persisted administrator password hash")
		}
		reopened.close()
	}

	if _, err := newApp(filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "backups"), "", ""); err == nil || !strings.Contains(err.Error(), "ORCA_ADMIN_USER and ORCA_ADMIN_PASSWORD") {
		t.Fatalf("empty env on a new installation should fail clearly, got %v", err)
	}
}

func TestPasswordChangeInvalidatesSessionsAndPersists(t *testing.T) {
	base := t.TempDir()
	app := testApp(t, base, testAdminUser, testInitialPassword)
	h := app.handler()
	cookie1 := loginForTest(t, h, testAdminUser, testInitialPassword)
	cookie2 := loginForTest(t, h, testAdminUser, testInitialPassword)

	wrong := httptest.NewRecorder()
	h.ServeHTTP(wrong, requestWithOrigin(http.MethodPost, "/api/auth/password", `{"currentPassword":"wrong-current-password","newPassword":"test-new-password-123"}`, cookie1))
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong current password status=%d body=%s", wrong.Code, wrong.Body.String())
	}
	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, requestWithOrigin(http.MethodPost, "/api/auth/password", `{"currentPassword":"x","newPassword":"test-new-password-123"}`, nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated password change status=%d", unauth.Code)
	}
	invalid := httptest.NewRecorder()
	h.ServeHTTP(invalid, requestWithOrigin(http.MethodPost, "/api/auth/password", `{"currentPassword":"`+testInitialPassword+`","newPassword":"short"}`, cookie1))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid new password status=%d", invalid.Code)
	}

	newPassword := "test-new-password-123"
	changed := httptest.NewRecorder()
	h.ServeHTTP(changed, requestWithOrigin(http.MethodPost, "/api/auth/password", `{"currentPassword":"`+testInitialPassword+`","newPassword":"`+newPassword+`"}`, cookie1))
	if changed.Code != http.StatusOK || !strings.Contains(changed.Body.String(), `"message"`) {
		t.Fatalf("change status=%d body=%s", changed.Code, changed.Body.String())
	}
	if cookies := changed.Result().Cookies(); len(cookies) != 1 || cookies[0].MaxAge >= 0 || cookies[0].Value != "" {
		t.Fatalf("password change did not expire session cookie: %#v", cookies)
	}
	var sessions int
	if err := app.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("session count=%d err=%v, want 0", sessions, err)
	}
	for _, cookie := range []*http.Cookie{cookie1, cookie2} {
		me := httptest.NewRecorder()
		h.ServeHTTP(me, requestWithOrigin(http.MethodGet, "/api/auth/me", "", cookie))
		if me.Code != http.StatusUnauthorized {
			t.Errorf("old session remained valid: status=%d body=%s", me.Code, me.Body.String())
		}
	}

	app.close()
	app = testApp(t, base, testAdminUser, testInitialPassword) // Old env does not restore the old password.
	defer app.close()
	h = app.handler()
	oldLogin := httptest.NewRecorder()
	h.ServeHTTP(oldLogin, requestWithOrigin(http.MethodPost, "/api/auth/login", `{"username":"`+testAdminUser+`","password":"`+testInitialPassword+`"}`, nil))
	if oldLogin.Code != http.StatusUnauthorized {
		t.Fatalf("old password login status=%d", oldLogin.Code)
	}
	_ = loginForTest(t, h, testAdminUser, newPassword)
	var hash string
	if err := app.db.QueryRow(`SELECT password_hash FROM administrators WHERE id=1`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(newPassword)); err != nil {
		t.Fatal("new password did not persist as bcrypt hash:", err)
	}
	if err := app.db.QueryRow(`SELECT COUNT(*) FROM administrators`).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatalf("administrator rows=%d err=%v, want singleton row", sessions, err)
	}
}

func TestPasswordChangeRequiresOrigin(t *testing.T) {
	app := testApp(t, t.TempDir(), testAdminUser, testInitialPassword)
	defer app.close()
	cookie := loginForTest(t, app.handler(), testAdminUser, testInitialPassword)
	req := httptest.NewRequest(http.MethodPost, "http://example.test/api/auth/password", strings.NewReader(`{"currentPassword":"`+testInitialPassword+`","newPassword":"test-new-password-123"}`))
	req.Header.Set("Origin", "http://attacker.test")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad Origin status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestInitializeAdministratorReadsExistingSchema(t *testing.T) {
	// Simulate a deployed database from before administrators were added.
	base := t.TempDir()
	dataDir := filepath.Join(base, "state")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	legacyDB, err := sql.Open("sqlite", filepath.Join(dataDir, "orca.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacyDB.Exec(`CREATE TABLE sessions (token_hash TEXT PRIMARY KEY, username TEXT NOT NULL, expires_at INTEGER NOT NULL)`); err != nil {
		legacyDB.Close()
		t.Fatal(err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatal(err)
	}

	app := testApp(t, base, testAdminUser, testInitialPassword)
	defer app.close()
	var count int
	if err := app.db.QueryRow(`SELECT COUNT(*) FROM administrators`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("administrator count=%d, want one", count)
	}
	var username, hash string
	if err := app.db.QueryRow(`SELECT username, password_hash FROM administrators WHERE id=1`).Scan(&username, &hash); err != nil {
		t.Fatal(err)
	}
	if username != testAdminUser || bcrypt.CompareHashAndPassword([]byte(hash), []byte(testInitialPassword)) != nil {
		t.Fatal("legacy database did not import configured administrator credentials")
	}
}
