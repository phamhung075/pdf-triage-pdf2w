package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/settings"
)

// NewSDKServer builds an MCP server exposing the nine tools. Each connection gets its own instance
// (the SDK's Server.Connect is 1:1 with a transport): stdio keeps one for the process lifetime and
// every stateless HTTP request creates one, mirroring createToolServer() (mcp-server.ts:480).
func NewSDKServer(deps Deps) *mcp.Server {
	handler := NewHandler(deps)
	server := mcp.NewServer(&mcp.Implementation{Name: ServerName, Version: ServerVersion}, nil)
	for _, tool := range ToolDefinitions() {
		server.AddTool(tool, handler.ToolHandler())
	}
	return server
}

// NewHTTPHandler is the Streamable HTTP surface: POST /mcp, stateless per request, behind the
// bearer-token check. It is exported so tests can drive it with httptest / an ephemeral listener
// without going through Start.
func NewHTTPHandler(deps Deps, token string) http.Handler {
	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return NewSDKServer(deps) },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
	mux := http.NewServeMux()
	mux.Handle("/mcp", streamable)
	return bearerAuth(token, mux)
}

// bearerAuth ports the Express `app.post('/mcp')` authorization gate (mcp-server.ts:516-524).
func bearerAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := extractBearerToken(r.Header.Get("Authorization"))
		if !TokenMatches(presented, token) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Unauthorized — missing or invalid Bearer token."}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// StartOptions carries Start's injectable seams. Zero values fall back to the real implementations
// (net.Listen, the SDK stdio transport, os.Stderr logging), so tests never bind a real port and
// never open stdin/stdout.
type StartOptions struct {
	// Config overrides deps.Config when non-zero (MCP_HTTP_PORT / MCP_HTTP_HOST are read from it).
	Config settings.Config
	// BaseDir holds .mcp-api-token. Defaults to deps.BaseDir, then Config.CategoriesFile's dir.
	BaseDir string
	Logf    func(format string, args ...any)
	// Listen defaults to net.Listen. Tests pass a fake returning EADDRINUSE, or a real
	// 127.0.0.1:0 listener.
	Listen func(network, address string) (net.Listener, error)
	// RunStdio defaults to running the SDK stdio session. It blocks until the client disconnects
	// or ctx is cancelled; tests replace it so no stdin is touched.
	RunStdio func(ctx context.Context) error
	// Handler overrides the HTTP handler (tests).
	Handler http.Handler
}

func (o StartOptions) withDefaults(deps Deps) StartOptions {
	if o.Logf == nil {
		o.Logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	}
	if o.Listen == nil {
		o.Listen = net.Listen
	}
	if o.RunStdio == nil {
		o.RunStdio = func(ctx context.Context) error {
			return NewSDKServer(deps).Run(ctx, &mcp.StdioTransport{})
		}
	}
	if o.Config.MCPHTTPPort == 0 && o.Config.MCPHTTPHost == "" {
		o.Config = deps.Config
	}
	return o
}

// Start ports startMCPServer() (mcp-server.ts:574): the stdio transport is primary, the HTTP
// transport is a secondary surface. A busy port (EADDRINUSE) or any other HTTP listen failure
// degrades to stdio-only instead of taking the process down with it.
func Start(ctx context.Context, deps Deps, opts StartOptions) error {
	opts = opts.withDefaults(deps)

	baseDir := opts.BaseDir
	if baseDir == "" {
		baseDir = defaultBaseDir(deps)
	}

	token, _, err := GetOrCreateToken(baseDir)
	if err != nil {
		opts.Logf("PDF Triage MCP Server: could not load or create the API token (%v) — continuing with stdio only.", err)
		return opts.RunStdio(ctx)
	}
	// Printed once per process start, exactly where the TS prints it (mcp-server.ts:555), so it is
	// easy to copy into an agent config; it is never logged anywhere else.
	opts.Logf("  Auth: send header  Authorization: Bearer %s", token)
	opts.Logf("  Token file: %s (gitignored — do not commit or share)", TokenPath(baseDir))

	stopHTTP := startHTTP(ctx, deps, opts, token)
	defer stopHTTP()

	opts.Logf("PDF Triage MCP Server connected via stdio")
	return opts.RunStdio(ctx)
}

// startHTTP binds the HTTP listener and serves it in the background. It returns a stop function
// that is always safe to call. Every failure is logged and swallowed, matching the TS `error`
// handler that only ever printed.
func startHTTP(ctx context.Context, deps Deps, opts StartOptions, token string) func() {
	addr := net.JoinHostPort(opts.Config.MCPHTTPHost, strconv.Itoa(opts.Config.MCPHTTPPort))
	listener, err := opts.Listen("tcp", addr)
	if err != nil {
		if isAddrInUse(err) {
			opts.Logf(
				"PDF Triage MCP Server: port %d is already in use — continuing with stdio only. "+
					"Another MCP client is probably already serving HTTP on it; set MCP_HTTP_PORT to use a different one.",
				opts.Config.MCPHTTPPort,
			)
			return func() {}
		}
		opts.Logf("PDF Triage MCP Server: HTTP transport failed to start (%v) — continuing with stdio only.", err)
		return func() {}
	}

	handler := opts.Handler
	if handler == nil {
		handler = NewHTTPHandler(deps, token)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()

	displayHost := opts.Config.MCPHTTPHost
	if displayHost == "0.0.0.0" {
		displayHost = "<this-machine-LAN-IP>"
	}
	opts.Logf("PDF Triage MCP Server listening over HTTP at http://%s:%d/mcp", displayHost, opts.Config.MCPHTTPPort)
	if opts.Config.MCPHTTPHost == "0.0.0.0" {
		opts.Logf("  Reachable from any device on your LAN. Set MCP_HTTP_HOST=127.0.0.1 to restrict to this machine only.")
	}

	var once sync.Once
	stop := func() {
		once.Do(func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdownCtx)
		})
	}
	go func() {
		<-ctx.Done()
		stop()
	}()
	return stop
}

func defaultBaseDir(deps Deps) string {
	if deps.BaseDir != "" {
		return deps.BaseDir
	}
	if deps.Config.CategoriesFile != "" {
		return filepath.Dir(deps.Config.CategoriesFile)
	}
	return "."
}

// isAddrInUse recognises EADDRINUSE without importing platform-specific constants beyond the
// portable syscall.Errno, plus a message fallback for wrapped errors.
func isAddrInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "address already in use")
}
