// Package logger is a Go port of pdf-triage's src/infrastructure/logger.ts (265 lines): the LogEntry
// and DocumentLogSession shapes, the logger object with its four levels and forDocument child
// loggers, the 1000-entry ring buffer, size-based rotation with retention, grouped session logs,
// log-line formatting, and the EventEmitter subscribe/emit hook.
//
// The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/logger.test.ts` -> 5 passed), so no upstream case is pinned
// red. All five cases are ported, plus cases for the behaviours the TS suite left unasserted
// (formatting, the ring buffer, forDocument, grouped sessions, subscribe and filename extraction).
//
// The settings port is a later phase, so the TS module-load env/`CONFIG` resolution is exposed as an
// explicit Options struct; OptionsFromEnv keeps the same PDF_TRIAGE_LOG_DIR / PDF_TRIAGE_DATA_DIR /
// PDF_TRIAGE_LOG_MAX_BYTES / PDF_TRIAGE_LOG_RETAIN semantics for the caller that wires it up. The
// EventEmitter becomes a small callback registry (Subscribe returns an unsubscribe function); SSE
// will subscribe to it later. Every method is guarded by a mutex because Go handlers are concurrent.
//
// Deviations, all resolved in favor of matching the TS acceptance bar:
//
//  1. Timestamps. JS Date.toISOString() always renders exactly three fractional digits; Go's
//     time.RFC3339Nano trims trailing zeros, so the timestamp is formatted explicitly as
//     "2006-01-02T15:04:05.000Z" in UTC.
//  2. Buffer eviction. TS pushes then `shift()`s while over the cap; this port keeps the last
//     MaxBuffer entries, which is equivalent for the observable "oldest entries are dropped" rule.
//  3. `slice(-limit)`. JS `arr.slice(-0)` returns the whole array, so RecentLogs(0) (and any
//     non-positive limit) returns every buffered entry.
//  4. Meta formatting. TS `typeof meta === 'object'` is true for maps, slices and null; JSON.stringify
//     is used for those, and plain interpolation for scalars. Go has no `undefined`, so a nil meta is
//     treated as the omitted case (TS `meta !== undefined`), and a typed nil pointer is treated as an
//     object. A value that json.Marshal rejects renders "[Circular]", exactly as the TS catch did.
//  5. Filename extraction. The message regex is the TS regex with `(?i)`; Go's `\s` is ASCII-only
//     where JS `\s` is Unicode, and filepath.Base follows the host (Linux runs split only '/', which
//     matches the Linux Node suite this port was validated against). The "no match" case returns ""
//     where TS returned null.
//  6. Metadata field reads in grouped sessions understand map[string]any (the shape a decoded JSON
//     meta has); other map types are ignored.
//  7. Rotation failure. TS catches a failure anywhere in the rename chain and keeps appending to the
//     file that is still there; this port aborts the chain on the first error, reports it through
//     Stderr as `Failed to rotate log file:`, and still appends the triggering line.
//  8. Rotation/append I/O is injectable only for rename (the field renameFile) so the TS
//     `vi.spyOn(fs, 'renameSync')` case is portable.
package logger

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Level mirrors the TS `LogEntry['level']` union.
type Level string

const (
	LevelDebug Level = "DEBUG"
	LevelInfo  Level = "INFO"
	LevelWarn  Level = "WARN"
	LevelError Level = "ERROR"
)

// Defaults mirror the TS module-load fallbacks.
const (
	DefaultMaxBytes  int64 = 5 * 1024 * 1024
	DefaultRetain          = 3
	DefaultMaxBuffer       = 1000
)

// jsISOLayout is Date.toISOString(): always three fractional digits and a literal Z.
const jsISOLayout = "2006-01-02T15:04:05.000Z"

// LogEntry mirrors the TS `LogEntry` interface. Filename is "" where TS had undefined.
type LogEntry struct {
	ID         int
	Timestamp  string
	Level      Level
	ModuleName string
	Message    string
	Filename   string
	Meta       any
	Line       string
}

// DocumentLogSession mirrors the TS `DocumentLogSession` interface.
type DocumentLogSession struct {
	Filename       string
	StartedAt      string
	UpdatedAt      string
	LogsCount      int
	Status         string // COMPLETED | FAILED | IN_PROGRESS
	Category       string
	Subcategory    string
	DecisionReason string
	Logs           []LogEntry
}

