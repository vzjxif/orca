package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func installDockerFixture(t *testing.T, path, jsonRows, ids, config string) string {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = compose ]; then printf '%%s' '%s'; exit 0; fi\nif [ \"$1\" = ps ]; then\n  for arg in \"$@\"; do if [ \"$arg\" = --filter ]; then printf '%%s\\n' '%s'; exit 0; fi; done\n  printf '%%s\\n' '%s'; exit 0\nfi\nif [ \"$1\" = inspect ]; then printf '%%s' '%s'; exit 0; fi\nexit 1\n", config, ids, ids, jsonRows)
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScanUsesExistingComposeIdentityAndCachesDetectedStatus(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "roots")
	projectPath := filepath.Join(root, "site")
	if err := os.MkdirAll(projectPath, 0700); err != nil {
		t.Fatal(err)
	}
	composeFile := filepath.Join(projectPath, "compose.yaml")
	if err := os.WriteFile(composeFile, []byte("name: configured-name\nservices: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	labels := fmt.Sprintf(`"com.docker.compose.project":"legacy-project","com.docker.compose.project.working_dir":%q,"com.docker.compose.project.config_files":%q`, projectPath, composeFile+","+filepath.Join(projectPath, "override.yaml"))
	rows := fmt.Sprintf(`[{"Id":"abc123","Name":"/site-web-1","Config":{"Image":"nginx","Labels":{%s}},"State":{"Status":"running"}}]`, labels)
	docker := installDockerFixture(t, filepath.Join(base, "docker"), rows, "abc123", `{"name":"configured-name","services":{"web":{}}}`)
	app, err := newApp(filepath.Join(base, "state"), filepath.Join(base, "backups"), "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer app.close()
	app.docker = docker
	app.roots = []string{root}
	projects := app.scan(context.Background())
	if len(projects) != 1 {
		t.Fatalf("projects=%#v", projects)
	}
	p := projects[0]
	if p.RuntimeName != "legacy-project" || p.Status != "running" {
		t.Fatalf("scan did not adopt existing label identity: %#v", p)
	}
	cached, ok := app.getProject(p.ID)
	if !ok || cached.Status != "running" || cached.RuntimeName != "legacy-project" {
		t.Fatalf("detected identity/status not cached: %#v", cached)
	}
	cs, err := app.containers(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].ID != "abc123" {
		t.Fatalf("containers=%#v", cs)
	}
}

func TestScanMarksMultipleMatchingComposeIdentitiesAsError(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "roots")
	projectPath := filepath.Join(root, "same")
	if err := os.MkdirAll(projectPath, 0700); err != nil {
		t.Fatal(err)
	}
	composeFile := filepath.Join(projectPath, "compose.yaml")
	if err := os.WriteFile(composeFile, []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	labels := func(name string) string {
		return fmt.Sprintf(`"com.docker.compose.project":%q,"com.docker.compose.project.working_dir":%q,"com.docker.compose.project.config_files":%q`, name, projectPath, composeFile)
	}
	rows := fmt.Sprintf(`[{"Id":"a","Name":"/a","Config":{"Labels":{%s}},"State":{"Status":"running"}},{"Id":"b","Name":"/b","Config":{"Labels":{%s}},"State":{"Status":"running"}}]`, labels("first"), labels("second"))
	app, err := newApp(filepath.Join(base, "state"), filepath.Join(base, "backups"), "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer app.close()
	app.docker = installDockerFixture(t, filepath.Join(base, "docker"), rows, "a b", `{"services":{"web":{}}}`)
	app.roots = []string{root}
	projects := app.scan(context.Background())
	if len(projects) != 1 {
		t.Fatalf("projects=%#v", projects)
	}
	if projects[0].Status != "error" || projects[0].Error == "" {
		t.Fatalf("ambiguous identity was not rejected: %#v", projects[0])
	}
}
