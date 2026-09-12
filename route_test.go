package main

import (
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os/exec"
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
