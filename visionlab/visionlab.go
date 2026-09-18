// Package visionlab is a Go port of pdf-triage's standalone Vision Lab diagnostic Express app:
// src/vision-lab-server.ts (99 lines), src/vision-lab-main.ts (3 lines) and
// src/vision-lab-server.test.ts (8 cases). It serves one stateless endpoint,
// POST /api/vision/diagnose-step, for pipeline steps 1-3, plus the shared public/ directory.
//
// # Provenance
//
//	NewApp               -> src/vision-lab-server.ts:18 createVisionLabApp
//	diagnose-step route  -> src/vision-lab-server.ts:39-60
//	Start/attemptListen  -> src/vision-lab-server.ts:65-98
//	Main                 -> src/vision-lab-main.ts:1-3
//	server test shapes   -> src/vision-lab-server.test.ts
//
// Step 4 (extract) was retired upstream on 2026-09-17 and is intentionally absent: it now returns
// the same 400 as any other out-of-range step, with the exact message from
// vision-lab-server.ts:43:
//
//	step must be 1, 2, or 3 (step 4 is retired)
//
// # Injection
//
// TS imported createVisionLabApp's collaborators directly (settings, image-to-pdf, logger,
// pid-lock) and its unit test mocked those modules with vi.mock. Go has no module mocking, so every
// collaborator is a field of Deps: production resolves them in Main (or an equivalent composition
// root), and tests pass fakes. This is the same seam shape httpapi.Start already uses for its port
// takeover, and app/imagetopdf already uses for its neighbour set.
//
// # Static public/
//
// express.static(BASE_DIR/public, { index: false, setHeaders: Cache-Control: no-store }) becomes
// staticHandler wrapped by noStore. The WHY comment from vision-lab-server.ts:27-31 is preserved
// verbatim:
//
//	This serves the ENTIRE public/ directory (shared with the main triage app). Without
//	index: false, Express's default index:'index.html' behavior would render the main
//	app's dashboard at this server's root — an unrelated page whose API calls all 404
//	here. The diagnostic page stays reachable at its explicit path, /test-image-to-pdf.html.
//
// # Deviation, resolved in favor of matching the TS acceptance bar
//
//  1. base64 decoding. Node's Buffer.from(s, 'base64') ignores characters outside the base64
//     alphabet and tolerates missing padding; Go's base64.StdEncoding is strict. decodeBase64
//     reproduces the Node behavior, and never throws (an undecodable value becomes an empty
//     buffer, which the step function then reports as an ordinary result error).
//  2. Errors. imagetopdf's step functions return a PipelineStepResult with an Error field instead
//     of throwing, so a normal step failure is a 200 carrying result.error, exactly as TS. A
//     panic (the Go analogue of the TS mock's rejected promise) is recovered and returned as the
//     same 500 `{ "error": <message> }` shape.
//  3. JSON body limit. express.json({ limit: '25mb' }) becomes maxJSONBody; a larger body is
//     rejected with 413 before a handler runs. Express's default error body is HTML, while this
//     port answers `{"error": ...}`; no captured golden depends on the over-limit body.
//  4. Unknown method. Express returns 404 for a known path with an unregistered method; Go 1.22's
//     ServeMux returns 405 with an Allow header. The port keeps Go's more informative response.
package visionlab

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/app/imagetopdf"
)

// maxJSONBody mirrors express.json({ limit: '25mb' }). Phone photos as base64 run several MB — well
// past Express's 100kb JSON default (vision-lab-server.ts:21-22).
const maxJSONBody = 25 * 1024 * 1024

// DiagnoseStepPath is the one endpoint this app owns.
const DiagnoseStepPath = "/api/vision/diagnose-step"

// Validation messages, byte-for-byte from vision-lab-server.ts:43 and :48.
const (
	stepInvalidMessage   = "step must be 1, 2, or 3 (step 4 is retired)"
	imageRequiredMessage = "inputImageBase64 (string) is required"
)

// moduleName is the logger module tag every log line carries (vision-lab-server.ts:42,47,57).
const moduleName = "VISION_LAB"

// Stepper is the subset of app/imagetopdf the route dispatches to. *imagetopdf.Stepper satisfies it
// directly; the seam lets tests return canned step results, an error field, or a panic.
type Stepper interface {
	RunOrientStep(ctx context.Context, imageBuffer []byte) imagetopdf.PipelineStepResult
	RunCropStep(ctx context.Context, orientedBuffer []byte) imagetopdf.PipelineStepResult
	RunEnhanceStep(ctx context.Context, croppedBuffer []byte) imagetopdf.PipelineStepResult
}

