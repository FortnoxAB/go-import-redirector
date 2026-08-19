package githubprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
)

type cacheEntry struct {
	repoURL string
	found   bool
	expires time.Time
}

// Prober checks GitHub for repo existence and caches results.
type Prober struct {
	// token auth (New)
	token  string
	client *http.Client

	// app auth (NewWithAppKey)
	appID          int64
	privateKey     []byte
	appClient      *http.Client // JWT transport for app-level API calls
	installMu      sync.Mutex
	installClients map[string]*http.Client

	baseURL string
	timeout time.Duration
	ttl     time.Duration
	mu      sync.RWMutex
	cache   map[string]cacheEntry
}

// New returns a Prober authenticated with token.
// An empty token makes unauthenticated requests (strict rate limit applies).
func New(token string, ttl, timeout time.Duration) *Prober {
	return &Prober{
		token:   token,
		baseURL: "https://api.github.com",
		client:  &http.Client{Timeout: timeout},
		ttl:     ttl,
		cache:   make(map[string]cacheEntry),
	}
}

// NewWithAppKey returns a Prober that authenticates as a GitHub App.
// It discovers the installation for each org on first use and caches the per-org client.
func NewWithAppKey(appID int64, privateKeyPEM []byte, ttl, timeout time.Duration) (*Prober, error) {
	appTr, err := ghinstallation.NewAppsTransport(http.DefaultTransport, appID, privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parsing GitHub App private key: %w", err)
	}
	return &Prober{
		baseURL:        "https://api.github.com",
		appID:          appID,
		privateKey:     privateKeyPEM,
		appClient:      &http.Client{Transport: appTr, Timeout: timeout},
		timeout:        timeout,
		ttl:            ttl,
		cache:          make(map[string]cacheEntry),
		installClients: make(map[string]*http.Client),
	}, nil
}

// Lookup returns the GitHub HTTPS URL if repo exists in org.
// Returns "", false on any failure or miss; errors are logged, never returned.
func (p *Prober) Lookup(ctx context.Context, org, repo string) (string, bool) {
	if url, found, hit := p.cacheGet(org, repo); hit {
		return url, found
	}
	url, found := p.probe(ctx, org, repo)
	p.cacheSet(org, repo, url, found)
	return url, found
}

func (p *Prober) probe(ctx context.Context, org, repo string) (string, bool) {
	var client *http.Client
	if p.appID != 0 {
		c, err := p.installClientForOrg(ctx, org)
		if err != nil {
			log.Printf("githubprobe: %v", err)
			return "", false
		}
		client = c
	} else {
		client = p.client
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/repos/%s/%s", p.baseURL, org, repo), nil)
	if err != nil {
		log.Printf("githubprobe: build request %s/%s: %v", org, repo, err)
		return "", false
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("githubprobe: probe %s/%s: %v", org, repo, err)
		return "", false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusOK {
		return fmt.Sprintf("https://github.com/%s/%s", org, repo), true
	}
	if resp.StatusCode != http.StatusNotFound {
		log.Printf("githubprobe: %s/%s returned HTTP %d", org, repo, resp.StatusCode)
	}
	return "", false
}

// installClientForOrg returns a per-org installation HTTP client, creating and caching it on first call.
func (p *Prober) installClientForOrg(ctx context.Context, org string) (*http.Client, error) {
	p.installMu.Lock()
	defer p.installMu.Unlock()
	if c, ok := p.installClients[org]; ok {
		return c, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/orgs/%s/installation", p.baseURL, org), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := p.appClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("looking up installation for %s: %w", org, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("no installation found for org %s (HTTP %d)", org, resp.StatusCode)
	}
	var result struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding installation for %s: %w", org, err)
	}
	tr, err := ghinstallation.New(http.DefaultTransport, p.appID, result.ID, p.privateKey)
	if err != nil {
		return nil, fmt.Errorf("creating installation transport for %s: %w", org, err)
	}
	c := &http.Client{Transport: tr, Timeout: p.timeout}
	p.installClients[org] = c
	return c, nil
}

func (p *Prober) cacheGet(org, repo string) (url string, found bool, hit bool) {
	p.mu.RLock()
	e, ok := p.cache[org+"/"+repo]
	p.mu.RUnlock()
	if !ok || time.Now().After(e.expires) {
		return "", false, false
	}
	return e.repoURL, e.found, true
}

func (p *Prober) cacheSet(org, repo, url string, found bool) {
	p.mu.Lock()
	p.cache[org+"/"+repo] = cacheEntry{repoURL: url, found: found, expires: time.Now().Add(p.ttl)}
	p.mu.Unlock()
}
