// Copyright 2015 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Go-import-redirector is an HTTP server for a custom Go import domain.
// It responds to requests in a given import path root with a meta tag
// specifying the source repository for the “go get” command and an
// HTML redirect to the godoc.org documentation page for that package.
//
// Usage:
//
//	go-import-redirector [-addr address] [-tls] [-vcs sys] <import> <bitbucket-repo> <github-repo>
//
// Go-import-redirector listens on address (default “:80”)
// and responds to requests for URLs in the given import path root
// with one meta tag specifying the source repository for “go get”
// and another meta tag causing a redirect to the corresponding
// godoc.org documentation page.
//
// Two candidate repositories are configured: one on Bitbucket and one on
// GitHub. On each request, go-import-redirector checks (with caching)
// whether the repo still exists on Bitbucket; if so it points “go get” at
// Bitbucket, otherwise it falls back to the GitHub location. This supports
// gradually migrating repos from Bitbucket to GitHub without having to
// reconfigure or redeploy the redirector as each repo moves.
//
// For example, if invoked as:
//
//	go-import-redirector 9fans.net/go https://gitrepo_primary.example.com/scm/9fans/go https://github.com/9fans/go
//
// then the response for 9fans.net/go/acme/editinacme will include these tags,
// depending on whether the repo is still found on Bitbucket:
//
//	<meta name="go-import" content="9fans.net/go git https://gitrepo_primary.example.com/scm/9fans/go">
//	<meta http-equiv="refresh" content="0; url=https://godoc.org/9fans.net/go/acme/editinacme">
//
// or, once the repo has moved:
//
//	<meta name="go-import" content="9fans.net/go git https://github.com/9fans/go">
//	<meta http-equiv="refresh" content="0; url=https://godoc.org/9fans.net/go/acme/editinacme">
//
// If <import> and both repos end in /*, the corresponding path element
// is taken from the import path and substituted in each repo on every request.
// For example, if invoked as:
//
//	go-import-redirector rsc.io/* https://gitrepo_primary.example.com/scm/rsc/* https://github.com/rsc/*
//
// then the response for rsc.io/x86/x86asm will include a tag such as:
//
//	<meta name="go-import" content="rsc.io/x86 git https://github.com/rsc/x86">
//	<meta http-equiv="refresh" content="0; url=https://godoc.org/rsc.io/x86/x86asm">
//
// Note that the wildcard element (x86) has been included in the Git repo path.
//
// The -addr option specifies the HTTP address to serve (default “:http”).
//
// The -tls option causes go-import-redirector to serve HTTPS on port 443,
// loading an X.509 certificate and key pair from files in the current directory
// named after the host in the import path with .crt and .key appended
// (for example, rsc.io.crt and rsc.io.key).
// Like for http.ListenAndServeTLS, the certificate file should contain the
// concatenation of the server's certificate and the signing certificate authority's certificate.
//
// The -vcs option specifies the version control system, git, hg, or svn (default “git”).
//
// The -bitbucket-token option (or BITBUCKET_TOKEN environment variable) supplies
// a bearer token used when checking whether a repo still exists on Bitbucket.
//
// The -cache-ttl option controls how long a successful Bitbucket existence check
// is cached for a given repo before being checked again (default 5m).
//
// The -error-cache-ttl option controls how long a failed or ambiguous Bitbucket
// existence check is cached before being retried (default 30s).
//
// The -unreachable-timeout option controls how long Bitbucket must be
// continuously unreachable for a given repo before go-import-redirector assumes
// the repo has moved to GitHub, so migrations can complete even after the
// Bitbucket host is fully decommissioned (default 15m).
//
// # Deployment on Google Cloud Platform
//
// For the case of a redirector for an entire domain (such as rsc.io above),
// the Makefile in this directory contains recipes to deploy a trivial VM running
// just this program, using a static IP address that can be loaded into the
// DNS configuration for the target domain.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	addr               = flag.String("addr", ":http", "serve http on `address`")
	vcs                = flag.String("vcs", "git", "set version control `system`")
	godocURL           = flag.String("godoc-url", "", "URL to send the browser to if not fetched using go get")
	bitbucketToken     = flag.String("bitbucket-token", os.Getenv("BITBUCKET_TOKEN"), "auth `token` used to check whether a repo still exists on Bitbucket")
	cacheTTL           = flag.Duration("cache-ttl", 5*time.Minute, "how long to cache a successful Bitbucket existence check for a repo")
	errorCacheTTL      = flag.Duration("error-cache-ttl", 30*time.Second, "how long to cache the result of a failed Bitbucket existence check before retrying")
	unreachableTimeout = flag.Duration("unreachable-timeout", 15*time.Minute, "if Bitbucket has been unreachable for a repo continuously for this long, assume the repo has moved to GitHub")
	importPath         string
	bitbucketPath      string
	githubPath         string
	wildcard           int
	checker            *repoChecker
)

