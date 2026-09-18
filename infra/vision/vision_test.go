package vision

// Ports src/infrastructure/vision-client.test.ts case-for-case. The TS suite is GREEN at port time
// (`npx vitest run src/infrastructure/vision-client.test.ts` -> 8 passed).
//
// The TS test mocked the `ollama` client and @napi-rs/canvas loadImage. This port instead runs a
// real net/http/httptest server (so the wire contract is exercised, not just the parsed result) and
// feeds a real PNG whose encoded header imagedimensions reads. The four "propagates an error"
// assertions are preserved exactly.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type capturedGenerate struct {
	Model   string         `json:"model"`
	Prompt  string         `json:"prompt"`
	Images  []string       `json:"images"`
	Format  string         `json:"format"`
	Think   *bool          `json:"think"`
	Options map[string]any `json:"options"`
	Stream  *bool          `json:"stream"`
}

func newTestClient(t *testing.T, response string) (*Client, *capturedGenerate) {
	t.Helper()
	captured := &capturedGenerate{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(captured); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"response": response})
	}))
	t.Cleanup(server.Close)
	return &Client{Host: server.URL, Model: "minicpm-v4.6:latest"}, captured
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 120, G: 130, B: 140, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestDetectOrientationCallsOllamaWithVisionModelImageAndFormatJSON(t *testing.T) {
	client, captured := newTestClient(t, `{"rotationDegrees": 90}`)
	buf := []byte("fake-image-bytes")

	if _, err := client.DetectOrientation(context.Background(), buf); err != nil {
		t.Fatalf("DetectOrientation error = %v", err)
	}

	if captured.Model != "minicpm-v4.6:latest" {
		t.Errorf("model = %q, want minicpm-v4.6:latest", captured.Model)
	}
	if len(captured.Images) != 1 || captured.Images[0] != base64.StdEncoding.EncodeToString(buf) {
		t.Errorf("images = %v, want one base64 image of the input", captured.Images)
	}
	if captured.Format != "json" {
		t.Errorf("format = %q, want json", captured.Format)
	}
	if captured.Think == nil || *captured.Think {
		t.Errorf("think = %v, want false", captured.Think)
	}
	if temp, ok := captured.Options["temperature"].(float64); !ok || temp != 0.1 {
		t.Errorf("options.temperature = %v, want 0.1", captured.Options["temperature"])
	}
}

func TestDetectOrientationParsesValidRotationDegrees(t *testing.T) {
	client, _ := newTestClient(t, `{"rotationDegrees": 270}`)
	result, err := client.DetectOrientation(context.Background(), []byte("x"))
	if err != nil {
		t.Fatalf("DetectOrientation error = %v", err)
	}
	if result.RotationDegrees != 270 {
		t.Errorf("rotationDegrees = %d, want 270", result.RotationDegrees)
	}
	if result.Raw != `{"rotationDegrees": 270}` {
		t.Errorf("raw = %q", result.Raw)
	}
}

func TestDetectOrientationFallsBackToZeroForOutOfSetValue(t *testing.T) {
	client, _ := newTestClient(t, `{"rotationDegrees": 45}`)
	result, err := client.DetectOrientation(context.Background(), []byte("x"))
	if err != nil {
		t.Fatalf("DetectOrientation error = %v", err)
	}
	if result.RotationDegrees != 0 {
		t.Errorf("rotationDegrees = %d, want 0", result.RotationDegrees)
	}
	if result.Raw != `{"rotationDegrees": 45}` {
		t.Errorf("raw = %q", result.Raw)
	}
}

func TestDetectOrientationPropagatesUnparseableResponse(t *testing.T) {
	client, _ := newTestClient(t, "sorry, I cannot help with that")
	if _, err := client.DetectOrientation(context.Background(), []byte("x")); err == nil {
		t.Fatal("DetectOrientation error = nil, want an error")
	}
}

// Extra: TS does `Number(parsed.rotationDegrees)`, so a quoted numeric string is accepted, while a
// unit-suffixed string is NaN and falls back to 0 (Number(), not parseInt()).
func TestDetectOrientationCoercesQuotedNumberButNotUnitSuffix(t *testing.T) {
	for _, tc := range []struct {
		response string
		want     int
	}{
		{`{"rotationDegrees": "90"}`, 90},
		{`{"rotationDegrees": "90px"}`, 0},
		{`{"rotationDegrees": true}`, 0},
		{`{"rotationDegrees": null}`, 0},
		{`{"rotationDegrees": 90.5}`, 0},
	} {
		client, _ := newTestClient(t, tc.response)
		result, err := client.DetectOrientation(context.Background(), []byte("x"))
		if err != nil {
			t.Fatalf("DetectOrientation(%s): %v", tc.response, err)
		}
		if result.RotationDegrees != tc.want {
			t.Errorf("DetectOrientation(%s) rotation = %d, want %d", tc.response, result.RotationDegrees, tc.want)
		}
	}
}

func TestDetectCropBoxIncludesImageDimensionsInPrompt(t *testing.T) {
	client, captured := newTestClient(t, `{"cropBox": {"x": 10, "y": 20, "width": 700, "height": 500}}`)
	png := pngBytes(t, 800, 600)

	if _, err := client.DetectCropBox(context.Background(), png); err != nil {
		t.Fatalf("DetectCropBox error = %v", err)
	}
	if !strings.Contains(captured.Prompt, "800x600") {
		t.Errorf("prompt = %q, want it to contain 800x600", captured.Prompt)
	}
}

func TestDetectCropBoxParsesValidCropBox(t *testing.T) {
	client, _ := newTestClient(t, `{"cropBox": {"x": 10, "y": 20, "width": 700, "height": 500}}`)
	result, err := client.DetectCropBox(context.Background(), pngBytes(t, 800, 600))
	if err != nil {
		t.Fatalf("DetectCropBox error = %v", err)
	}
	want := CropBox{X: 10, Y: 20, Width: 700, Height: 500}
	if result.CropBox == nil || *result.CropBox != want {
		t.Errorf("cropBox = %+v, want %+v", result.CropBox, want)
	}
}

func TestDetectCropBoxReturnsNilForDegenerateBox(t *testing.T) {
	client, _ := newTestClient(t, `{"cropBox": {"x": 0, "y": 0, "width": 0, "height": 500}}`)
	result, err := client.DetectCropBox(context.Background(), pngBytes(t, 800, 600))
	if err != nil {
		t.Fatalf("DetectCropBox error = %v", err)
	}
	if result.CropBox != nil {
		t.Errorf("cropBox = %+v, want nil", result.CropBox)
	}
	if !strings.Contains(result.Raw, `"width": 0`) {
		t.Errorf("raw = %q, want it to contain the degenerate width", result.Raw)
	}
}

func TestDetectCropBoxPropagatesUnparseableResponse(t *testing.T) {
	client, _ := newTestClient(t, "not json")
	if _, err := client.DetectCropBox(context.Background(), pngBytes(t, 800, 600)); err == nil {
		t.Fatal("DetectCropBox error = nil, want an error")
	}
}