// Options are the module-load values TS read from the environment. MaxBytes == 0 disables rotation;
// a negative MaxBytes falls back to DefaultMaxBytes. Retain < 1 falls back to DefaultRetain.
// MaxBuffer < 1 falls back to DefaultMaxBuffer. Stdout/Stderr and Now are optional.
type Options struct {
	LogDir    string
	LogFile   string // defaults to <LogDir>/triage_debug.log
	MaxBytes  int64
	Retain    int
	MaxBuffer int
	Now       func() time.Time
	Stdout    io.Writer
	Stderr    io.Writer
}

// DefaultOptions returns the production defaults for a log directory (matching the TS fallbacks).
func DefaultOptions(logDir string) Options {
	return Options{LogDir: logDir, MaxBytes: DefaultMaxBytes, Retain: DefaultRetain}
}

// OptionsFromEnv reproduces logger.ts's module-load resolution using an explicit app root and
// getenv. It mirrors: PDF_TRIAGE_LOG_DIR (absolute-ised) else <PDF_TRIAGE_DATA_DIR or appRoot>/logs,
// and the PDF_TRIAGE_LOG_MAX_BYTES / PDF_TRIAGE_LOG_RETAIN numeric knobs. Unset values keep the
// defaults.
func OptionsFromEnv(appRoot string, getenv func(string) string) Options {
	logRoot := appRoot
	if dataDir := getenv("PDF_TRIAGE_DATA_DIR"); dataDir != "" {
		logRoot = dataDir
	}
	logDir := filepath.Join(logRoot, "logs")
	if override := getenv("PDF_TRIAGE_LOG_DIR"); override != "" {
		logDir = override
	}

	maxBytes := DefaultMaxBytes
	if raw := getenv("PDF_TRIAGE_LOG_MAX_BYTES"); raw != "" {
		if n, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 {
			maxBytes = int64(n)
		}
	}
	retain := DefaultRetain
	if raw := getenv("PDF_TRIAGE_LOG_RETAIN"); raw != "" {
		if n, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 1 {
			retain = int(math.Floor(n))
		}
	}
	return Options{LogDir: logDir, MaxBytes: maxBytes, Retain: retain}
}

// Logger is the concrete replacement for the TS `logger` singleton plus its module state. Each
// instance owns its own buffer, ID counter, rotation state and subscribers.
type Logger struct {
	mu           sync.Mutex
	logDir       string
	logFile      string
	maxBytes     int64
	retain       int
	maxBuffer    int
	now          func() time.Time
	stdout       io.Writer
	stderr       io.Writer
	nextID       int
	buffer       []LogEntry
	bytesKnown   bool
	currentBytes int64
	listeners    []listener
	nextListener int
	renameFile   func(oldpath, newpath string) error
	removeFile   func(path string) error
}

type listener struct {
	id int
	fn func(LogEntry)
}

