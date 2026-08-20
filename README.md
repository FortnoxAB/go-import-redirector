# go-import-redirector

An HTTP service that implements Go's [vanity import path](https://pkg.go.dev/cmd/go#hdr-Remote_import_paths) protocol. It serves `go-import` meta tags so that `go get` can resolve custom import paths (e.g. `go.fnox.se/gl/myrepo`) to the actual VCS repository.

## How it works

When `go get go.example.com/team/myrepo` runs, the Go toolchain issues:

```
GET https://go.example.com/team/myrepo?go-get=1
```

The redirector responds with a `go-import` meta tag:

```html
<meta name="go-import" content="go.example.com/team/myrepo git ssh://git@github.com/my-org/myrepo">
```

For each request the service:

1. Matches the import path against the configured mappings.
2. For each entry in `repoPaths` (in order), runs `git ls-remote <url>` to check if the repository is reachable.
3. Returns the first reachable URL. The last entry is always served as a fallback without probing.

Probe results are cached for `-probe-cache-ttl` (default 10 minutes), so `go mod download` over a large dependency graph does not fan out into repeated SSH calls.

## Configuration

Copy `redirects.example.json` to `redirects.json` (which is git-ignored) and fill in your mappings:

```json
[
  { "importPath": "go.example.com/team/*", "repoPaths": [
      "ssh://git@github.com/my-org/*",
      "ssh://git@git.example.com/team/*"
  ]},
  { "importPath": "go.example.com/other/*", "repoPaths": [
      "ssh://git@git.example.com/other/*"
  ]}
]
```

| Field | Required | Description |
|---|---|---|
| `importPath` | yes | Vanity import path prefix. Supports `/*` wildcard. |
| `repoPaths` | yes | Ordered list of candidate VCS URLs. Each must be a full URL and use the same wildcard pattern as `importPath`. The first reachable entry wins; the last is always the fallback. |

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
| `-probe-cache-ttl` | `10m` | How long to cache probe results. |
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

