go get [-u] github.com/fortnoxab/go-import-redirector

## Usage

	go-import-redirector [-addr address] [-vcs sys] [-bitbucket-token token] [-cache-ttl duration] [-error-cache-ttl duration] [-unreachable-timeout duration] <import> <bitbucket-repo> <github-repo>

For each request, the server checks (cached for `-cache-ttl`, default 5m)
whether the repo still exists on Bitbucket and redirects `go get` there;
otherwise it falls back to the GitHub repo. This allows repos to be migrated
from Bitbucket to GitHub without reconfiguring the redirector.

	go-import-redirector git.fortnox.se/* https://gitrepo_primary.example.com/scm/PROJECT/* https://github.com/FortnoxAB/*

The Bitbucket existence check can be authenticated via `-bitbucket-token` or
the `BITBUCKET_TOKEN` environment variable.

A failed or ambiguous check (network error, timeout, or a non-404/non-2xx
status such as 401/403/429/5xx) is cached only briefly (`-error-cache-ttl`,
default 30s) and assumes the repo is still on Bitbucket, so a transient
outage doesn't wrongly migrate traffic to GitHub. If Bitbucket stays
unreachable for a given repo continuously for longer than
`-unreachable-timeout` (default 15m), the redirector assumes the repo has
moved and fails over to GitHub — this lets migrations complete even after
the Bitbucket host is fully decommissioned.