// New constructs a Logger. LogDir defaults to a temp subdirectory, LogFile to
// <LogDir>/triage_debug.log; Stdout/Stderr default to os.Stdout/os.Stderr; Now defaults to time.Now.
func New(opts Options) *Logger {
	logDir := opts.LogDir
	logFile := opts.LogFile
	if logFile == "" {
		if logDir == "" {
			logDir = filepath.Join(os.TempDir(), "pdf-triage-logs")
		}
		logFile = filepath.Join(logDir, "triage_debug.log")
	} else {
		logDir = filepath.Dir(logFile)
	}

	maxBytes := opts.MaxBytes
	if maxBytes < 0 {
		maxBytes = DefaultMaxBytes
	}
	retain := opts.Retain
	if retain < 1 {
		retain = DefaultRetain
	}
	maxBuffer := opts.MaxBuffer
	if maxBuffer < 1 {
		maxBuffer = DefaultMaxBuffer
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	return &Logger{
		logDir:     logDir,
		logFile:    logFile,
		maxBytes:   maxBytes,
		retain:     retain,
		maxBuffer:  maxBuffer,
		now:        now,
		stdout:     stdout,
		stderr:     stderr,
		nextID:     1, // TS `let logIdCounter = 1`
		buffer:     make([]LogEntry, 0, maxBuffer),
		renameFile: os.Rename,
		removeFile: os.Remove,
	}
}

// LogFilePath is the `__getLogFilePathForTests()` test seam.
func (l *Logger) LogFilePath() string { return l.logFile }

// Debug logs at DEBUG level. filename is optional and maps to the TS fourth argument.
func (l *Logger) Debug(moduleName, message string, meta any, filename ...string) {
	l.log(LevelDebug, moduleName, message, meta, firstOrEmpty(filename))
}

// Info logs at INFO level.
func (l *Logger) Info(moduleName, message string, meta any, filename ...string) {
	l.log(LevelInfo, moduleName, message, meta, firstOrEmpty(filename))
}

// Warn logs at WARN level.
func (l *Logger) Warn(moduleName, message string, meta any, filename ...string) {
	l.log(LevelWarn, moduleName, message, meta, firstOrEmpty(filename))
}

// Error logs at ERROR level.
func (l *Logger) Error(moduleName, message string, meta any, filename ...string) {
	l.log(LevelError, moduleName, message, meta, firstOrEmpty(filename))
}

func firstOrEmpty(values []string) string {
	if len(values) > 0 {
		return values[0]
	}
	return ""
}

// DocumentLogger is the object returned by forDocument(). It binds the clean basename as the
// filename on every call.
type DocumentLogger struct {
	logger   *Logger
	filename string
}

// ForDocument is `logger.forDocument(filename)`: the filename is reduced to its basename once.
func (l *Logger) ForDocument(filename string) *DocumentLogger {
	return &DocumentLogger{logger: l, filename: filepath.Base(filename)}
}

// Debug logs at DEBUG level for this document.
func (d *DocumentLogger) Debug(moduleName, message string, meta any) {
	d.logger.Debug(moduleName, message, meta, d.filename)
}

// Info logs at INFO level for this document.
func (d *DocumentLogger) Info(moduleName, message string, meta any) {
	d.logger.Info(moduleName, message, meta, d.filename)
}

// Warn logs at WARN level for this document.
func (d *DocumentLogger) Warn(moduleName, message string, meta any) {
	d.logger.Warn(moduleName, message, meta, d.filename)
}

// Error logs at ERROR level for this document.
func (d *DocumentLogger) Error(moduleName, message string, meta any) {
	d.logger.Error(moduleName, message, meta, d.filename)
}

// Subscribe registers a listener for every emitted entry and returns the unsubscribe function. It
// replaces `logEmitter.on('log', ...)`; the SSE endpoint will subscribe here later.
func (l *Logger) Subscribe(fn func(LogEntry)) func() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextListener++
	id := l.nextListener
	l.listeners = append(l.listeners, listener{id: id, fn: fn})
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		for i, existing := range l.listeners {
			if existing.id == id {
				l.listeners = append(l.listeners[:i], l.listeners[i+1:]...)
				return
			}
		}
	}
}

// RecentLogs is `getRecentLogs(limit = 300)`.
func (l *Logger) RecentLogs(limit int) []LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 || limit > len(l.buffer) {
		return append([]LogEntry(nil), l.buffer...)
	}
	return append([]LogEntry(nil), l.buffer[len(l.buffer)-limit:]...)
}

// GroupedSessionLogs is `getGroupedSessionLogs()`.
func (l *Logger) GroupedSessionLogs() []DocumentLogSession {
	l.mu.Lock()
	defer l.mu.Unlock()

	byName := map[string]*DocumentLogSession{}
	order := []string{}

	for _, entry := range l.buffer {
		fn := entry.Filename
		if fn == "" {
			fn = ExtractFilenameFromLog(entry)
		}
		if fn == "" {
			continue
		}

		session, ok := byName[fn]
		if !ok {
			session = &DocumentLogSession{
				Filename:  fn,
				StartedAt: entry.Timestamp,
				UpdatedAt: entry.Timestamp,
				Status:    "IN_PROGRESS",
				Logs:      []LogEntry{},
			}
			byName[fn] = session
			order = append(order, fn)
		}

		session.Logs = append(session.Logs, entry)
		session.LogsCount = len(session.Logs)
		session.UpdatedAt = entry.Timestamp

		if meta, ok := entry.Meta.(map[string]any); ok {
			if v := metaString(meta, "category", "categorie"); v != "" {
				session.Category = v
			}
			if v := metaString(meta, "subcategory", "subcategorie"); v != "" {
				session.Subcategory = v
			}
			if v := metaString(meta, "reason"); v != "" {
				session.DecisionReason = v
			}
		}

		if entry.Level == LevelError || strings.Contains(entry.Message, "FAILED") || strings.Contains(entry.Message, "BLOCKED") {
			session.Status = "FAILED"
		} else if strings.Contains(entry.Message, "COMPLETED") || strings.Contains(entry.Message, "MOVED") ||
			strings.Contains(entry.Message, "Classification success") || strings.Contains(entry.Message, "Relocalized") {
			if session.Status != "FAILED" {
				session.Status = "COMPLETED"
			}
		}
	}

	sessions := make([]DocumentLogSession, 0, len(order))
	for _, fn := range order {
		sessions = append(sessions, *byName[fn])
	}
	// JS Array.prototype.sort is stable, so equal updatedAt keeps first-seen order.
	sort.SliceStable(sessions, func(i, j int) bool {
		return parseTimestamp(sessions[i].UpdatedAt) > parseTimestamp(sessions[j].UpdatedAt)
	})
	return sessions
}

