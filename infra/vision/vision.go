// Package vision is a Go port of pdf-triage's src/infrastructure/vision-client.ts (75 lines):
// DetectOrientation (vision-client.ts:30) and DetectCropBox (vision-client.ts:50), the two
// single-purpose calls the photo pipeline makes against the pinned Ollama vision model
// (minicpm-v4.6:latest).
//
// # Provenance
//
//	DetectOrientation   -> src/infrastructure/vision-client.ts:30
//	DetectCropBox       -> src/infrastructure/vision-client.ts:50
//	CropBox             -> src/infrastructure/vision-client.ts:6
//	OrientationResult   -> src/infrastructure/vision-client.ts:13
//	CropResult          -> src/infrastructure/vision-client.ts:18
//
// The WHY-comments from the TS source are preserved verbatim below.
//
// # Transport
//
// TS used the `ollama` npm client (`new Ollama({host})` + `generate`). The task that owns this port
// forbids importing the concurrently-written infra/ollama package, so this package talks to the
// Ollama HTTP API directly with net/http and defines its own minimal request/response DTOs. The
// later unification with infra/ollama is a drop-in edit of Client.generate: the request body fields
// below are exactly the ones TS sent (model, prompt, images, format:"json", think:false,
// options:{temperature:0.1}) plus stream:false, which the JS client sent implicitly to receive a
// single final response instead of a stream.
//
// # EXIF is deliberately NOT handled here
//
// Golden Rule 17: the pipeline normalizes orientation FIRST (imageprocessor.NormalizeOrientation)
// and strips the EXIF tag; every caller of these functions sees a buffer with no orientation
// metadata. exifDegrees is null by design (see infra/orientation).
//
// # Deviations, all resolved in favor of matching the TS acceptance bar
//
//  1. Dimensions. detectCropBox's prompt embeds the image's pixel dimensions. TS read them with
//     @napi-rs/canvas loadImage (a full decode of any supported format). This port reads the encoded
//     header with the already-ported imagedimensions package (PNG and JPEG only). Every production
//     caller passes the PNG produced by NormalizeOrientation, so the two agree there; a non-PNG/JPEG
//     buffer returns an error where TS could still decode. This is the ONE intentional semantic gap
//     and it is bounded by the pipeline's own invariant (normalize -> cascade), which always hands
//     this package a PNG.
//  2. Client construction. TS built `new Ollama({host: CONFIG.OLLAMA_HOST})` inside each function.
//     A Go package cannot import infra/settings without coupling, and the model/host are now wired
//     by the composition root; Client.Host/Model are explicit fields. A nil Client.HTTPClient uses
//     http.DefaultClient.
//  3. Number coercion. TS does `Number(parsed.rotationDegrees)`, which coerces strings ("90"), and
//     only then checks membership in {0,90,180,270}; a non-object parsed JSON value reads
//     `undefined` and degrades to 0. jsNumber reproduces that coercion exactly. detectCropBox uses
//     `Number.isFinite`, which does NOT coerce, so its fields must be JSON numbers — reproduced by
//     requiring a float64.
//  4. Errors. TS throws; Go returns errors, and the same inputs produce the same error/no-error
//     split: an unparseable model response is an error, while a parseable-but-nonsensical value
//     degrades to the safe default (0 / nil box) with raw still carrying the model's words.
package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/phamhung075/pdf-triage-pdf2w/classification"
	"github.com/phamhung075/pdf-triage-pdf2w/imagedimensions"
)

// CropBox is the TS `CropBox` interface (vision-client.ts:6).
type CropBox struct {
	X      float64
	Y      float64
	Width  float64
	Height float64
}

// OrientationResult is the TS `OrientationResult` interface (vision-client.ts:13).
type OrientationResult struct {
	RotationDegrees int
	Raw             string
}

// CropResult is the TS `CropResult` interface (vision-client.ts:18). A nil CropBox is the TS null.
type CropResult struct {
	CropBox *CropBox
	Raw     string
}

// validRotations mirrors `VALID_ROTATIONS` (vision-client.ts:23).
var validRotations = []int{0, 90, 180, 270}

// Client talks to one Ollama host for the pinned vision model. Host is the base URL
// (CONFIG.OLLAMA_HOST, e.g. http://127.0.0.1:11434) and Model the pinned vision model name
// (CONFIG.OLLAMA_VISION_MODEL). HTTPClient may be nil.
type Client struct {
	Host       string
	Model      string
	HTTPClient *http.Client
}

// NewClient returns a Client with the default HTTP client.
func NewClient(host, model string) *Client {
	return &Client{Host: host, Model: model, HTTPClient: http.DefaultClient}
}

// orientationPrompt is the prompt from vision-client.ts:32-33, byte-for-byte.
const orientationPrompt = `This photo shows a paper document (letter, receipt, or invoice) that may have been captured at any rotation. Determine the clockwise rotation in degrees needed to make its text upright and readable.
Respond with ONLY a JSON object, no other text: {"rotationDegrees": 0} where the value is exactly one of 0, 90, 180, or 270.`

// cropPrompt is `detectCropBox`'s prompt template from vision-client.ts:53-54. %d/%d are the image
// width/height; the rest is byte-for-byte.
const cropPrompt = `This photo is %dx%d pixels and shows a paper document lying on a background (desk, table, floor). Identify the bounding box of just the document, excluding the background around it.
Respond with ONLY a JSON object, no other text: {"cropBox": {"x": 0, "y": 0, "width": %d, "height": %d}} using pixel coordinates measured from the top-left corner. If the document already fills the whole photo, return the full image bounds.`

