package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

//go:embed web/dist/*
var webFiles embed.FS

const cookieName = "orca_session"
const maxBody = 1 << 20
const maxPasswordBytes = 72

type Settings struct {
	Roots []string `json:"roots"`
}
type Project struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Path        string   `json:"path"`
	ComposeFile string   `json:"composeFile"`
	Status      string   `json:"status"`
	Services    []string `json:"services"`
	Error       string   `json:"error"`
	ConfigDir   string   `json:"configDir"`
	DataDir     string   `json:"dataDir"`
	RuntimeName string   `json:"-"`
}
type Container struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Image  string `json:"image"`
	Health string `json:"-"`
}
type Task struct {
	ID          string     `json:"id"`
	ProjectID   string     `json:"projectId"`
	Action      string     `json:"action"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"createdAt"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
	Error       string     `json:"error"`
	IncludeData bool       `json:"includeData"`
}
type Backup struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"projectId"`
	ProjectName string    `json:"projectName"`
	File        string    `json:"-"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	CreatedAt   time.Time `json:"createdAt"`
	Mode        string    `json:"mode"`
}
type composeConfig struct {
	Name     string                     `json:"name"`
	Services map[string]json.RawMessage `json:"services"`
}
type App struct {
	db                            *sql.DB
	dataDir, backupDir, adminUser string
	authMu                        sync.Mutex
	rootsMu                       sync.RWMutex
	roots                         []string
	projectsMu                    sync.RWMutex
	projects                      map[string]Project
	backupDeleteMu                sync.Mutex
	taskMu                        sync.Mutex
	projectLocks                  map[string]*sync.Mutex
	queue                         chan string
	tasks                         map[string]Task
	rateMu                        sync.Mutex
	rate                          map[string]rateEntry
	docker                        string
	logsSem                       chan struct{}
	now                           func() time.Time
}
type rateEntry struct {
	start time.Time
	count int
}

func ensureColumn(db *sql.DB, table, column, declaration string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if found {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + declaration)
	return err
}

func newApp(dataDir, backupDir, user, password string) (*App, error) {
	if dataDir == "" {
		dataDir = "./data"
	}
	if backupDir == "" {
		backupDir = filepath.Join(dataDir, "backups")
	}
	for _, p := range []string{dataDir, backupDir} {
		if err := os.MkdirAll(p, 0700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "orca.sqlite"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS settings (id INTEGER PRIMARY KEY CHECK(id=1), roots TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS project_settings (project_id TEXT NOT NULL, project_path TEXT NOT NULL, config_dir TEXT NOT NULL DEFAULT 'config', data_dir TEXT NOT NULL DEFAULT 'data', PRIMARY KEY(project_id, project_path));
CREATE TABLE IF NOT EXISTS sessions (token_hash TEXT PRIMARY KEY, username TEXT NOT NULL, expires_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS administrators (id INTEGER PRIMARY KEY CHECK(id=1), username TEXT NOT NULL, password_hash TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS tasks (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, action TEXT NOT NULL, status TEXT NOT NULL, created_at TEXT NOT NULL, started_at TEXT, finished_at TEXT, error TEXT NOT NULL DEFAULT '', include_data INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS backups (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, project_name TEXT NOT NULL, file TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL, created_at TEXT NOT NULL, mode TEXT NOT NULL DEFAULT 'config');`); err != nil {
		db.Close()
		return nil, err
	}
	for _, col := range []struct{ table, name, ddl string }{
		{"project_settings", "data_dir", `TEXT NOT NULL DEFAULT 'data'`},
		{"tasks", "include_data", `INTEGER NOT NULL DEFAULT 0`},
		{"backups", "mode", `TEXT NOT NULL DEFAULT 'config'`},
	} {
		if err := ensureColumn(db, col.table, col.name, col.ddl); err != nil {
			db.Close()
			return nil, err
		}
	}
	adminUser, err := initializeAdministrator(db, user, password)
	if err != nil {
		db.Close()
		return nil, err
	}
	a := &App{db: db, dataDir: dataDir, backupDir: backupDir, adminUser: adminUser, projects: map[string]Project{}, projectLocks: map[string]*sync.Mutex{}, queue: make(chan string, 128), tasks: map[string]Task{}, rate: map[string]rateEntry{}, docker: "docker", logsSem: make(chan struct{}, 32), now: time.Now}
	var raw string
	err = db.QueryRow(`SELECT roots FROM settings WHERE id=1`).Scan(&raw)
	if err == nil {
		var stored Settings
		if err := json.Unmarshal([]byte(raw), &stored); err != nil {
			db.Close()
			return nil, fmt.Errorf("load settings: %w", err)
		}
		a.roots = stored.Roots
	} else if !errors.Is(err, sql.ErrNoRows) {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(`UPDATE tasks SET status='interrupted', error='server restarted while task was running', finished_at=? WHERE status IN ('queued','running')`, a.now().UTC().Format(time.RFC3339Nano)); err != nil {
		db.Close()
		return nil, err
	}
	if err := a.recoverStagedBackupDeletes(); err != nil {
		log.Printf("recover staged backup deletions: %v", err)
	}
	return a, nil
}
func (a *App) close() { _ = a.db.Close() }
func randomID(n int) string {
	b := make([]byte, n)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func pathID(p string) string { h := sha256.Sum256([]byte(p)); return hex.EncodeToString(h[:8]) }
func safePath(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", errors.New("path must be absolute")
	}
	real, e := filepath.EvalSymlinks(p)
	if e != nil {
		return "", e
	}
	st, e := os.Stat(real)
	if e != nil {
		return "", e
	}
	if !st.IsDir() {
		return "", errors.New("path is not a directory")
	}
	return filepath.Clean(real), nil
}
func within(parent, child string) bool {
	rel, e := filepath.Rel(parent, child)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func validateConfigDir(projectPath, configDir string) (string, error) {
	if configDir == "" || filepath.IsAbs(configDir) || filepath.VolumeName(configDir) != "" {
		return "", errors.New("configDir must be a relative directory path")
	}
	clean := filepath.Clean(configDir)
	if clean == "." || clean != configDir || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("configDir must be a clean subdirectory path")
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if strings.EqualFold(part, "data") {
			return "", errors.New("configDir cannot contain a data path segment")
		}
	}
	root, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		return "", fmt.Errorf("resolve project directory: %w", err)
	}
	target, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return "", fmt.Errorf("resolve configDir: %w", err)
	}
	if !within(root, target) || target == root {
		return "", errors.New("configDir resolves outside the project directory")
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("stat configDir: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("configDir is not a directory")
	}
	return clean, nil
}