// log is the shared level implementation: format under the lock, emit to subscribers in
// registration order, print to the console, then append to the file.
func (l *Logger) log(level Level, moduleName, message string, meta any, filename string) {
	l.mu.Lock()
	line, entry := l.formatLocked(level, moduleName, message, meta, filename)
	listeners := append([]listener(nil), l.listeners...)
	l.mu.Unlock()

	for _, sub := range listeners {
		sub.fn(entry)
	}
	l.print(level, moduleName, message, meta)
	l.writeToFile(line)
}

// formatLocked builds the line and entry and appends to the ring buffer. Callers hold l.mu.
func (l *Logger) formatLocked(level Level, moduleName, message string, meta any, filename string) (string, LogEntry) {
	timestamp := l.now().UTC().Format(jsISOLayout)

	metaStr := ""
	if meta != nil {
		if isObjectLike(meta) {
			if raw, err := json.Marshal(meta); err == nil {
				metaStr = " | Meta: " + string(raw)
			} else {
				metaStr = " | Meta: [Circular]"
			}
		} else {
			metaStr = " | Meta: " + fmt.Sprintf("%v", meta)
		}
	}

	resolvedFilename := filename
	if resolvedFilename == "" {
		resolvedFilename = ExtractFilenameFromLog(LogEntry{Message: message, Meta: meta})
	}
	filePrefix := ""
	if resolvedFilename != "" {
		filePrefix = " [" + resolvedFilename + "]"
	}
	line := "[" + timestamp + "] [" + string(level) + "] [" + moduleName + "]" + filePrefix + " " + message + metaStr + "\n"

	entry := LogEntry{
		ID:         l.nextID,
		Timestamp:  timestamp,
		Level:      level,
		ModuleName: moduleName,
		Message:    message,
		Filename:   resolvedFilename,
		Meta:       meta,
		Line:       line,
	}
	l.nextID++
	l.buffer = append(l.buffer, entry)
	if len(l.buffer) > l.maxBuffer {
		l.buffer = l.buffer[len(l.buffer)-l.maxBuffer:]
	}
	return line, entry
}

// print mirrors the TS console output. Debug/Info go to Stdout, Warn/Error to Stderr.
func (l *Logger) print(level Level, moduleName, message string, meta any) {
	color := "32"
	switch level {
	case LevelDebug:
		color = "36"
	case LevelWarn:
		color = "33"
	case LevelError:
		color = "31"
	}
	prefix := "\x1b[" + color + "m[" + string(level) + "]\x1b[0m \x1b[35m[" + moduleName + "]\x1b[0m " + message
	writer := l.stdout
	if level == LevelWarn || level == LevelError {
		writer = l.stderr
	}
	if meta != nil {
		fmt.Fprintln(writer, prefix, meta)
	} else {
		fmt.Fprintln(writer, prefix)
	}
}

// writeToFile is `writeToFile(logLine)`: ensure the directory, rotate if needed, append, and track
// the byte count. A rotation failure is reported and does not lose the line.
func (l *Logger) writeToFile(logLine string) {
	if err := os.MkdirAll(l.logDir, 0o755); err != nil {
		l.consoleError("Failed to write to log file:", err)
		return
	}

	bytes := int64(len(logLine))

	l.mu.Lock()
	l.rotateLocked(bytes)
	l.mu.Unlock()

	f, err := os.OpenFile(l.logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		l.consoleError("Failed to write to log file:", err)
		return
	}
	if _, err := f.WriteString(logLine); err != nil {
		f.Close()
		l.consoleError("Failed to write to log file:", err)
		return
	}
	_ = f.Close()

	l.mu.Lock()
	l.currentBytes += bytes
	l.mu.Unlock()
}

