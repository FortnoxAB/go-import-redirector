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
2. Probes each `repoPaths` entry **except the last** via `git ls-remote`, in order, to check whether the repo is there.
3. Serves the first entry that's found. Once none of the probed entries have the repo, the last entry is served automatically — it is never probed, it's the trusted default.

Probe results are cached (`-probe-cache-ttl`, default 10 minutes). Ambiguous errors (network outage, auth failure) are retried after a shorter interval (`-probe-error-ttl`, default 30 seconds) and default to "still there" for whichever entry is being probed — so a transient outage never wrongly flips traffic. If an entry has been unreachable for longer than `-probe-unreachable-ttl` (default 15 minutes), it is treated as gone and the next entry in the list is served instead.

The order of `repoPaths` decides which server is trusted by default: put the old server first and the new one last to migrate only once the old repo is confirmed gone (the classic case), or the other way around to switch to the new server as soon as it exists, without waiting for the old one to be decommissioned (see `redirects.example.json`, which checks GitHub first and falls back to the old server).

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
| `importPath` | yes | Vanity import path prefix. Supports `/*` wildcard. **Never include a port** (e.g. `localhost:8080/team/*`) — Go's `http.ServeMux` strips the port from the request's `Host` header before matching host-qualified patterns, so a pattern with a literal port can never match. For local testing use a bare host (e.g. `localhost/team/*`) and override the `Host` header instead: `curl -H "Host: localhost" http://localhost:8080/team/myrepo?go-get=1`. |
| `repoPaths` | yes | Ordered list of VCS URLs. All entries except the last are probed, in order; the first one found is served. The last entry is never probed — it's the trusted default once the others are gone (or not yet created). No config change is needed as individual repos migrate. |

A request for `go.example.com/team/myrepo/v2` produces `importRoot = go.example.com/team/myrepo` regardless of major version suffix.

### Renaming repos during migration

In a wildcard `repoPaths` entry, `*` is a literal placeholder for the matched repo name and can have a static prefix/suffix in the same segment, e.g. `*-go-lib` or `go-*`. Combined with probing, this lets a whole project fall back through several naming conventions — handy when a repo must be renamed on the new server to avoid a name clash with something that already exists there:

```json
{ "importPath": "go.example.com/team/*", "repoPaths": [
    "ssh://git@git.example.com/team/*",
    "ssh://git@github.com/my-org/*-go-lib",
    "ssh://git@github.com/my-org/*"
]}
```

Each entry is probed in order until one exists; the last one is always served as the final fallback, even if it doesn't exist yet.

* `go get go.example.com/team/users` — `users` was renamed to avoid clashing with an unrelated, pre-existing `users` repo on GitHub:
  1. `git.example.com/team/users` → not found (already migrated)
  2. `github.com/my-org/users-go-lib` → **found, served**
* `go get go.example.com/team/orders` — `orders` has no name clash, so it kept its plain name:
  1. `git.example.com/team/orders` → not found (already migrated)
  2. `github.com/my-org/orders-go-lib` → not found (was never renamed)
  3. `github.com/my-org/orders` → **found, served**

Non-wildcard entries take precedence over wildcard entries due to Go's mux longest-prefix rule. This is useful for one-off exceptions — e.g. `users` is the *only* repo in `team` that needs renaming, so instead of adding a `*-go-lib` fallback to the whole project's wildcard mapping, pin just that one import path:

```json
{ "importPath": "go.example.com/team/users", "repoPaths": [
    "ssh://git@git.example.com/team/users",
    "ssh://git@github.com/my-org/users-go-lib"
]}
```

`go get go.example.com/team/users` now matches this exact mapping instead of the `team/*` wildcard:
1. `git.example.com/team/users` → not found (already migrated)
2. `github.com/my-org/users-go-lib` → **found, served**

Just like wildcard mappings, this old-first/new-last list is probed and migrates automatically.

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

