package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsReloadAfterRestart(t *testing.T) {
	t.Setenv("ORCA_PROJECTS_PATH", "")
	base := t.TempDir()
	dataDir := filepath.Join(base, "state")
	backupDir := filepath.Join(base, "backups")
	root := filepath.Join(base, "projects")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	app, err := newApp(dataDir, backupDir, "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(Settings{Roots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO settings(id,roots) VALUES(1,?)`, string(raw)); err != nil {
		app.close()
		t.Fatal(err)
	}
	app.close()

	restarted, err := newApp(dataDir, backupDir, "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.close()
	if len(restarted.roots) != 1 || restarted.roots[0] != root {
		t.Fatalf("settings not restored after restart: %#v", restarted.roots)
	}
}

func TestEnvironmentScanRootOverridesAndLocksStoredSettings(t *testing.T) {
	base := t.TempDir()
	stateDir, backupDir := filepath.Join(base, "state"), filepath.Join(base, "backups")
	storedRoot, envRoot := filepath.Join(base, "stored"), filepath.Join(base, "environment")
	for _, root := range []string{storedRoot, envRoot} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ORCA_PROJECTS_PATH", "")
	initial, err := newApp(stateDir, backupDir, "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(Settings{Roots: []string{storedRoot}})
	if err != nil {
		initial.close()
		t.Fatal(err)
	}
	if _, err = initial.db.Exec(`INSERT INTO settings(id,roots) VALUES(1,?)`, string(raw)); err != nil {
		initial.close()
		t.Fatal(err)
	}
	initial.close()

	t.Setenv("ORCA_PROJECTS_PATH", envRoot)
	managed, err := newApp(stateDir, backupDir, "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if !managed.rootsFromEnv || len(managed.roots) != 1 || managed.roots[0] != envRoot {
		t.Fatalf("environment root did not override stored roots: roots=%#v managed=%v", managed.roots, managed.rootsFromEnv)
	}
	h := managed.handler()
	cookie := loginForTest(t, h, "admin", "pw")
	get := httptest.NewRecorder()
	h.ServeHTTP(get, requestWithOrigin(http.MethodGet, "/api/settings", "", cookie))
	var response Settings
	if get.Code != http.StatusOK || json.Unmarshal(get.Body.Bytes(), &response) != nil || !response.ManagedByEnv || len(response.Roots) != 1 || response.Roots[0] != envRoot {
		t.Fatalf("settings response did not report env-managed root: status=%d body=%s", get.Code, get.Body.String())
	}
	put := httptest.NewRecorder()
	h.ServeHTTP(put, requestWithOrigin(http.MethodPut, "/api/settings", `{"roots":["`+storedRoot+`"]}`, cookie))
	if put.Code != http.StatusConflict {
		t.Fatalf("settings PUT should be read-only when env-managed, got %d: %s", put.Code, put.Body.String())
	}
	managed.close()

	t.Setenv("ORCA_PROJECTS_PATH", "")
	restarted, err := newApp(stateDir, backupDir, "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.close()
	if restarted.rootsFromEnv || len(restarted.roots) != 1 || restarted.roots[0] != envRoot {
		t.Fatalf("environment root was not persisted across restart: roots=%#v managed=%v", restarted.roots, restarted.rootsFromEnv)
	}
}