func usage() {
	fmt.Fprintf(os.Stderr, "usage: go-import-redirector <import> <bitbucket-repo> <github-repo>\n")
	fmt.Fprintf(os.Stderr, "options:\n")
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, "examples:\n")
	fmt.Fprintf(os.Stderr, "\tgo-import-redirector rsc.io/* https://gitrepo_primary.example.com/scm/rsc/* https://github.com/rsc/*\n")
	fmt.Fprintf(os.Stderr, "\tgo-import-redirector 9fans.net/go https://gitrepo_primary.example.com/scm/9fans/go https://github.com/9fans/go\n")
	os.Exit(2)
}

func main() {
	log.SetPrefix("go-import-redirector: ")
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() != 3 {
		flag.Usage()
	}
	importPath = flag.Arg(0)
	bitbucketPath = flag.Arg(1)
	githubPath = flag.Arg(2)
	if !strings.Contains(bitbucketPath, "://") || !strings.Contains(githubPath, "://") {
		log.Fatal("repo paths must be full URLs")
	}
	importIsWildcard := strings.HasSuffix(importPath, "/*")
	if importIsWildcard != strings.HasSuffix(bitbucketPath, "/*") || importIsWildcard != strings.HasSuffix(githubPath, "/*") {
		log.Fatal("either import and both repos must have /* or none of them")
	}
	for strings.HasSuffix(importPath, "/*") {
		wildcard++
		importPath = strings.TrimSuffix(importPath, "/*")
		bitbucketPath = strings.TrimSuffix(bitbucketPath, "/*")
		githubPath = strings.TrimSuffix(githubPath, "/*")
	}

	*godocURL = strings.TrimRight(*godocURL, "/")

	checker = newRepoChecker(*bitbucketToken, *cacheTTL, *errorCacheTTL, *unreachableTimeout)

	http.HandleFunc(strings.TrimSuffix(importPath, "/")+"/", redirect)
	http.HandleFunc(importPath+"/.ping", pong) // non-redirecting URL for debugging TLS certificates
	err := http.ListenAndServe(*addr, nil)
	if err != nil {
		log.Fatal(err)
	}
}

// cacheEntry holds the outcome of the most recent check for a repo URL.
// unreachableSince tracks how long probes have been erroring or returning
// an ambiguous (non-2xx, non-404) status; it is reset to zero as soon as a
// probe gets a clean answer.
type cacheEntry struct {
	exists           bool
	expiresAt        time.Time
	unreachableSince time.Time
}

type repoChecker struct {
	client             *http.Client
	token              string
	ttl                time.Duration
	errTTL             time.Duration
	unreachableTimeout time.Duration

	mu       sync.Mutex
	cache    map[string]cacheEntry
	inflight map[string]*inflightProbe
}

// inflightProbe lets concurrent callers checking the same URL share a
// single outbound probe instead of each firing their own.
type inflightProbe struct {
	wg     sync.WaitGroup
	exists bool
}

func newRepoChecker(token string, ttl, errTTL, unreachableTimeout time.Duration) *repoChecker {
	c := &repoChecker{
		client:             &http.Client{Timeout: 5 * time.Second},
		token:              token,
		ttl:                ttl,
		errTTL:             errTTL,
		unreachableTimeout: unreachableTimeout,
		cache:              make(map[string]cacheEntry),
		inflight:           make(map[string]*inflightProbe),
	}
	go c.sweepLoop()
	return c
}

// sweepLoop periodically drops expired cache entries so that repos which
// stop being requested don't linger in memory forever.
func (c *repoChecker) sweepLoop() {
	for range time.Tick(c.ttl) {
		now := time.Now()
		c.mu.Lock()
		for url, entry := range c.cache {
			if now.After(entry.expiresAt) {
				delete(c.cache, url)
			}
		}
		c.mu.Unlock()
	}
}

// existsOnBitbucket reports whether the given Bitbucket repo URL still
// exists, using a cached result when available. Concurrent callers for the
// same uncached URL share a single outbound probe.
func (c *repoChecker) existsOnBitbucket(url string) bool {
	c.mu.Lock()
	if entry, ok := c.cache[url]; ok && time.Now().Before(entry.expiresAt) {
		c.mu.Unlock()
		return entry.exists
	}
	if call, ok := c.inflight[url]; ok {
		c.mu.Unlock()
		call.wg.Wait()
		return call.exists
	}
	call := &inflightProbe{}
	call.wg.Add(1)
	c.inflight[url] = call
	c.mu.Unlock()

	exists := c.check(url)

	call.exists = exists
	call.wg.Done()
	c.mu.Lock()
	delete(c.inflight, url)
	c.mu.Unlock()
	return exists
}

