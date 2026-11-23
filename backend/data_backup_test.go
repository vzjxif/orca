package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func backupEntries(t *testing.T, b Backup) (map[string]string, map[string]any) {
	t.Helper()
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
	files := map[string]string{}
	var manifest map[string]any
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "MANIFEST.json" {
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
		} else {
			files[h.Name] = string(data)
		}
	}
	return files, manifest
}

func TestBackupModesAreStrictAndManifested(t *testing.T) {
	base := t.TempDir()
	projectDir := filepath.Join(base, "project")
	for _, dir := range []string{"settings", "runtime"} {
		if err := os.MkdirAll(filepath.Join(projectDir, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]string{"compose.yaml": "compose", ".env": "secret", "settings/app.yml": "config", "settings/data/skip.db": "excluded", "runtime/db.sqlite": "database"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(projectDir, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(projectDir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	app, err := newApp(filepath.Join(base, "state"), filepath.Join(base, "backups"), "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer app.close()
	p := Project{ID: "p", Name: "project", Path: projectDir, ComposeFile: filepath.Join(projectDir, "compose.yaml"), ConfigDir: "settings", DataDir: "runtime"}
	cases := []struct {
		mode         string
		want, absent []string
	}{
		{"config", []string{"compose.yaml", ".env", "settings/app.yml"}, []string{"runtime/db.sqlite", "settings/data/skip.db"}},
		{"config+data", []string{"compose.yaml", ".env", "settings/app.yml", "runtime/db.sqlite"}, []string{"settings/data/skip.db"}},
		{"data-only", []string{"runtime/db.sqlite"}, []string{"compose.yaml", ".env", "settings/app.yml"}},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			b, err := app.makeBackupMode(context.Background(), p, tc.mode)
			if err != nil {
				t.Fatal(err)
			}
			files, manifest := backupEntries(t, b)
			if b.Mode != tc.mode || manifest["mode"] != tc.mode {
				t.Fatalf("mode absent: backup=%q manifest=%v", b.Mode, manifest["mode"])
			}
			for _, n := range tc.want {
				if _, ok := files[n]; !ok {
					t.Errorf("missing %s", n)
				}
			}
			for _, n := range tc.absent {
				if _, ok := files[n]; ok {
					t.Errorf("unexpected %s", n)
				}
			}
		})
	}
}

func TestDataDirValidationAndPersistence(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "projects")
	projectDir := filepath.Join(root, "demo")
	for _, d := range []string{"config", "data", "runtime"} {
		if err := os.MkdirAll(filepath.Join(projectDir, d), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectDir, "compose.yaml"), []byte("services: {}\\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "outside"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside"), filepath.Join(projectDir, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../outside", filepath.Join(base, "outside"), "escape", "config", "config/nested"} {
		if _, err := validateDataDir(projectDir, path, "config"); err == nil {
			t.Errorf("accepted unsafe/overlapping dataDir %q", path)
		}
	}
	app, err := newApp(filepath.Join(base, "state"), filepath.Join(base, "backups"), "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	app.roots = []string{root}
	app.docker = filepath.Join(base, "docker")
	if err := os.WriteFile(app.docker, []byte("#!/bin/sh\nif [ \"$1\" = compose ]; then printf '%s' '{\"services\":{}}'; exit 0; fi\nif [ \"$1\" = ps ]; then exit 0; fi\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	projects := app.scan(context.Background())
	if len(projects) != 1 {
		t.Fatalf("scan: %#v", projects)
	}
	app.projects[projects[0].ID] = projects[0]
	req := httptest.NewRequest(http.MethodPut, "http://example.test/api/projects/"+projects[0].ID+"/data-dir", strings.NewReader(`{"dataDir":"runtime"}`))
	req.Header.Set("Origin", "http://example.test")
	req.AddCookie(loginCookie(t, app))
	rec := httptest.NewRecorder()
	app.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"dataDir":"runtime"`) {
		t.Fatalf("PUT data-dir: %d %s", rec.Code, rec.Body.String())
	}
	app.close()
	restarted, err := newApp(filepath.Join(base, "state"), filepath.Join(base, "backups"), "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.close()
	restarted.roots = []string{root}
	restarted.docker = app.docker
	reloaded := restarted.scan(context.Background())
	if len(reloaded) != 1 || reloaded[0].DataDir != "runtime" {
		t.Fatalf("dataDir not restored: %#v", reloaded)
	}
}

func loginCookie(t *testing.T, app *App) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://example.test/api/auth/login", strings.NewReader(`{"username":"admin","password":"pw"}`))
	req.Header.Set("Origin", "http://example.test")
	rec := httptest.NewRecorder()
	app.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	return rec.Result().Cookies()[0]
}

func TestSQLiteLegacySchemaMigratesNewColumns(t *testing.T) {
	base := t.TempDir()
	dbPath := filepath.Join(base, "state", "orca.sqlite")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE settings (id INTEGER PRIMARY KEY CHECK(id=1), roots TEXT NOT NULL);
CREATE TABLE project_settings (project_id TEXT NOT NULL, project_path TEXT NOT NULL, config_dir TEXT NOT NULL, PRIMARY KEY(project_id,project_path));
CREATE TABLE sessions (token_hash TEXT PRIMARY KEY, username TEXT NOT NULL, expires_at INTEGER NOT NULL);
CREATE TABLE tasks (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, action TEXT NOT NULL, status TEXT NOT NULL, created_at TEXT NOT NULL, started_at TEXT, finished_at TEXT, error TEXT NOT NULL DEFAULT '');
CREATE TABLE backups (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, project_name TEXT NOT NULL, file TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL, created_at TEXT NOT NULL);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	app, err := newApp(filepath.Join(base, "state"), filepath.Join(base, "backups"), "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer app.close()
	for _, tc := range []struct{ table, column string }{{"project_settings", "data_dir"}, {"tasks", "include_data"}, {"backups", "mode"}} {
		var count int
		if err := app.db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, tc.table, tc.column).Scan(&count); err != nil || count != 1 {
			t.Fatalf("missing migrated %s.%s count=%d err=%v", tc.table, tc.column, count, err)
		}
	}
}