// rotateLocked is `rotateLogIfNeeded`. Callers hold l.mu.
func (l *Logger) rotateLocked(incomingBytes int64) {
	if l.maxBytes == 0 {
		return
	}

	if !l.bytesKnown {
		if info, err := os.Stat(l.logFile); err == nil {
			l.currentBytes = info.Size()
		} else {
			l.currentBytes = 0 // no file yet
		}
		l.bytesKnown = true
	}

	if l.currentBytes+incomingBytes <= l.maxBytes {
		return
	}

	if err := l.rotate(); err != nil {
		// A failed rotation must never lose the line being written - fall through and keep appending
		// to whatever file is still there rather than throwing out of logger.info(). The comment is
		// preserved from the TS source.
		l.consoleError("Failed to rotate log file:", err)
	}
	l.currentBytes = 0
}

// rotate drops the oldest generation and shifts each one down: .2 -> .3, .1 -> .2, log -> .1.
func (l *Logger) rotate() error {
	oldest := l.logFile + "." + strconv.Itoa(l.retain)
	if fileExists(oldest) {
		_ = l.removeFile(oldest)
	}
	for i := l.retain - 1; i >= 1; i-- {
		from := l.logFile + "." + strconv.Itoa(i)
		if !fileExists(from) {
			continue
		}
		if err := l.renameFile(from, l.logFile+"."+strconv.Itoa(i+1)); err != nil {
			return err
		}
	}
	if fileExists(l.logFile) {
		if err := l.renameFile(l.logFile, l.logFile+".1"); err != nil {
			return err
		}
	}
	return nil
}

func (l *Logger) consoleError(message string, err error) {
	fmt.Fprintln(l.stderr, message, err)
}

// ExtractFilenameFromLog is `extractFilenameFromLog`. It returns "" where TS returned null.
func ExtractFilenameFromLog(entry LogEntry) string {
	if entry.Filename != "" {
		return filepath.Base(entry.Filename)
	}

	if meta, ok := entry.Meta.(map[string]any); ok {
		for _, key := range []string{"filename", "filePath", "originalPath", "original_path", "from", "to", "newPath", "path"} {
			if val, ok := meta[key].(string); ok && strings.Contains(strings.ToLower(val), ".pdf") {
				return filepath.Base(val)
			}
		}
	}

	if entry.Message != "" {
		if match := pdfNameRe.FindString(entry.Message); match != "" {
			clean := strings.TrimFunc(match, isFilenameTrimChar)
			if clean != "" {
				return filepath.Base(clean)
			}
		}
	}

	return ""
}

// pdfNameRe is the TS message regex with `(?i)` (see the package comment deviation 5).
var pdfNameRe = regexp.MustCompile(`(?i)(?:[a-zA-Z]:[\\/][^:*?"<>|\r\n]+\.pdf|[^\s\\/:*?"<>|\r\n\['"]+(?:\s+[^\s\\/:*?"<>|\r\n\['"]+)*\.pdf)`)

// isFilenameTrimChar is the TS `['"` + whitespace] trim class.
func isFilenameTrimChar(r rune) bool {
	if r == '\'' || r == '"' || r == '`' {
		return true
	}
	return isJSWhitespace(r)
}

// isJSWhitespace reports whether r is in JavaScript's WhiteSpace+LineTerminator set.
func isJSWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x00A0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// isObjectLike is TS `typeof meta === 'object'`: false only for strings, booleans and numbers.
func isObjectLike(v any) bool {
	if v == nil {
		return true // typeof null === 'object'
	}
	switch reflect.TypeOf(v).Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return false
	}
	return true
}

// metaString returns the first non-empty string among keys, reproducing `a || b`.
func metaString(meta map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := meta[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// parseTimestamp parses the ISO layout for the grouped-session sort; an unparseable value sorts last.
func parseTimestamp(value string) int64 {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return 0
	}
	return t.UnixNano()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
