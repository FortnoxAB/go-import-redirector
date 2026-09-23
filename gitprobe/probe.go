package gitprobe

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type cacheEntry struct {
	exists           bool
	expires          time.Time
	unreachableSince time.Time // non-zero while consecutive ambiguous errors persist
}

// maxCacheEntries bounds cache size so an attacker requesting many distinct
// wildcard names cannot grow the map without limit.
const maxCacheEntries = 10000

// maxConcurrentProbes bounds how many git subprocesses can run at once, so
// requests for many distinct wildcard names can't exhaust process/CPU/memory.
const maxConcurrentProbes = 32

// Verbose enables logging of every probe attempt (not just found/not-found
// state changes and errors). Intended to be set once at startup from a CLI
// flag; noisy in production if left on.
var Verbose bool

// logVerbose logs only when Verbose is enabled.
func logVerbose(format string, args ...any) {
	if Verbose {
		log.Printf(format, args...)
	}
}

// evictSweepInterval throttles the full-map eviction scan in evictLocked so
// it runs periodically rather than on every single cache write.
const evictSweepInterval = time.Minute

// Prober checks git repo existence via git ls-remote and caches results.
// A definitive "not found" and a repo that has been unreachable longer than
// unreachableTTL are both treated as gone. Ambiguous errors (network, auth)
// are cached briefly and default to "still exists" until the timeout elapses.
type Prober struct {
	ttl            time.Duration
	errorTTL       time.Duration // short TTL for ambiguous errors
	unreachableTTL time.Duration // treat persistent errors as "gone" after this
	timeout        time.Duration
	mu             sync.Mutex
	cache          map[string]cacheEntry
	inflight       map[string]*probeCall // serializes concurrent probes of the same URL
	sem            chan struct{}         // bounds concurrent git subprocesses across all URLs
	nextSweep      time.Time             // evictLocked skips the full scan until this time, unless oversized
}

// probeCall lets concurrent callers for the same URL share one in-flight
// probe instead of racing to write the cache entry out of order.
type probeCall struct {
	done   chan struct{}
	result bool
}

// New returns a Prober. errorTTL controls retry interval on ambiguous errors;
// unreachableTTL is how long errors must persist before assuming the repo is gone.
func New(ttl, errorTTL, unreachableTTL, timeout time.Duration) *Prober {
	return &Prober{
		ttl:            ttl,
		errorTTL:       errorTTL,
		unreachableTTL: unreachableTTL,
		timeout:        timeout,
		cache:          make(map[string]cacheEntry),
		sem:            make(chan struct{}, maxConcurrentProbes),
	}
}

// Probe returns true if the repo at repoURL still exists.
// false means definitively gone or unreachable long enough to assume migration.
func (p *Prober) Probe(ctx context.Context, repoURL string) bool {
	p.mu.Lock()
	e, ok := p.cache[repoURL]
	if ok && time.Now().Before(e.expires) {
		p.mu.Unlock()
		return e.exists
	}
	// Join an in-flight probe for the same URL instead of racing it: two
	// concurrent probes can finish out of order and the slower one would
	// otherwise overwrite a fresher cache entry.
	if c, inflight := p.inflight[repoURL]; inflight {
		p.mu.Unlock()
		select {
		case <-c.done:
			return c.result
		case <-ctx.Done():
			// Our own context gave up before the in-flight probe finished;
			// says nothing about the repo, so fall back to the last known
			// result like the other cancellation paths in this file.
			p.mu.Lock()
			e, ok := p.cache[repoURL]
			p.mu.Unlock()
			if ok {
				return e.exists
			}
			return true
		}
	}
	c := &probeCall{done: make(chan struct{})}
	if p.inflight == nil {
		p.inflight = make(map[string]*probeCall)
	}
	p.inflight[repoURL] = c
	unreachableSince := e.unreachableSince
	p.mu.Unlock()

	result := p.probe(ctx, repoURL, unreachableSince)

	p.mu.Lock()
	delete(p.inflight, repoURL)
	p.mu.Unlock()
	c.result = result
	close(c.done)
	return result
}

