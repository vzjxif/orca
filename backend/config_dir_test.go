package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateConfigDir(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	if err := os.MkdirAll(filepath.Join(project, "settings"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(project, "escape")); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, path string }{
		{name: "parent traversal", path: "../outside"},
		{name: "absolute", path: outside},
		{name: "unclean", path: "settings/../settings"},
		{name: "data segment", path: "settings/data"},
		{name: "symlink escape", path: "escape"},
		{name: "missing", path: "missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateConfigDir(project, tc.path); err == nil {
				t.Fatalf("validateConfigDir(%q) unexpectedly succeeded", tc.path)
			}
		})
	}
	if got, err := validateConfigDir(project, "settings"); err != nil || got != "settings" {
		t.Fatalf("valid relative dir: got %q, err %v", got, err)
	}
}

func TestMakeBackupUsesConfiguredConfigDir(t *testing.T) {
	base := t.TempDir()
	projectDir := filepath.Join(base, "sample")
	for _, dir := range []string{"settings/nested", "settings/data", "config"} {
		if err := os.MkdirAll(filepath.Join(projectDir, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"compose.yaml": "services: {}\n", ".env": "TOKEN=secret\n",
		"settings/nested/app.yml": "safe: true\n", "settings/data/database.db": "private runtime data",
		"config/old.yml": "old config must not be included",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(projectDir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	app, err := newApp(filepath.Join(base, "state"), filepath.Join(base, "backups"), "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer app.close()
	p := Project{ID: "p1", Name: "sample", Path: projectDir, ComposeFile: filepath.Join(projectDir, "compose.yaml"), ConfigDir: "settings"}
	backup, err := app.makeBackup(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(backup.File)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	names := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[h.Name] = true
	}
	for _, name := range []string{"compose.yaml", ".env", "settings/nested/app.yml", "MANIFEST.json"} {
		if !names[name] {
			t.Errorf("archive missing %q", name)
		}
	}
	for _, name := range []string{"config/old.yml", "settings/data/database.db"} {
		if names[name] {
			t.Errorf("archive unexpectedly contains %q", name)
		}
	}
}

func TestConfigDirSettingSurvivesRestartAndScan(t *testing.T) {
	base := t.TempDir()
	dataDir, backupDir := filepath.Join(base, "state"), filepath.Join(base, "backups")
	root, projectDir := filepath.Join(base, "projects"), filepath.Join(base, "projects", "demo")
	if err := os.MkdirAll(filepath.Join(projectDir, "settings"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "compose.yaml"), []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	docker := filepath.Join(base, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nif [ \"$1\" = compose ]; then printf '%s' '{\"services\":{}}'; exit 0; fi\nif [ \"$1\" = ps ]; then exit 0; fi\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	app, err := newApp(dataDir, backupDir, "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	app.docker, app.roots = docker, []string{root}
	projects := app.scan(context.Background())
	if len(projects) != 1 || projects[0].ConfigDir != "config" {
		t.Fatalf("default configDir = %#v", projects)
	}
	if _, err := app.db.Exec(`INSERT INTO project_settings(project_id,project_path,config_dir) VALUES(?,?,?)`, projects[0].ID, projectDir, "settings"); err != nil {
		t.Fatal(err)
	}
	app.close()
	restarted, err := newApp(dataDir, backupDir, "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.close()
	restarted.docker, restarted.roots = docker, []string{root}
	reloaded := restarted.scan(context.Background())
	if len(reloaded) != 1 || reloaded[0].ConfigDir != "settings" {
		t.Fatalf("configDir not restored by scan: %#v", reloaded)
	}
}

func TestConfigDirAPIRouteAuthAndResponse(t *testing.T) {
	base := t.TempDir()
	projectDir := filepath.Join(base, "project")
	if err := os.MkdirAll(filepath.Join(projectDir, "settings"), 0700); err != nil {
		t.Fatal(err)
	}
	app, err := newApp(filepath.Join(base, "state"), filepath.Join(base, "backups"), "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer app.close()
	project := Project{ID: "p1", Name: "demo", Path: projectDir, ConfigDir: "config"}
	app.projects[project.ID] = project
	h := app.handler()
	unauth := httptest.NewRecorder()
	unauthReq := httptest.NewRequest(http.MethodPut, "http://example.test/api/projects/p1/config-dir", strings.NewReader(`{"configDir":"settings"}`))
	unauthReq.Header.Set("Origin", "http://example.test")
	h.ServeHTTP(unauth, unauthReq)
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated PUT status = %d", unauth.Code)
	}
	loginReq := httptest.NewRequest(http.MethodPost, "http://example.test/api/auth/login", strings.NewReader(`{"username":"admin","password":"pw"}`))
	loginReq.Header.Set("Origin", "http://example.test")
	login := httptest.NewRecorder()
	h.ServeHTTP(login, loginReq)
	if login.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	putReq := httptest.NewRequest(http.MethodPut, "http://example.test/api/projects/p1/config-dir", strings.NewReader(`{"configDir":"settings"}`))
	putReq.Header.Set("Origin", "http://example.test")
	putReq.AddCookie(cookie)
	put := httptest.NewRecorder()
	h.ServeHTTP(put, putReq)
	if put.Code != http.StatusOK || !strings.Contains(put.Body.String(), `"configDir":"settings"`) || !strings.Contains(put.Body.String(), `"id":"p1"`) {
		t.Fatalf("configDir response status=%d body=%s", put.Code, put.Body.String())
	}
	getReq := httptest.NewRequest(http.MethodGet, "http://example.test/api/projects/p1", nil)
	getReq.AddCookie(cookie)
	get := httptest.NewRecorder()
	h.ServeHTTP(get, getReq)
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"configDir":"settings"`) {
		t.Fatalf("project GET status=%d body=%s", get.Code, get.Body.String())
	}
}
