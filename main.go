// Copyright 2015 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Go-import-redirector is an HTTP server for a custom Go import domain.
// It responds to requests in a given import path root with a meta tag
// specifying the source repository for the ``go get'' command and an
// HTML redirect to the godoc.org documentation page for that package.
//
// Usage:
//
//	go-import-redirector [-addr address] [-tls] [-vcs sys] <import> <repo>
//
// Go-import-redirector listens on address (default ``:80'')
// and responds to requests for URLs in the given import path root
// with one meta tag specifying the given source repository for ``go get''
// and another meta tag causing a redirect to the corresponding
// godoc.org documentation page.
//
// For example, if invoked as:
//
//	go-import-redirector 9fans.net/go https://github.com/9fans/go
//
// then the response for 9fans.net/go/acme/editinacme will include these tags:
//
//	<meta name="go-import" content="9fans.net/go git https://github.com/9fans/go">
//	<meta http-equiv="refresh" content="0; url=https://godoc.org/9fans.net/go/acme/editinacme">
//
// If both <import> and <repo> end in /*, the corresponding path element
// is taken from the import path and substituted in repo on each request.
// For example, if invoked as:
//
//	go-import-redirector rsc.io/* https://github.com/rsc/*
//
// then the response for rsc.io/x86/x86asm will include these tags:
//
//	<meta name="go-import" content="rsc.io/x86 git https://github.com/rsc/x86">
//	<meta http-equiv="refresh" content="0; url=https://godoc.org/rsc.io/x86/x86asm">
//
// Note that the wildcard element (x86) has been included in the Git repo path.
//
// The -addr option specifies the HTTP address to serve (default ``:http'').
//
// The -tls option causes go-import-redirector to serve HTTPS on port 443,
// loading an X.509 certificate and key pair from files in the current directory
// named after the host in the import path with .crt and .key appended
// (for example, rsc.io.crt and rsc.io.key).
// Like for http.ListenAndServeTLS, the certificate file should contain the
// concatenation of the server's certificate and the signing certificate authority's certificate.
//
// The -vcs option specifies the version control system, git, hg, or svn (default ``git'').
//
// Deployment on Google Cloud Platform
//
// For the case of a redirector for an entire domain (such as rsc.io above),
// the Makefile in this directory contains recipes to deploy a trivial VM running
// just this program, using a static IP address that can be loaded into the
// DNS configuration for the target domain.
//
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"net/url"
	"strings"
	"time"

	"github.com/fortnoxab/go-import-redirector/githubprobe"
)

var (
	addr               = flag.String("addr", ":http", "serve http on `address`")
	vcs                = flag.String("vcs", "git", "set version control `system`")
	godocURL           = flag.String("godoc-url", "", "URL to send the browser to if not fetched using go get")
	config             = flag.String("config", "", "path to JSON config file (see redirects.example.json)")
	githubCacheTTL     = flag.Duration("github-probe-cache-ttl", 10*time.Minute, "how long to cache GitHub probe results")
	githubProbeTimeout = flag.Duration("github-probe-timeout", 5*time.Second, "timeout per GitHub API probe")
)

type configEntry struct {
	ImportPath string   `json:"importPath"`
	RepoPaths  []string `json:"repoPaths"`
}

type mapping struct {
	importPath string
	repoPaths  []string // wildcard stripped, ordered; github.com entries probed first
	wildcard   int
}

// githubLooker is satisfied by *githubprobe.Prober; separated for test injection.
type githubLooker interface {
	Lookup(ctx context.Context, org, repo string) (string, bool)
}

var githubProber githubLooker

func usage() {
	fmt.Fprintf(os.Stderr, "usage: go-import-redirector [-config file] [<import> <origin>]\n")
	fmt.Fprintf(os.Stderr, "options:\n")
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, "examples:\n")
	fmt.Fprintf(os.Stderr, "\tgo-import-redirector -config redirects.json\n")
	fmt.Fprintf(os.Stderr, "\tgo-import-redirector rsc.io/* https://github.com/rsc/*\n")
	os.Exit(2)
}

func main() {
	log.SetPrefix("go-import-redirector: ")
	flag.Usage = usage
	flag.Parse()
	*godocURL = strings.TrimRight(*godocURL, "/")

	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		githubProber = githubprobe.New(token, *githubCacheTTL, *githubProbeTimeout)
		log.Printf("GitHub probing enabled via token (cache TTL: %v)", *githubCacheTTL)
	}

	if appIDStr := os.Getenv("GITHUB_APP_ID"); appIDStr != "" {
		appID, _ := strconv.ParseInt(appIDStr, 10, 64)
		var pem []byte
		if f := os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE"); f != "" {
			var err error
			if pem, err = os.ReadFile(f); err != nil {
				log.Fatalf("reading GitHub App private key: %v", err)
			}
		} else if p := os.Getenv("GITHUB_APP_PRIVATE_KEY"); p != "" {
			pem = []byte(p)
		}
		if appID == 0 || len(pem) == 0 {
			log.Fatal("GITHUB_APP_ID requires GITHUB_APP_PRIVATE_KEY / GITHUB_APP_PRIVATE_KEY_FILE")
		}
		prober, err := githubprobe.NewWithAppKey(appID, pem, *githubCacheTTL, *githubProbeTimeout)
		if err != nil {
			log.Fatalf("GitHub App auth: %v", err)
		}
		githubProber = prober
		log.Printf("GitHub probing enabled via GitHub App %d (cache TTL: %v)", appID, *githubCacheTTL)
	}

	if *config != "" {
		f, err := os.Open(*config)
		if err != nil {
			log.Printf("warning: cannot open config %s: %v; starting with no routes", *config, err)
		} else {
			defer f.Close()
			var entries []configEntry
			if err := json.NewDecoder(f).Decode(&entries); err != nil {
				log.Fatalf("parsing config: %v", err)
			}
			for _, e := range entries {
				registerMapping(parseMapping(e.ImportPath, e.RepoPaths))
			}
		}
	} else if flag.NArg() == 2 {
		registerMapping(parseMapping(flag.Arg(0), []string{flag.Arg(1)}))
	} else {
		log.Print("no -config or import/origin args provided; starting with no routes registered")
	}

	if err := http.ListenAndServe(*addr, nil); err != nil {
		log.Fatal(err)
	}
}

