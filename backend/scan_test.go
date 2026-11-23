package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestScanUsesImmediateComposeProjectsAndParsesServices(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	project := filepath.Join(root, "alpha")
	if err := os.MkdirAll(filepath.Join(project, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"docker-compose.yml", "compose.yml"} {
		if err := os.WriteFile(filepath.Join(project, file), []byte("services: {}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(project, "nested", "compose.yaml"), []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(base, "linked")
	if err := os.MkdirAll(linked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, "compose.yaml"), []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linked, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	docker := filepath.Join(base, "docker")
	script := "#!/bin/sh\nif [ \"$1\" = compose ]; then printf '%s' '{\"services\":{\"api\":{},\"db\":{}}}'; exit 0; fi\nif [ \"$1\" = ps ]; then exit 0; fi\nexit 1\n"
	if err := os.WriteFile(docker, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	app, err := newApp(filepath.Join(base, "state"), filepath.Join(base, "backups"), "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer app.close()
	app.docker = docker
	app.roots = []string{root}
	projects := app.scan(context.Background())
	if len(projects) != 1 {
		t.Fatalf("found %d projects, want only immediate real child; %#v", len(projects), projects)
	}
	p := projects[0]
	if p.Path != project || filepath.Base(p.ComposeFile) != "compose.yml" {
		t.Fatalf("unexpected project discovery: %#v", p)
	}
	if len(p.Services) != 2 || p.Services[0] != "api" || p.Services[1] != "db" {
		t.Fatalf("services not parsed/sorted: %#v", p.Services)
	}
	if p.Status != "stopped" || p.Error != "" {
		t.Fatalf("unexpected project status: %#v", p)
	}
}
