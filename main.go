// subgit serves selected directories of monorepos as ordinary, read-only Git
// repositories.  The virtual repository contains a filtered copy of the
// source history, so standard Git clients need no special support.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Listen          string       `json:"listen"`
	DataDir         string       `json:"data_dir"`
	RefreshInterval string       `json:"refresh_interval"`
	Repositories    []Repository `json:"repositories"`
}

type Repository struct {
	Name     string `json:"name"`
	Upstream string `json:"upstream"`
	Ref      string `json:"ref"`
	Path     string `json:"path"`
}

type Server struct {
	config Config
	mu     sync.RWMutex // excludes git-http-backend while a repo directory is replaced
	status sync.Map
}

type repoStatus struct {
	LastSync time.Time `json:"last_sync"`
	Error    string    `json:"error,omitempty"`
}

func main() {
	configPath := os.Getenv("SUBGIT_CONFIG")
	if configPath == "" {
		configPath = "/data/config.json"
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatal(err)
	}
	s := &Server{config: cfg}
	for _, repo := range cfg.Repositories {
		if err := validateRepository(repo); err != nil {
			log.Fatal(err)
		}
		if err := s.sync(repo); err != nil {
			log.Printf("initial sync of %s failed: %v", repo.Name, err)
		}
	}
	go s.refreshLoop()
	http.HandleFunc("/status", s.handleStatus)
	http.HandleFunc("/", s.handleGit)
	log.Printf("subgit listening on %s", cfg.Listen)
	log.Fatal(http.ListenAndServe(cfg.Listen, nil))
}

func loadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if dataDir := os.Getenv("SUBGIT_DATA_DIR"); dataDir != "" {
		cfg.DataDir = dataDir
	}
	// Convenient for local testing and single-repository deployments; the
	// checked-in config remains safe to use unchanged in production.
	if upstream := os.Getenv("SUBGIT_UPSTREAM"); upstream != "" && len(cfg.Repositories) == 1 {
		cfg.Repositories[0].Upstream = upstream
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "/data"
	}
	if cfg.RefreshInterval == "" {
		cfg.RefreshInterval = "15m"
	}
	if _, err := time.ParseDuration(cfg.RefreshInterval); err != nil {
		return Config{}, fmt.Errorf("invalid refresh_interval: %w", err)
	}
	if len(cfg.Repositories) == 0 {
		return Config{}, errors.New("config has no repositories")
	}
	return cfg, os.MkdirAll(cfg.DataDir, 0755)
}

func validateRepository(r Repository) error {
	if r.Name == "" || strings.ContainsAny(r.Name, "/\\") || r.Upstream == "" || r.Path == "" {
		return fmt.Errorf("invalid repository configuration: %+v", r)
	}
	if r.Ref == "" {
		return fmt.Errorf("repository %q needs a ref", r.Name)
	}
	return nil
}

func (s *Server) refreshLoop() {
	d, _ := time.ParseDuration(s.config.RefreshInterval)
	for range time.NewTicker(d).C {
		for _, repo := range s.config.Repositories {
			if err := s.sync(repo); err != nil {
				log.Printf("sync %s: %v", repo.Name, err)
			}
		}
	}
}