func (a *App) projectConfigDir(id, projectPath string) string {
	var configDir string
	err := a.db.QueryRow(`SELECT config_dir FROM project_settings WHERE project_id=? AND project_path=?`, id, projectPath).Scan(&configDir)
	if err != nil || configDir == "" {
		return "config"
	}
	return configDir
}
func (a *App) projectDataDir(id, projectPath string) string {
	var dataDir string
	err := a.db.QueryRow(`SELECT data_dir FROM project_settings WHERE project_id=? AND project_path=?`, id, projectPath).Scan(&dataDir)
	if err != nil || dataDir == "" {
		return "data"
	}
	return dataDir
}

func directoryPathsOverlap(a, b string) bool {
	resolve := func(path string) string {
		if real, err := filepath.EvalSymlinks(path); err == nil {
			return real
		}
		abs, _ := filepath.Abs(path)
		return filepath.Clean(abs)
	}
	a, b = resolve(a), resolve(b)
	return within(a, b) || within(b, a)
}

func validateDataDir(projectPath, dataDir, configDir string) (string, error) {
	if dataDir == "" || filepath.IsAbs(dataDir) || filepath.VolumeName(dataDir) != "" {
		return "", errors.New("dataDir must be a relative directory path")
	}
	clean := filepath.Clean(dataDir)
	if clean == "." || clean != dataDir || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("dataDir must be a clean subdirectory path")
	}
	root, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		return "", fmt.Errorf("resolve project directory: %w", err)
	}
	target, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return "", fmt.Errorf("resolve dataDir: %w", err)
	}
	if !within(root, target) || target == root {
		return "", errors.New("dataDir resolves outside the project directory")
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("stat dataDir: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("dataDir is not a directory")
	}
	if directoryPathsOverlap(filepath.Join(root, configDir), target) {
		return "", errors.New("dataDir and configDir must not overlap")
	}
	return clean, nil
}

