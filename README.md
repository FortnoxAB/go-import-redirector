# go-import-redirector

An HTTP service that implements Go's [vanity import path](https://pkg.go.dev/cmd/go#hdr-Remote_import_paths) protocol. It serves `go-import` meta tags so that `go get` can resolve custom import paths (e.g. `go.fnox.se/gl/myrepo`) to the actual VCS repository.

## How it works

When `go get go.example.com/team/myrepo` runs, the Go toolchain issues:

```
GET https://go.example.com/team/myrepo?go-get=1
```

The redirector responds with a `go-import` meta tag:

```html
<meta name="go-import" content="go.example.com/team/myrepo git ssh://git@git.example.com/team/myrepo">
```

For each request the service:

1. Matches the import path against the configured mappings.
2. Probes each `repoPaths` entry **except the last** (the old/primary servers) via `git ls-remote` to check whether the repo is still there.
3. Serves the first old server that still has the repo. Once all old servers return "not found", the last entry (the new server / migration target) is served automatically.

Probe results are cached (`-probe-cache-ttl`, default 10 minutes). Ambiguous errors (network outage, auth failure) are retried after a shorter interval (`-probe-error-ttl`, default 30 seconds) and default to serving the old server — so a transient outage never wrongly migrates traffic. If an old server has been unreachable for longer than `-probe-unreachable-ttl` (default 15 minutes), it is treated as gone and the new server is served instead.

## Configuration

Copy `redirects.example.json` to `redirects.json` (which is git-ignored) and fill in your mappings:

```json
[
  { "importPath": "go.example.com/team/*", "repoPaths": [
      "ssh://git@git.example.com/team/*",
      "ssh://git@github.com/my-org/*"
  ]},
  { "importPath": "go.example.com/other/*", "repoPaths": [
      "ssh://git@git.example.com/other/*"
  ]}
]
```

| Field | Required | Description |
|---|---|---|
| `importPath` | yes | Vanity import path prefix. Supports `/*` wildcard. |
| `repoPaths` | yes | Ordered list of VCS URLs, **old server first, new server last**. The old servers are probed; when a repo is gone from all of them the last entry (new server) is served automatically. No config change is needed as individual repos migrate. |

A request for `go.example.com/team/myrepo/v2` produces `importRoot = go.example.com/team/myrepo` regardless of major version suffix.

Non-wildcard entries take precedence over wildcard entries due to Go's mux longest-prefix rule:

```json
{ "importPath": "go.example.com/team/myrepo", "repoPaths": ["ssh://git@github.com/my-org/myrepo"] }
```

## SSH configuration

The service uses the system SSH config by default. To use a specific deploy key, set `GIT_SSH_COMMAND`:

```sh
export GIT_SSH_COMMAND="ssh -i /path/to/deploy-key -o BatchMode=yes -o StrictHostKeyChecking=accept-new"
```

In Kubernetes, mount the key as a Secret and pass the env var:

```yaml
env:
  - name: GIT_SSH_COMMAND
    value: "ssh -i /secrets/deploy-key -o BatchMode=yes -o StrictHostKeyChecking=accept-new"
```

The deploy key needs read access to all repositories that will be probed.

## Running

```sh
# With a config file
go-import-redirector -config redirects.json

# Single mapping via positional args
go-import-redirector rsc.io/* ssh://git@github.com/rsc/*
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `-config` | | Path to JSON config file. |
| `-addr` | `:http` | Address to listen on. |
| `-vcs` | `git` | VCS type for the `go-import` tag. |
| `-godoc-url` | | URL to redirect browsers to (non-`go-get` requests). |
| `-probe-cache-ttl` | `10m` | How long to cache definitive probe results (repo found or cleanly not found). |
| `-probe-error-ttl` | `30s` | How long to cache ambiguous errors (network, auth) before retry. |
| `-probe-unreachable-ttl` | `15m` | Treat an old server as gone if it has been unreachable this long. |
| `-probe-timeout` | `5s` | Timeout per `git ls-remote` probe. |

## Building

```sh
go build ./...
go test ./...
```

Docker image:

```sh
make docker VERSION=1.0.0
```