func (s *Server) sync(r Repository) error {
	root := filepath.Join(s.config.DataDir, "repositories")
	mirror := filepath.Join(root, r.Name+".source.git")
	target := filepath.Join(root, r.Name+".git")
	if err := os.MkdirAll(root, 0755); err != nil {
		return err
	}
	if _, err := os.Stat(mirror); os.IsNotExist(err) {
		if err := run(context.Background(), "git", "clone", "--mirror", r.Upstream, mirror); err != nil {
			return fmt.Errorf("clone source: %w", err)
		}
	} else if err != nil {
		return err
	} else if err := run(context.Background(), "git", "-C", mirror, "remote", "update", "--prune"); err != nil {
		return fmt.Errorf("fetch source: %w", err)
	}

	// Build away from the live repository. --no-local prevents filter-repo from
	// mutating objects in the source mirror through hard links.
	tmp, err := os.MkdirTemp(root, r.Name+".next-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := run(context.Background(), "git", "clone", "--mirror", "--no-local", mirror, tmp); err != nil {
		return fmt.Errorf("copy source: %w", err)
	}
	prefix := strings.Trim(r.Path, "/") + "/"
	if err := run(context.Background(), "git", "-C", tmp, "filter-repo", "--force", "--refs", "refs/heads/"+r.Ref, "--path", prefix, "--path-rename", prefix+":"); err != nil {
		return fmt.Errorf("filter history: %w", err)
	}
	if err := run(context.Background(), "git", "-C", tmp, "config", "http.receivepack", "false"); err != nil {
		return err
	}

	s.mu.Lock()
	old := target + ".previous"
	_ = os.RemoveAll(old)
	if _, err := os.Stat(target); err == nil {
		if err := os.Rename(target, old); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	err = os.Rename(tmp, target)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	_ = os.RemoveAll(old)
	s.status.Store(r.Name, repoStatus{LastSync: time.Now().UTC()})
	log.Printf("synced %s from %s:%s", r.Name, r.Upstream, r.Path)
	return nil
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	out := map[string]repoStatus{}
	for _, r := range s.config.Repositories {
		if v, ok := s.status.Load(r.Name); ok {
			out[r.Name] = v.(repoStatus)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleGit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) < 2 || !strings.HasSuffix(parts[0], ".git") {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimSuffix(parts[0], ".git")
	if !s.hasRepository(name) {
		http.NotFound(w, r)
		return
	}
	if strings.Contains(r.URL.Path, "git-receive-pack") {
		http.Error(w, "subgit is read-only", http.StatusForbidden)
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	cmd := exec.CommandContext(r.Context(), "git", "http-backend")
	cmd.Stdin = r.Body
	cmd.Env = append(os.Environ(),
		"GIT_PROJECT_ROOT="+filepath.Join(s.config.DataDir, "repositories"), "GIT_HTTP_EXPORT_ALL=1",
		"REQUEST_METHOD="+r.Method, "PATH_INFO="+r.URL.Path, "QUERY_STRING="+r.URL.RawQuery,
		"CONTENT_TYPE="+r.Header.Get("Content-Type"), "CONTENT_LENGTH="+r.Header.Get("Content-Length"),
		"REMOTE_ADDR="+r.RemoteAddr, "HTTP_GIT_PROTOCOL="+r.Header.Get("Git-Protocol"))
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		http.Error(w, "git backend: "+strings.TrimSpace(stderr.String()), http.StatusBadGateway)
		return
	}
	writeCGI(w, out.Bytes())
}

func (s *Server) hasRepository(name string) bool {
	for _, r := range s.config.Repositories {
		if r.Name == name {
			return true
		}
	}
	return false
}

func writeCGI(w http.ResponseWriter, response []byte) {
	sep := []byte("\r\n\r\n")
	i := bytes.Index(response, sep)
	if i < 0 {
		sep, i = []byte("\n\n"), bytes.Index(response, []byte("\n\n"))
	}
	if i < 0 {
		http.Error(w, "invalid git backend response", http.StatusBadGateway)
		return
	}
	h, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(response[:i+len(sep)]))).ReadMIMEHeader()
	if err != nil {
		http.Error(w, "invalid git backend headers", http.StatusBadGateway)
		return
	}
	for key, values := range h {
		if strings.EqualFold(key, "Status") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	if status := h.Get("Status"); status != "" {
		var code int
		_, _ = fmt.Sscanf(status, "%d", &code)
		if code > 0 {
			w.WriteHeader(code)
		}
	}
	_, _ = io.Copy(w, bytes.NewReader(response[i+len(sep):]))
}
