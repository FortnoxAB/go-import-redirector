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
	"strings"
	"time"

	"github.com/fortnoxab/go-import-redirector/gitprobe"
)

var (
	addr               = flag.String("addr", ":http", "serve http on `address`")
	vcs                = flag.String("vcs", "git", "set version control `system`")
	godocURL           = flag.String("godoc-url", "", "URL to send the browser to if not fetched using go get")
	config             = flag.String("config", "", "path to JSON config file (see redirects.example.json)")
	probeCacheTTL       = flag.Duration("probe-cache-ttl", 10*time.Minute, "how long to cache definitive probe results")
	probeErrorTTL       = flag.Duration("probe-error-ttl", 30*time.Second, "how long to cache ambiguous probe errors before retry")
	probeUnreachableTTL = flag.Duration("probe-unreachable-ttl", 15*time.Minute, "assume repo migrated if old server is unreachable this long")
	probeTimeout        = flag.Duration("probe-timeout", 5*time.Second, "timeout per git ls-remote probe")
)

type configEntry struct {
	ImportPath string   `json:"importPath"`
	RepoPaths  []string `json:"repoPaths"`
}

type mapping struct {
	importPath string
	repoPaths  []string // wildcard stripped, ordered old→new; non-last entries are probed
	wildcard   int
}

// repoProber is satisfied by *gitprobe.Prober; separated for test injection.
type repoProber interface {
	Probe(ctx context.Context, repoURL string) bool
}

var prober repoProber

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

	prober = gitprobe.New(*probeCacheTTL, *probeErrorTTL, *probeUnreachableTTL, *probeTimeout)
	log.Printf("git SSH probing enabled (cache TTL: %v, error TTL: %v, unreachable TTL: %v)",
		*probeCacheTTL, *probeErrorTTL, *probeUnreachableTTL)

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

// resolveRepoPath probes the first entries (old servers) to detect migration.
// If an old server still has the repo → serve it. If all old servers are gone → serve last (new server).
// Ambiguous errors default to serving the old server; persistent errors fall over to new.
func resolveRepoPath(req *http.Request, m mapping, importRoot, elem string) string {
	candidate := func(rp string) string {
		if m.wildcard > 0 {
			return rp + "/" + elem
		}
		return rp
	}
	if len(m.repoPaths) == 1 || m.wildcard == 0 {
		return candidate(m.repoPaths[0])
	}
	if prober == nil {
		return candidate(m.repoPaths[0]) // can't probe, assume old server
	}
	// Probe old servers (all but last); serve last (new server) only when all are gone.
	for i := 0; i < len(m.repoPaths)-1; i++ {
		if prober.Probe(req.Context(), candidate(m.repoPaths[i])) {
			return candidate(m.repoPaths[i])
		}
	}
	return candidate(m.repoPaths[len(m.repoPaths)-1])
}

func pong(w http.ResponseWriter, req *http.Request) {
	fmt.Fprintf(w, "pong")
}
