// Log routes #14-#16, ported from web-server.ts:442-486.
//
// GET /api/logs/recent returns `{ logs }` (default limit 300). GET /api/logs/sessions returns
// `{ total, sessions }`. GET /api/logs/stream is SSE: it writes one INIT frame with the last 100
// logs, then one LOG frame per logger event, and unsubscribes when the client disconnects. No
// Access-Control-Allow-Origin is sent, per the no-CORS rationale at web-server.ts:468-470.
package httpapi

import (
	"net/http"

	"github.com/phamhung075/pdf-triage-pdf2w/infra/logger"
)

func (s *server) registerLogs(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/logs/recent", s.logsRecentHandler)
	mux.HandleFunc("GET /api/logs/sessions", s.logsSessionsHandler)
	mux.HandleFunc("GET /api/logs/stream", s.logsStreamHandler)
}

// logEntryJSON mirrors the TS LogEntry JSON keys. `filename` and `meta` are omitted when absent,
// matching JSON.stringify dropping `undefined`; `line` is always present.
type logEntryJSON struct {
	ID         int    `json:"id"`
	Timestamp  string `json:"timestamp"`
	Level      string `json:"level"`
	ModuleName string `json:"moduleName"`
	Message    string `json:"message"`
	Filename   string `json:"filename,omitempty"`
	Meta       any    `json:"meta,omitempty"`
	Line       string `json:"line"`
}

// logSessionJSON mirrors DocumentLogSession. Optional string fields are omitted when empty.
type logSessionJSON struct {
	Filename       string         `json:"filename"`
	StartedAt      string         `json:"startedAt"`
	UpdatedAt      string         `json:"updatedAt"`
	LogsCount      int            `json:"logsCount"`
	Status         string         `json:"status"`
	Category       string         `json:"category,omitempty"`
	Subcategory    string         `json:"subcategory,omitempty"`
	DecisionReason string         `json:"decisionReason,omitempty"`
	Logs           []logEntryJSON `json:"logs"`
}

func toLogEntryJSON(entry logger.LogEntry) logEntryJSON {
	return logEntryJSON{
		ID:         entry.ID,
		Timestamp:  entry.Timestamp,
		Level:      string(entry.Level),
		ModuleName: entry.ModuleName,
		Message:    entry.Message,
		Filename:   entry.Filename,
		Meta:       entry.Meta,
		Line:       entry.Line,
	}
}

func toLogEntryJSONList(entries []logger.LogEntry) []logEntryJSON {
	out := make([]logEntryJSON, 0, len(entries))
	for _, entry := range entries {
		out = append(out, toLogEntryJSON(entry))
	}
	return out
}

func (s *server) logsRecentHandler(w http.ResponseWriter, r *http.Request) {
	limit := 300
	if raw := r.URL.Query().Get("limit"); raw != "" {
		// TS: `req.query.limit ? parseInt(req.query.limit, 10) : 300`. A non-numeric limit parses
		// to NaN and is passed on: getRecentLogs(NaN) is `logBuffer.slice(-NaN)`, i.e. `slice(0)`,
		// the whole buffer. infra/logger documents the same equivalence for RecentLogs(0)
		// (logger.go deviation 3: JS `slice(-0)` and any non-positive limit return every entry),
		// so a non-numeric limit forwards 0 rather than silently keeping the 300 default.
		if parsed, ok := parseJSInt(raw); ok {
			limit = int(parsed)
		} else {
			limit = 0
		}
	}
	writeJSON(w, 200, map[string]any{"logs": toLogEntryJSONList(s.deps.Logs.RecentLogs(limit))})
}

func (s *server) logsSessionsHandler(w http.ResponseWriter, r *http.Request) {
	sessions := s.deps.Logs.GroupedSessionLogs()
	out := make([]logSessionJSON, 0, len(sessions))
	for _, session := range sessions {
		out = append(out, logSessionJSON{
			Filename:       session.Filename,
			StartedAt:      session.StartedAt,
			UpdatedAt:      session.UpdatedAt,
			LogsCount:      session.LogsCount,
			Status:         session.Status,
			Category:       session.Category,
			Subcategory:    session.Subcategory,
			DecisionReason: session.DecisionReason,
			Logs:           toLogEntryJSONList(session.Logs),
		})
	}
	writeJSON(w, 200, map[string]any{"total": len(sessions), "sessions": out})
}

// logsStreamHandler ports web-server.ts:464-486. The log listener only enqueues: every write to the
// ResponseWriter happens on the handler goroutine, because Go forbids writing a response from
// another goroutine (Node's single-threaded EventEmitter did not have that constraint). The
// listener is removed on close, matching req.on('close').
func (s *server) logsStreamHandler(w http.ResponseWriter, r *http.Request) {
	setSSEHeaders(w)
	flush(w)

	writeFrame := func(payload any) bool {
		body, err := marshalNoHTMLEscape(payload)
		if err != nil {
			return true
		}
		if _, err := w.Write(append(append([]byte("data: "), body...), '\n', '\n')); err != nil {
			return false
		}
		flush(w)
		return true
	}

	initialLogs := s.deps.Logs.RecentLogs(100)
	writeFrame(map[string]any{"type": "INIT", "logs": toLogEntryJSONList(initialLogs)})

	entries := make(chan logger.LogEntry, 64)
	unsubscribe := s.deps.Logs.Subscribe(func(entry logger.LogEntry) {
		select {
		case entries <- entry:
		default:
			// Slow client: drop this log event rather than block the logger.
		}
	})
	defer unsubscribe()

	for {
		select {
		case <-r.Context().Done():
			return
		case entry := <-entries:
			if !writeFrame(map[string]any{"type": "LOG", "entry": toLogEntryJSON(entry)}) {
				return
			}
		}
	}
}
