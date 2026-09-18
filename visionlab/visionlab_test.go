package visionlab

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/app/imagetopdf"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/crop"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/imageprocessor"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/orientation"
)

// fakeStepper is the injected step seam: it records dispatch order and inputs, returns canned
// results, and can panic to exercise the 500 path.
type fakeStepper struct {
	results map[int]imagetopdf.PipelineStepResult
	panics  map[int]any
	calls   []int
	inputs  [][]byte
}

func newFakeStepper() *fakeStepper {
	return &fakeStepper{results: map[int]imagetopdf.PipelineStepResult{}, panics: map[int]any{}}
}

func (f *fakeStepper) run(step int, buffer []byte) imagetopdf.PipelineStepResult {
	f.calls = append(f.calls, step)
	f.inputs = append(f.inputs, append([]byte(nil), buffer...))
	if f.panics != nil {
		if recovered, ok := f.panics[step]; ok {
			panic(recovered)
		}
	}
	return f.results[step]
}

func (f *fakeStepper) RunOrientStep(_ context.Context, buffer []byte) imagetopdf.PipelineStepResult {
	return f.run(1, buffer)
}

func (f *fakeStepper) RunCropStep(_ context.Context, buffer []byte) imagetopdf.PipelineStepResult {
	return f.run(2, buffer)
}

func (f *fakeStepper) RunEnhanceStep(_ context.Context, buffer []byte) imagetopdf.PipelineStepResult {
	return f.run(3, buffer)
}

// recordStepper records the exact buffer each step method receives.
type recordStepper struct {
	oriented bool
	cropped  bool
	enhanced bool
	input    []byte
}

func (s *recordStepper) RunOrientStep(_ context.Context, buffer []byte) imagetopdf.PipelineStepResult {
	s.oriented = true
	s.input = append([]byte(nil), buffer...)
	return imagetopdf.PipelineStepResult{Step: 1, Label: "oriented", ImageBase64: "abc", DurationMS: 5}
}

func (s *recordStepper) RunCropStep(_ context.Context, buffer []byte) imagetopdf.PipelineStepResult {
	s.cropped = true
	s.input = append([]byte(nil), buffer...)
	return imagetopdf.PipelineStepResult{Step: 2, Label: "cropped", ImageBase64: "abc", DurationMS: 5}
}

func (s *recordStepper) RunEnhanceStep(_ context.Context, buffer []byte) imagetopdf.PipelineStepResult {
	s.enhanced = true
	s.input = append([]byte(nil), buffer...)
	return imagetopdf.PipelineStepResult{Step: 3, Label: "enhanced", ImageBase64: "abc", DurationMS: 5}
}

func doRequest(t *testing.T, handler http.Handler, method, target string, body any, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		switch typed := body.(type) {
		case []byte:
			reader = bytes.NewReader(typed)
		case string:
			reader = strings.NewReader(typed)
		default:
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			reader = bytes.NewReader(raw)
		}
	}
	req := httptest.NewRequest(method, target, reader)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	} else if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return payload.Error
}

func TestDiagnoseStepValidation(t *testing.T) {
	cases := []struct {
		name string
		body any
		want string
	}{
		{"missing step", map[string]any{"inputImageBase64": "ZmFrZQ=="}, stepInvalidMessage},
		{"step out of range", map[string]any{"step": 5, "inputImageBase64": "ZmFrZQ=="}, stepInvalidMessage},
		{"retired step 4", map[string]any{"step": 4, "inputImageBase64": "ZmFrZQ=="}, stepInvalidMessage},
		{"step string", map[string]any{"step": "1", "inputImageBase64": "ZmFrZQ=="}, stepInvalidMessage},
		{"missing image", map[string]any{"step": 1}, imageRequiredMessage},
		{"image non-string", map[string]any{"step": 1, "inputImageBase64": 123}, imageRequiredMessage},
		{"empty image", map[string]any{"step": 1, "inputImageBase64": ""}, imageRequiredMessage},
		{"non-object body", []any{1, 2, 3}, stepInvalidMessage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stepper := &recordStepper{}
			rec := doRequest(t, NewApp(Deps{Stepper: stepper}), http.MethodPost, DiagnoseStepPath, tc.body, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			if got := decodeError(t, rec); got != tc.want {
				t.Fatalf("error = %q, want %q", got, tc.want)
			}
			if stepper.oriented || stepper.cropped || stepper.enhanced {
				t.Fatal("a step function was called for an invalid request")
			}
		})
	}
}

