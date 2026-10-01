package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryForURL(t *testing.T) {
	r, id, suffix, err := repositoryForURL("/monperrus/test-repo-public/.github.git")
	if err != nil {
		t.Fatal(err)
	}
	if r.Path != ".github" || id != "monperrus/test-repo-public/.github" || suffix != "" {
		t.Fatalf("got %#v %q %q", r, id, suffix)
	}
}

func TestStreamCGI(t *testing.T) {
	cmd := exec.Command("sh", "-c", "printf 'Status: 201 Created\\r\\nContent-Type: application/x-test\\r\\n\\r\\npack-data'")
	w := httptest.NewRecorder()
	if err := streamCGI(w, cmd); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusCreated)
	}
	if got := w.Header().Get("Content-Type"); got != "application/x-test" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := w.Body.String(); got != "pack-data" {
		t.Fatalf("body = %q", got)
	}
}

func TestWriteCGIHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	writeCGIHeaders(w, textproto.MIMEHeader{
		"Content-Type": {"application/x-git-upload-pack-advertisement"},
		"Status":       {"200 OK"},
	})
	if got := w.Header().Get("Content-Type"); got != "application/x-git-upload-pack-advertisement" {
		t.Fatalf("Content-Type = %q", got)
	}
}

// upstream creates a local repository with a paper/ directory on main.
func upstream(t *testing.T) string {
	t.Helper()
	if exec.Command("git", "filter-repo", "--version").Run() != nil {
		t.Skip("git filter-repo not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	git(t, dir, "config", "user.name", "Test")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "commit.gpgsign", "false")
	commitFile(t, dir, "paper/paper.tex", "v1\n")
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runOutput(context.Background(), "git", append([]string{"-C", dir}, args...)...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", name)
	git(t, dir, "commit", "-m", "update "+name)
}

func testServer(t *testing.T, wait string) *Server {
	return &Server{config: Config{DataDir: t.TempDir(), RefreshInterval: "1m", RefreshWait: wait, SyncTimeout: "1m"}}
}

func virtualFile(t *testing.T, s *Server, r Repository, name string) string {
	t.Helper()
	return git(t, filepath.Join(s.config.DataDir, "repositories", r.Name+".git"), "show", "refs/heads/main:"+name)
}

func TestEnsureFollowsUpstreamWithoutWaitingForInterval(t *testing.T) {
	up := upstream(t)
	s := testServer(t, "1m")
	r := Repository{Name: "paper", Upstream: up, Ref: "main", Path: "paper"}
	if err := s.ensure(context.Background(), r, "id"); err != nil {
		t.Fatal(err)
	}
	if got := virtualFile(t, s, r, "paper.tex"); got != "v1" {
		t.Fatalf("paper.tex = %q", got)
	}
	commitFile(t, up, "paper/paper.tex", "v2\n")
	// Well within refresh_interval: the very next fetch must see v2.
	if err := s.ensure(context.Background(), r, "id"); err != nil {
		t.Fatal(err)
	}
	if got := virtualFile(t, s, r, "paper.tex"); got != "v2" {
		t.Fatalf("stale virtual repository: paper.tex = %q", got)
	}
	if s.served(r) != git(t, up, "rev-parse", "main") {
		t.Fatal("served head does not match upstream")
	}
}

func TestEnsureRebuildsCacheAfterRestart(t *testing.T) {
	up := upstream(t)
	r := Repository{Name: "paper", Upstream: up, Ref: "main", Path: "paper"}
	s := testServer(t, "1m")
	if err := s.ensure(context.Background(), r, "id"); err != nil {
		t.Fatal(err)
	}
	commitFile(t, up, "paper/paper.tex", "v2\n")
	restarted := &Server{config: s.config}
	if err := restarted.ensure(context.Background(), r, "id"); err != nil {
		t.Fatal(err)
	}
	if got := virtualFile(t, restarted, r, "paper.tex"); got != "v2" {
		t.Fatalf("restart served stale cache: paper.tex = %q", got)
	}
}

func TestEnsureRebuildsLegacyCacheWithoutRecordedUpstream(t *testing.T) {
	up := upstream(t)
	r := Repository{Name: "paper", Upstream: up, Ref: "main", Path: "paper"}
	s := testServer(t, "1m")
	if err := s.ensure(context.Background(), r, "id"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.config.DataDir, "repositories", r.Name+".git", upstreamFile)); err != nil {
		t.Fatal(err)
	}
	if err := s.ensure(context.Background(), r, "id"); err != nil {
		t.Fatal(err)
	}
	if s.served(r) == "" {
		t.Fatal("legacy cache was served without being rebuilt")
	}
}

func TestEnsureRefusesWhenUpstreamUnreachable(t *testing.T) {
	s := testServer(t, "1m")
	r := Repository{Name: "paper", Upstream: filepath.Join(t.TempDir(), "missing"), Ref: "main", Path: "paper"}
	if err := s.ensure(context.Background(), r, "id"); !errors.Is(err, errUnverifiable) {
		t.Fatalf("err = %v, want errUnverifiable", err)
	}
}

func TestEnsureReturnsRetryableErrorWhileRebuilding(t *testing.T) {
	up := upstream(t)
	s := testServer(t, "1ns")
	r := Repository{Name: "paper", Upstream: up, Ref: "main", Path: "paper"}
	if err := s.ensure(context.Background(), r, "id"); !errors.Is(err, errRefreshing) {
		t.Fatalf("err = %v, want errRefreshing", err)
	}
	if v, ok := s.builds.Load("id"); ok {
		<-v.(*build).done
	}
	if err := s.ensure(context.Background(), r, "id"); err != nil {
		t.Fatalf("after rebuild: %v", err)
	}
}

func TestKeepOnlyRef(t *testing.T) {
	repository := t.TempDir()
	if err := run(context.Background(), "git", "init", repository); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), "git", "-C", repository, "config", "user.name", "Test"); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), "git", "-C", repository, "config", "user.email", "test@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "paper.txt"), []byte("paper\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), "git", "-C", repository, "add", "paper.txt"); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), "git", "-C", repository, "commit", "-m", "paper"); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), "git", "-C", repository, "branch", "other"); err != nil {
		t.Fatal(err)
	}
	head, err := runOutput(context.Background(), "git", "-C", repository, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), "git", "-C", repository, "update-ref", "refs/tags/latest", strings.TrimSpace(string(head))); err != nil {
		t.Fatal(err)
	}
	if err := keepOnlyRef(context.Background(), filepath.Join(repository, ".git"), "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	refs, err := runOutput(context.Background(), "git", "-C", repository, "for-each-ref", "--format=%(refname)")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(refs)); len(got) != 1 || got[0] != "refs/heads/main" {
		t.Fatalf("refs = %v", got)
	}
}