// check performs the actual probe and updates the cache. On a genuine 404
// it trusts the result immediately. On an error or ambiguous status it
// assumes the repo is still on Bitbucket for errTTL, unless probes have
// been failing continuously for longer than unreachableTimeout, in which
// case it assumes the repo has moved to GitHub so migrations can complete
// even after the Bitbucket host is fully decommissioned.
func (c *repoChecker) check(url string) bool {
	now := time.Now()
	exists, err := c.probe(url)
	if err == nil {
		c.mu.Lock()
		c.cache[url] = cacheEntry{exists: exists, expiresAt: now.Add(c.ttl)}
		c.mu.Unlock()
		return exists
	}

	c.mu.Lock()
	unreachableSince := c.cache[url].unreachableSince
	if unreachableSince.IsZero() {
		unreachableSince = now
	}
	c.mu.Unlock()

	assumeMoved := now.Sub(unreachableSince) > c.unreachableTimeout
	if assumeMoved {
		log.Printf("warning: %s has been unreachable on Bitbucket since %s, assuming it has moved to GitHub: %v", url, unreachableSince, err)
	} else {
		log.Printf("warning: failed to check %s on Bitbucket, assuming it still exists: %v", url, err)
	}

	c.mu.Lock()
	c.cache[url] = cacheEntry{exists: !assumeMoved, expiresAt: now.Add(c.errTTL), unreachableSince: unreachableSince}
	c.mu.Unlock()
	return !assumeMoved
}

// probe reports whether url still resolves to a repo on Bitbucket. A 404
// unambiguously means the repo is gone. Any other non-2xx status (401,
// 403, 429, 5xx, or an unexpected redirect) is ambiguous rather than proof
// the repo moved, so it is reported as an error and handled the same way
// as a network failure by the caller.
func (c *repoChecker) probe(url string) (bool, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, nil
	default:
		return false, fmt.Errorf("unexpected status %s checking %s", resp.Status, url)
	}
}

var tmpl = template.Must(template.New("main").Parse(`<!DOCTYPE html>
<html>
<head>
<meta http-equiv="Content-Type" content="text/html; charset=utf-8"/>
<meta name="go-import" content="{{.ImportRoot}} {{.VCS}} {{.VCSRoot}}">
<meta http-equiv="refresh" content="0; url={{.GoDocURL}}/{{.ImportRoot}}{{.Suffix}}">
</head>
<body>
Redirecting to docs at <a href="{{.GoDocURL}}/{{.ImportRoot}}{{.Suffix}}">{{.GoDocURL}}/{{.ImportRoot}}{{.Suffix}}</a>...
</body>
</html>
`))

type data struct {
	ImportRoot string
	VCS        string
	VCSRoot    string
	Suffix     string
	GoDocURL   string
}

func redirect(w http.ResponseWriter, req *http.Request) {
	path := strings.TrimSuffix(req.Host+req.URL.Path, "/")
	var importRoot, bitbucketRoot, githubRoot, suffix string
	if wildcard > 0 {
		if path == importPath {
			http.Redirect(w, req, *godocURL+"/"+importPath, http.StatusFound)
			return
		}
		if !strings.HasPrefix(path, importPath+"/") {
			http.NotFound(w, req)
			return
		}
		elem := path[len(importPath)+1:]
		if parts := strings.Split(elem, "/"); len(parts) >= wildcard {
			elem = strings.Join(parts[:wildcard], "/")
			suffix = strings.Join(parts[wildcard:], "/")
			if suffix != "" {
				suffix = "/" + suffix
			}
		} else {
			http.NotFound(w, req)
			return
		}
		importRoot = importPath + "/" + elem
		bitbucketRoot = bitbucketPath + "/" + elem
		githubRoot = githubPath + "/" + elem
	} else {
		if path != importPath && !strings.HasPrefix(path, importPath+"/") {
			http.NotFound(w, req)
			return
		}
		importRoot = importPath
		bitbucketRoot = bitbucketPath
		githubRoot = githubPath
		suffix = path[len(importPath):]
	}

	repoRoot := bitbucketRoot
	if !checker.existsOnBitbucket(bitbucketRoot) {
		repoRoot = githubRoot
	}
	d := &data{
		ImportRoot: importRoot,
		VCS:        *vcs,
		VCSRoot:    repoRoot,
		Suffix:     suffix,
		GoDocURL:   *godocURL,
	}
	var buf bytes.Buffer
	err := tmpl.Execute(&buf, d)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Write(buf.Bytes())
}

func pong(w http.ResponseWriter, req *http.Request) {
	fmt.Fprintf(w, "pong")
}
