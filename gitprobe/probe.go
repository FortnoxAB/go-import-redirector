package gitprobe

import (
	"bytes"
	"context"
	"log"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode"
)

type cacheEntry struct {
	exists  bool
	expires time.Time
}

// maxCacheEntries bounds cache size so an attacker requesting many distinct
// wildcard names cannot grow the map without limit.
const maxCacheEntries = 10000

// maxConcurrentProbes bounds how many git subprocesses can run at once, so
// requests for many distinct wildcard names can't exhaust process/CPU/memory.
const maxConcurrentProbes = 32

// maxPendingProbes bounds how many distinct URLs can have a probe in flight
// (running or waiting for one of the maxConcurrentProbes slots). Beyond it,
// Probe answers from the last known result instead of queueing more work.
const maxPendingProbes = 4 * maxConcurrentProbes

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

// killWaitDelay is how long cmd.Run may keep waiting for git's stdio to close
// after the probe timeout killed it. Without it, a descendant that escaped
// the kill (and still holds stderr) would stall the probe until it exits.
const killWaitDelay = time.Second

// maxLoggedStderr caps how much of git's stderr goes into a single log line.
const maxLoggedStderr = 300

// probeEnv is appended to the environment of every git subprocess so a probe
// fails fast instead of waiting for input nobody will give: git must not ask
// for HTTPS credentials, and ssh / Git Credential Manager must not fall back
// to a GUI askpass helper (ssh already can't use a terminal, see isolate).
var probeEnv = []string{
	"GIT_TERMINAL_PROMPT=0",
	"SSH_ASKPASS_REQUIRE=never",
	"GCM_INTERACTIVE=never",
}

// Defaults for how hard a probe tries before it gives up. A failed ls-remote
// is retried so a short blip (network, server hiccup) isn't mistaken for the
// repo being gone, which would then be cached for the full ttl.
const (
	defaultProbeAttempts = 3
	defaultRetryDelay    = 500 * time.Millisecond
)

// evictSweepInterval throttles the full-map eviction scan in evictLocked so
// it runs periodically rather than on every single cache write.
const evictSweepInterval = time.Minute

// Prober checks git repo existence via git ls-remote and caches results.
// A successful ls-remote means the repo exists. A failed one is retried, and
// once every attempt failed, whatever the reason, the repo counts as not
// there. Either result is cached for ttl.
type Prober struct {
	ttl        time.Duration
	timeout    time.Duration
	attempts   int           // ls-remote attempts per probe
	retryDelay time.Duration // pause between attempts
	mu         sync.Mutex
	cache      map[string]cacheEntry
	inflight   map[string]*probeCall // serializes concurrent probes of the same URL
	sem        chan struct{}         // bounds concurrent git subprocesses across all URLs
	nextSweep  time.Time             // evictLocked skips the full scan until this time, unless oversized
}

// probeCall lets concurrent callers for the same URL share one in-flight
// probe instead of racing to write the cache entry out of order.
type probeCall struct {
	done   chan struct{}
	result bool // set before done is closed
}

// New returns a Prober that caches probe results for ttl and gives each git
// ls-remote at most timeout to finish.
func New(ttl, timeout time.Duration) *Prober {
	return &Prober{
		ttl:        ttl,
		timeout:    timeout,
		attempts:   defaultProbeAttempts,
		retryDelay: defaultRetryDelay,
		cache:      make(map[string]cacheEntry),
		inflight:   make(map[string]*probeCall),
		sem:        make(chan struct{}, maxConcurrentProbes),
	}
}