func parseMapping(imp string, repos []string) mapping {
	if len(repos) == 0 {
		log.Fatalf("mapping for %s has no repos", imp)
	}
	for _, r := range repos {
		if !strings.Contains(r, "://") {
			log.Fatalf("repo must be a full URL: %s", r)
		}
		if strings.HasSuffix(imp, "/*") != strings.HasSuffix(r, "/*") {
			log.Fatalf("import and repos must have matching /* wildcards: %s vs %s", imp, r)
		}
	}
	m := mapping{importPath: imp, repoPaths: append([]string(nil), repos...)}
	for strings.HasSuffix(m.importPath, "/*") {
		m.wildcard++
		m.importPath = strings.TrimSuffix(m.importPath, "/*")
		for i := range m.repoPaths {
			m.repoPaths[i] = strings.TrimSuffix(m.repoPaths[i], "/*")
		}
	}
	return m
}

// githubOrgFromRepoPath returns the GitHub org if repoPath is a github.com URL.
func githubOrgFromRepoPath(repoPath string) (org string, isGitHub bool) {
	u, err := url.Parse(repoPath)
	if err != nil || u.Hostname() != "github.com" {
		return "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", false
	}
	return parts[0], true
}

func registerMapping(m mapping) {
	http.HandleFunc(strings.TrimSuffix(m.importPath, "/")+"/", makeHandler(m))
	http.HandleFunc(m.importPath+"/.ping", pong)
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

func makeHandler(m mapping) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		path := strings.TrimSuffix(req.Host+req.URL.Path, "/")
		var importRoot, suffix string
		var elem string
		if m.wildcard > 0 {
			if path == m.importPath {
				http.Redirect(w, req, *godocURL+"/"+m.importPath, http.StatusFound)
				return
			}
			if !strings.HasPrefix(path, m.importPath+"/") {
				http.NotFound(w, req)
				return
			}
			elem = path[len(m.importPath)+1:]
			if parts := strings.Split(elem, "/"); len(parts) >= m.wildcard {
				elem = strings.Join(parts[:m.wildcard], "/")
				suffix = strings.Join(parts[m.wildcard:], "/")
				if suffix != "" {
					suffix = "/" + suffix
				}
			} else {
				http.NotFound(w, req)
				return
			}
			importRoot = m.importPath + "/" + elem
		} else {
			if path != m.importPath && !strings.HasPrefix(path, m.importPath+"/") {
				http.NotFound(w, req)
				return
			}
			importRoot = m.importPath
			suffix = path[len(m.importPath):]
		}

		vcsRoot := resolveRepoPath(req, m, importRoot, elem)
		d := &data{
			ImportRoot: importRoot,
			VCS:        *vcs,
			VCSRoot:    vcsRoot,
			Suffix:     suffix,
			GoDocURL:   *godocURL,
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, d); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Write(buf.Bytes())
	}
}

// resolveRepoPath tries each configured repoPath in order.
// GitHub URLs are probed via the API; the first hit wins.
// Non-GitHub URLs are served directly. The last entry is always a fallback.
func resolveRepoPath(req *http.Request, m mapping, importRoot, elem string) string {
	for i, rp := range m.repoPaths {
		candidate := rp
		if m.wildcard > 0 {
			candidate = rp + "/" + elem
		}
		org, isGH := githubOrgFromRepoPath(rp)
		isLast := i == len(m.repoPaths)-1
		if !isGH {
			return candidate
		}
		// GitHub entry: probe if possible
		if githubProber != nil && m.wildcard > 0 {
			if name := repoNameFromRoot(importRoot, m.importPath); name != "" {
				if _, ok := githubProber.Lookup(req.Context(), org, name); ok {
					return candidate
				}
			}
		}
		if isLast {
			return candidate // last resort
		}
	}
	return m.repoPaths[len(m.repoPaths)-1] // unreachable
}

func pong(w http.ResponseWriter, req *http.Request) {
	fmt.Fprintf(w, "pong")
}

// repoNameFromRoot returns the bare repo name from importRoot by stripping the
// importPath prefix. Returns "" for non-wildcard mappings.
func repoNameFromRoot(importRoot, importPath string) string {
	if !strings.HasPrefix(importRoot, importPath+"/") {
		return ""
	}
	after := importRoot[len(importPath)+1:]
	parts := strings.Split(after, "/")
	return parts[len(parts)-1]
}
