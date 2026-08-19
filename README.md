# go-import-redirector

An HTTP service that implements Go's [vanity import path](https://pkg.go.dev/cmd/go#hdr-Remote_import_paths) protocol. It serves `go-import` meta tags so that `go get` can resolve custom import paths (e.g. `go.example.com/team/myrepo`) to the actual VCS repository — whether it lives on a self-hosted Git server or on GitHub.

## How it works

When `go get go.example.com/team/myrepo` runs, the Go toolchain issues:

```
GET https://go.example.com/team/myrepo?go-get=1
```

The redirector responds with a `go-import` meta tag:

```html
<meta name="go-import" content="go.example.com/team/myrepo git https://github.com/my-org/myrepo">
```

For each request the service:

1. Matches the import path against the configured mappings.
2. If a `github_org` is configured for that mapping and `GITHUB_TOKEN` is set, probes `api.github.com/repos/{github_org}/{repo}` to check whether the repository has been migrated to GitHub.
3. On a GitHub hit → serves the GitHub HTTPS URL. On a miss or no token → serves the `origin` URL from the config.

Probe results (both found and not-found) are cached for the duration of `-github-probe-cache-ttl` (default 10 minutes), so `go mod download` over a large dependency graph does not fan out into repeated API calls.

## Configuration

Copy `redirects.example.json` to `redirects.json` (which is git-ignored) and fill in your mappings:

```json
[
  { "import": "go.example.com/team/*",  "origin": "https://git.example.com/team/*",  "github": "https://github.com/my-github-org/*" },
  { "import": "go.example.com/other/*", "origin": "https://git.example.com/other/*" }
]
```

| Field | Required | Description |
|---|---|---|
| `import` | yes | Vanity import path prefix. Supports `/*` wildcard. |
| `origin` | yes | Fallback VCS URL. Must be a full URL; supports `/*` to match the wildcard. |
| `github` | no | GitHub URL to probe, e.g. `https://github.com/my-org/*`. Omit to disable GitHub probing for this mapping. |

Wildcard expansion strips the last segment of `origin` and replaces it with the matched repo name. A request for `go.example.com/team/myrepo/v2` produces `importRoot = go.example.com/team/myrepo` and serves the correct repo regardless of major version suffix.

Non-wildcard entries take precedence over wildcard entries due to Go's mux longest-prefix rule, so you can override individual repos:

```json
{ "import": "go.example.com/team/myrepo", "origin": "https://github.com/my-org/myrepo" }
```

## Running

```sh
# With a config file and GitHub probing
GITHUB_TOKEN=<token> go-import-redirector -config redirects.json

# Config file, no GitHub probing (pure passthrough)
go-import-redirector -config redirects.json

# Single mapping via positional args (original usage, no GitHub probing)
go-import-redirector rsc.io/* https://github.com/rsc/*
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `-config` | | Path to JSON config file. |
| `-addr` | `:http` | Address to listen on. |
| `-vcs` | `git` | VCS type for the `go-import` tag. |
| `-godoc-url` | | URL to redirect browsers to (non-`go-get` requests). |
| `-github-probe-cache-ttl` | `10m` | How long to cache GitHub probe results. |
| `-github-probe-timeout` | `5s` | Timeout per GitHub API probe. |

### Environment variables

| Variable | Description |
|---|---|
| `GITHUB_TOKEN` | GitHub personal access token or fine-grained token used to authenticate `api.github.com` probes. If unset, GitHub probing is disabled regardless of config. |

## Building

```sh
go build ./...
go test ./...
```

Docker image:

```sh
make docker VERSION=1.0.0
```