func composeCandidate(dir string) string {
	for _, n := range []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"} {
		p := filepath.Join(dir, n)
		if st, e := os.Lstat(p); e == nil && st.Mode().IsRegular() {
			return p
		}
	}
	return ""
}
func projectRuntimeName(path string) string {
	base := strings.ToLower(filepath.Base(path))
	var b strings.Builder
	for _, r := range base {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-_")
	if name == "" {
		return "project"
	}
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}
func runtimeName(p Project) string {
	if p.RuntimeName != "" {
		return p.RuntimeName
	}
	return projectRuntimeName(p.Path)
}
func composeFilesMatch(label, composeFile string) bool {
	for _, file := range strings.Split(label, ",") {
		if filepath.Clean(strings.TrimSpace(file)) == composeFile {
			return true
		}
	}
	return false
}

func (a *App) scan(ctx context.Context) []Project {
	a.rootsMu.RLock()
	roots := append([]string(nil), a.roots...)
	a.rootsMu.RUnlock()
	found := make([]Project, 0)
	for _, root := range roots {
		realRoot, e := safePath(root)
		if e != nil {
			continue
		}
		entries, e := os.ReadDir(realRoot)
		if e != nil {
			continue
		}
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			dir := filepath.Join(realRoot, ent.Name())
			real, e := filepath.EvalSymlinks(dir)
			if e != nil || !within(realRoot, real) {
				continue
			}
			file := composeCandidate(real)
			if file == "" {
				continue
			}
			p := Project{ID: pathID(real), Name: filepath.Base(real), Path: real, ComposeFile: file, Status: "unknown", Services: []string{}, ConfigDir: a.projectConfigDir(pathID(real), real), DataDir: a.projectDataDir(pathID(real), real)}
			cfg, err := a.readComposeConfig(ctx, p)
			if err != nil {
				p.Status = "error"
				p.Error = err.Error()
			} else {
				var c composeConfig
				if e = json.Unmarshal(cfg, &c); e != nil {
					p.Status = "error"
					p.Error = "invalid compose config: " + e.Error()
				} else {
					for s := range c.Services {
						p.Services = append(p.Services, s)
					}
					sort.Strings(p.Services)
					p.RuntimeName = strings.TrimSpace(c.Name)
					if p.RuntimeName == "" {
						p.RuntimeName = projectRuntimeName(p.Path)
					}
					p.Status = "stopped"
					actual, cs, e := a.discoverRuntime(ctx, p)
					if e != nil {
						p.Status = "error"
						p.Error = e.Error()
					} else {
						p.RuntimeName = actual
						for _, c := range cs {
							if c.State == "running" {
								p.Status = "running"
								break
							}
						}
					}
				}
			}
			found = append(found, p)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Path < found[j].Path })
	// A shared effective name can make Compose commands address another project.
	owners := map[string]int{}
	for _, p := range found {
		if p.RuntimeName != "" {
			owners[p.RuntimeName]++
		}
	}
	for i := range found {
		if found[i].RuntimeName != "" && owners[found[i].RuntimeName] > 1 {
			found[i].Status = "error"
			found[i].Error = "compose project name conflicts with another scanned directory"
		}
	}
	a.projectsMu.Lock()
	a.projects = make(map[string]Project, len(found))
	for _, p := range found {
		a.projects[p.ID] = p
	}
	a.projectsMu.Unlock()
	return found
}
func (a *App) readComposeConfig(ctx context.Context, p Project) ([]byte, error) {
	cmd := exec.CommandContext(ctx, a.docker, "compose", "-f", p.ComposeFile, "config", "--format", "json")
	cmd.Dir = p.Path
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker compose config: %s", strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}
func (a *App) compose(ctx context.Context, p Project, args ...string) ([]byte, error) {
	base := []string{"compose", "-f", p.ComposeFile, "-p", runtimeName(p)}
	cmd := exec.CommandContext(ctx, a.docker, append(base, args...)...)
	cmd.Dir = p.Path
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker compose %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}
func (a *App) getProject(id string) (Project, bool) {
	a.projectsMu.RLock()
	defer a.projectsMu.RUnlock()
	p, ok := a.projects[id]
	return p, ok
}

type dockerInspect struct {
	ID, Name string
	Config   struct {
		Image  string
		Labels map[string]string
	}
	State struct {
		Status string
		Health *struct{ Status string }
	}
}

func (a *App) inspectIDs(ctx context.Context, ids []string) ([]dockerInspect, error) {
	rows := make([]dockerInspect, 0, len(ids))
	for start := 0; start < len(ids); start += 100 {
		end := start + 100
		if end > len(ids) {
			end = len(ids)
		}
		cmd := exec.CommandContext(ctx, a.docker, append([]string{"inspect"}, ids[start:end]...)...)
		raw, err := cmd.Output()
		if err != nil {
			return nil, err
		}
		var batch []dockerInspect
		if err := json.Unmarshal(raw, &batch); err != nil {
			return nil, err
		}
		rows = append(rows, batch...)
	}
	return rows, nil
}
func associatedWith(labels map[string]string, p Project) bool {
	return filepath.Clean(labels["com.docker.compose.project.working_dir"]) == p.Path && composeFilesMatch(labels["com.docker.compose.project.config_files"], p.ComposeFile)
}
func dockerContainer(r dockerInspect) Container {
	health := ""
	if r.State.Health != nil {
		health = r.State.Health.Status
	}
	return Container{ID: r.ID, Name: strings.TrimPrefix(r.Name, "/"), State: r.State.Status, Image: r.Config.Image, Health: health}
}
func (a *App) discoverRuntime(ctx context.Context, p Project) (string, []Container, error) {
	cmd := exec.CommandContext(ctx, a.docker, "ps", "-aq")
	raw, err := cmd.Output()
	if err != nil {
		return "", nil, fmt.Errorf("could not inspect Docker containers: %w", err)
	}
	rows, err := a.inspectIDs(ctx, strings.Fields(string(raw)))
	if err != nil {
		return "", nil, fmt.Errorf("could not inspect Docker containers: %w", err)
	}
	projects := map[string][]Container{}
	for _, r := range rows {
		if associatedWith(r.Config.Labels, p) {
			name := r.Config.Labels["com.docker.compose.project"]
			if name != "" {
				projects[name] = append(projects[name], dockerContainer(r))
			}
		}
	}
	if len(projects) > 1 {
		return "", nil, errors.New("multiple Docker Compose project identities match this directory; actions are disabled")
	}
	for name, cs := range projects {
		return name, cs, nil
	}
	return runtimeName(p), []Container{}, nil
}
func (a *App) containers(ctx context.Context, p Project) ([]Container, error) {
	cmd := exec.CommandContext(ctx, a.docker, "ps", "-aq", "--filter", "label=com.docker.compose.project="+runtimeName(p))
	raw, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	rows, err := a.inspectIDs(ctx, strings.Fields(string(raw)))
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(rows))
	for _, r := range rows {
		labels := r.Config.Labels
		if labels["com.docker.compose.project"] == runtimeName(p) && associatedWith(labels, p) {
			out = append(out, dockerContainer(r))
		}
	}
	return out, nil
}
func (a *App) startWorkers(n int) {
	for i := 0; i < n; i++ {
		go func() {
			for id := range a.queue {
				a.runTask(id)
			}
		}()
	}
}
func (a *App) createTask(p Project, action string, includeData ...bool) (Task, error) {
	inc := len(includeData) > 0 && includeData[0]
	t := Task{ID: randomID(12), ProjectID: p.ID, Action: action, Status: "queued", CreatedAt: a.now().UTC(), IncludeData: inc}
	_, err := a.db.Exec(`INSERT INTO tasks(id,project_id,action,status,created_at,include_data) VALUES(?,?,?,?,?,?)`, t.ID, t.ProjectID, t.Action, t.Status, t.CreatedAt.Format(time.RFC3339Nano), inc)
	if err != nil {
		return t, err
	}
	a.taskMu.Lock()
	a.tasks[t.ID] = t
	a.taskMu.Unlock()
	select {
	case a.queue <- t.ID:
	default:
		_, _ = a.db.Exec(`UPDATE tasks SET status='failed',error='task queue is full',finished_at=? WHERE id=?`, a.now().UTC().Format(time.RFC3339Nano), t.ID)
		t.Status = "failed"
		t.Error = "task queue is full"
		a.taskMu.Lock()
		a.tasks[t.ID] = t
		a.taskMu.Unlock()
	}
	return t, nil
}
func (a *App) projectLock(id string) *sync.Mutex {
	a.taskMu.Lock()
	defer a.taskMu.Unlock()
	l := a.projectLocks[id]
	if l == nil {
		l = &sync.Mutex{}
		a.projectLocks[id] = l
	}
	return l
}
func (a *App) updateTask(t Task) {
	a.taskMu.Lock()
	a.tasks[t.ID] = t
	a.taskMu.Unlock()
	var start, finish any
	if t.StartedAt != nil {
		start = t.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	if t.FinishedAt != nil {
		finish = t.FinishedAt.UTC().Format(time.RFC3339Nano)
	}
	_, _ = a.db.Exec(`UPDATE tasks SET status=?,started_at=?,finished_at=?,error=?,include_data=? WHERE id=?`, t.Status, start, finish, t.Error, t.IncludeData, t.ID)
}
func (a *App) runTask(id string) {
	a.taskMu.Lock()
	t, ok := a.tasks[id]
	a.taskMu.Unlock()
	if !ok {
		return
	}
	lock := a.projectLock(t.ProjectID)
	lock.Lock()
	defer lock.Unlock()
	now := a.now().UTC()
	t.Status = "running"
	t.StartedAt = &now
	a.updateTask(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	p, ok := a.getProject(t.ProjectID)
	if !ok {
		t.Error = "project no longer exists"
	} else {
		switch t.Action {
		case "start":
			_, e := a.compose(ctx, p, "up", "-d")
			if e != nil {
				t.Error = e.Error()
			}
		case "stop":
			_, e := a.compose(ctx, p, "stop")
			if e != nil {
				t.Error = e.Error()
			}
		case "restart":
			_, e := a.compose(ctx, p, "restart")
			if e != nil {
				t.Error = e.Error()
			}
		case "backup":
			_, e := a.makeBackup(ctx, p, t.IncludeData)
			if e != nil {
				t.Error = e.Error()
			}
		case "backup-data":
			_, e := a.makeBackupMode(ctx, p, "data-only")
			if e != nil {
				t.Error = e.Error()
			}
		case "update":
			t.Error = a.updateProject(ctx, p, t.IncludeData)
		}
	}
	end := a.now().UTC()
	t.FinishedAt = &end
	if t.Error != "" {
		t.Status = "failed"
	} else {
		t.Status = "succeeded"
	}
	a.updateTask(t)
}
func (a *App) updateProject(ctx context.Context, p Project, includeData bool) string {
	if _, e := a.compose(ctx, p, "config", "--format", "json"); e != nil {
		return "compose validation failed: " + e.Error()
	}
	if _, e := a.makeBackup(ctx, p, includeData); e != nil {
		return "backup failed: " + e.Error()
	}
	if _, e := a.compose(ctx, p, "pull"); e != nil {
		return "image pull failed: " + e.Error()
	}
	if _, e := a.compose(ctx, p, "up", "-d", "--remove-orphans"); e != nil {
		return "compose up failed: " + e.Error()
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		cs, e := a.containers(ctx, p)
		if e == nil {
			all := len(cs) > 0
			for _, c := range cs {
				if c.State != "running" || (c.Health != "" && c.Health != "healthy") {
					all = false
				}
			}
			if all {
				return ""
			}
		}
		select {
		case <-ctx.Done():
			return "health wait canceled"
		case <-time.After(3 * time.Second):
		}
	}
	return "containers did not become healthy before timeout"
}

func (a *App) makeBackup(ctx context.Context, p Project, includeData ...bool) (Backup, error) {
	mode := "config"
	if len(includeData) > 0 && includeData[0] {
		mode = "config+data"
	}
	return a.makeBackupMode(ctx, p, mode)
}

func (a *App) makeBackupMode(ctx context.Context, p Project, mode string) (Backup, error) {
	if mode != "config" && mode != "config+data" && mode != "data-only" {
		return Backup{}, errors.New("invalid backup mode")
	}
	id := randomID(12)
	tmp, err := os.CreateTemp(a.backupDir, ".orca-*.tmp")
	if err != nil {
		return Backup{}, err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	hash := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(tmp, hash))
	tw := tar.NewWriter(gz)
	manifest := map[string]string{}
	files := []string{}
	if mode != "data-only" {
		files = append(files, p.ComposeFile)
		env := filepath.Join(p.Path, ".env")
		if st, e := os.Lstat(env); e == nil && st.Mode().IsRegular() {
			files = append(files, env)
		}
	}
	if mode != "data-only" {
		configDir := p.ConfigDir
		if configDir == "" {
			configDir = "config"
		}
		cfg := filepath.Join(p.Path, configDir)
		if st, e := os.Lstat(cfg); e == nil && configDir == "config" && !st.IsDir() {
			return Backup{}, errors.New("configured configDir is not a directory")
		} else if e != nil && !(configDir == "config" && errors.Is(e, os.ErrNotExist)) {
			return Backup{}, fmt.Errorf("configured configDir is unavailable: %w", e)
		}
		if st, e := os.Lstat(cfg); e == nil {
			if !st.IsDir() {
				return Backup{}, errors.New("configured configDir is not a directory")
			}
			if _, e = validateConfigDir(p.Path, configDir); e != nil {
				return Backup{}, fmt.Errorf("invalid configured configDir: %w", e)
			}
			e = filepath.WalkDir(cfg, func(path string, d fs.DirEntry, e error) error {
				if e != nil {
					return e
				}
				if path != cfg && d.IsDir() && strings.EqualFold(d.Name(), "data") {
					return filepath.SkipDir
				}
				if d.Type()&os.ModeSymlink != 0 || d.IsDir() {
					return nil
				}
				info, e := d.Info()
				if e != nil {
					return e
				}
				if info.Mode().IsRegular() {
					files = append(files, path)
				}
				return nil
			})
			if e != nil {
				return Backup{}, e
			}
		}
	}
	if mode == "config+data" || mode == "data-only" {
		dataDir := p.DataDir
		if dataDir == "" {
			dataDir = "data"
		}
		configForValidation := p.ConfigDir
		if configForValidation == "" {
			configForValidation = "config"
		}
		validated, e := validateDataDir(p.Path, dataDir, configForValidation)
		if e != nil {
			return Backup{}, fmt.Errorf("configured dataDir is unavailable: %w", e)
		}
		dataRoot := filepath.Join(p.Path, validated)
		e = filepath.WalkDir(dataRoot, func(path string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			info, e := d.Info()
			if e != nil {
				return e
			}
			if info.Mode().IsRegular() {
				files = append(files, path)
			}
			return nil
		})
		if e != nil {
			return Backup{}, fmt.Errorf("walk dataDir: %w", e)
		}
	}
	seen := map[string]bool{}
	for _, path := range files {
		path = filepath.Clean(path)
		if seen[path] {
			continue
		}
		seen[path] = true
		rel, e := filepath.Rel(p.Path, path)
		if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return Backup{}, errors.New("backup path escaped project")
		}
		info, e := os.Lstat(path)
		if e != nil {
			return Backup{}, e
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		f, e := os.Open(path)
		if e != nil {
			return Backup{}, e
		}
		openedInfo, e := f.Stat()
		if e != nil {
			f.Close()
			return Backup{}, e
		}
		if !openedInfo.Mode().IsRegular() {
			f.Close()
			continue
		}
		size := openedInfo.Size()
		fileHash := sha256.New()
		hdr := &tar.Header{Name: filepath.ToSlash(rel), Mode: int64(openedInfo.Mode().Perm()), Size: size, ModTime: openedInfo.ModTime(), Typeflag: tar.TypeReg}
		if e = tw.WriteHeader(hdr); e == nil {
			_, e = io.CopyN(tw, io.TeeReader(f, fileHash), size)
		}
		closeErr := f.Close()
		if e != nil {
			return Backup{}, fmt.Errorf("archive %s: %w", rel, e)
		}
		if closeErr != nil {
			return Backup{}, closeErr
		}
		manifest[filepath.ToSlash(rel)] = hex.EncodeToString(fileHash.Sum(nil))
	}
	mb, e := json.MarshalIndent(map[string]any{"project": p.Name, "projectPath": p.Path, "createdAt": a.now().UTC(), "mode": mode, "files": manifest}, "", "  ")
	if e != nil {
		return Backup{}, e
	}
	if e = tw.WriteHeader(&tar.Header{Name: "MANIFEST.json", Mode: 0600, Size: int64(len(mb)), Typeflag: tar.TypeReg}); e == nil {
		_, e = tw.Write(mb)
	}
	if e != nil {
		return Backup{}, e
	}
	if e = tw.Close(); e != nil {
		return Backup{}, e
	}
	if e = gz.Close(); e != nil {
		return Backup{}, e
	}
	if e = tmp.Sync(); e != nil {
		return Backup{}, e
	}
	if e = tmp.Close(); e != nil {
		return Backup{}, e
	}
	dest := filepath.Join(a.backupDir, id+".tar.gz")
	if e = os.Rename(tmpName, dest); e != nil {
		return Backup{}, e
	}
	ok = true
	st, e := os.Stat(dest)
	if e != nil {
		return Backup{}, e
	}
	b := Backup{ID: id, ProjectID: p.ID, ProjectName: p.Name, File: dest, Size: st.Size(), SHA256: hex.EncodeToString(hash.Sum(nil)), CreatedAt: a.now().UTC(), Mode: mode}
	_, e = a.db.Exec(`INSERT INTO backups(id,project_id,project_name,file,size,sha256,created_at,mode) VALUES(?,?,?,?,?,?,?,?)`, b.ID, b.ProjectID, b.ProjectName, b.File, b.Size, b.SHA256, b.CreatedAt.Format(time.RFC3339Nano), b.Mode)
	if e != nil {
		_ = os.Remove(dest)
		return Backup{}, e
	}
	return b, nil
}

func jsonReply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func errorReply(w http.ResponseWriter, status int, msg string) {
	jsonReply(w, status, map[string]string{"error": msg})
}
func decodeJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxBody)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func (a *App) originOK(r *http.Request) bool {
	raw := r.Header.Get("Origin")
	if raw == "" {
		raw = r.Header.Get("Referer")
	}
	if raw == "" {
		return false
	}
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return strings.EqualFold(u.Host, r.Host) && strings.EqualFold(u.Scheme, scheme)
}
func remoteIP(r *http.Request) string {
	h, _, e := net.SplitHostPort(r.RemoteAddr)
	if e == nil {
		return h
	}
	return r.RemoteAddr
}
func (a *App) allowRate(key string, limit int, period time.Duration) bool {
	a.rateMu.Lock()
	defer a.rateMu.Unlock()
	now := a.now()
	e := a.rate[key]
	if now.Sub(e.start) >= period {
		e = rateEntry{start: now}
	}
	if e.count >= limit {
		a.rate[key] = e
		return false
	}
	e.count++
	a.rate[key] = e
	return true
}
func (a *App) authenticated(r *http.Request) bool {
	c, e := r.Cookie(cookieName)
	if e != nil {
		return false
	}
	sum := sha256.Sum256([]byte(c.Value))
	var exp int64
	e = a.db.QueryRow(`SELECT expires_at FROM sessions WHERE token_hash=?`, hex.EncodeToString(sum[:])).Scan(&exp)
	if e != nil || exp < a.now().Unix() {
		if e == nil {
			_, _ = a.db.Exec(`DELETE FROM sessions WHERE token_hash=?`, hex.EncodeToString(sum[:]))
		}
		return false
	}
	return true
}
func (a *App) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authenticated(r) {
			errorReply(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, r)
	}
}
func (a *App) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/login", a.login)
	mux.HandleFunc("POST /api/auth/password", a.requireAuth(a.changePassword))
	mux.HandleFunc("POST /api/auth/logout", a.requireAuth(a.logout))
	mux.HandleFunc("GET /api/auth/me", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		jsonReply(w, 200, map[string]any{"user": a.adminUser, "authenticated": true})
	}))
	mux.HandleFunc("GET /api/settings", a.requireAuth(a.getSettings))
	mux.HandleFunc("PUT /api/settings", a.requireAuth(a.putSettings))
	mux.HandleFunc("POST /api/scan", a.requireAuth(a.scanHandler))
	mux.HandleFunc("GET /api/projects", a.requireAuth(a.projectsHandler))
	mux.HandleFunc("GET /api/projects/{id}", a.requireAuth(a.projectHandler))
	mux.HandleFunc("PUT /api/projects/{id}/config-dir", a.requireAuth(a.configDirHandler))
	mux.HandleFunc("PUT /api/projects/{id}/data-dir", a.requireAuth(a.dataDirHandler))
	mux.HandleFunc("GET /api/projects/{id}/containers", a.requireAuth(a.containersHandler))
	mux.HandleFunc("GET /api/projects/{id}/logs", a.requireAuth(a.logsHandler))
	mux.HandleFunc("POST /api/projects/{id}/actions", a.requireAuth(a.actionHandler))
	mux.HandleFunc("GET /api/tasks", a.requireAuth(a.tasksHandler))
	mux.HandleFunc("GET /api/backups", a.requireAuth(a.backupsHandler))
	mux.HandleFunc("GET /api/backups/{id}/download", a.requireAuth(a.downloadBackup))
	mux.HandleFunc("DELETE /api/backups/{id}", a.requireAuth(a.deleteBackup))
	mux.HandleFunc("DELETE /api/backups/batch", a.requireAuth(a.deleteBackups))
	web, _ := fs.Sub(webFiles, "web/dist")
	mux.Handle("/", http.FileServer(http.FS(web)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" && !a.originOK(r) {
			errorReply(w, http.StatusForbidden, "origin check failed")
			return
		}
		if !a.allowRate(remoteIP(r), 240, time.Minute) {
			errorReply(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		mux.ServeHTTP(w, r)
	})
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if !a.allowRate("login:"+remoteIP(r), 8, time.Minute) {
		errorReply(w, 429, "too many login attempts")
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if e := decodeJSON(r, &in); e != nil {
		errorReply(w, 400, "invalid JSON body")
		return
	}
	a.authMu.Lock()
	defer a.authMu.Unlock()
	var username, passwordHash string
	if e := a.db.QueryRow(`SELECT username, password_hash FROM administrators WHERE id=1`).Scan(&username, &passwordHash); e != nil {
		errorReply(w, 503, "administrator credentials are not configured")
		return
	}
	if subtle.ConstantTimeCompare([]byte(in.Username), []byte(username)) != 1 || bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(in.Password)) != nil {
		errorReply(w, 401, "invalid credentials")
		return
	}
	token := randomID(32)
	sum := sha256.Sum256([]byte(token))
	expires := a.now().Add(24 * time.Hour)
	if _, e := a.db.Exec(`INSERT INTO sessions(token_hash,username,expires_at) VALUES(?,?,?)`, hex.EncodeToString(sum[:]), username, expires.Unix()); e != nil {
		errorReply(w, 500, "could not create session")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: 86400})
	jsonReply(w, 200, map[string]any{"user": username, "authenticated": true})
}
func (a *App) changePassword(w http.ResponseWriter, r *http.Request) {
	if !a.allowRate("password:"+remoteIP(r), 8, time.Minute) {
		errorReply(w, http.StatusTooManyRequests, "too many password change attempts")
		return
	}
	var in struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := decodeJSON(r, &in); err != nil {
		errorReply(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if n := len([]byte(in.NewPassword)); n < 12 || n > maxPasswordBytes {
		errorReply(w, http.StatusBadRequest, "new password must be 12 to 72 bytes")
		return
	}
	a.authMu.Lock()
	defer a.authMu.Unlock()
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		errorReply(w, http.StatusUnauthorized, "authentication required")
		return
	}
	tokenHash := sha256.Sum256([]byte(cookie.Value))
	tokenHashHex := hex.EncodeToString(tokenHash[:])
	tx, err := a.db.Begin()
	if err != nil {
		errorReply(w, http.StatusInternalServerError, "could not change password")
		return
	}
	defer tx.Rollback()
	var sessionExpiry int64
	if err = tx.QueryRow(`SELECT expires_at FROM sessions WHERE token_hash=?`, tokenHashHex).Scan(&sessionExpiry); err != nil || sessionExpiry < a.now().Unix() {
		errorReply(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var username, passwordHash string
	if err = tx.QueryRow(`SELECT username, password_hash FROM administrators WHERE id=1`).Scan(&username, &passwordHash); err != nil {
		errorReply(w, http.StatusInternalServerError, "could not load administrator credentials")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(in.CurrentPassword)) != nil {
		errorReply(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(in.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		errorReply(w, http.StatusInternalServerError, "could not hash new password")
		return
	}
	if _, err = tx.Exec(`UPDATE administrators SET password_hash=? WHERE id=1`, string(newHash)); err != nil {
		errorReply(w, http.StatusInternalServerError, "could not save new password")
		return
	}
	if _, err = tx.Exec(`DELETE FROM sessions`); err != nil {
		errorReply(w, http.StatusInternalServerError, "could not invalidate sessions")
		return
	}
	if err = tx.Commit(); err != nil {
		errorReply(w, http.StatusInternalServerError, "could not commit password change")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	jsonReply(w, http.StatusOK, map[string]string{"message": "password changed"})
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie(cookieName); e == nil {
		sum := sha256.Sum256([]byte(c.Value))
		_, _ = a.db.Exec(`DELETE FROM sessions WHERE token_hash=?`, hex.EncodeToString(sum[:]))
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}
func (a *App) getSettings(w http.ResponseWriter, r *http.Request) {
	a.rootsMu.RLock()
	defer a.rootsMu.RUnlock()
	jsonReply(w, 200, Settings{Roots: append([]string{}, a.roots...)})
}
func (a *App) putSettings(w http.ResponseWriter, r *http.Request) {
	var s Settings
	if e := decodeJSON(r, &s); e != nil {
		errorReply(w, 400, "invalid settings JSON")
		return
	}
	if len(s.Roots) > 64 {
		errorReply(w, 400, "at most 64 roots are allowed")
		return
	}
	clean := make([]string, 0, len(s.Roots))
	seen := map[string]bool{}
	for _, root := range s.Roots {
		p, e := safePath(root)
		if e != nil {
			errorReply(w, 400, "each root must be an existing absolute directory")
			return
		}
		if !seen[p] {
			clean = append(clean, p)
			seen[p] = true
		}
	}
	raw, _ := json.Marshal(Settings{Roots: clean})
	if _, e := a.db.Exec(`INSERT INTO settings(id,roots) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET roots=excluded.roots`, string(raw)); e != nil {
		errorReply(w, 500, "could not save settings")
		return
	}
	a.rootsMu.Lock()
	a.roots = clean
	a.rootsMu.Unlock()
	jsonReply(w, 200, Settings{Roots: clean})
}
func (a *App) scanHandler(w http.ResponseWriter, r *http.Request) {
	jsonReply(w, 200, a.scan(r.Context()))
}
func (a *App) projectsHandler(w http.ResponseWriter, r *http.Request) {
	a.projectsMu.RLock()
	list := make([]Project, 0, len(a.projects))
	for _, p := range a.projects {
		list = append(list, p)
	}
	a.projectsMu.RUnlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	jsonReply(w, 200, list)
}
func (a *App) projectHandler(w http.ResponseWriter, r *http.Request) {
	p, ok := a.getProject(r.PathValue("id"))
	if !ok {
		errorReply(w, 404, "project not found; scan first")
		return
	}
	jsonReply(w, 200, p)
}
func (a *App) configDirHandler(w http.ResponseWriter, r *http.Request) {
	p, ok := a.getProject(r.PathValue("id"))
	if !ok {
		errorReply(w, http.StatusNotFound, "project not found; scan first")
		return
	}
	var in struct {
		ConfigDir string `json:"configDir"`
	}
	if err := decodeJSON(r, &in); err != nil {
		errorReply(w, http.StatusBadRequest, "invalid configDir JSON")
		return
	}
	configDir, err := validateConfigDir(p.Path, in.ConfigDir)
	if err != nil {
		errorReply(w, http.StatusBadRequest, err.Error())
		return
	}
	dataDir := p.DataDir
	if dataDir == "" {
		dataDir = "data"
	}
	if directoryPathsOverlap(filepath.Join(p.Path, configDir), filepath.Join(p.Path, dataDir)) {
		errorReply(w, http.StatusBadRequest, "dataDir and configDir must not overlap")
		return
	}
	if _, err = a.db.Exec(`INSERT INTO project_settings(project_id,project_path,config_dir) VALUES(?,?,?) ON CONFLICT(project_id,project_path) DO UPDATE SET config_dir=excluded.config_dir`, p.ID, p.Path, configDir); err != nil {
		errorReply(w, http.StatusInternalServerError, "could not save project configDir")
		return
	}
	a.projectsMu.Lock()
	current, exists := a.projects[p.ID]
	if exists && current.Path == p.Path {
		current.ConfigDir = configDir
		a.projects[p.ID] = current
		p = current
	}
	a.projectsMu.Unlock()
	jsonReply(w, http.StatusOK, p)
}
func (a *App) dataDirHandler(w http.ResponseWriter, r *http.Request) {
	p, ok := a.getProject(r.PathValue("id"))
	if !ok {
		errorReply(w, http.StatusNotFound, "project not found; scan first")
		return
	}
	var in struct {
		DataDir string `json:"dataDir"`
	}
	if err := decodeJSON(r, &in); err != nil {
		errorReply(w, http.StatusBadRequest, "invalid dataDir JSON")
		return
	}
	dataDir, err := validateDataDir(p.Path, in.DataDir, p.ConfigDir)
	if err != nil {
		errorReply(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err = a.db.Exec(`INSERT INTO project_settings(project_id,project_path,config_dir,data_dir) VALUES(?,?,?,?) ON CONFLICT(project_id,project_path) DO UPDATE SET data_dir=excluded.data_dir`, p.ID, p.Path, p.ConfigDir, dataDir); err != nil {
		errorReply(w, http.StatusInternalServerError, "could not save project dataDir")
		return
	}
	a.projectsMu.Lock()
	current, exists := a.projects[p.ID]
	if exists && current.Path == p.Path {
		current.DataDir = dataDir
		a.projects[p.ID] = current
		p = current
	}
	a.projectsMu.Unlock()
	jsonReply(w, http.StatusOK, p)
}

func (a *App) containersHandler(w http.ResponseWriter, r *http.Request) {
	p, ok := a.getProject(r.PathValue("id"))
	if !ok {
		errorReply(w, 404, "project not found")
		return
	}
	if p.Status == "error" || p.Error != "" {
		errorReply(w, http.StatusConflict, "project identity or compose configuration is ambiguous; rescan after resolving it")
		return
	}
	cs, e := a.containers(r.Context(), p)
	if e != nil {
		errorReply(w, 502, "could not query Docker")
		return
	}
	jsonReply(w, 200, cs)
}
func (a *App) actionHandler(w http.ResponseWriter, r *http.Request) {
	p, ok := a.getProject(r.PathValue("id"))
	if !ok {
		errorReply(w, 404, "project not found")
		return
	}
	if p.Status == "error" || p.Error != "" {
		errorReply(w, http.StatusConflict, "project identity or compose configuration is ambiguous; rescan after resolving it")
		return
	}
	var in struct {
		Action      string `json:"action"`
		IncludeData bool   `json:"includeData"`
	}
	if e := decodeJSON(r, &in); e != nil {
		errorReply(w, 400, "invalid action JSON")
		return
	}
	switch in.Action {
	case "start", "stop", "restart", "backup", "backup-data", "update":
	default:
		errorReply(w, 400, "action must be start, stop, restart, backup, backup-data, or update")
		return
	}
	t, e := a.createTask(p, in.Action, in.IncludeData)
	if e != nil {
		errorReply(w, 500, "could not queue task")
		return
	}
	jsonReply(w, http.StatusAccepted, t)
}
func (a *App) tasksHandler(w http.ResponseWriter, r *http.Request) {
	rows, e := a.db.Query(`SELECT id,project_id,action,status,created_at,started_at,finished_at,error,include_data FROM tasks ORDER BY created_at DESC LIMIT 500`)
	if e != nil {
		errorReply(w, 500, "could not read tasks")
		return
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		var t Task
		var c, st, ft sql.NullString
		if e = rows.Scan(&t.ID, &t.ProjectID, &t.Action, &t.Status, &c, &st, &ft, &t.Error, &t.IncludeData); e != nil {
			errorReply(w, 500, "could not read tasks")
			return
		}
		t.CreatedAt, _ = time.Parse(time.RFC3339Nano, c.String)
		if st.Valid {
			x, _ := time.Parse(time.RFC3339Nano, st.String)
			t.StartedAt = &x
		}
		if ft.Valid {
			x, _ := time.Parse(time.RFC3339Nano, ft.String)
			t.FinishedAt = &x
		}
		out = append(out, t)
	}
	jsonReply(w, 200, out)
}
func (a *App) backupsHandler(w http.ResponseWriter, r *http.Request) {
	rows, e := a.db.Query(`SELECT id,project_id,project_name,file,size,sha256,created_at,mode FROM backups ORDER BY created_at DESC`)
	if e != nil {
		errorReply(w, 500, "could not read backups")
		return
	}
	defer rows.Close()
	out := []Backup{}
	for rows.Next() {
		var b Backup
		var c string
		if e = rows.Scan(&b.ID, &b.ProjectID, &b.ProjectName, &b.File, &b.Size, &b.SHA256, &c, &b.Mode); e != nil {
			errorReply(w, 500, "could not read backups")
			return
		}
		b.CreatedAt, _ = time.Parse(time.RFC3339Nano, c)
		b.File = ""
		out = append(out, b)
	}
	jsonReply(w, 200, out)
}
func (a *App) backupByID(id string) (Backup, error) {
	var b Backup
	var c string
	e := a.db.QueryRow(`SELECT id,project_id,project_name,file,size,sha256,created_at,mode FROM backups WHERE id=?`, id).Scan(&b.ID, &b.ProjectID, &b.ProjectName, &b.File, &b.Size, &b.SHA256, &c, &b.Mode)
	b.CreatedAt, _ = time.Parse(time.RFC3339Nano, c)
	if e != nil {
		return b, e
	}
	root, _ := filepath.Abs(a.backupDir)
	file, _ := filepath.Abs(b.File)
	if !within(root, file) {
		return Backup{}, errors.New("invalid backup path")
	}
	return b, nil
}
func (a *App) recoverStagedBackupDeletes() error {
	entries, err := os.ReadDir(a.backupDir)
	if err != nil {
		return err
	}
	const prefix = ".orca-delete-"
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) || !strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), prefix), ".tmp")
		separator := strings.IndexByte(name, '-')
		if separator != 24 {
			continue
		}
		id := name[:separator]
		if _, err := hex.DecodeString(id); err != nil {
			continue
		}
		temporary := filepath.Join(a.backupDir, entry.Name())
		backup, err := a.backupByID(id)
		if errors.Is(err, sql.ErrNoRows) {
			if err := os.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if _, err := os.Lstat(backup.File); errors.Is(err, os.ErrNotExist) {
			if err := os.Rename(temporary, backup.File); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if err := os.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
func (a *App) downloadBackup(w http.ResponseWriter, r *http.Request) {
	b, e := a.backupByID(r.PathValue("id"))
	if e != nil {
		errorReply(w, 404, "backup not found")
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+b.ID+`.tar.gz"`)
	http.ServeFile(w, r, b.File)
}
func (a *App) deleteBackup(w http.ResponseWriter, r *http.Request) {
	a.backupDeleteMu.Lock()
	defer a.backupDeleteMu.Unlock()
	b, e := a.backupByID(r.PathValue("id"))
	if e != nil {
		errorReply(w, 404, "backup not found")
		return
	}
	if e = os.Remove(b.File); e != nil && !errors.Is(e, os.ErrNotExist) {
		errorReply(w, 500, "could not remove backup file")
		return
	}
	if _, e = a.db.Exec(`DELETE FROM backups WHERE id=?`, b.ID); e != nil {
		errorReply(w, 500, "could not remove backup record")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (a *App) deleteBackups(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []string `json:"ids"`
	}
	if err := decodeJSON(r, &in); err != nil {
		errorReply(w, http.StatusBadRequest, "invalid backup id list")
		return
	}
	if len(in.IDs) == 0 || len(in.IDs) > 1000 {
		errorReply(w, http.StatusBadRequest, "select between 1 and 1000 backups")
		return
	}
	a.backupDeleteMu.Lock()
	defer a.backupDeleteMu.Unlock()
	seen := make(map[string]bool, len(in.IDs))
	backups := make([]Backup, 0, len(in.IDs))
	for _, id := range in.IDs {
		if id == "" || seen[id] {
			errorReply(w, http.StatusBadRequest, "backup ids must be non-empty and unique")
			return
		}
		seen[id] = true
		backup, err := a.backupByID(id)
		if errors.Is(err, sql.ErrNoRows) {
			errorReply(w, http.StatusNotFound, "one or more backups were not found")
			return
		}
		if err != nil {
			errorReply(w, http.StatusInternalServerError, "could not validate backup path")
			return
		}
		backups = append(backups, backup)
	}

	staged := make([][2]string, 0, len(backups))
	restoreFiles := func() {
		for i := len(staged) - 1; i >= 0; i-- {
			original, temporary := staged[i][0], staged[i][1]
			if _, err := os.Lstat(original); err == nil {
				log.Printf("bulk backup delete rollback skipped occupied path %q", original)
				continue
			}
			if err := os.Rename(temporary, original); err != nil {
				log.Printf("bulk backup delete rollback failed for %q: %v", original, err)
			}
		}
	}
	for _, backup := range backups {
		info, err := os.Lstat(backup.File)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			restoreFiles()
			errorReply(w, http.StatusInternalServerError, "could not inspect backup file")
			return
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			restoreFiles()
			errorReply(w, http.StatusInternalServerError, "backup path is not a file")
			return
		}
		temporary, err := os.CreateTemp(a.backupDir, ".orca-delete-"+backup.ID+"-*.tmp")
		if err != nil {
			restoreFiles()
			errorReply(w, http.StatusInternalServerError, "could not stage backup files")
			return
		}
		temporaryName := temporary.Name()
		if err = temporary.Close(); err != nil {
			_ = os.Remove(temporaryName)
			restoreFiles()
			errorReply(w, http.StatusInternalServerError, "could not stage backup files")
			return
		}
		if err = os.Rename(backup.File, temporaryName); errors.Is(err, os.ErrNotExist) {
			_ = os.Remove(temporaryName)
			continue
		} else if err != nil {
			_ = os.Remove(temporaryName)
			restoreFiles()
			errorReply(w, http.StatusInternalServerError, "could not stage backup files")
			return
		}
		staged = append(staged, [2]string{backup.File, temporaryName})
	}

	tx, err := a.db.Begin()
	if err != nil {
		restoreFiles()
		errorReply(w, http.StatusInternalServerError, "could not begin backup deletion")
		return
	}
	defer tx.Rollback()
	for _, id := range in.IDs {
		result, err := tx.Exec(`DELETE FROM backups WHERE id=?`, id)
		if err != nil {
			restoreFiles()
			errorReply(w, http.StatusInternalServerError, "could not remove backup records")
			return
		}
		count, err := result.RowsAffected()
		if err != nil || count != 1 {
			restoreFiles()
			errorReply(w, http.StatusConflict, "backup list changed; refresh and try again")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		restoreFiles()
		errorReply(w, http.StatusInternalServerError, "could not commit backup deletion")
		return
	}
	for _, item := range staged {
		if err := os.Remove(item[1]); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("bulk backup delete left temporary file %q: %v", item[1], err)
		}
	}
	jsonReply(w, http.StatusOK, map[string]int{"deleted": len(in.IDs)})
}
func (a *App) logsHandler(w http.ResponseWriter, r *http.Request) {
	p, ok := a.getProject(r.PathValue("id"))
	if !ok {
		errorReply(w, 404, "project not found")
		return
	}
	if p.Status == "error" || p.Error != "" {
		errorReply(w, http.StatusConflict, "project identity or compose configuration is ambiguous; rescan after resolving it")
		return
	}
	container := r.URL.Query().Get("container")
	if container == "" {
		errorReply(w, 400, "container is required")
		return
	}
	known := false
	cs, e := a.containers(r.Context(), p)
	if e != nil {
		errorReply(w, 502, "could not query Docker")
		return
	}
	for _, c := range cs {
		if c.ID == container || c.Name == container {
			container = c.ID
			known = true
			break
		}
	}
	if !known {
		errorReply(w, 404, "container does not belong to project")
		return
	}
	tail := r.URL.Query().Get("tail")
	n := 100
	if tail != "" {
		v, e := strconv.Atoi(tail)
		if e != nil || v < 0 || v > 10000 {
			errorReply(w, 400, "tail must be between 0 and 10000")
			return
		}
		n = v
	}
	select {
	case a.logsSem <- struct{}{}:
		defer func() { <-a.logsSem }()
	default:
		errorReply(w, http.StatusTooManyRequests, "too many concurrent log streams")
		return
	}
	cmd := exec.CommandContext(r.Context(), a.docker, "logs", "--follow", "--timestamps", "--tail", strconv.Itoa(n), container)
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		errorReply(w, 500, "could not open Docker log stream")
		return
	}
	stderr, e := cmd.StderrPipe()
	if e != nil {
		errorReply(w, 500, "could not open Docker log stream")
		return
	}
	if e = cmd.Start(); e != nil {
		errorReply(w, 502, "could not start Docker log stream")
		return
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	flusher.Flush()
	lines := make(chan logLine, 64)
	var wg sync.WaitGroup
	scan := func(rd io.Reader, stream string) {
		defer wg.Done()
		const maxLogLine = 1 << 20
		const marker = "...[truncated]"
		buf := make([]byte, 0, 4096)
		chunk := make([]byte, 4096)
		truncated := false
		emit := func() bool {
			line := strings.TrimSuffix(string(buf), "\r")
			if truncated {
				if len(line) > maxLogLine-len(marker) {
					line = line[:maxLogLine-len(marker)]
				}
				line += marker
			}
			select {
			case lines <- logLine{Line: line, Stream: stream}:
				buf = buf[:0]
				truncated = false
				return true
			case <-r.Context().Done():
				return false
			}
		}
		for {
			nr, er := rd.Read(chunk)
			if nr > 0 {
				for _, b := range chunk[:nr] {
					if b == '\n' {
						if !emit() {
							return
						}
					} else if len(buf) < maxLogLine {
						buf = append(buf, b)
					} else {
						truncated = true
					}
				}
			}
			if er != nil {
				if len(buf) > 0 || truncated {
					_ = emit()
				}
				return
			}
		}
	}
	wg.Add(2)
	go scan(stdout, "stdout")
	go scan(stderr, "stderr")
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-done:
			return
		case ln := <-lines:
			t := a.now().UTC()
			line, when := ln.Line, t // Docker --timestamps prefixes RFC3339Nano; strip and preserve timestamp where valid.
			if stamp, rest, ok := strings.Cut(ln.Line, " "); ok {
				if parsed, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
					when = parsed
					line = rest
				}
			}
			data, _ := json.Marshal(map[string]any{"line": line, "stream": ln.Stream, "time": when})
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}

type logLine struct{ Line, Stream string }

func main() {
	dataDir := os.Getenv("ORCA_DATA_DIR")
	backupDir := os.Getenv("ORCA_BACKUP_DIR")
	a, e := newApp(dataDir, backupDir, os.Getenv("ORCA_ADMIN_USER"), os.Getenv("ORCA_ADMIN_PASSWORD"))
	if e != nil {
		log.Fatal(e)
	}
	defer a.close()
	a.startWorkers(4)
	srv := &http.Server{Addr: ":8080", Handler: a.handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second}
	log.Printf("Orca backend listening on :8080")
	log.Fatal(srv.ListenAndServe())
}
