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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Listen  string `json:"listen"`
	DataDir string `json:"data_dir"`
	// RefreshInterval is how often known repositories are polled in the
	// background. Correctness never depends on it: every ref advertisement
	// is checked against the upstream head.
	RefreshInterval string `json:"refresh_interval"`
	// RefreshWait bounds how long a Git request waits for a rebuild after
	// upstream moved; past it, the client gets a retryable 503, never stale refs.
	RefreshWait string `json:"refresh_wait"`
	// SyncTimeout bounds one materialization so a hung Git process cannot
	// block refreshes forever.
	SyncTimeout string `json:"sync_timeout"`
}

// upstreamFile records, inside each virtual repository, the upstream commit
// it was materialized from. Freshness is decided by comparing it with
// the live upstream head, not by elapsed time.
const upstreamFile = "subgit-upstream"

var (
	errRefreshing   = errors.New("upstream moved; the virtual repository is being rebuilt, retry shortly")
	errUnverifiable = errors.New("cannot verify freshness against upstream")
)

type Repository struct {
	Name     string `json:"name"`
	Upstream string `json:"upstream"`
	Ref      string `json:"ref"`
	Path     string `json:"path"`
}

type Server struct {
	config Config
	oauth  *OAuth
	mu     sync.RWMutex // excludes git-http-backend while a repo directory is replaced
	builds sync.Map     // id -> *build; at most one materialization per virtual repo
	known  sync.Map     // id -> Repository; polled in the background
	status sync.Map
	// statusMu serializes status read-modify-writes.
	statusMu sync.Mutex
}

type build struct {
	done chan struct{}
	err  error
}

