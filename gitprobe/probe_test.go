package gitprobe

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := exec.Command("git", "init", dir).Run(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// stubGit installs a fake "git" executable at the front of PATH for the
// duration of the test, so ls-remote timing/output can be controlled
// deterministically instead of depending on a real (network) remote.
// sleep is how long the stub waits before exiting; exitCode and stderr
// control the simulated git ls-remote result.
func stubGit(t *testing.T, sleep time.Duration, exitCode int, stderr string) {
	t.Helper()
	stubGitScript(t, fmt.Sprintf("sleep %f\n>&2 printf '%%s' %q\nexit %d\n", sleep.Seconds(), stderr, exitCode))
}

// stubGitScript installs a fake "git" whose body is the given sh script.
func stubGitScript(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub git script requires a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "git")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestProbeFound(t *testing.T) {
	skipIfNoGit(t)
	url := "file://" + initRepo(t)
	p := New(time.Hour, 30*time.Second, 15*time.Minute, 5*time.Second)
	if !p.Probe(context.Background(), url) {
		t.Error("expected repo to be found")
	}
}

func TestProbeNotFound(t *testing.T) {
	skipIfNoGit(t)
	p := New(time.Hour, 30*time.Second, 15*time.Minute, 5*time.Second)
	if p.Probe(context.Background(), "file:///this-path-does-not-exist") {
		t.Error("expected repo not found")
	}
}

func TestProbeCachesHit(t *testing.T) {
	skipIfNoGit(t)
	url := "file://" + initRepo(t)
	p := New(time.Hour, 30*time.Second, 15*time.Minute, 5*time.Second)
	p.Probe(context.Background(), url)
	p.Probe(context.Background(), url)
	p.mu.Lock()
	e, ok := p.cache[url]
	p.mu.Unlock()
	if !ok || !e.exists {
		t.Error("expected positive result to be cached")
	}
}

func TestProbeCachesMiss(t *testing.T) {
	skipIfNoGit(t)
	const url = "file:///nonexistent-path-cache-test"
	p := New(time.Hour, 30*time.Second, 15*time.Minute, 5*time.Second)
	p.Probe(context.Background(), url)
	p.mu.Lock()
	e, ok := p.cache[url]
	p.mu.Unlock()
	if !ok || e.exists {
		t.Error("expected negative result to be cached")
	}
}

func TestProbeCacheExpiry(t *testing.T) {
	skipIfNoGit(t)
	url := "file://" + initRepo(t)
	p := New(10*time.Millisecond, 5*time.Second, 15*time.Minute, 5*time.Second)
	p.Probe(context.Background(), url)
	time.Sleep(20 * time.Millisecond)
	// seed expired negative entry to verify re-probe happens
	p.mu.Lock()
	p.cache[url] = cacheEntry{exists: false, expires: time.Now().Add(-time.Second)}
	p.mu.Unlock()
	if !p.Probe(context.Background(), url) {
		t.Error("expected expired cache to trigger re-probe and find repo")
	}
}

func TestProbeConcurrent(t *testing.T) {
	skipIfNoGit(t)
	url := "file://" + initRepo(t)
	p := New(time.Hour, 30*time.Second, 15*time.Minute, 5*time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); p.Probe(context.Background(), url) }()
	}
	wg.Wait()
}

// TestProbeInflightJoinCancelFallsBackToCache guards against the join branch
// of Probe unconditionally answering "exists" on cancellation instead of
// consulting the cache like every other cancellation path in this file.
func TestProbeInflightJoinCancelFallsBackToCache(t *testing.T) {
	const url = "file:///whatever-inflight-cancel-test"
	p := New(time.Hour, 30*time.Second, 15*time.Minute, 5*time.Second)

	p.mu.Lock()
	p.cache[url] = cacheEntry{exists: false, expires: time.Now().Add(-time.Second)} // expired
	p.inflight = map[string]*probeCall{url: {done: make(chan struct{})}}            // never completes
	p.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := p.Probe(ctx, url); got != false {
		t.Errorf("expected fallback to cached exists=false, got %v", got)
	}
}