// Probe returns true if git ls-remote of repoURL succeeds, false if every
// attempt fails, for any reason.
//
// ctx only bounds how long this caller waits: the git probe itself runs
// detached from any caller, so one client disconnecting can neither abort a
// probe other callers have joined nor leave them with a result that was never
// actually probed. A canceled caller gets the last known result instead, or
// false if there is none.
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
	c, inflight := p.inflight[repoURL]
	if !inflight {
		// Probes outlive their callers, so without a cap a flood of distinct
		// wildcard names would pile up goroutines waiting for a probe slot.
		if len(p.inflight) >= maxPendingProbes {
			p.mu.Unlock()
			logVerbose("gitprobe: %s: %d probes already pending, using last known result", redactForLog(repoURL), maxPendingProbes)
			return lastKnown(e, ok)
		}
		c = &probeCall{done: make(chan struct{})}
		p.inflight[repoURL] = c
		go p.run(c, repoURL)
	}
	p.mu.Unlock()

	select {
	case <-c.done:
		return c.result
	case <-ctx.Done():
		p.mu.Lock()
		e, ok := p.cache[repoURL]
		p.mu.Unlock()
		return lastKnown(e, ok)
	}
}

// run performs the probe for c and publishes its result to every waiter.
func (p *Prober) run(c *probeCall, repoURL string) {
	c.result = p.probe(repoURL)
	p.mu.Lock()
	delete(p.inflight, repoURL)
	p.mu.Unlock()
	close(c.done)
}

// lastKnown is the answer when no fresh probe result is available: the
// cached result if there is one, else false, since no answer counts as not
// found.
func lastKnown(e cacheEntry, ok bool) bool {
	return ok && e.exists
}

func (p *Prober) probe(repoURL string) bool {
	// Bound the wait for a free probe slot by p.timeout too, so a probe can't
	// sit pending forever when all slots are busy. This budget is only for
	// acquiring a slot: the git subprocess below gets its own fresh p.timeout
	// once started, so slot contention can never eat into (and thus falsely
	// time out) the actual probe.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), p.timeout)
	defer waitCancel()

	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-waitCtx.Done():
		// No free probe slot in time; says nothing about the repo, so don't
		// touch the cache.
		p.mu.Lock()
		e, ok := p.cache[repoURL]
		p.mu.Unlock()
		return lastKnown(e, ok)
	}

	var err error
	var stderr string
	for attempt := 1; attempt <= p.attempts; attempt++ {
		if attempt > 1 {
			time.Sleep(p.retryDelay)
		}
		logVerbose("gitprobe: probing %s (attempt %d/%d)", redactForLog(repoURL), attempt, p.attempts)
		if stderr, err = p.lsRemote(repoURL); err == nil {
			break
		}
		logVerbose("gitprobe: %s: attempt %d/%d failed: %v: %s", redactForLog(repoURL), attempt, p.attempts, err, logSafeStderr(stderr, repoURL))
	}

	exists := err == nil
	if exists {
		logVerbose("gitprobe: %s: found", redactForLog(repoURL))
	} else {
		log.Printf("gitprobe: %s: not found after %d attempts: %v: %s", redactForLog(repoURL), p.attempts, err, logSafeStderr(stderr, repoURL))
	}
	now := time.Now()
	p.mu.Lock()
	p.cache[repoURL] = cacheEntry{exists: exists, expires: now.Add(p.ttl)}
	p.evictLocked(now)
	p.mu.Unlock()
	return exists
}

// lsRemote runs one git ls-remote of repoURL and returns its stderr and error.
func (p *Prober) lsRemote(repoURL string) (string, error) {
	timeoutCtx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	var stderr bytes.Buffer
	// The HEAD pattern keeps the reply to one ref; existence is all we need,
	// not every branch, tag and refs/pull/* of the repo.
	cmd := exec.CommandContext(timeoutCtx, "git", "ls-remote", "--", repoURL, "HEAD")
	cmd.Env = append(os.Environ(), probeEnv...)
	cmd.Stderr = &stderr
	cmd.WaitDelay = killWaitDelay
	isolate(cmd)
	err := cmd.Run()
	return stderr.String(), err
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

// logSafeStderr flattens git's stderr into one bounded line with control
// characters removed (so it can't forge extra log lines) and repoURL's
// credentials redacted, in case git echoes the URL back.
func logSafeStderr(stderr, repoURL string) string {
	s := strings.ReplaceAll(stderr, repoURL, redactForLog(repoURL))
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxLoggedStderr {
		s = string(r[:maxLoggedStderr]) + "..."
	}
	return s
}

// evictLocked removes expired entries, then—if the
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
		if now.After(e.expires) {
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
