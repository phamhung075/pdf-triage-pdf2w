package httpapi

import (
	"net/http"
	"testing"
)

// hookTestCalls counts how many times registerHookTest has run, so the test can assert the hook
// fires exactly once per NewServer call.
var hookTestCalls int

// registerHookTest is the method a real route-group file would contribute: a method expression
// appended to routeGroupHooks from init(), adding its routes to the server's mux.
func (s *Server) registerHookTest() {
	hookTestCalls++
	s.mux.HandleFunc("/api/hook-test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hook-ok"))
	})
}

// TestRouteGroupHookRunsOncePerServerAndServes proves that a hook appended to routeGroupHooks runs
// exactly once for each NewServer call, in order, and that the route it registers is reachable
// through the finished handler. The slice is saved and restored so the hook does not leak into
// other tests.
func TestRouteGroupHookRunsOncePerServerAndServes(t *testing.T) {
	saved := routeGroupHooks
	hookTestCalls = 0
	t.Cleanup(func() {
		routeGroupHooks = saved
		hookTestCalls = 0
	})

	routeGroupHooks = append(routeGroupHooks, (*Server).registerHookTest)

	env := newTestEnv()
	if hookTestCalls != 1 {
		t.Fatalf("hook calls after first NewServer = %d, want 1", hookTestCalls)
	}

	rec := doJSON(t, env.handler, http.MethodGet, "/api/hook-test", nil, nil)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("GET /api/hook-test status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	if body := rec.Body.String(); body != "hook-ok" {
		t.Fatalf("GET /api/hook-test body = %q, want %q", body, "hook-ok")
	}

	NewServer(testDeps(env))
	if hookTestCalls != 2 {
		t.Fatalf("hook calls after second NewServer = %d, want 2 (exactly once per call)", hookTestCalls)
	}
}