// Logger is the subset of infra/logger the app uses. *logger.Logger satisfies it; nil skips
// logging.
type Logger interface {
	Warn(moduleName, message string, meta any, filename ...string)
	Error(moduleName, message string, meta any, filename ...string)
}

// Deps is the injected collaborator set. Stepper and PublicDir shape the handler; the remaining
// fields are Start's startup seams (zero values fall back to the real implementations).
type Deps struct {
	// Stepper runs the three pipeline steps. Required by the route.
	Stepper Stepper
	// PublicDir is BASE_DIR/public. The whole directory is served only when it exists, matching
	// vision-lab-server.ts:24-34. Empty disables static serving.
	PublicDir string
	// Logger receives the validation warnings and step failures. Nil skips logging.
	Logger Logger

	// Listen, KillProcessOnPort, Exit, Sleep and Logf are Start's seams, injected so tests never
	// bind a real port, kill a real process, or exit the test binary.
	Listen            func(network, address string) (net.Listener, error)
	KillProcessOnPort func(port int) bool
	Exit              func(code int)
	Sleep             func(d time.Duration)
	Logf              func(format string, args ...any)
}

// NewApp builds the http.Handler for POST /api/vision/diagnose-step and, when PublicDir exists, the
// public/ static mount. It is the Go createVisionLabApp.
func NewApp(deps Deps) http.Handler {
	app := &app{deps: deps}

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+DiagnoseStepPath, app.handleDiagnoseStep)

	// Static public/ is mounted last and only when the directory exists, exactly like
	// vision-lab-server.ts:24-34. "/" is the least specific pattern, so it never shadows the API
	// route. index stays disabled: staticHandler does not map "/" to index.html.
	if deps.PublicDir != "" {
		if info, err := os.Stat(deps.PublicDir); err == nil && info.IsDir() {
			mux.Handle("/", noStore(staticHandler(deps.PublicDir)))
		}
	}

	return jsonBodyMiddleware(mux)
}

// app holds the handler's collaborators.
type app struct {
	deps Deps
}

