package main

import (
	"context"
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

func TestEnsureUsesCachedRepositoryAfterRestart(t *testing.T) {
	dataDir := t.TempDir()
	r := Repository{Name: "paper", Upstream: "https://invalid.example/paper.git", Ref: "main", Path: "paper"}
	cache := filepath.Join(dataDir, "repositories", r.Name+".git")
	if err := os.MkdirAll(cache, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "HEAD"), []byte("ref: refs/heads/main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	s := &Server{config: Config{DataDir: dataDir, RefreshInterval: "15m"}}
	if err := s.ensure(r, "owner/repo/paper"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.status.Load("owner/repo/paper"); !ok {
		t.Fatal("cached repository was not marked ready")
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