type repoStatus struct {
	LastSync   time.Time `json:"last_sync"` // last successful materialization
	LastCheck  time.Time `json:"last_check,omitempty"`
	Upstream   string    `json:"upstream,omitempty"` // live upstream head at last check
	Served     string    `json:"served,omitempty"`   // upstream commit the cache was built from
	Error      string    `json:"error,omitempty"`
	Refreshing bool      `json:"refreshing,omitempty"`
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
	s := &Server{config: cfg, oauth: newOAuth()}
	go s.poll()
	http.HandleFunc("/status", s.handleStatus)
	http.HandleFunc("/auth/github", s.oauth.begin)
	http.HandleFunc("/auth/github/callback", s.oauth.callback)
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
	for _, d := range []struct {
		name  string
		value *string
		def   string
	}{{"refresh_interval", &cfg.RefreshInterval, "1m"}, {"refresh_wait", &cfg.RefreshWait, "30s"}, {"sync_timeout", &cfg.SyncTimeout, "30m"}} {
		if *d.value == "" {
			*d.value = d.def
		}
		if v, err := time.ParseDuration(*d.value); err != nil || v <= 0 {
			return Config{}, fmt.Errorf("invalid %s: %q", d.name, *d.value)
		}
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

func (s *Server) sync(ctx context.Context, r Repository) error {
	root := filepath.Join(s.config.DataDir, "repositories")
	mirror := filepath.Join(root, r.Name+".source.git")
	target := filepath.Join(root, r.Name+".git")
	if err := os.MkdirAll(root, 0755); err != nil {
		return err
	}
	if _, err := os.Stat(mirror); os.IsNotExist(err) {
		if err := run(ctx, "git", "clone", "--mirror", r.Upstream, mirror); err != nil {
			return fmt.Errorf("clone source: %w", err)
		}
	} else if err != nil {
		return err
	} else if err := run(ctx, "git", "-C", mirror, "remote", "update", "--prune"); err != nil {
		return fmt.Errorf("fetch source: %w", err)
	}
	source, err := runOutput(ctx, "git", "-C", mirror, "rev-parse", "refs/heads/"+r.Ref)
	if err != nil {
		return fmt.Errorf("read source head: %w", err)
	}

	// Build away from the live repository. --no-local prevents filter-repo from
	// mutating objects in the source mirror through hard links.
	tmp, err := os.MkdirTemp(root, r.Name+".next-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := run(ctx, "git", "clone", "--mirror", "--no-local", mirror, tmp); err != nil {
		return fmt.Errorf("copy source: %w", err)
	}
	prefix := strings.Trim(r.Path, "/") + "/"
	if err := run(ctx, "git", "-C", tmp, "filter-repo", "--force", "--refs", "refs/heads/"+r.Ref, "--path", prefix, "--path-rename", prefix+":"); err != nil {
		return fmt.Errorf("filter history: %w", err)
	}
	if err := keepOnlyRef(ctx, tmp, "refs/heads/"+r.Ref); err != nil {
		return fmt.Errorf("prune virtual refs: %w", err)
	}
	if err := run(ctx, "git", "-C", tmp, "config", "http.receivepack", "true"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, upstreamFile), source, 0644); err != nil {
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
	log.Printf("synced %s from %s:%s at %s", r.Name, r.Upstream, r.Path, strings.TrimSpace(string(source)))
	return nil
}

func keepOnlyRef(ctx context.Context, repository, keep string) error {
	refs, err := runOutput(ctx, "git", "-C", repository, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return fmt.Errorf("list refs: %w", err)
	}
	for _, ref := range strings.Fields(string(refs)) {
		if ref == keep {
			continue
		}
		if err := run(ctx, "git", "-C", repository, "update-ref", "-d", ref); err != nil {
			return fmt.Errorf("delete %s: %w", ref, err)
		}
	}
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

// ensure guarantees that the virtual repository reflects the current upstream
// head before its refs are advertised. It never serves a cache that is known
// or suspected to be stale: if upstream cannot be checked, or a rebuild does
// not finish within refresh_wait, the request fails and the client retries.
func (s *Server) ensure(ctx context.Context, r Repository, id string) error {
	s.known.Store(id, r)
	wait, _ := time.ParseDuration(s.config.RefreshWait)
	head, err := s.upstreamHead(ctx, r)
	if err != nil {
		s.updateStatus(id, func(st *repoStatus) { st.LastCheck, st.Error = time.Now().UTC(), err.Error() })
		return fmt.Errorf("%w: %v", errUnverifiable, err)
	}
	served := s.served(r)
	s.updateStatus(id, func(st *repoStatus) {
		st.LastCheck, st.Upstream, st.Served = time.Now().UTC(), head, served
	})
	if served == head {
		return nil
	}
	b := s.startBuild(r, id)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-b.done:
		return b.err
	case <-timer.C:
		return errRefreshing
	case <-ctx.Done():
		return ctx.Err()
	}
}

// startBuild returns the in-flight materialization of id, starting one if
// none runs. The build outlives the request that started it.
func (s *Server) startBuild(r Repository, id string) *build {
	b := &build{done: make(chan struct{})}
	if v, loaded := s.builds.LoadOrStore(id, b); loaded {
		return v.(*build)
	}
	s.updateStatus(id, func(st *repoStatus) { st.Refreshing = true })
	go func() {
		timeout, _ := time.ParseDuration(s.config.SyncTimeout)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		b.err = s.sync(ctx, r)
		s.updateStatus(id, func(st *repoStatus) {
			st.Refreshing, st.Served = false, s.served(r)
			if b.err != nil {
				st.Error = b.err.Error()
				return
			}
			st.LastSync, st.Error = time.Now().UTC(), ""
		})
		s.builds.Delete(id)
		close(b.done)
	}()
	return b
}

// poll keeps known repositories warm so that Git requests rarely wait for a
// rebuild. It is an optimization only; ensure checks upstream on every fetch.
func (s *Server) poll() {
	interval, _ := time.ParseDuration(s.config.RefreshInterval)
	for range time.Tick(interval) {
		s.known.Range(func(key, value any) bool {
			ctx, cancel := context.WithTimeout(context.Background(), interval)
			defer cancel()
			if err := s.ensure(ctx, value.(Repository), key.(string)); err != nil && !errors.Is(err, errRefreshing) && !errors.Is(err, context.DeadlineExceeded) {
				log.Printf("poll %s: %v", key, err)
			}
			return true
		})
	}
}

func (s *Server) upstreamHead(ctx context.Context, r Repository) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ref := "refs/heads/" + r.Ref
	out, err := runOutput(ctx, "git", "ls-remote", r.Upstream, ref)
	if err != nil {
		return "", fmt.Errorf("git ls-remote %s: %w", r.Upstream, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[1] == ref {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("%s has no %s", r.Upstream, ref)
}

// served returns the upstream commit the current cache was built from, or ""
// for a missing cache or one built before subgit recorded it.
func (s *Server) served(r Repository) string {
	b, err := os.ReadFile(filepath.Join(s.config.DataDir, "repositories", r.Name+".git", upstreamFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (s *Server) updateStatus(id string, f func(*repoStatus)) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	var st repoStatus
	if v, ok := s.status.Load(id); ok {
		st = v.(repoStatus)
	}
	f(&st)
	s.status.Store(id, st)
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

func runOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	b, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (s *Server) writeThrough(ctx context.Context, r Repository, token string) error {
	root := filepath.Join(s.config.DataDir, "repositories")
	virtual := filepath.Join(root, r.Name+".git")
	head, err := runOutput(ctx, "git", "-C", virtual, "rev-parse", "refs/heads/"+r.Ref)
	if err != nil {
		return fmt.Errorf("read pushed head: %w", err)
	}
	work, err := os.MkdirTemp(root, "write-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	// The token is passed only to Git's HTTPS credential parser, never logged.
	upstream := r.Upstream
	if strings.HasPrefix(upstream, "https://github.com/") {
		upstream = "https://x-access-token:" + url.QueryEscape(token) + "@github.com/" + strings.TrimPrefix(strings.TrimSuffix(r.Upstream, ".git"), "https://github.com/") + ".git"
	}
	if err := run(ctx, "git", "clone", "--depth=1", "--filter=blob:none", "--no-checkout", "--branch", r.Ref, upstream, work); err != nil {
		return fmt.Errorf("clone upstream for write: %w", err)
	}
	if err := run(ctx, "git", "-C", work, "sparse-checkout", "set", "--no-cone", r.Path); err != nil {
		return fmt.Errorf("select upstream directory for write: %w", err)
	}
	if err := run(ctx, "git", "-C", work, "checkout"); err != nil {
		return fmt.Errorf("checkout upstream directory for write: %w", err)
	}
	destination := filepath.Join(work, filepath.FromSlash(r.Path))
	if err := os.RemoveAll(destination); err != nil {
		return err
	}
	if err := os.MkdirAll(destination, 0755); err != nil {
		return err
	}
	archive := exec.CommandContext(ctx, "git", "-C", virtual, "archive", strings.TrimSpace(string(head)))
	tar := exec.CommandContext(ctx, "tar", "-x", "-C", destination)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		return err
	}
	tar.Stdin = pipe
	if err := tar.Start(); err != nil {
		return err
	}
	if err := archive.Run(); err != nil {
		return err
	}
	if err := tar.Wait(); err != nil {
		return err
	}
	if err := run(ctx, "git", "-C", work, "add", "--", r.Path); err != nil {
		return err
	}
	raw, err := runOutput(ctx, "git", "-C", virtual, "cat-file", "commit", strings.TrimSpace(string(head)))
	if err != nil {
		return fmt.Errorf("read pushed commit message: %w", err)
	}
	_, message, _ := bytes.Cut(raw, []byte("\n\n"))
	identity, err := runOutput(ctx, "git", "-C", virtual, "log", "-1", "--format=%an%x00%ae%x00%ad%x00%cn%x00%ce%x00%cd", "--date=raw", strings.TrimSpace(string(head)))
	if err != nil {
		return fmt.Errorf("read pushed commit identity: %w", err)
	}
	id := strings.Split(strings.TrimSpace(string(identity)), "\x00")
	if len(id) != 6 {
		return fmt.Errorf("read pushed commit identity: unexpected %q", identity)
	}
	messageFile := filepath.Join(work, ".subgit-message")
	if err := os.WriteFile(messageFile, message, 0600); err != nil {
		return err
	}
	// Reuse the pushed commit's author and committer verbatim so that the next
	// materialization of the upstream commit reproduces the pushed commit hash
	// instead of rewriting the pusher's history.
	commit := exec.CommandContext(ctx, "git", "-C", work, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "--cleanup=verbatim", "-F", messageFile)
	commit.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME="+id[0], "GIT_AUTHOR_EMAIL="+id[1], "GIT_AUTHOR_DATE="+id[2],
		"GIT_COMMITTER_NAME="+id[3], "GIT_COMMITTER_EMAIL="+id[4], "GIT_COMMITTER_DATE="+id[5])
	if out, err := commit.CombinedOutput(); err != nil {
		return fmt.Errorf("commit upstream projection: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := run(ctx, "git", "-C", work, "push", "origin", "HEAD:refs/heads/"+r.Ref); err != nil {
		return fmt.Errorf("push upstream projection: %w", err)
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
	isPush := strings.Contains(r.URL.Path, "git-receive-pack")
	var token string
	var oldHead string
	if isPush {
		var ok bool
		token, ok = s.oauth.token(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="subgit GitHub OAuth session"`)
			http.Error(w, "authorize at /auth/github first", http.StatusUnauthorized)
			return
		}
		if previous, err := runOutput(r.Context(), "git", "-C", filepath.Join(s.config.DataDir, "repositories", virtual.Name+".git"), "rev-parse", "refs/heads/"+virtual.Ref); err == nil {
			oldHead = strings.TrimSpace(string(previous))
		}
	}
	// Every clone, fetch and push starts with a ref advertisement; that is where
	// freshness against upstream is enforced.
	if strings.HasSuffix(suffix, "info/refs") {
		if err := s.ensure(r.Context(), virtual, id); err != nil {
			code := http.StatusBadGateway
			if errors.Is(err, errRefreshing) || errors.Is(err, errUnverifiable) {
				code = http.StatusServiceUnavailable
				w.Header().Set("Retry-After", "10")
			}
			http.Error(w, "materializing virtual repository: "+err.Error(), code)
			return
		}
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
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if !isPush {
		if err := streamCGI(w, cmd); err != nil {
			log.Printf("git backend %s: %v: %s", id, err, strings.TrimSpace(stderr.String()))
		}
		return
	}

	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		http.Error(w, "git backend: "+strings.TrimSpace(stderr.String()), http.StatusBadGateway)
		return
	}
	if isPush {
		if err := s.writeThrough(r.Context(), virtual, token); err != nil {
			if oldHead != "" {
				_ = run(context.Background(), "git", "-C", filepath.Join(s.config.DataDir, "repositories", virtual.Name+".git"), "update-ref", "refs/heads/"+virtual.Ref, oldHead)
			}
			log.Printf("write-through %s: %v", id, err)
			http.Error(w, "upstream projection failed; virtual push was rolled back: "+err.Error(), http.StatusConflict)
			return
		}
	}
	writeCGI(w, out.Bytes())
}

// streamCGI forwards a CGI response without holding its body in memory. Git
// generates clone packfiles lazily, so buffering them delays the first byte
// until after a reverse proxy's read timeout.
func streamCGI(w http.ResponseWriter, cmd *exec.Cmd) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open git backend stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start git backend: %w", err)
	}

	reader := bufio.NewReader(stdout)
	headers, err := textproto.NewReader(reader).ReadMIMEHeader()
	if err != nil {
		_ = cmd.Wait()
		return fmt.Errorf("read git backend headers: %w", err)
	}
	writeCGIHeaders(w, headers)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	if _, err := io.Copy(w, reader); err != nil {
		_ = cmd.Wait()
		return fmt.Errorf("stream git backend body: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("wait for git backend: %w", err)
	}
	return nil
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
	writeCGIHeaders(w, h)
	_, _ = io.Copy(w, bytes.NewReader(response[i+len(sep):]))
}

func writeCGIHeaders(w http.ResponseWriter, h textproto.MIMEHeader) {
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
}
