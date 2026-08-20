package githubprobe

import (
	"context"
	"log"
	"os/exec"
	"sync"
	"time"
)

type cacheEntry struct {
	exists  bool
	expires time.Time
}

// Prober checks git repo existence via git ls-remote and caches results.
type Prober struct {
	ttl     time.Duration
	timeout time.Duration
	mu      sync.RWMutex
	cache   map[string]cacheEntry
}

// New returns a Prober that uses git ls-remote to check repo reachability.
func New(ttl, timeout time.Duration) *Prober {
	return &Prober{ttl: ttl, timeout: timeout, cache: make(map[string]cacheEntry)}
}

// Probe returns true if repoURL is reachable. Results are cached for the TTL.
func (p *Prober) Probe(ctx context.Context, repoURL string) bool {
	if exists, hit := p.cacheGet(repoURL); hit {
		return exists
	}
	exists := p.probe(ctx, repoURL)
	p.cacheSet(repoURL, exists)
	return exists
}

func (p *Prober) probe(ctx context.Context, repoURL string) bool {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	err := exec.CommandContext(ctx, "git", "ls-remote", repoURL).Run()
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("gitprobe: %s: %v", repoURL, err)
		}
		return false
	}
	return true
}

func (p *Prober) cacheGet(repoURL string) (exists bool, hit bool) {
	p.mu.RLock()
	e, ok := p.cache[repoURL]
	p.mu.RUnlock()
	if !ok || time.Now().After(e.expires) {
		return false, false
	}
	return e.exists, true
}

func (p *Prober) cacheSet(repoURL string, exists bool) {
	p.mu.Lock()
	p.cache[repoURL] = cacheEntry{exists: exists, expires: time.Now().Add(p.ttl)}
	p.mu.Unlock()
}