// TestProbeInflightJoinCancelNoCacheDefaultsExists checks the same path
// defaults to "exists" (the documented safe default) when there is nothing
// cached to fall back to.
func TestProbeInflightJoinCancelNoCacheDefaultsExists(t *testing.T) {
	const url = "file:///whatever-inflight-cancel-nocache-test"
	p := New(time.Hour, 30*time.Second, 15*time.Minute, 5*time.Second)

	p.mu.Lock()
	p.inflight = map[string]*probeCall{url: {done: make(chan struct{})}} // never completes
	p.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := p.Probe(ctx, url); got != true {
		t.Errorf("expected safe default exists=true, got %v", got)
	}
}

// TestProbeCallerCancelFallsBackToCache verifies that a caller whose context
// is canceled mid-probe gets the last cached value, while the probe itself
// keeps running detached from that caller and still refreshes the cache.
func TestProbeCallerCancelFallsBackToCache(t *testing.T) {
	stubGit(t, 300*time.Millisecond, 0, "")
	const url = "ssh://example.invalid/repo"
	p := New(time.Hour, 30*time.Second, 15*time.Minute, 5*time.Second)
	p.mu.Lock()
	p.cache[url] = cacheEntry{exists: false, expires: time.Now().Add(-time.Second)} // expired
	p.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if got := p.Probe(ctx, url); got != false {
		t.Errorf("expected fallback to cached exists=false on caller cancellation, got %v", got)
	}

	waitInflight(t, p, url)
	p.mu.Lock()
	e := p.cache[url]
	p.mu.Unlock()
	if !e.exists {
		t.Error("expected the probe to finish despite caller cancellation and cache exists=true")
	}
}