func TestWriteThroughKeepsPushedCommitHashAfterRebuild(t *testing.T) {
	work := upstream(t)
	up := filepath.Join(t.TempDir(), "upstream.git")
	if err := run(context.Background(), "git", "clone", "--bare", work, up); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, "1m")
	r := Repository{Name: "paper", Upstream: "file://" + up, Ref: "main", Path: "paper"}
	if err := s.ensure(context.Background(), r, "id"); err != nil {
		t.Fatal(err)
	}
	virtual := filepath.Join(s.config.DataDir, "repositories", r.Name+".git")
	clone := filepath.Join(t.TempDir(), "clone")
	if err := run(context.Background(), "git", "clone", virtual, clone); err != nil {
		t.Fatal(err)
	}
	git(t, clone, "config", "user.name", "Pusher")
	git(t, clone, "config", "user.email", "pusher@example.com")
	git(t, clone, "config", "commit.gpgsign", "false") // signed pushes cannot be reproduced
	commitFile(t, clone, "paper.tex", "pushed\n")
	git(t, clone, "push", "origin", "main")
	pushed := git(t, clone, "rev-parse", "HEAD")

	if err := s.writeThrough(context.Background(), r, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ensure(context.Background(), r, "id"); err != nil {
		t.Fatal(err)
	}
	if got := git(t, virtual, "rev-parse", "refs/heads/main"); got != pushed {
		t.Fatalf("rebuild rewrote the pushed commit: %s != %s\n%s\n---\n%s", got, pushed, git(t, virtual, "cat-file", "commit", got), git(t, clone, "cat-file", "commit", pushed))
	}
}