// DetectOrientation ports detectOrientation (vision-client.ts:30). The comment below is preserved
// verbatim from the TS source:
//
// Two separate model calls (orientation, then crop) rather than one combined prompt — simpler,
// more focused prompt per sub-task. A JSON-unparseable response is a real failure and propagates
// (the diagnostic pipeline surfaces it and stops); a parseable-but-nonsensical value (invalid
// rotation, degenerate box) degrades to a safe default instead of throwing, so a single odd
// model answer doesn't kill the whole pipeline, while `raw` still carries what the model said.
func (c *Client) DetectOrientation(ctx context.Context, imageBuffer []byte) (OrientationResult, error) {
	raw, err := c.generate(ctx, orientationPrompt, imageBuffer)
	if err != nil {
		return OrientationResult{}, err
	}
	parsed, err := classification.CleanAndParseJSON(raw)
	if err != nil {
		return OrientationResult{}, err
	}
	value := jsNumber(rotationDegreesField(parsed))
	rotation := 0
	for _, valid := range validRotations {
		if value == float64(valid) {
			rotation = valid
			break
		}
	}
	return OrientationResult{RotationDegrees: rotation, Raw: raw}, nil
}

// DetectCropBox ports detectCropBox (vision-client.ts:50).
func (c *Client) DetectCropBox(ctx context.Context, imageBuffer []byte) (CropResult, error) {
	dims, ok := imagedimensions.ReadImageDimensions(imageBuffer)
	if !ok {
		return CropResult{}, fmt.Errorf("vision: detectCropBox: could not read image dimensions (only PNG/JPEG headers are recognized)")
	}
	prompt := fmt.Sprintf(cropPrompt, dims.Width, dims.Height, dims.Width, dims.Height)

	raw, err := c.generate(ctx, prompt, imageBuffer)
	if err != nil {
		return CropResult{}, err
	}
	parsed, err := classification.CleanAndParseJSON(raw)
	if err != nil {
		return CropResult{}, err
	}
	box, valid := cropBoxField(parsed)
	if !valid {
		return CropResult{CropBox: nil, Raw: raw}, nil
	}
	return CropResult{CropBox: box, Raw: raw}, nil
}

// rotationDegreesField reads `parsed.rotationDegrees` with JS property-access semantics: any value
// that is not a JSON object yields undefined.
func rotationDegreesField(parsed any) any {
	if m, ok := parsed.(map[string]any); ok {
		return m["rotationDegrees"]
	}
	return nil
}

// cropBoxField reads `parsed.cropBox` and applies the validity check from vision-client.ts:67-70.
// Number.isFinite does not coerce, so every field must be a JSON number; a nil/absent box and a
// zero or negative width/height are all invalid and yield (nil, false).
func cropBoxField(parsed any) (*CropBox, bool) {
	m, ok := parsed.(map[string]any)
	if !ok {
		return nil, false
	}
	box, ok := m["cropBox"].(map[string]any)
	if !ok {
		return nil, false
	}
	x, okX := finiteNumber(box["x"])
	y, okY := finiteNumber(box["y"])
	width, okW := finiteNumber(box["width"])
	height, okH := finiteNumber(box["height"])
	if !okX || !okY || !okW || !okH || !(width > 0) || !(height > 0) {
		return nil, false
	}
	return &CropBox{X: x, Y: y, Width: width, Height: height}, true
}

// finiteNumber is `Number.isFinite(v)`: true only for a finite JSON number (float64).
func finiteNumber(v any) (float64, bool) {
	f, ok := v.(float64)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// jsNumber is JS `Number(v)` for the JSON-decoded values a model response can contain. JSON has no
// NaN/Infinity literals, but a string field can spell one, which JS Number accepts.
func jsNumber(v any) float64 {
	switch t := v.(type) {
	case nil:
		return math.NaN()
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case bool:
		if t {
			return 1
		}
		return 0
	case string:
		return jsNumberString(t)
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return math.NaN()
		}
		return f
	default:
		return math.NaN()
	}
}

// jsNumberString is JS `Number(string)`: trimmed; "" and whitespace -> 0; "Infinity"/"-Infinity" ->
// +/-Inf; "0x..." hex -> the integer; otherwise the WHOLE remaining string must be a numeric
// literal, else NaN. Note this is Number(), not parseInt(): Number("12px") is NaN.
func jsNumberString(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	switch s {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	case "NaN":
		return math.NaN()
	}
	if len(s) > 2 && (s[0:2] == "0x" || s[0:2] == "0X") {
		if n, err := strconv.ParseUint(s[2:], 16, 64); err == nil {
			return float64(n)
		}
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return math.NaN()
}

// generate is the one HTTP call both functions make: POST {host}/api/generate with the exact
// request body fields the TS `ollama.generate` call carried.
func (c *Client) generate(ctx context.Context, prompt string, imageBuffer []byte) (string, error) {
	body := map[string]any{
		"model":   c.Model,
		"prompt":  prompt,
		"images":  []string{base64.StdEncoding.EncodeToString(imageBuffer)},
		"format":  "json",
		"think":   false,
		"options": map[string]any{"temperature": 0.1},
		"stream":  false,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	url := strings.TrimRight(c.Host, "/") + "/api/generate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("vision: ollama generate: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var parsed struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("vision: decode ollama generate response: %w", err)
	}
	return parsed.Response, nil
}