func (p *Prober) probe(ctx context.Context, repoURL string, unreachableSince time.Time) bool {
	// Bound the wait for a free probe slot by p.timeout too, so -probe-timeout
	// caps total latency even when all slots are busy. This budget is only
	// for acquiring a slot: the git subprocess below gets its own fresh
	// p.timeout once started, so slot contention can never eat into (and
	// thus falsely time out) the actual probe.
	waitCtx, waitCancel := context.WithTimeout(ctx, p.timeout)
	defer waitCancel()

	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-waitCtx.Done():
		// Caller gave up waiting for a free probe slot; says nothing about
		// the repo, so don't touch the cache or outage tracking.
		p.mu.Lock()
		e, ok := p.cache[repoURL]
		p.mu.Unlock()
		if ok {
			return e.exists
		}
		return true
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	logVerbose("gitprobe: probing %s", redactForLog(repoURL))

	var stderr bytes.Buffer
	cmd := exec.CommandContext(timeoutCtx, "git", "ls-remote", repoURL)
	cmd.Stderr = &stderr
	err := cmd.Run()

	now := time.Now()
	if err == nil {
		logVerbose("gitprobe: %s: found", redactForLog(repoURL))
		p.mu.Lock()
		p.cache[repoURL] = cacheEntry{exists: true, expires: now.Add(p.ttl)}
		p.evictLocked(now)
		p.mu.Unlock()
		return true
	}

	// Definitively gone: repo not found (not a transient error)
	if isDefinitivelyGone(stderr.String()) {
		log.Printf("gitprobe: %s: not found", redactForLog(repoURL))
		p.mu.Lock()
		p.cache[repoURL] = cacheEntry{exists: false, expires: now.Add(p.ttl)}
		p.evictLocked(now)
		p.mu.Unlock()
		return false
	}

	// The caller (HTTP request) was canceled/disconnected before our own
	// timeout elapsed; that says nothing about the repo, so leave the cache
	// and outage tracking untouched and fall back to the last known result.
	if errors.Is(timeoutCtx.Err(), context.Canceled) {
		p.mu.Lock()
		e, ok := p.cache[repoURL]
		p.mu.Unlock()
		if ok {
			return e.exists
		}
		return true
	}

	// Ambiguous error (network, auth, timeout): safe default is "still exists"
	// unless we've been failing continuously longer than unreachableTTL.
log.Printf("gitprobe: %s: %v", redactForLog(repoURL), err)
	if unreachableSince.IsZero() {
		unreachableSince = now
	}
	assumeGone := now.Sub(unreachableSince) >= p.unreachableTTL
	if assumeGone {
		log.Printf("gitprobe: %s unreachable since %s, assuming migrated", redactForLog(repoURL), unreachableSince.Format(time.RFC3339))
	}
	p.mu.Lock()
	p.cache[repoURL] = cacheEntry{exists: !assumeGone, expires: now.Add(p.errorTTL), unreachableSince: unreachableSince}
	p.evictLocked(now)
	p.mu.Unlock()
	return !assumeGone
}

// redactForLog strips credentials from a repo URL's userinfo before logging.
func redactForLog(repoURL string) string {
	u, err := url.Parse(repoURL)
	if err != nil || u.User == nil {
		return repoURL
	}
	u.User = url.User("redacted")
	return u.String()
}

// evictLocked removes entries that no longer need tracking, then—if the
// cache is still oversized—falls back to dropping the oldest entries so
// memory use stays bounded regardless of how many distinct keys are probed.
// The full scan is throttled to evictSweepInterval (bypassed immediately if
// the cache is actually oversized) since it's called after every probe and
// would otherwise be an O(n) scan under the lock on every single write.
// Callers must hold p.mu.
func (p *Prober) evictLocked(now time.Time) {
	oversized := len(p.cache) > maxCacheEntries
	if !oversized && now.Before(p.nextSweep) {
		return
	}
	p.nextSweep = now.Add(evictSweepInterval)

	for k, e := range p.cache {
		// unreachableSince must persist past expiry to track outages across
		// errorTTL cycles, so only reap entries that no longer need that.
		if e.unreachableSince.IsZero() && now.After(e.expires) {
			delete(p.cache, k)
		}
	}
	if len(p.cache) <= maxCacheEntries {
		return
	}
	// Still oversized (e.g. bursts of unique keys within their TTL window):
	// drop arbitrary entries as a last resort so the map can't grow forever.
	for k := range p.cache {
		if len(p.cache) <= maxCacheEntries {
			return
		}
		delete(p.cache, k)
	}
}

// definitivelyGoneMarkers lists stderr substrings from git ls-remote that
// mean "treat this repoPath as gone", triggering immediate fallover instead
// of waiting out unreachableTTL. GitHub (and other hosts) return "repository
// not found" both for a deleted repo and for a private repo the credentials
// can't access — but a fully broken/revoked key surfaces as a distinct SSH-
// level "Permission denied (publickey)" error instead (handled by the
// ambiguous/unreachableTTL path), so in practice "not found" overwhelmingly
// means "doesn't exist here (yet)", which matters for configs that probe the
// new server first and fall back to a trusted old one: treating it as
// ambiguous would serve a broken URL for every not-yet-migrated repo until
// unreachableTTL elapses.
var definitivelyGoneMarkers = []string{
	"does not appear to be a git repository",
	"repository not found",
	"remote: not found",
}

// isDefinitivelyGone reports whether stderr from git ls-remote indicates the
// repo was cleanly rejected (as opposed to a network or auth failure).
func isDefinitivelyGone(stderr string) bool {
	s := strings.ToLower(stderr)
	for _, marker := range definitivelyGoneMarkers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}
