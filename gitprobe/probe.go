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
	result bool // set before done is closed
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
		inflight:       make(map[string]*probeCall),
		sem:            make(chan struct{}, maxConcurrentProbes),
	}
}

// Probe returns true if the repo at repoURL still exists.
// false means definitively gone or unreachable long enough to assume migration.
//
// ctx only bounds how long this caller waits: the git probe itself runs
// detached from any caller, so one client disconnecting can neither abort a
// probe other callers have joined nor leave them with a result that was never
// actually probed. A canceled caller gets the last known result instead.
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
		go p.run(c, repoURL, e.unreachableSince)
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
func (p *Prober) run(c *probeCall, repoURL string, unreachableSince time.Time) {
	c.result = p.probe(repoURL, unreachableSince)
	p.mu.Lock()
	delete(p.inflight, repoURL)
	p.mu.Unlock()
	close(c.done)
}

// lastKnown is the answer when no fresh probe result is available: the
// cached result if there is one, else the documented safe default "exists".
func lastKnown(e cacheEntry, ok bool) bool {
	if ok {
		return e.exists
	}
	return true
}

func (p *Prober) probe(repoURL string, unreachableSince time.Time) bool {
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
		// touch the cache or outage tracking.
		p.mu.Lock()
		e, ok := p.cache[repoURL]
		p.mu.Unlock()
		return lastKnown(e, ok)
	}

	timeoutCtx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	logVerbose("gitprobe: probing %s", redactForLog(repoURL))

	var stderr bytes.Buffer
	// The HEAD pattern keeps the reply to one ref; existence is all we need,
	// not every branch, tag and refs/pull/* of the repo.
	cmd := exec.CommandContext(timeoutCtx, "git", "ls-remote", "--", repoURL, "HEAD")
	cmd.Env = append(os.Environ(), probeEnv...)
	cmd.Stderr = &stderr
	cmd.WaitDelay = killWaitDelay
	isolate(cmd)
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
		// Hosts also say "not found" when the key can't read a private repo,
		// which can't be told apart here; say so in the log.
		log.Printf("gitprobe: %s: not found (or no read access)", redactForLog(repoURL))
		p.mu.Lock()
		p.cache[repoURL] = cacheEntry{exists: false, expires: now.Add(p.ttl)}
		p.evictLocked(now)
		p.mu.Unlock()
		return false
	}

	// A misconfigured probe (unknown host key, rejected key or credentials)
	// is answered by a reachable server and says nothing about whether the
	// repo moved: keep serving the probed entry and don't start or advance
	// outage tracking, so fixing the config rather than a silent fallover
	// after unreachableTTL is what resolves it.
	detail := logSafeStderr(stderr.String(), repoURL)
	if isMisconfiguration(stderr.String()) {
		log.Printf("gitprobe: %s: probe misconfigured, not counted as an outage: %v: %s", redactForLog(repoURL), err, detail)
		assumeGone := !unreachableSince.IsZero() && now.Sub(unreachableSince) >= p.unreachableTTL
		p.mu.Lock()
		p.cache[repoURL] = cacheEntry{exists: !assumeGone, expires: now.Add(p.errorTTL), unreachableSince: unreachableSince}
		p.evictLocked(now)
		p.mu.Unlock()
		return !assumeGone
	}

	// Ambiguous error (network, timeout): safe default is "still exists"
	// unless we've been failing continuously longer than unreachableTTL.
	log.Printf("gitprobe: %s: %v: %s", redactForLog(repoURL), err, detail)
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
// misconfiguration path), so in practice "not found" overwhelmingly
// means "doesn't exist here (yet)", which matters for configs that probe the
// new server first and fall back to a trusted old one: treating it as
// ambiguous would serve a broken URL for every not-yet-migrated repo until
// unreachableTTL elapses.
//
// "could not read username" is the HTTPS counterpart: a host that answers a
// missing (or unreadable private) repo with 401 instead of 404, e.g. GitLab,
// makes git ask for credentials, which probeEnv forbids. Same trade-off.
var definitivelyGoneMarkers = []string{
	"does not appear to be a git repository",
	"repository not found",
	"remote: not found",
	"could not read username",
}

// misconfigurationMarkers lists stderr substrings from git ls-remote that
// mean the probe itself is misconfigured for the whole host (unknown host
// key, SSH key or HTTPS credentials rejected before any repo lookup), as
// opposed to the host being unreachable or the repo being gone.
var misconfigurationMarkers = []string{
	"host key verification failed",
	"permission denied (publickey",
	"authentication failed",
}

// isDefinitivelyGone reports whether stderr from git ls-remote indicates the
// repo was cleanly rejected (as opposed to a network or auth failure).
func isDefinitivelyGone(stderr string) bool {
	return containsAnyFold(stderr, definitivelyGoneMarkers)
}

// isMisconfiguration reports whether stderr from git ls-remote indicates the
// probe's SSH/HTTPS setup was rejected by the host.
func isMisconfiguration(stderr string) bool {
	return containsAnyFold(stderr, misconfigurationMarkers)
}

// containsAnyFold reports whether s contains any of the lowercase markers,
// ignoring case.
func containsAnyFold(s string, markers []string) bool {
	s = strings.ToLower(s)
	for _, marker := range markers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}
