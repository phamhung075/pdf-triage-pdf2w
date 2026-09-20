package aiprovider

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHealthClock is an advanceable time source for the cloud health cache TTL checks.
type fakeHealthClock struct {
	mu sync.Mutex
	at time.Time
}

func newFakeHealthClock() *fakeHealthClock {
	return &fakeHealthClock{at: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeHealthClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeHealthClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// newOpenAIManager builds a cloud Manager pointed at the in-process fake upstream and injects the
// fake clock. The provider uses its default HTTP client, which can reach the httptest URL.
func newOpenAIManager(t *testing.T, baseURL string, clock *fakeHealthClock) *Manager {
	t.Helper()
	mgr := NewManager(Config{
		AIProvider:    "cloud",
		CloudProvider: "openai",
		OpenAIAPIKey:  "test-key-one",
		OpenAIModel:   "gpt-4o-mini",
		OpenAIBaseURL: baseURL,
	}, nil)
	mgr.now = clock.now
	return mgr
}

// openAIHealthServer counts upstream calls and answers with either a valid completion or an error.
func openAIHealthServer(t *testing.T, fail bool) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if fail {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid key","type":"auth_error"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestCloudHealthCacheServesWithinTTLThenExpires(t *testing.T) {
	srv, calls := openAIHealthServer(t, false)
	clock := newFakeHealthClock()
	mgr := newOpenAIManager(t, srv.URL, clock)

	first := mgr.CheckModelCanGenerate("gpt-4o-mini", false)
	clock.advance(59 * time.Second)
	second := mgr.CheckModelCanGenerate("gpt-4o-mini", false)

	if !first.OK || !second.OK {
		t.Fatalf("health = %+v / %+v, want both OK", first, second)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("upstream calls within TTL = %d, want 1", got)
	}

	// The freshness check is `< TTL`, so advancing to exactly 60 s expires the entry.
	clock.advance(time.Second)
	third := mgr.CheckModelCanGenerate("gpt-4o-mini", false)
	if !third.OK {
		t.Fatalf("health after TTL = %+v, want OK", third)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("upstream calls after TTL = %d, want 2", got)
	}
}

func TestCloudHealthCacheCachesFailures(t *testing.T) {
	srv, calls := openAIHealthServer(t, true)
	clock := newFakeHealthClock()
	mgr := newOpenAIManager(t, srv.URL, clock)

	first := mgr.CheckModelCanGenerate("gpt-4o-mini", false)
	second := mgr.CheckModelCanGenerate("gpt-4o-mini", false)

	if first.OK || second.OK {
		t.Fatalf("health = %+v / %+v, want both failing", first, second)
	}
	if first.Error == "" || first.Error != second.Error {
		t.Fatalf("errors = %q / %q, want the same cached error text", first.Error, second.Error)
	}
	if !strings.Contains(first.Error, "OpenAI error:") {
		t.Errorf("error = %q, want the provider label and error", first.Error)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("upstream calls for a cached failure = %d, want 1", got)
	}
}

func TestCloudHealthForceRefreshBypassesCache(t *testing.T) {
	srv, calls := openAIHealthServer(t, false)
	clock := newFakeHealthClock()
	mgr := newOpenAIManager(t, srv.URL, clock)

	if h := mgr.CheckModelCanGenerate("gpt-4o-mini", false); !h.OK {
		t.Fatalf("first health = %+v, want OK", h)
	}
	// forceRefresh must hit the upstream again even though the cache is fresh.
	if h := mgr.CheckModelCanGenerate("gpt-4o-mini", true); !h.OK {
		t.Fatalf("forceRefresh health = %+v, want OK", h)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("upstream calls after forceRefresh = %d, want 2", got)
	}
	// The forced call refreshed the cache, so an immediate normal call is served from it.
	if h := mgr.CheckModelCanGenerate("gpt-4o-mini", false); !h.OK {
		t.Fatalf("post-refresh health = %+v, want OK", h)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("upstream calls after refresh + cached call = %d, want 2", got)
	}
}

func TestCloudHealthCacheMissesOnKeyAndModelChange(t *testing.T) {
	srv, calls := openAIHealthServer(t, false)
	clock := newFakeHealthClock()
	mgr := newOpenAIManager(t, srv.URL, clock)

	if h := mgr.CheckModelCanGenerate("gpt-4o-mini", false); !h.OK {
		t.Fatalf("first health = %+v, want OK", h)
	}

	// A different API key misses the cache.
	mgr.UpdateConfig(Config{
		AIProvider: "cloud", CloudProvider: "openai",
		OpenAIAPIKey: "test-key-two", OpenAIModel: "gpt-4o-mini", OpenAIBaseURL: srv.URL,
	})
	if h := mgr.CheckModelCanGenerate("gpt-4o-mini", false); !h.OK {
		t.Fatalf("health after key change = %+v, want OK", h)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("upstream calls after key change = %d, want 2", got)
	}

	// A different model misses it again.
	mgr.UpdateConfig(Config{
		AIProvider: "cloud", CloudProvider: "openai",
		OpenAIAPIKey: "test-key-two", OpenAIModel: "gpt-4o", OpenAIBaseURL: srv.URL,
	})
	if h := mgr.CheckModelCanGenerate("gpt-4o", false); !h.OK {
		t.Fatalf("health after model change = %+v, want OK", h)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Fatalf("upstream calls after model change = %d, want 3", got)
	}

	// The new key/model pair is now cached.
	if h := mgr.CheckModelCanGenerate("gpt-4o", false); !h.OK {
		t.Fatalf("health on cached new pair = %+v, want OK", h)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Fatalf("upstream calls after caching the new pair = %d, want 3", got)
	}
}

func TestCloudHealthCacheNeverStoresKeyText(t *testing.T) {
	srv, _ := openAIHealthServer(t, false)
	clock := newFakeHealthClock()
	const secret = "super-secret-api-key"
	mgr := NewManager(Config{
		AIProvider: "cloud", CloudProvider: "openai",
		OpenAIAPIKey: secret, OpenAIModel: "gpt-4o-mini", OpenAIBaseURL: srv.URL,
	}, nil)
	mgr.now = clock.now

	if h := mgr.CheckModelCanGenerate("gpt-4o-mini", false); !h.OK {
		t.Fatalf("health = %+v, want OK", h)
	}
	if strings.Contains(mgr.healthKey, secret) {
		t.Fatalf("cache key %q stores the API key in clear text", mgr.healthKey)
	}
}

func TestCloudHealthCacheSingleFlight(t *testing.T) {
	// A delay widens the window so the goroutines really overlap on the same key.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(30 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	clock := newFakeHealthClock()
	mgr := newOpenAIManager(t, srv.URL, clock)

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if h := mgr.CheckModelCanGenerate("gpt-4o-mini", false); !h.OK {
				t.Errorf("health = %+v, want OK", h)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream calls under %d concurrent misses = %d, want 1", goroutines, got)
	}
}
