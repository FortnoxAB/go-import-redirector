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

func TestHandlerNonWildcardSingleRepoSkipsProber(t *testing.T) {
	fp := setProber(t, map[string]bool{"ssh://git@git.example.com/team/myrepo": true})
	m := parseMapping("go.example.com/team/myrepo", []string{"ssh://git@git.example.com/team/myrepo"})
	handlerResponse(t, makeHandler(m), "go.example.com", "/team/myrepo")
	if len(fp.calls) != 0 {
		t.Errorf("prober should not be called for single-repo mapping; calls: %v", fp.calls)
	}
}

func TestHandlerNonWildcardRenameProbed(t *testing.T) {
	// override entry renaming the repo on migration; still probed like a wildcard mapping
	setProber(t, nil)
	m := parseMapping("go.example.com/team/users", []string{"ssh://git@git.example.com/team/users", "ssh://git@github.com/my-org/users-go-lib"})
	body := handlerResponse(t, makeHandler(m), "go.example.com", "/team/users")
	assertGoImport(t, body, "go.example.com/team/users", "git", "ssh://git@github.com/my-org/users-go-lib")
}

func TestHandlerWildcardSuffixFallback(t *testing.T) {
	// project-wide mapping: check the renamed (suffixed) repo first, fall back to the plain name
	// for repos that weren't renamed (e.g. no name collision on the new server).
	m := parseMapping("go.example.com/team/*", []string{
		"ssh://git@git.example.com/team/*",
		"ssh://git@github.com/my-org/go-*",
		"ssh://git@github.com/my-org/*",
	})

	// "users" collided and was renamed → found under the suffixed name.
	setProber(t, map[string]bool{"ssh://git@github.com/my-org/users-go-lib": true})
	body := handlerResponse(t, makeHandler(m), "go.example.com", "/team/users")
	assertGoImport(t, body, "go.example.com/team/users", "git", "ssh://git@github.com/my-org/users-go-lib")

	// "sune" has no collision and kept its plain name → suffixed miss, plain-name fallback.
	setProber(t, map[string]bool{"ssh://git@github.com/my-org/sune": true})
	body = handlerResponse(t, makeHandler(m), "go.example.com", "/team/sune")
	assertGoImport(t, body, "go.example.com/team/sune", "git", "ssh://git@github.com/my-org/sune")
}

