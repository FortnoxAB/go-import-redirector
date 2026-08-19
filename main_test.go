package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeProber struct {
	hits  map[string]string // repo → GitHub URL; missing key = miss
	calls []string
}

func (f *fakeProber) Lookup(_ context.Context, _ string, repo string) (string, bool) {
	f.calls = append(f.calls, repo)
	url, ok := f.hits[repo]
	return url, ok
}

func setProber(t *testing.T, hits map[string]string) *fakeProber {
	t.Helper()
	fp := &fakeProber{hits: hits}
	orig := githubProber
	githubProber = fp
	t.Cleanup(func() { githubProber = orig })
	return fp
}

func handlerResponse(t *testing.T, h http.HandlerFunc, host, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path+"?go-get=1", nil)
	req.Host = host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Body.String()
}

func assertGoImport(t *testing.T, body, importRoot, vcs, vcsRoot string) {
	t.Helper()
	want := `content="` + importRoot + " " + vcs + " " + vcsRoot + `"`
	if !strings.Contains(body, want) {
		t.Errorf("go-import meta not found\nwant: %s\ngot:  %s", want, body)
	}
}

func TestHandlerProberDisabled(t *testing.T) {
	githubProber = nil
	m := parseMapping("go.example.com/team/*", "https://git.example.com/team/*", "https://github.com/my-org/*")
	body := handlerResponse(t, makeHandler(m), "go.example.com", "/team/myrepo")
	assertGoImport(t, body, "go.example.com/team/myrepo", "git", "https://git.example.com/team/myrepo")
}

func TestHandlerProberHit(t *testing.T) {
	setProber(t, map[string]string{"myrepo": "https://github.com/my-org/myrepo"})
	m := parseMapping("go.example.com/team/*", "https://git.example.com/team/*", "https://github.com/my-org/*")
	body := handlerResponse(t, makeHandler(m), "go.example.com", "/team/myrepo")
	assertGoImport(t, body, "go.example.com/team/myrepo", "git", "https://github.com/my-org/myrepo")
}

func TestHandlerProberMiss(t *testing.T) {
	setProber(t, nil)
	m := parseMapping("go.example.com/team/*", "https://git.example.com/team/*", "https://github.com/my-org/*")
	body := handlerResponse(t, makeHandler(m), "go.example.com", "/team/myrepo")
	assertGoImport(t, body, "go.example.com/team/myrepo", "git", "https://git.example.com/team/myrepo")
}

func TestHandlerMajorVersionSuffix(t *testing.T) {
	setProber(t, map[string]string{"myrepo": "https://github.com/my-org/myrepo"})
	m := parseMapping("go.example.com/team/*", "https://git.example.com/team/*", "https://github.com/my-org/*")
	body := handlerResponse(t, makeHandler(m), "go.example.com", "/team/myrepo/v2")
	// importRoot must be the repo root, not include /v2
	assertGoImport(t, body, "go.example.com/team/myrepo", "git", "https://github.com/my-org/myrepo")
}

func TestHandlerNoGithubOrgSkipsProber(t *testing.T) {
	fp := setProber(t, map[string]string{"myrepo": "https://github.com/my-org/myrepo"})
	m := parseMapping("go.example.com/team/*", "https://git.example.com/team/*", "")
	handlerResponse(t, makeHandler(m), "go.example.com", "/team/myrepo")
	if len(fp.calls) != 0 {
		t.Errorf("prober should not be called when githubOrg is empty; calls: %v", fp.calls)
	}
}

func TestHandlerNonWildcardSkipsProber(t *testing.T) {
	fp := setProber(t, map[string]string{"myrepo": "https://github.com/my-org/myrepo"})
	m := parseMapping("go.example.com/team/myrepo", "https://git.example.com/team/myrepo", "https://github.com/my-org/myrepo")
	handlerResponse(t, makeHandler(m), "go.example.com", "/team/myrepo")
	if len(fp.calls) != 0 {
		t.Errorf("prober should not be called for non-wildcard mapping; calls: %v", fp.calls)
	}
}

func TestRepoNameFromRoot(t *testing.T) {
	tests := []struct {
		importRoot string
		importPath string
		want       string
	}{
		{"go.example.com/team/myrepo", "go.example.com/team", "myrepo"},
		{"go.example.com/team/myrepo", "go.example.com/team/myrepo", ""},
		{"go.example.com/a/b/myrepo", "go.example.com/a", "myrepo"},
	}
	for _, tc := range tests {
		got := repoNameFromRoot(tc.importRoot, tc.importPath)
		if got != tc.want {
			t.Errorf("repoNameFromRoot(%q, %q) = %q, want %q",
				tc.importRoot, tc.importPath, got, tc.want)
		}
	}
}
