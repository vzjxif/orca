package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestBulkDeleteBackupsIsValidatedAndRemovesFilesAndRecords(t *testing.T) {
	base := t.TempDir()
	projectDir := filepath.Join(base, "project")
	if err := os.Mkdir(projectDir, 0700); err != nil {
		t.Fatal(err)
	}
	composeFile := filepath.Join(projectDir, "compose.yaml")
	if err := os.WriteFile(composeFile, []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	app, err := newApp(filepath.Join(base, "state"), filepath.Join(base, "backups"), "admin", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	defer app.close()
	project := Project{ID: "test-project", Name: "test", Path: projectDir, ComposeFile: composeFile}
	first, err := app.makeBackup(t.Context(), project)
	if err != nil {
		t.Fatal(err)
	}
	second, err := app.makeBackup(t.Context(), project)
	if err != nil {
		t.Fatal(err)
	}
	handler := app.handler()
	cookie := loginForTest(t, handler, "admin", "test-password")

	if err := os.Remove(second.File); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`UPDATE backups SET file=? WHERE id=?`, filepath.Join(base, "outside.tar.gz"), second.ID); err != nil {
		t.Fatal(err)
	}
	invalidPath := httptest.NewRecorder()
	handler.ServeHTTP(invalidPath, requestWithOrigin(http.MethodDelete, "/api/backups/batch", `{"ids":["`+first.ID+`","`+second.ID+`"]}`, cookie))
	if invalidPath.Code != http.StatusInternalServerError {
		t.Fatalf("outside backup path status=%d body=%s", invalidPath.Code, invalidPath.Body.String())
	}
	if _, err := os.Stat(first.File); err != nil {
		t.Fatalf("validation failure removed a valid selected backup: %v", err)
	}
	var count int
	if err := app.db.QueryRow(`SELECT count(*) FROM backups`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("validation failure changed backup records: count=%d err=%v", count, err)
	}
	if _, err := app.db.Exec(`UPDATE backups SET file=? WHERE id=?`, second.File, second.ID); err != nil {
		t.Fatal(err)
	}

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, requestWithOrigin(http.MethodDelete, "/api/backups/batch", `{"ids":["`+first.ID+`","missing"]}`, cookie))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing backup status=%d body=%s", missing.Code, missing.Body.String())
	}
	if _, err := os.Stat(first.File); err != nil {
		t.Fatalf("missing id validation removed a valid selected backup: %v", err)
	}

	duplicate := httptest.NewRecorder()
	handler.ServeHTTP(duplicate, requestWithOrigin(http.MethodDelete, "/api/backups/batch", `{"ids":["`+first.ID+`","`+first.ID+`"]}`, cookie))
	if duplicate.Code != http.StatusBadRequest {
		t.Fatalf("duplicate ids status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}

	removed := httptest.NewRecorder()
	handler.ServeHTTP(removed, requestWithOrigin(http.MethodDelete, "/api/backups/batch", `{"ids":["`+first.ID+`","`+second.ID+`"]}`, cookie))
	if removed.Code != http.StatusOK {
		t.Fatalf("batch delete status=%d body=%s", removed.Code, removed.Body.String())
	}
	var result struct {
		Deleted int `json:"deleted"`
	}
	if err := json.Unmarshal(removed.Body.Bytes(), &result); err != nil || result.Deleted != 2 {
		t.Fatalf("unexpected delete result: %#v err=%v", result, err)
	}
	for _, file := range []string{first.File, second.File} {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Errorf("backup file should be removed: file=%s err=%v", file, err)
		}
	}
	if err := app.db.QueryRow(`SELECT count(*) FROM backups`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("backup records were not removed: count=%d err=%v", count, err)
	}
}

func TestRecoverStagedBackupDeletesRestoresOrPurgesFiles(t *testing.T) {
	base := t.TempDir()
	projectDir := filepath.Join(base, "project")
	if err := os.Mkdir(projectDir, 0700); err != nil {
		t.Fatal(err)
	}
	composeFile := filepath.Join(projectDir, "compose.yaml")
	if err := os.WriteFile(composeFile, []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stateDir, backupDir := filepath.Join(base, "state"), filepath.Join(base, "backups")
	app, err := newApp(stateDir, backupDir, "admin", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	backup, err := app.makeBackup(t.Context(), Project{ID: "test-project", Name: "test", Path: projectDir, ComposeFile: composeFile})
	if err != nil {
		app.close()
		t.Fatal(err)
	}
	pending := filepath.Join(backupDir, ".orca-delete-"+backup.ID+"-interrupted.tmp")
	if err := os.Rename(backup.File, pending); err != nil {
		app.close()
		t.Fatal(err)
	}
	app.close()

	restarted, err := newApp(stateDir, backupDir, "admin", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backup.File); err != nil {
		restarted.close()
		t.Fatalf("interrupted deletion should restore backup file: %v", err)
	}
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		restarted.close()
		t.Fatalf("restored backup left a staged file: %v", err)
	}
	pending = filepath.Join(backupDir, ".orca-delete-"+backup.ID+"-committed.tmp")
	if err := os.Rename(backup.File, pending); err != nil {
		restarted.close()
		t.Fatal(err)
	}
	if _, err := restarted.db.Exec(`DELETE FROM backups WHERE id=?`, backup.ID); err != nil {
		restarted.close()
		t.Fatal(err)
	}
	restarted.close()

	final, err := newApp(stateDir, backupDir, "admin", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	defer final.close()
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatalf("committed deletion left a staged file: %v", err)
	}
}
