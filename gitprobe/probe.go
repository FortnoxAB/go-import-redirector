package gitprobe

import (
	"bytes"
	"context"
	"log"
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
	}
}

// Probe returns true if the repo at repoURL still exists.
// false means definitively gone or unreachable long enough to assume migration.
func (p *Prober) Probe(ctx context.Context, repoURL string) bool {
	p.mu.Lock()
	e, ok := p.cache[repoURL]
	unreachableSince := e.unreachableSince
	if ok && time.Now().Before(e.expires) {
		p.mu.Unlock()
		return e.exists
	}
	p.mu.Unlock()
	return p.probe(ctx, repoURL, unreachableSince)
}

func (p *Prober) probe(ctx context.Context, repoURL string, unreachableSince time.Time) bool {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", "ls-remote", repoURL)
	cmd.Stderr = &stderr
	err := cmd.Run()

	now := time.Now()
	if err == nil {
		p.mu.Lock()
		p.cache[repoURL] = cacheEntry{exists: true, expires: now.Add(p.ttl)}
		p.evictLocked(now)
		p.mu.Unlock()
		return true
	}

	// Definitively gone: repo not found (not a transient error)
	if isDefinitivelyGone(stderr.String()) {
		log.Printf("gitprobe: %s: not found", repoURL)
		p.mu.Lock()
		p.cache[repoURL] = cacheEntry{exists: false, expires: now.Add(p.ttl)}
		p.evictLocked(now)
		p.mu.Unlock()
		return false
	}

	// Ambiguous error (network, auth, timeout): safe default is "still exists"
	// unless we've been failing continuously longer than unreachableTTL.
	if ctx.Err() == nil {
		log.Printf("gitprobe: %s: %v", repoURL, err)
	}
	if unreachableSince.IsZero() {
		unreachableSince = now
	}
	assumeGone := now.Sub(unreachableSince) >= p.unreachableTTL
	if assumeGone {
		log.Printf("gitprobe: %s unreachable since %s, assuming migrated", repoURL, unreachableSince.Format(time.RFC3339))
	}
	p.mu.Lock()
	p.cache[repoURL] = cacheEntry{exists: !assumeGone, expires: now.Add(p.errorTTL), unreachableSince: unreachableSince}
	p.evictLocked(now)
	p.mu.Unlock()
	return !assumeGone
}

// evictLocked removes entries that no longer need tracking, then—if the
// cache is still oversized—falls back to dropping the oldest entries so
// memory use stays bounded regardless of how many distinct keys are probed.
// Callers must hold p.mu.
func (p *Prober) evictLocked(now time.Time) {
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

// isDefinitivelyGone reports whether stderr from git ls-remote indicates the
// repo was cleanly rejected (as opposed to a network or auth failure).
// Deliberately excludes "repository not found"/"remote: not found": GitHub
// (and other hosts) return that exact message both for a deleted repo and
// for a private repo the credentials can't access, so it's ambiguous and
// left to the unreachableTTL path instead of being treated as instant-gone.
func isDefinitivelyGone(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "does not appear to be a git repository")
}