// handleDiagnoseStep ports the request validation and dispatch of vision-lab-server.ts:39-60.
func (a *app) handleDiagnoseStep(w http.ResponseWriter, r *http.Request) {
	fields := bodyFields(r)

	step, ok := validStep(fields["step"])
	if !ok {
		a.warn("Rejected diagnose-step request: step must be 1, 2, or 3 (step 4 is retired)", map[string]any{"step": fields["step"]})
		writeError(w, http.StatusBadRequest, stepInvalidMessage)
		return
	}
	input, ok := fields["inputImageBase64"].(string)
	if !ok || input == "" {
		a.warn("Rejected diagnose-step request: inputImageBase64 missing or not a string", nil)
		writeError(w, http.StatusBadRequest, imageRequiredMessage)
		return
	}

	buffer := decodeBase64(input)
	result, err := a.runStep(r.Context(), step, buffer)
	if err != nil {
		a.error("diagnose-step request failed", map[string]any{"step": step, "error": err.Error()})
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

// runStep dispatches to the Stepper method for step and converts a panic into an error, which the
// caller renders as the 500 shape (the Go analogue of the TS `catch (err)`).
func (a *app) runStep(ctx context.Context, step int, buffer []byte) (result imagetopdf.PipelineStepResult, err error) {
	if a.deps.Stepper == nil {
		return result, fmt.Errorf("vision lab: no stepper configured")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = recoveredError(recovered)
		}
	}()
	switch step {
	case 1:
		return a.deps.Stepper.RunOrientStep(ctx, buffer), nil
	case 2:
		return a.deps.Stepper.RunCropStep(ctx, buffer), nil
	case 3:
		return a.deps.Stepper.RunEnhanceStep(ctx, buffer), nil
	default:
		return result, fmt.Errorf(stepInvalidMessage)
	}
}

func (a *app) warn(message string, meta any) {
	if a.deps.Logger != nil {
		a.deps.Logger.Warn(moduleName, message, meta)
	}
}

func (a *app) error(message string, meta any) {
	if a.deps.Logger != nil {
		a.deps.Logger.Error(moduleName, message, meta)
	}
}

// validStep is JavaScript's `[1, 2, 3].includes(step)`: the value must be the JSON number 1, 2 or
// 3. A string "1", a boolean, null or a fraction all fail, exactly as TS.
func validStep(value any) (int, bool) {
	number, ok := value.(float64)
	if !ok {
		return 0, false
	}
	switch number {
	case 1:
		return 1, true
	case 2:
		return 2, true
	case 3:
		return 3, true
	default:
		return 0, false
	}
}

// recoveredError renders a recovered panic as an error: an error value keeps its message, anything
// else is formatted, matching TS err.message / String(err).
func recoveredError(recovered any) error {
	if err, ok := recovered.(error); ok {
		return err
	}
	return fmt.Errorf("%v", recovered)
}

// decodeBase64 is Node's Buffer.from(input, 'base64'): characters outside the base64 alphabet are
// ignored, padding is optional, and a trailing single character is dropped. An undecodable value
// yields an empty buffer rather than an error.
func decodeBase64(input string) []byte {
	filtered := make([]byte, 0, len(input))
	for i := 0; i < len(input); i++ {
		c := input[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
			filtered = append(filtered, c)
		}
	}
	// A base64 group of one character cannot encode a byte; Node ignores it.
	if len(filtered)%4 == 1 {
		filtered = filtered[:len(filtered)-1]
	}
	decoded, err := base64.RawStdEncoding.DecodeString(string(filtered))
	if err != nil {
		return []byte{}
	}
	return decoded
}

// ---- body handling -----------------------------------------------------------------------------

// bodyFieldsKey stores the decoded JSON object read by jsonBodyMiddleware.
type bodyFieldsKey struct{}

// jsonBodyMiddleware is express.json({ limit: '25mb' }): it reads and decodes a JSON body once, up
// to maxJSONBody, so the handler can read `req.body || {}`. A non-JSON Content-Type leaves the body
// unread, as Express did.
func jsonBodyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && isJSONContentType(r.Header.Get("Content-Type")) {
			data, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody+1))
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			if len(data) > maxJSONBody {
				writeError(w, http.StatusRequestEntityTooLarge, "request entity too large")
				return
			}
			if len(bytes.TrimSpace(data)) > 0 {
				var parsed any
				if err := json.Unmarshal(data, &parsed); err != nil {
					writeError(w, http.StatusBadRequest, err.Error())
					return
				}
				fields, _ := parsed.(map[string]any)
				if fields == nil {
					// `req.body || {}`: null, an array or a scalar all read as an object with no
					// step, so the step validation is what rejects them.
					fields = map[string]any{}
				}
				r = r.WithContext(context.WithValue(r.Context(), bodyFieldsKey{}, fields))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// bodyFields is `req.body || {}`.
func bodyFields(r *http.Request) map[string]any {
	if fields, ok := r.Context().Value(bodyFieldsKey{}).(map[string]any); ok {
		return fields
	}
	return map[string]any{}
}

// isJSONContentType accepts application/json and application/*+json, optionally with parameters,
// matching body-parser's default type.
func isJSONContentType(contentType string) bool {
	mediaType := strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])
	mediaType = strings.ToLower(mediaType)
	if mediaType == "application/json" {
		return true
	}
	return strings.HasPrefix(mediaType, "application/") && strings.HasSuffix(mediaType, "+json")
}

// ---- static serving ----------------------------------------------------------------------------

// staticHandler serves dir like express.static(dir, { index: false }): a file request is served,
// while "/" and a directory are 404 rather than an index.html (or a Go FileServer redirect).
func staticHandler(dir string) http.Handler {
	root := http.Dir(dir)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" || name == "." {
			http.NotFound(w, r)
			return
		}
		file, err := root.Open(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, name, info.ModTime(), file)
	})
}

// noStore sets Cache-Control: no-store on every static response, mirroring the express.static
// setHeaders hook (vision-lab-server.ts:32).
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// ---- response helpers --------------------------------------------------------------------------

// writeJSON serializes v exactly as Express res.json does: no HTML escaping and no trailing
// newline, with Content-Type application/json; charset=utf-8.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := marshalNoHTMLEscape(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"failed to serialize response"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError is the `res.status(code).json({ error: message })` shape every branch uses.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// marshalNoHTMLEscape is JSON.stringify: encoding/json escapes <, > and & by default, Express does
// not. The Encoder's trailing newline is trimmed so the bytes match res.json.
func marshalNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
