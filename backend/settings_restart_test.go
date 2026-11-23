package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsReloadAfterRestart(t *testing.T) {
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
