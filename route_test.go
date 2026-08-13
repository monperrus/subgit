package main

import "testing"

func TestRepositoryForURL(t *testing.T) {
	r, id, suffix, err := repositoryForURL("/monperrus/test-repo-public/.github.git")
	if err != nil {
		t.Fatal(err)
	}
	if r.Path != ".github" || id != "monperrus/test-repo-public/.github" || suffix != "" {
		t.Fatalf("got %#v %q %q", r, id, suffix)
	}
}
