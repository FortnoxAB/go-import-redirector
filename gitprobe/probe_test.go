package gitprobe

import (
	"context"
	"os/exec"
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
