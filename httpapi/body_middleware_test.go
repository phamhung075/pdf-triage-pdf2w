package httpapi

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestJSONBodyMiddleware pins the express.json() contract fixed by audit gap G1/G3:
//   - only application/json is parsed (the project's body-parser default type is exactly that, and
//     NOT application/vnd.*+json);
//   - the body is left unread for every other content type, so the octet-stream import owns its own
//     stream and 64 MB limit;
//   - malformed JSON and a non-object/array top level are rejected with 400 before the handler runs.
func TestJSONBodyMiddleware(t *testing.T) {
	t.Run("parses application/json and exposes the raw bytes", func(t *testing.T) {
		called := false
		handler := jsonBodyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			if got := string(bodyBytes(r)); got != `{"a":1}` {
				t.Errorf("bodyBytes = %q", got)
			}
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"a":1}`))
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if !called || rec.Code != http.StatusOK {
			t.Fatalf("called=%v status=%d", called, rec.Code)
		}
	})

	t.Run("leaves a non-JSON body unread for the route", func(t *testing.T) {
		for _, contentType := range []string{"application/octet-stream", "text/plain", "application/vnd.api+json"} {
			called := false
			var got []byte
			handler := jsonBodyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				got, _ = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader([]byte("raw-bytes")))
			req.Header.Set("Content-Type", contentType)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if !called {
				t.Fatalf("%s: handler not called", contentType)
			}
			if string(got) != "raw-bytes" {
				t.Fatalf("%s: body read by middleware, got %q", contentType, got)
			}
		}
	})

	t.Run("rejects malformed JSON before the handler", func(t *testing.T) {
		called := false
		handler := jsonBodyMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
		req := httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(`{"enabled":true`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if called {
			t.Fatal("handler ran on malformed JSON")
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
			t.Fatalf("Content-Type = %q", ct)
		}
		if !strings.Contains(rec.Body.String(), `"error"`) {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})

	t.Run("rejects a scalar top-level body (strict mode) before the handler", func(t *testing.T) {
		for _, payload := range []string{"5", `"x"`, "true", "null"} {
			called := false
			handler := jsonBodyMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
			req := httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if called || rec.Code != http.StatusBadRequest {
				t.Fatalf("payload %q: called=%v status=%d", payload, called, rec.Code)
			}
		}
	})

	t.Run("caps application/json at 100 kB", func(t *testing.T) {
		called := false
		handler := jsonBodyMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
		req := httptest.NewRequest(http.MethodPut, "/x", bytes.NewReader(make([]byte, maxJSONBody+1)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if called || rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("called=%v status=%d", called, rec.Code)
		}
	})
}

// TestMalformedJSONRejectedOnComposedRoutes pins audit G3 at the composed handler: the middleware
// runs before every route, so PUT /api/manual-decisions/{id} (which previously swallowed the
// unmarshal error and answered 200) now gets 400 and never touches the store.
func TestMalformedJSONRejectedOnComposedRoutes(t *testing.T) {
	env := newWriteEnv()
	req := httptest.NewRequest(http.MethodPut, "/api/manual-decisions/3", strings.NewReader(`{"enabled":true`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if env.base.decisions.updateCalls != 0 {
		t.Fatalf("store called %d times on malformed JSON", env.base.decisions.updateCalls)
	}
	if body := decodeJSON(t, rec); body["error"] == nil {
		t.Fatalf("body = %v", body)
	}

	cases := []struct {
		method string
		target string
	}{
		{http.MethodPut, "/api/config"},
		{http.MethodPut, "/api/categories"},
		{http.MethodPost, "/api/pdf/merge"},
		{http.MethodPost, "/api/chat"},
		{http.MethodPost, "/api/subcategories/rename"},
		{http.MethodPut, "/api/documents/1"},
		{http.MethodPost, "/api/documents/1/relocalize"},
		{http.MethodPost, "/api/open-location"},
		{http.MethodPost, "/api/open-chrome"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.target, strings.NewReader("{not json"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s %s: status = %d body=%s", tc.method, tc.target, rec.Code, rec.Body.String())
		}
	}
}
