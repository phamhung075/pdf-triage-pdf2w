package mcpserver

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/settings"
)

func TestInMemoryTransportRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.categories.config = testCategoriesConfig()

	server := NewSDKServer(h.deps)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	ctx := context.Background()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect (initialize): %v", err)
	}
	defer clientSession.Close()

	tools, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) != 9 {
		t.Fatalf("tool count = %d, want 9", len(tools.Tools))
	}
	gotNames := map[string]bool{}
	for _, tool := range tools.Tools {
		gotNames[tool.Name] = true
	}
	for _, want := range []string{
		"search_documents", "get_full_document_text", "update_document_metadata", "trigger_triage",
		"list_categories", "prepare_dossier", "get_document_markdown", "open_document_folder", "package_documents",
	} {
		if !gotNames[want] {
			t.Fatalf("tools/list missing %q (got %v)", want, gotNames)
		}
	}

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "list_categories", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool returned isError: %s", resultText(t, result))
	}
	if got := resultText(t, result); !strings.Contains(got, `"invoices"`) {
		t.Fatalf("list_categories text = %q", got)
	}
}

func TestHTTPTransportBearerAuthAndRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.categories.config = testCategoriesConfig()
	token := strings.Repeat("ab", 24) // 48 hex chars, the TS token length

	handler := NewHTTPHandler(h.deps, token)

	// Missing token -> the exact TS 401 body.
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got, want := rec.Body.String(), `{"error":"Unauthorized — missing or invalid Bearer token."}`; got != want {
		t.Fatalf("401 body = %q, want %q", got, want)
	}

	// Wrong token -> still 401.
	req = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("cd", 24))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-token status = %d, want 401", rec.Code)
	}

	// With the token, the SDK client completes initialize + tools/list + tools/call over HTTP.
	ts := httptest.NewServer(handler)
	defer ts.Close()

	httpClient := &http.Client{Transport: bearerRoundTripper{token: token, base: http.DefaultTransport}}
	client := mcp.NewClient(&mcp.Implementation{Name: "http-test-client", Version: "1.0.0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:             ts.URL + "/mcp",
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}
	ctx := context.Background()
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("client.Connect over HTTP: %v", err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools over HTTP: %v", err)
	}
	if len(tools.Tools) != 9 {
		t.Fatalf("tool count = %d, want 9", len(tools.Tools))
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_categories", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool over HTTP: %v", err)
	}
	if result.IsError || !strings.Contains(resultText(t, result), `"invoices"`) {
		t.Fatalf("list_categories result = %q", resultText(t, result))
	}
}

func TestStartDegradesToStdioOnAddrInUse(t *testing.T) {
	h := newHarness(t)
	var logs []string
	stdioCalled := false

	err := Start(context.Background(), h.deps, StartOptions{
		Config:  settings.Config{MCPHTTPPort: 3972, MCPHTTPHost: "127.0.0.1"},
		BaseDir: t.TempDir(),
		Listen: func(network, address string) (net.Listener, error) {
			return nil, &net.OpError{Op: "listen", Net: "tcp", Err: syscall.EADDRINUSE}
		},
		Logf:     func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
		RunStdio: func(ctx context.Context) error { stdioCalled = true; return nil },
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if !stdioCalled {
		t.Fatal("stdio was not started")
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "already in use — continuing with stdio only") {
		t.Fatalf("logs missing degrade message:\n%s", joined)
	}
}

func TestStartPrintsTokenOnceAndUsesInjectedStdio(t *testing.T) {
	h := newHarness(t)
	baseDir := t.TempDir()
	var logs []string

	err := Start(context.Background(), h.deps, StartOptions{
		Config:  settings.Config{MCPHTTPPort: 0, MCPHTTPHost: "127.0.0.1"},
		BaseDir: baseDir,
		Listen: func(network, address string) (net.Listener, error) {
			return net.Listen("tcp", "127.0.0.1:0")
		},
		Logf:     func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
		RunStdio: func(ctx context.Context) error { return nil },
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	token, err := os.ReadFile(TokenPath(baseDir))
	if err != nil {
		t.Fatalf("token file not written: %v", err)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, string(token)) {
		t.Fatalf("token not printed at start:\n%s", joined)
	}
	// The token is printed exactly once, on the Authorization line, matching the TS startup log.
	if strings.Count(joined, string(token)) != 1 {
		t.Fatalf("token printed %d times, want 1:\n%s", strings.Count(joined, string(token)), joined)
	}
}

func TestGetOrCreateToken(t *testing.T) {
	dir := t.TempDir()

	token, created, err := GetOrCreateToken(dir)
	if err != nil {
		t.Fatalf("GetOrCreateToken: %v", err)
	}
	if !created {
		t.Fatal("first call should create the token")
	}
	if len(token) != 48 {
		t.Fatalf("token length = %d, want 48", len(token))
	}
	if _, err := hex.DecodeString(token); err != nil {
		t.Fatalf("token is not hex: %v", err)
	}

	again, createdAgain, err := GetOrCreateToken(dir)
	if err != nil {
		t.Fatalf("GetOrCreateToken: %v", err)
	}
	if again != token || createdAgain {
		t.Fatalf("second call = (%q, %v), want (%q, false)", again, createdAgain, token)
	}

	// A blank file is treated as absent and regenerates.
	if err := os.WriteFile(TokenPath(dir), []byte("  \n"), 0o600); err != nil {
		t.Fatalf("write blank token: %v", err)
	}
	regenerated, createdThird, err := GetOrCreateToken(dir)
	if err != nil {
		t.Fatalf("GetOrCreateToken: %v", err)
	}
	if !createdThird || regenerated == token {
		t.Fatalf("blank token did not regenerate: created=%v equal=%v", createdThird, regenerated == token)
	}
}

func TestTokenMatches(t *testing.T) {
	if !TokenMatches("abc123", "abc123") {
		t.Fatal("equal tokens should match")
	}
	if TokenMatches("abc123", "abc124") {
		t.Fatal("different tokens should not match")
	}
	if TokenMatches("abc", "abcd") {
		t.Fatal("different lengths should not match")
	}
}

type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (b bearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(clone)
}
