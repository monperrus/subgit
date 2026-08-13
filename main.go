// subgit serves selected directories of monorepos as ordinary, read-only Git
// repositories.  The virtual repository contains a filtered copy of the
// source history, so standard Git clients need no special support.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
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
	Listen          string `json:"listen"`
	DataDir         string `json:"data_dir"`
	RefreshInterval string `json:"refresh_interval"`
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
	build  sync.Mutex   // avoids concurrent materialization of the same virtual repo
	status sync.Map
}

type repoStatus struct {
	LastSync time.Time `json:"last_sync"`
	Error    string    `json:"error,omitempty"`
}

func main() {
	configPath := os.Getenv("SUBGIT_CONFIG")
	if configPath == "" {
		configPath = "/etc/subgit/config.json"
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatal(err)
	}
	s := &Server{config: cfg}
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
	if cfg.DataDir == "" {
		cfg.DataDir = "/data"
	}
	if cfg.RefreshInterval == "" {
		cfg.RefreshInterval = "15m"
	}
	if _, err := time.ParseDuration(cfg.RefreshInterval); err != nil {
		return Config{}, fmt.Errorf("invalid refresh_interval: %w", err)
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
	log.Printf("synced %s from %s:%s", r.Name, r.Upstream, r.Path)
	return nil
}

// repositoryForURL translates one public identifier into an internal safe
// cache name. Identifiers always name a public GitHub owner/repository/path.
func repositoryForURL(path string) (Repository, string, string, error) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	gitAt := -1
	for i, part := range parts {
		if strings.HasSuffix(part, ".git") {
			gitAt = i
			break
		}
	}
	if gitAt < 2 {
		return Repository{}, "", "", errors.New("expected /OWNER/REPOSITORY/FOLDER.git")
	}
	identifier := append([]string(nil), parts[:gitAt+1]...)
	identifier[gitAt] = strings.TrimSuffix(identifier[gitAt], ".git")
	for _, part := range identifier {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\?&#%") {
			return Repository{}, "", "", errors.New("invalid GitHub repository identifier")
		}
	}
	key := strings.Join(identifier, "/")
	if len(identifier) < 3 {
		return Repository{}, "", "", errors.New("a folder is required")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))[:20]
	return Repository{Name: digest, Upstream: "https://github.com/" + identifier[0] + "/" + identifier[1] + ".git", Ref: "main", Path: strings.Join(identifier[2:], "/")}, key, strings.Join(parts[gitAt+1:], "/"), nil
}

func (s *Server) ensure(r Repository, id string) error {
	if v, ok := s.status.Load(id); ok {
		status := v.(repoStatus)
		d, _ := time.ParseDuration(s.config.RefreshInterval)
		if status.Error == "" && time.Since(status.LastSync) < d {
			return nil
		}
	}
	s.build.Lock()
	defer s.build.Unlock()
	if err := s.sync(r); err != nil {
		s.status.Store(id, repoStatus{Error: err.Error()})
		return err
	}
	s.status.Store(id, repoStatus{LastSync: time.Now().UTC()})
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
	s.status.Range(func(key, value any) bool { out[key.(string)] = value.(repoStatus); return true })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleGit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	virtual, id, suffix, err := repositoryForURL(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.Contains(r.URL.Path, "git-receive-pack") {
		http.Error(w, "subgit is read-only", http.StatusForbidden)
		return
	}
	if err := s.ensure(virtual, id); err != nil {
		http.Error(w, "materializing virtual repository: "+err.Error(), http.StatusBadGateway)
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	cmd := exec.CommandContext(r.Context(), "git", "http-backend")
	cmd.Stdin = r.Body
	cmd.Env = append(os.Environ(),
		"GIT_PROJECT_ROOT="+filepath.Join(s.config.DataDir, "repositories"), "GIT_HTTP_EXPORT_ALL=1",
		"REQUEST_METHOD="+r.Method, "PATH_INFO=/"+virtual.Name+".git/"+suffix, "QUERY_STRING="+r.URL.RawQuery,
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
