package githubprobe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type testGitHub struct {
	mu        sync.Mutex
	responses map[string]int // "org/repo" → status code; default 404
	calls     map[string]int
}

func (g *testGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/repos/")
	g.mu.Lock()
	g.calls[path]++
	code, ok := g.responses[path]
	g.mu.Unlock()
	if !ok {
		code = http.StatusNotFound
	}
	w.WriteHeader(code)
}

func (g *testGitHub) callCount(org, repo string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[org+"/"+repo]
}

func newTestProber(t *testing.T, responses map[string]int, ttl time.Duration) (*Prober, *testGitHub) {
	t.Helper()
	gh := &testGitHub{responses: responses, calls: make(map[string]int)}
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	p := New("test-token", ttl, 2*time.Second)
	p.baseURL = srv.URL
	return p, gh
}

func TestLookupFound(t *testing.T) {
	p, _ := newTestProber(t, map[string]int{"orgA/myrepo": 200}, time.Hour)
	url, ok := p.Lookup(context.Background(), "orgA", "myrepo")
	if !ok {
		t.Fatal("expected found")
	}
	if url != "https://github.com/orgA/myrepo" {
		t.Errorf("url = %q", url)
	}
}

func TestLookupNotFound(t *testing.T) {
	p, _ := newTestProber(t, nil, time.Hour)
	_, ok := p.Lookup(context.Background(), "orgA", "myrepo")
	if ok {
		t.Fatal("expected not found")
	}
}

func TestLookupCachesHit(t *testing.T) {
	p, gh := newTestProber(t, map[string]int{"orgA/myrepo": 200}, time.Hour)
	p.Lookup(context.Background(), "orgA", "myrepo")
	p.Lookup(context.Background(), "orgA", "myrepo")
	if gh.callCount("orgA", "myrepo") != 1 {
		t.Errorf("expected 1 HTTP call, got %d", gh.callCount("orgA", "myrepo"))
	}
}

func TestLookupCachesMiss(t *testing.T) {
	p, gh := newTestProber(t, nil, time.Hour)
	p.Lookup(context.Background(), "orgA", "myrepo")
	p.Lookup(context.Background(), "orgA", "myrepo")
	if gh.callCount("orgA", "myrepo") != 1 {
		t.Errorf("negative result should be cached; expected 1 call, got %d", gh.callCount("orgA", "myrepo"))
	}
}

func TestLookupCacheExpiry(t *testing.T) {
	p, gh := newTestProber(t, map[string]int{"orgA/myrepo": 200}, 10*time.Millisecond)
	p.Lookup(context.Background(), "orgA", "myrepo")
	time.Sleep(20 * time.Millisecond)
	p.Lookup(context.Background(), "orgA", "myrepo")
	if gh.callCount("orgA", "myrepo") != 2 {
		t.Errorf("expected re-probe after expiry, got %d calls", gh.callCount("orgA", "myrepo"))
	}
}

func TestLookupErrorTreatedAsNotFound(t *testing.T) {
	p, _ := newTestProber(t, map[string]int{"orgA/myrepo": 500}, time.Hour)
	_, ok := p.Lookup(context.Background(), "orgA", "myrepo")
	if ok {
		t.Error("500 should be treated as not-found")
	}
}

func TestLookupContextCancelled(t *testing.T) {
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(slow)
	t.Cleanup(srv.Close)
	p := New("", time.Hour, 2*time.Second)
	p.baseURL = srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, ok := p.Lookup(ctx, "orgA", "myrepo")
	if ok {
		t.Error("expected not found on cancelled context")
	}
}

func TestLookupConcurrency(t *testing.T) {
	p, _ := newTestProber(t, map[string]int{"orgA/myrepo": 200}, time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Lookup(context.Background(), "orgA", "myrepo")
		}()
	}
	wg.Wait()
}
