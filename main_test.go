package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

func TestHandlerNonGitVCSSkipsProber(t *testing.T) {
	fp := setProber(t, map[string]bool{"ssh://hg@hg.example.com/team/myrepo": true})
	orig := *vcs
	*vcs = "hg"
	t.Cleanup(func() { *vcs = orig })
	m := parseMapping("go.example.com/team/*", []string{"ssh://hg@hg.example.com/team/*", "ssh://hg@github.com/my-org/*"})
	body := handlerResponse(t, makeHandler(m), "go.example.com", "/team/myrepo")
	if len(fp.calls) != 0 {
		t.Errorf("git ls-remote prober must not run against a non-git VCS; calls: %v", fp.calls)
	}
	assertGoImport(t, body, "go.example.com/team/myrepo", "hg", "ssh://hg@hg.example.com/team/myrepo")
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
		"ssh://git@github.com/my-org/*-go-lib",
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

func TestHandlerMultiLevelWildcard(t *testing.T) {
	setProber(t, nil)
	cases := []struct{ repo, want string }{
		{"https://github.com/*/*", "https://github.com/a/b"},       // one "*" per level
		{"https://github.com/*", "https://github.com/a/b"},         // whole-segment "*" takes the whole elem
		{"https://github.com/*/go-*", "https://github.com/a/go-b"}, // per-level rename
	}
	for _, c := range cases {
		m := parseMapping("rsc.io/*/*", []string{c.repo})
		body := handlerResponse(t, makeHandler(m), "rsc.io", "/a/b/sub/pkg")
		assertGoImport(t, body, "rsc.io/a/b", "git", c.want)
	}
}

func TestHandlerRejectsInvalidElem(t *testing.T) {
	fp := setProber(t, map[string]bool{})
	m := parseMapping("go.example.com/team/*", []string{"ssh://git@git.example.com/team/*", "ssh://git@github.com/my-org/*"})
	for _, target := range []string{
		"/team/..%2Fsecret?go-get=1", // decodes to elem ".."
		"/team/.?go-get=1",
		"/team/foo%0Agitprobe:%20forged?go-get=1", // newline would forge a log line
		"/team/foo%20bar?go-get=1",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Host = "go.example.com"
		rec := httptest.NewRecorder()
		makeHandler(m).ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: expected 404, got %d: %s", target, rec.Code, rec.Body.String())
		}
	}
	if len(fp.calls) != 0 {
		t.Errorf("invalid elems must never be probed; calls: %q", fp.calls)
	}
}

func TestHandlerStripsBracketedIPv6HostPort(t *testing.T) {
	m := parseMapping("::1/team/myrepo", []string{"ssh://git@git.example.com/team/myrepo"})
	body := handlerResponse(t, makeHandler(m), "[::1]:8080", "/team/myrepo")
	assertGoImport(t, body, "::1/team/myrepo", "git", "ssh://git@git.example.com/team/myrepo")
}

func TestHandlerStripsPlainHostPort(t *testing.T) {
	m := parseMapping("go.example.com/team/myrepo", []string{"ssh://git@git.example.com/team/myrepo"})
	body := handlerResponse(t, makeHandler(m), "go.example.com:8080", "/team/myrepo")
	assertGoImport(t, body, "go.example.com/team/myrepo", "git", "ssh://git@git.example.com/team/myrepo")
}

// TestFatalValidation exercises the log.Fatal-based validation paths in
// parseMapping/registerMapping. These call os.Exit, so each case re-execs
// this test binary as a subprocess (the standard pattern for testing
// os.Exit-calling code) and asserts it exits non-zero.
func TestFatalValidation(t *testing.T) {
	cases := map[string]func(){
		"MismatchedWildcard": func() {
			parseMapping("go.example.com/*", []string{"ssh://git@git.example.com/team/repo"})
		},
		"RepoNotFullURL": func() {
			parseMapping("go.example.com/team/myrepo", []string{"git.example.com/team/myrepo"})
		},
		"MultipleWildcardsInRepo": func() {
			parseMapping("go.example.com/*", []string{"ssh://git@git.example.com/*/repo-*"})
		},
		"MultiLevelWrongStarCount": func() {
			parseMapping("go.example.com/*/*/*", []string{"ssh://git@git.example.com/*/*"})
		},
		"MultiLevelSingleStarNotWholeSegment": func() {
			parseMapping("go.example.com/*/*", []string{"ssh://git@git.example.com/go-*"})
		},
		"NoRepos": func() {
			parseMapping("go.example.com/team/myrepo", nil)
		},
		"DuplicateImportPath": func() {
			m := mapping{importPath: "go.example.com/team/myrepo"}
			registerMapping(m)
			registerMapping(m)
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			if os.Getenv("GO_WANT_FATAL_TEST_PROCESS") == name {
				fn()
				return
			}
			cmd := exec.Command(os.Args[0], "-test.run=TestFatalValidation/"+name)
			cmd.Env = append(os.Environ(), "GO_WANT_FATAL_TEST_PROCESS="+name)
			out, err := cmd.CombinedOutput()
			if exitErr, ok := err.(*exec.ExitError); ok && !exitErr.Success() {
				return
			}
			t.Fatalf("expected process to exit with a fatal error; output:\n%s\nerr: %v", out, err)
		})
	}
}