func TestDiagnoseStepRoutesToTheChosenStep(t *testing.T) {
	cases := []struct {
		step int
		want func(*recordStepper) bool
	}{
		{1, func(s *recordStepper) bool { return s.oriented && !s.cropped && !s.enhanced }},
		{2, func(s *recordStepper) bool { return s.cropped && !s.oriented && !s.enhanced }},
		{3, func(s *recordStepper) bool { return s.enhanced && !s.oriented && !s.cropped }},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("step-%d", tc.step), func(t *testing.T) {
			stepper := &recordStepper{}
			rec := doRequest(t, NewApp(Deps{Stepper: stepper}), http.MethodPost, DiagnoseStepPath,
				map[string]any{"step": tc.step, "inputImageBase64": "ZmFrZQ=="}, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			if !tc.want(stepper) {
				t.Fatalf("wrong step dispatched: oriented=%v cropped=%v enhanced=%v", stepper.oriented, stepper.cropped, stepper.enhanced)
			}
			// Buffer.from('ZmFrZQ==', 'base64') === 'fake'.
			if string(stepper.input) != "fake" {
				t.Fatalf("step received %q, want %q", stepper.input, "fake")
			}
			var payload struct {
				Result imagetopdf.PipelineStepResult `json:"result"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode body %q: %v", rec.Body.String(), err)
			}
			if payload.Result.Step != tc.step {
				t.Fatalf("result.Step = %d, want %d", payload.Result.Step, tc.step)
			}
		})
	}
}

// TestDiagnoseStepReturnsAnErrorFieldAsA200 pins the imagetopdf contract: a step function catches
// its own failure and returns an error field, which is a 200 carrying result.error, not a 500.
func TestDiagnoseStepReturnsAnErrorFieldAsA200(t *testing.T) {
	stepper := newFakeStepper()
	stepper.results[1] = imagetopdf.PipelineStepResult{Step: 1, Label: "oriented", DurationMS: 5, Error: "model unreachable"}
	rec := doRequest(t, NewApp(Deps{Stepper: stepper}), http.MethodPost, DiagnoseStepPath,
		map[string]any{"step": 1, "inputImageBase64": "ZmFrZQ=="}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var payload struct {
		Result imagetopdf.PipelineStepResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload.Result.Error != "model unreachable" {
		t.Fatalf("result.error = %q, want %q", payload.Result.Error, "model unreachable")
	}
}

// TestDiagnoseStepRecoversAPanicIntoThe500Shape is the Go analogue of the upstream "returns 500
// with the error message when a step function throws" case.
func TestDiagnoseStepRecoversAPanicIntoThe500Shape(t *testing.T) {
	stepper := newFakeStepper()
	stepper.panics[1] = errors.New("unexpected crash")
	rec := doRequest(t, NewApp(Deps{Stepper: stepper}), http.MethodPost, DiagnoseStepPath,
		map[string]any{"step": 1, "inputImageBase64": "ZmFrZQ=="}, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec); got != "unexpected crash" {
		t.Fatalf("error = %q, want %q", got, "unexpected crash")
	}
}

func TestDiagnoseStepRecoversAStringPanic(t *testing.T) {
	stepper := newFakeStepper()
	stepper.panics[2] = "boom"
	rec := doRequest(t, NewApp(Deps{Stepper: stepper}), http.MethodPost, DiagnoseStepPath,
		map[string]any{"step": 2, "inputImageBase64": "ZmFrZQ=="}, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := decodeError(t, rec); got != "boom" {
		t.Fatalf("error = %q, want %q", got, "boom")
	}
}

func TestStaticServing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "test-image-to-pdf.html"), "<html>diagnostic</html>")
	writeFile(t, filepath.Join(dir, "index.html"), "<html>dashboard</html>")
	if err := os.MkdirAll(filepath.Join(dir, "js"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "js", "app.js"), "console.log(1)")

	handler := NewApp(Deps{Stepper: newFakeStepper(), PublicDir: dir})

	rec := doRequest(t, handler, http.MethodGet, "/test-image-to-pdf.html", nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "diagnostic") {
		t.Fatalf("diagnostic page: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	rec = doRequest(t, handler, http.MethodGet, "/js/app.js", nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "console.log") {
		t.Fatalf("nested asset: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// index: false — the main dashboard must not appear at the Vision Lab root.
	rec = doRequest(t, handler, http.MethodGet, "/", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("root status = %d, want 404 (index must stay disabled)", rec.Code)
	}

	// An explicit /index.html request is still served, as express.static does.
	rec = doRequest(t, handler, http.MethodGet, "/index.html", nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "dashboard") {
		t.Fatalf("/index.html: status=%d body=%q", rec.Code, rec.Body.String())
	}

	rec = doRequest(t, handler, http.MethodGet, "/missing.html", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing asset status = %d, want 404", rec.Code)
	}
}

func TestStaticNotMountedWhenPublicDirMissing(t *testing.T) {
	handler := NewApp(Deps{Stepper: newFakeStepper(), PublicDir: filepath.Join(t.TempDir(), "nope")})
	rec := doRequest(t, handler, http.MethodGet, "/test-image-to-pdf.html", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestNonJSONContentTypeIsAnEmptyBody(t *testing.T) {
	rec := doRequest(t, NewApp(Deps{Stepper: newFakeStepper()}), http.MethodPost, DiagnoseStepPath,
		`{"step":1,"inputImageBase64":"ZmFrZQ=="}`, "text/plain")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := decodeError(t, rec); got != stepInvalidMessage {
		t.Fatalf("error = %q, want %q", got, stepInvalidMessage)
	}
}

func TestMalformedJSONIsRejectedBeforeTheHandler(t *testing.T) {
	rec := doRequest(t, NewApp(Deps{Stepper: newFakeStepper()}), http.MethodPost, DiagnoseStepPath,
		`{not json`, "application/json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestJSONBodyLimitIs25MB(t *testing.T) {
	if maxJSONBody != 25*1024*1024 {
		t.Fatalf("maxJSONBody = %d, want 25 MiB", maxJSONBody)
	}
	oversized := `{"inputImageBase64":"` + strings.Repeat("a", maxJSONBody) + `"}`
	rec := doRequest(t, NewApp(Deps{Stepper: newFakeStepper()}), http.MethodPost, DiagnoseStepPath,
		oversized, "application/json")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body=%q)", rec.Code, rec.Body.String())
	}
}

func TestDecodeBase64MatchesNode(t *testing.T) {
	cases := map[string]string{
		"ZmFrZQ==":  "fake",
		"ZmFrZQ":    "fake", // missing padding
		"Zm FrZQ==": "fake", // invalid characters ignored
		"!!!!":      "",     // no base64 characters
		"Z":         "",     // a single trailing character is dropped
	}
	for input, want := range cases {
		if got := string(decodeBase64(input)); got != want {
			t.Fatalf("decodeBase64(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestDiagnoseStepSyntheticImageEndToEnd wires the REAL imagetopdf.Stepper with real imageprocessor
// operations and only the two vision cascades faked, then drives it through the HTTP route with a
// small synthetic PNG. No real photo, no real model, no real network.
func TestDiagnoseStepSyntheticImageEndToEnd(t *testing.T) {
	stepper := newSyntheticImageStepper()
	handler := NewApp(Deps{Stepper: stepper})

	original := syntheticPNG(t, 100, 80)

	// Step 1: orient.
	rec := doRequest(t, handler, http.MethodPost, DiagnoseStepPath,
		map[string]any{"step": 1, "inputImageBase64": base64.StdEncoding.EncodeToString(original)}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("step 1 status = %d (%s)", rec.Code, rec.Body.String())
	}
	oriented := resultImage(t, rec)
	if w, h := pngDims(t, oriented); w != 100 || h != 80 {
		t.Fatalf("oriented dims = %dx%d, want 100x80", w, h)
	}

	// Step 2: crop the oriented image.
	rec = doRequest(t, handler, http.MethodPost, DiagnoseStepPath,
		map[string]any{"step": 2, "inputImageBase64": base64.StdEncoding.EncodeToString(oriented)}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("step 2 status = %d (%s)", rec.Code, rec.Body.String())
	}
	cropped := resultImage(t, rec)
	if w, h := pngDims(t, cropped); w != 40 || h != 30 {
		t.Fatalf("cropped dims = %dx%d, want 40x30", w, h)
	}

	// Step 3: enhance the cropped image.
	rec = doRequest(t, handler, http.MethodPost, DiagnoseStepPath,
		map[string]any{"step": 3, "inputImageBase64": base64.StdEncoding.EncodeToString(cropped)}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("step 3 status = %d (%s)", rec.Code, rec.Body.String())
	}
	enhanced := resultImage(t, rec)
	if w, h := pngDims(t, enhanced); w <= 0 || h <= 0 {
		t.Fatalf("enhanced dims = %dx%d, want positive", w, h)
	}
}

func newSyntheticImageStepper() *imagetopdf.Stepper {
	box := imageprocessor.CropBox{X: 5, Y: 5, Width: 40, Height: 30}
	return imagetopdf.NewStepper(imagetopdf.Deps{
		NormalizeOrientation: imageprocessor.NormalizeOrientation,
		DetectOrientation: func(context.Context, []byte) (orientation.DetectionResult, error) {
			return orientation.DetectionResult{
				RotationDegrees: 0,
				ExifDegrees:     nil,
				ModelDegrees:    0,
				ModelRaw:        `{"rotationDegrees":0}`,
				Source:          orientation.SourceModelOnly,
			}, nil
		},
		RotateImage: imageprocessor.RotateImage,
		DetectCropBox: func(context.Context, []byte) (crop.DetectionResult, error) {
			return crop.DetectionResult{
				CropBox:      &box,
				ModelCropBox: &box,
				ModelRaw:     `{"cropBox":{"x":5,"y":5,"width":40,"height":30}}`,
				Source:       crop.SourceModelOnly,
			}, nil
		},
		CropImage:               imageprocessor.CropImage,
		ComputeAutoLevels:       imageprocessor.ComputeAutoLevelsForImage,
		ApplyBrightnessContrast: imageprocessor.ApplyBrightnessContrast,
		ApplySharpen:            imageprocessor.ApplySharpen,
	})
}

func resultImage(t *testing.T, rec *httptest.ResponseRecorder) []byte {
	t.Helper()
	var payload struct {
		Result imagetopdf.PipelineStepResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if payload.Result.Error != "" {
		t.Fatalf("step returned error %q", payload.Result.Error)
	}
	decoded, err := base64.StdEncoding.DecodeString(payload.Result.ImageBase64)
	if err != nil {
		t.Fatalf("decode imageBase64: %v", err)
	}
	return decoded
}

func syntheticPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{R: 200, G: 200, B: 200, A: 255}), image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(0, 0, width/4, height/4), image.NewUniform(color.RGBA{R: 20, G: 20, B: 20, A: 255}), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode synthetic PNG: %v", err)
	}
	return buf.Bytes()
}

func pngDims(t *testing.T, buffer []byte) (int, int) {
	t.Helper()
	config, err := png.DecodeConfig(bytes.NewReader(buffer))
	if err != nil {
		t.Fatalf("decode PNG config: %v", err)
	}
	return config.Width, config.Height
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