// TestProbeCallerDeadlineDoesNotRecordOutage verifies that a request-level
// deadline is treated the same as other caller-driven cancellations: the
// caller gets the safe default, and it must not create or update outage
// tracking for the repo once the detached probe completes.
func TestProbeCallerDeadlineDoesNotRecordOutage(t *testing.T) {
	stubGit(t, 300*time.Millisecond, 0, "")
	const url = "ssh://example.invalid/deadline-repo"
	p := New(time.Hour, 30*time.Second, 15*time.Minute, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if got := p.Probe(ctx, url); got != true {
		t.Fatalf("expected safe default exists=true on caller deadline, got %v", got)
	}

	waitInflight(t, p, url)
	p.mu.Lock()
	e, ok := p.cache[url]
	p.mu.Unlock()
	if !ok || !e.exists || !e.unreachableSince.IsZero() {
		t.Fatalf("expected the detached probe to cache a plain success, got %+v (present=%v)", e, ok)
	}
}

// TestProbeJoinerUnaffectedByStarterCancel guards against the caller that
// started a probe aborting it for everyone who joined it.
func TestProbeJoinerUnaffectedByStarterCancel(t *testing.T) {
	stubGit(t, 300*time.Millisecond, 0, "")
	const url = "ssh://example.invalid/shared-repo"
	p := New(time.Hour, 30*time.Second, 15*time.Minute, 5*time.Second)
	p.mu.Lock()
	p.cache[url] = cacheEntry{exists: false, expires: time.Now().Add(-time.Second)} // expired
	p.mu.Unlock()

	starterCtx, cancel := context.WithCancel(context.Background())
	go p.Probe(starterCtx, url)
	for {
		p.mu.Lock()
		_, inflight := p.inflight[url]
		p.mu.Unlock()
		if inflight {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.AfterFunc(50*time.Millisecond, cancel)

	if got := p.Probe(context.Background(), url); got != true {
		t.Errorf("expected joiner to get the real probe result exists=true, got %v", got)
	}
}

// TestProbePendingCapFallsBack verifies that once maxPendingProbes URLs are
// already in flight, a probe for a new URL is not queued and the caller gets
// the last known result (here: none cached, so the safe default).
func TestProbePendingCapFallsBack(t *testing.T) {
	const url = "file:///whatever-pending-cap-test" // would probe as not found if it ran
	p := New(time.Hour, 30*time.Second, 15*time.Minute, 5*time.Second)
	p.mu.Lock()
	for i := range maxPendingProbes {
		p.inflight[fmt.Sprintf("pending-%d", i)] = &probeCall{done: make(chan struct{})}
	}
	p.mu.Unlock()

	if got := p.Probe(context.Background(), url); got != true {
		t.Errorf("expected safe default exists=true when pending probes are capped, got %v", got)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.inflight[url]; ok {
		t.Error("expected no probe to be queued beyond maxPendingProbes")
	}
	if len(p.inflight) != maxPendingProbes {
		t.Errorf("expected %d pending probes, got %d", maxPendingProbes, len(p.inflight))
	}
}

// waitInflight blocks until any in-flight probe for url has finished, so a
// detached probe doesn't outlive the test that started it.
func waitInflight(t *testing.T, p *Prober, url string) {
	t.Helper()
	p.mu.Lock()
	c, ok := p.inflight[url]
	p.mu.Unlock()
	if !ok {
		return
	}
	select {
	case <-c.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for in-flight probe")
	}
}

// TestProbeSlotWaitDoesNotStarveExecTimeout is a regression test for the
// bug where the semaphore-wait budget and the git-exec budget shared one
// context: a probe that waited nearly the full p.timeout for a free slot
// would then have almost no time left to actually run git ls-remote. The
// slot wait and the exec must each get their own fresh p.timeout budget.
func TestProbeSlotWaitDoesNotStarveExecTimeout(t *testing.T) {
	const timeout = 500 * time.Millisecond
	const holdDuration = 300 * time.Millisecond // < timeout, but leaves little of it once acquired
	const execSleep = 350 * time.Millisecond    // > timeout-holdDuration, but < a fresh timeout

	stubGit(t, execSleep, 0, "")
	p := New(time.Hour, 30*time.Second, 15*time.Minute, timeout)

	// Saturate every probe slot, then release exactly one after holdDuration
	// so the incoming Probe call spends holdDuration just waiting.
	for range maxConcurrentProbes {
		p.sem <- struct{}{}
	}
	time.AfterFunc(holdDuration, func() { <-p.sem })

	const url = "ssh://example.invalid/slow-repo"
	if got := p.Probe(context.Background(), url); got != true {
		t.Fatalf("expected exec to succeed with its own fresh timeout budget, got %v", got)
	}
	// A return of true alone doesn't distinguish success from the buggy
	// shared-budget case, since an ambiguous/timed-out error also defaults
	// to "true" (safe default). Inspect the cache entry to tell them apart:
	// a genuine success caches with the full p.ttl and no outage tracking,
	// while a starved exec would be misclassified as an ambiguous error,
	// cached with the short errorTTL and a non-zero unreachableSince.
	p.mu.Lock()
	e, ok := p.cache[url]
	p.mu.Unlock()
	if !ok {
		t.Fatal("expected a cache entry after probe")
	}
	if !e.unreachableSince.IsZero() {
		t.Errorf("exec was starved and misclassified as an ambiguous error (unreachableSince=%v)", e.unreachableSince)
	}
	if time.Until(e.expires) < time.Minute {
		t.Errorf("expected a full-ttl cache entry from a genuine success, got expires in %v", time.Until(e.expires))
	}
}

// TestProbeAmbiguousErrorEscalatesAfterUnreachableTTL verifies the
// unreachableSince accumulation: a repo that keeps returning ambiguous
// errors is treated as "still exists" until unreachableTTL has elapsed,
// after which it flips to "gone".
func TestProbeAmbiguousErrorEscalatesAfterUnreachableTTL(t *testing.T) {
	stubGit(t, 0, 1, "fatal: could not read from remote repository")
	const unreachableTTL = 50 * time.Millisecond
	p := New(time.Hour, 10*time.Millisecond, unreachableTTL, 5*time.Second)
	const url = "ssh://example.invalid/flaky-repo"

	if got := p.Probe(context.Background(), url); got != true {
		t.Fatalf("expected ambiguous error to default to exists=true before unreachableTTL, got %v", got)
	}
	time.Sleep(unreachableTTL + 20*time.Millisecond)
	if got := p.Probe(context.Background(), url); got != false {
		t.Errorf("expected repo to be treated as gone after unreachableTTL of ambiguous errors, got %v", got)
	}
}

// TestProbeMisconfigurationDoesNotEscalate verifies that host-level probe
// misconfiguration (here an unknown host key) keeps serving the probed entry
// instead of being mistaken for a migration once unreachableTTL elapses.
func TestProbeMisconfigurationDoesNotEscalate(t *testing.T) {
	stubGit(t, 0, 128, "Host key verification failed.\nfatal: Could not read from remote repository.")
	const unreachableTTL = 50 * time.Millisecond
	p := New(time.Hour, 10*time.Millisecond, unreachableTTL, 5*time.Second)
	const url = "ssh://example.invalid/misconfigured-repo"

	if got := p.Probe(context.Background(), url); got != true {
		t.Fatalf("expected misconfiguration to keep exists=true, got %v", got)
	}
	time.Sleep(unreachableTTL + 20*time.Millisecond)
	if got := p.Probe(context.Background(), url); got != true {
		t.Errorf("expected misconfiguration not to escalate to gone after unreachableTTL, got %v", got)
	}
	p.mu.Lock()
	e := p.cache[url]
	p.mu.Unlock()
	if !e.unreachableSince.IsZero() {
		t.Errorf("expected no outage tracking for a misconfiguration, got unreachableSince=%v", e.unreachableSince)
	}
}

// TestProbeTimeoutKillsDescendants is a regression test for a timed-out
// probe waiting on a git descendant (e.g. ssh stuck in connect) that still
// holds stderr: the whole process group must be killed at the timeout.
func TestProbeTimeoutKillsDescendants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group kill is unix-only")
	}
	stubGitScript(t, "sleep 10 &\nwait\n") // background child inherits stderr
	const timeout = 200 * time.Millisecond
	p := New(time.Hour, 30*time.Second, 15*time.Minute, timeout)

	start := time.Now()
	p.Probe(context.Background(), "ssh://example.invalid/hanging-repo")
	// Well under timeout+killWaitDelay, so passing needs the group kill,
	// not just the WaitDelay backstop.
	if elapsed := time.Since(start); elapsed >= timeout+killWaitDelay/2 {
		t.Errorf("probe took %v after a %v timeout; descendants were not killed", elapsed, timeout)
	}
}

// TestProbeInvocation checks the git command line and environment: a HEAD
// pattern so only one ref is listed, and prompts disabled so a probe can't
// wait on credentials.
func TestProbeInvocation(t *testing.T) {
	stubGitScript(t, `[ "$1 $2 $4" = "ls-remote -- HEAD" ] && [ "$GIT_TERMINAL_PROMPT" = 0 ] && [ "$SSH_ASKPASS_REQUIRE" = never ] && exit 0
>&2 echo "remote: Repository not found."
exit 128
`)
	p := New(time.Hour, 30*time.Second, 15*time.Minute, 5*time.Second)
	if !p.Probe(context.Background(), "ssh://example.invalid/invocation-repo") {
		t.Error("unexpected git arguments or environment")
	}
}

func TestLogSafeStderr(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Host key verification failed.\r\nfatal: Could not read from remote repository.\n", "Host key verification failed. fatal: Could not read from remote repository."},
		{"fatal: bad\x1b[31m\x00 thing\ngitprobe: forged", "fatal: bad [31m thing gitprobe: forged"},
		{"fatal: unable to access 'ssh://user:secret@example.com/repo'", "fatal: unable to access 'ssh://redacted@example.com/repo'"},
	}
	for _, c := range cases {
		if got := logSafeStderr(c.in, "ssh://user:secret@example.com/repo"); got != c.want {
			t.Errorf("logSafeStderr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	long := logSafeStderr(strings.Repeat("x", 2*maxLoggedStderr), "")
	if len(long) != maxLoggedStderr+len("...") {
		t.Errorf("expected stderr truncated to %d chars plus ellipsis, got %d", maxLoggedStderr, len(long))
	}
}

func TestIsMisconfiguration(t *testing.T) {
	cases := []struct {
		stderr string
		want   bool
	}{
		{"Host key verification failed.", true},
		{"git@github.com: Permission denied (publickey).", true},
		{"fatal: Authentication failed for 'https://example.com/repo/'", true},
		{"ssh: connect to host example.com port 22: Connection timed out", false},
		{"remote: Repository not found.", false},
	}
	for _, c := range cases {
		if got := isMisconfiguration(c.stderr); got != c.want {
			t.Errorf("isMisconfiguration(%q) = %v, want %v", c.stderr, got, c.want)
		}
	}
}

func TestEvictLockedRemovesExpiredKeepsOutageTrackingAndBoundsSize(t *testing.T) {
	p := New(time.Hour, 30*time.Second, 15*time.Minute, 5*time.Second)
	now := time.Now()

	p.cache["expired-no-outage"] = cacheEntry{exists: true, expires: now.Add(-time.Second)}
	p.cache["expired-with-outage"] = cacheEntry{exists: false, expires: now.Add(-time.Second), unreachableSince: now.Add(-time.Minute)}
	p.cache["not-expired"] = cacheEntry{exists: true, expires: now.Add(time.Hour)}
	p.evictLocked(now)

	if _, ok := p.cache["expired-no-outage"]; ok {
		t.Error("expected expired entry with no outage tracking to be evicted")
	}
	if _, ok := p.cache["expired-with-outage"]; !ok {
		t.Error("expected expired entry with active outage tracking to be kept")
	}
	if _, ok := p.cache["not-expired"]; !ok {
		t.Error("expected unexpired entry to be kept")
	}

	p.cache = make(map[string]cacheEntry)
	for i := range maxCacheEntries + 50 {
		p.cache[fmt.Sprintf("overflow-%d", i)] = cacheEntry{exists: true, expires: now.Add(time.Hour)}
	}
	p.evictLocked(now)
	if len(p.cache) > maxCacheEntries {
		t.Errorf("expected cache to be bounded to %d entries, got %d", maxCacheEntries, len(p.cache))
	}
}

// TestEvictLockedThrottlesFullScan guards the optimization that skips the
// O(n) expired-entry scan until evictSweepInterval has passed (unless the
// cache is actually oversized), so it isn't redone on every single write.
func TestEvictLockedThrottlesFullScan(t *testing.T) {
	p := New(time.Hour, 30*time.Second, 15*time.Minute, 5*time.Second)
	now := time.Now()

	// First call always sweeps (nextSweep zero value) and schedules the next one.
	p.evictLocked(now)

	p.cache["expired"] = cacheEntry{exists: true, expires: now.Add(-time.Second)}
	p.evictLocked(now) // well within evictSweepInterval of the first call
	if _, ok := p.cache["expired"]; !ok {
		t.Error("expected sweep to be throttled, leaving the expired entry in place")
	}

	p.evictLocked(now.Add(evictSweepInterval + time.Second))
	if _, ok := p.cache["expired"]; ok {
		t.Error("expected sweep to run once evictSweepInterval elapsed, removing the expired entry")
	}
}

func TestRedactForLog(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ssh://user:secret@example.com/repo", "ssh://redacted@example.com/repo"},
		{"https://example.com/repo", "https://example.com/repo"},
		{"not a url", "not a url"},
	}
	for _, c := range cases {
		if got := redactForLog(c.in); got != c.want {
			t.Errorf("redactForLog(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsDefinitivelyGone(t *testing.T) {
	cases := []struct {
		stderr string
		want   bool
	}{
		{"fatal: 'x' does not appear to be a git repository", true},
		{"DOES NOT APPEAR TO BE A GIT REPOSITORY", true},
		{"remote: Repository not found.", true}, // treated as gone, not ambiguous; see definitivelyGoneMarkers
		{"fatal: could not read Username for 'https://gitlab.example.com': terminal prompts disabled", true},
		{"fatal: could not read from remote repository", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isDefinitivelyGone(c.stderr); got != c.want {
			t.Errorf("isDefinitivelyGone(%q) = %v, want %v", c.stderr, got, c.want)
		}
	}
}
