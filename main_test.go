package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeProber struct {
	hits  map[string]bool // repoURL → exists
	calls []string
}

func (f *fakeProber) Probe(_ context.Context, repoURL string) bool {
	f.calls = append(f.calls, repoURL)
	return f.hits[repoURL]
}

func setProber(t *testing.T, hits map[string]bool) *fakeProber {
	t.Helper()
	fp := &fakeProber{hits: hits}
	orig := prober
	prober = fp
	t.Cleanup(func() { prober = orig })
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
	prober = nil
	m := parseMapping("go.example.com/team/*", []string{"ssh://git@git.example.com/team/*", "ssh://git@github.com/my-org/*"})
	body := handlerResponse(t, makeHandler(m), "go.example.com", "/team/myrepo")
	// no prober → safe default: serve first (old server)
	assertGoImport(t, body, "go.example.com/team/myrepo", "git", "ssh://git@git.example.com/team/myrepo")
}

func TestHandlerProberHit(t *testing.T) {
	// old server still has the repo → serve it
	setProber(t, map[string]bool{"ssh://git@git.example.com/team/myrepo": true})
	m := parseMapping("go.example.com/team/*", []string{"ssh://git@git.example.com/team/*", "ssh://git@github.com/my-org/*"})
	body := handlerResponse(t, makeHandler(m), "go.example.com", "/team/myrepo")
	assertGoImport(t, body, "go.example.com/team/myrepo", "git", "ssh://git@git.example.com/team/myrepo")
}

func TestHandlerProberMiss(t *testing.T) {
	// old server returns not-found → repo migrated → serve github (last entry)
	setProber(t, nil)
	m := parseMapping("go.example.com/team/*", []string{"ssh://git@git.example.com/team/*", "ssh://git@github.com/my-org/*"})
	body := handlerResponse(t, makeHandler(m), "go.example.com", "/team/myrepo")
	assertGoImport(t, body, "go.example.com/team/myrepo", "git", "ssh://git@github.com/my-org/myrepo")
}

func TestHandlerMajorVersionSuffix(t *testing.T) {
	// old server still has the repo
	setProber(t, map[string]bool{"ssh://git@git.example.com/team/myrepo": true})
	m := parseMapping("go.example.com/team/*", []string{"ssh://git@git.example.com/team/*", "ssh://git@github.com/my-org/*"})
	body := handlerResponse(t, makeHandler(m), "go.example.com", "/team/myrepo/v2")
	// importRoot must be the repo root, not include /v2
	assertGoImport(t, body, "go.example.com/team/myrepo", "git", "ssh://git@git.example.com/team/myrepo")
}

func TestHandlerSingleRepoSkipsProber(t *testing.T) {
	fp := setProber(t, map[string]bool{"ssh://git@git.example.com/team/myrepo": true})
	m := parseMapping("go.example.com/team/*", []string{"ssh://git@git.example.com/team/*"})
	handlerResponse(t, makeHandler(m), "go.example.com", "/team/myrepo")
	if len(fp.calls) != 0 {
		t.Errorf("prober should not be called for single-repo mapping; calls: %v", fp.calls)
	}
}

func TestHandlerNonWildcardSkipsProber(t *testing.T) {
	fp := setProber(t, map[string]bool{"ssh://git@git.example.com/team/myrepo": true})
	m := parseMapping("go.example.com/team/myrepo", []string{"ssh://git@git.example.com/team/myrepo", "ssh://git@github.com/my-org/myrepo"})
	handlerResponse(t, makeHandler(m), "go.example.com", "/team/myrepo")
	if len(fp.calls) != 0 {
		t.Errorf("prober should not be called for non-wildcard mapping; calls: %v", fp.calls)
	}
}

