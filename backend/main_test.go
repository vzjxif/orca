package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposeCandidatePriorityAndRegularFilesOnly(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("services: {}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := filepath.Base(composeCandidate(dir)), "compose.yaml"; got != want {
		t.Fatalf("candidate = %q, want %q", got, want)
	}
	if err := os.Remove(filepath.Join(dir, "compose.yaml")); err != nil {
		t.Fatal(err)
	}
	if got, want := filepath.Base(composeCandidate(dir)), "compose.yml"; got != want {
		t.Fatalf("fallback candidate = %q, want %q", got, want)
	}
	if err := os.Remove(filepath.Join(dir, "compose.yml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "docker-compose.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "docker-compose.yml")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(target, []byte("services: {}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "compose.yaml")); err != nil {
		t.Fatal(err)
	}
	if got := composeCandidate(dir); got != "" {
		t.Fatalf("symlink compose file accepted: %q", got)
	}
}

func TestPathContainmentAndProjectRuntimeIdentity(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "srv", "apps")
	if !within(root, filepath.Join(root, "one")) {
		t.Fatal("child path should be within root")
	}
	if within(root, filepath.Join(root+"-evil", "app")) || within(root, filepath.Join(root, "..", "elsewhere")) {
		t.Fatal("escaped path considered within root")
	}
	p1, p2 := filepath.Join(root, "one"), filepath.Join(root, "two")
	if projectRuntimeName(p1) == projectRuntimeName(p2) {
		t.Fatal("different directories must have isolated compose runtime names")
	}
	if projectRuntimeName(p1) != projectRuntimeName(p1) {
		t.Fatal("runtime name must be stable")
	}
}

func TestBackupContainsOnlyAllowedFilesAndManifest(t *testing.T) {
	base := t.TempDir()
	projectDir := filepath.Join(base, "sample")
	if err := os.MkdirAll(filepath.Join(projectDir, "config", "data"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "compose.yaml"), []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".env"), []byte("TOKEN=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "config", "app.yml"), []byte("safe: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "config", "data", "database.db"), []byte("private runtime data"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside-secret")
	if err := os.WriteFile(outside, []byte("symlink target"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(projectDir, "config", "linked-secret")); err != nil {
		t.Fatal(err)
	}
	app, err := newApp(filepath.Join(base, "state"), filepath.Join(base, "backups"), "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer app.close()
	p := Project{ID: "p1", Name: "sample", Path: projectDir, ComposeFile: filepath.Join(projectDir, "compose.yaml")}
	b, err := app.makeBackup(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(b.File)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	names := map[string][]byte{}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		data, e := io.ReadAll(tr)
		if e != nil {
			t.Fatal(e)
		}
		names[h.Name] = data
	}
	for _, name := range []string{"compose.yaml", ".env", "config/app.yml", "MANIFEST.json"} {
		if _, ok := names[name]; !ok {
			t.Errorf("archive missing %q", name)
		}
	}
	for _, name := range []string{"config/data/database.db", "config/linked-secret", "outside-secret"} {
		if _, ok := names[name]; ok {
			t.Errorf("archive must exclude %q", name)
		}
	}
	var manifest struct {
		Files map[string]string `json:"files"`
	}
	if err := json.Unmarshal(names["MANIFEST.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 3 || manifest.Files["config/app.yml"] == "" {
		t.Fatalf("unexpected manifest: %s", names["MANIFEST.json"])
	}
	response := httptest.NewRecorder()
	app.backupsHandler(response, httptest.NewRequest(http.MethodGet, "/api/backups", nil))
	body := response.Body.String()
	if strings.Contains(body, `"file"`) || strings.Contains(body, b.File) || strings.Contains(body, base) {
		t.Fatalf("backup listing exposed a host path: %s", body)
	}
}

func TestAuthOriginAndSettingsAPI(t *testing.T) {
	t.Setenv("ORCA_PROJECTS_PATH", "")
	base := t.TempDir()
	root := filepath.Join(base, "projects")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	app, err := newApp(filepath.Join(base, "state"), filepath.Join(base, "backups"), "admin", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	defer app.close()
	h := app.handler()
	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/auth/me", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", unauth.Code)
	}
	loginReq := httptest.NewRequest(http.MethodPost, "http://example.test/api/auth/login", strings.NewReader(`{"username":"admin","password":"correct horse"}`))
	loginReq.Header.Set("Origin", "http://example.test")
	login := httptest.NewRecorder()
	h.ServeHTTP(login, loginReq)
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d, body=%s", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) == 0 || !cookies[0].HttpOnly {
		t.Fatal("login did not issue an HttpOnly cookie")
	}
	putReq := httptest.NewRequest(http.MethodPut, "http://example.test/api/settings", strings.NewReader(`{"roots":["`+root+`"]}`))
	putReq.Header.Set("Origin", "http://example.test")
	putReq.AddCookie(cookies[0])
	put := httptest.NewRecorder()
	h.ServeHTTP(put, putReq)
	if put.Code != http.StatusOK {
		t.Fatalf("settings PUT status = %d, body=%s", put.Code, put.Body.String())
	}
	if !strings.Contains(put.Body.String(), root) {
		t.Fatalf("saved settings missing root: %s", put.Body.String())
	}
	app.projectsMu.Lock()
	app.projects["p1"] = Project{ID: "p1", Name: "demo", Path: root, ComposeFile: filepath.Join(root, "compose.yaml")}
	app.projectsMu.Unlock()
	actionReq := httptest.NewRequest(http.MethodPost, "http://example.test/api/projects/p1/actions", strings.NewReader(`{"action":"restart"}`))
	actionReq.Header.Set("Origin", "http://example.test")
	actionReq.AddCookie(cookies[0])
	action := httptest.NewRecorder()
	h.ServeHTTP(action, actionReq)
	if action.Code != http.StatusAccepted || !strings.Contains(action.Body.String(), `"status":"queued"`) {
		t.Fatalf("action response status=%d body=%s", action.Code, action.Body.String())
	}
	for _, endpoint := range []string{"/api/tasks", "/api/backups"} {
		req := httptest.NewRequest(http.MethodGet, "http://example.test"+endpoint, nil)
		req.AddCookie(cookies[0])
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != http.StatusOK || !strings.HasPrefix(strings.TrimSpace(res.Body.String()), "[") {
			t.Errorf("%s should return direct JSON array, status=%d body=%s", endpoint, res.Code, res.Body.String())
		}
	}
	badOrigin := httptest.NewRequest(http.MethodPut, "http://example.test/api/settings", strings.NewReader(`{"roots":[]}`))
	badOrigin.Header.Set("Origin", "http://attacker.test")
	badOrigin.AddCookie(cookies[0])
	bad := httptest.NewRecorder()
	h.ServeHTTP(bad, badOrigin)
	if bad.Code != http.StatusForbidden {
		t.Fatalf("bad origin status = %d", bad.Code)
	}
}
