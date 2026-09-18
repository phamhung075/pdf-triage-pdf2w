package imageprocessor

// Differential pixel-parity tests against fixtures produced by the real TypeScript
// image-processor.ts with @napi-rs/canvas. The throwaway generator lived at
// scratch/phase6-fixtures/gen.ts and was deleted after the fixtures were checked in; the inputs are
// SMALL SYNTHETIC scenes (rectangles, gradients, text-like stripes) and no real photograph is read.
//
// Tolerances (stated per operation, as the task requires):
//
//   - rotateImage / cropImage / applySharpen: EXACT decoded-pixel match. Every one is integer
//     arithmetic on an 8-bit RGBA plane, so there is no rounding freedom: rotate/crop only move
//     bytes and applySharpen's ToUint8Clamp reproduces the canvas Uint8ClampedArray store.
//   - applyBrightnessContrast: mean absolute error (MAE) <= 1.0 per channel. Canvas applies the CSS
//     brightness()/contrast() chain through Skia's 8-bit filter pipeline; this port re-implements
//     the same sRGB transfer functions. A one-level rounding difference per channel is the entire
//     gap (exact measured MAE is asserted well below the bound in the failure message).
//   - normalizeOrientation: MAE <= 5.0 per channel. The input is a JPEG, and Go's YCbCr->RGB
//     conversion and chroma upsampling differ from Skia's, which shows up as a couple of levels of
//     mean error (and a larger miss at the synthetic hard colour edges). Dimensions must match
//     EXACTLY, and the output must carry no EXIF Orientation tag (that is the whole point of the
//     function). All eight tags measure the same 3.3164 MAE, which is what proves the eight-way
//     transform mapping is right rather than merely close.
//   - encodeJpeg: MAE <= 6.0 per channel after decoding both JPEGs. Go's image/jpeg and Skia's JPEG
//     encoder make different quantization choices at the same nominal quality; the comparison is
//     therefore on decoded pixels, never on bytes. Dimensions must match exactly.
//   - detectDocumentBoxLocally: box coordinates within the per-scene tolerance the upstream TS test
//     uses (5/8 px at 400x300, 16/24 px at 1600x1200), because Skia and x/image/draw cannot resample
//     bit-identically. The detected kind (box / no-signal / vetoed) must match EXACTLY.

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/exiforientation"
	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
)

const parityDir = "testdata/parity"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(parityDir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func decodeAny(t *testing.T, name string, data []byte) *image.NRGBA {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return toNRGBA(img)
}

// diffStats returns the maximum absolute channel difference and the mean absolute channel
// difference over RGBA.
func diffStats(a, b *image.NRGBA) (maxDiff int, mae float64) {
	if a.Bounds() != b.Bounds() {
		return 1 << 30, 1 << 30
	}
	var total float64
	var count int
	for i := range a.Pix {
		d := int(a.Pix[i]) - int(b.Pix[i])
		if d < 0 {
			d = -d
		}
		if d > maxDiff {
			maxDiff = d
		}
		total += float64(d)
		count++
	}
	if count == 0 {
		return 0, 0
	}
	return maxDiff, total / float64(count)
}

func assertExact(t *testing.T, name string, got, want *image.NRGBA) {
	t.Helper()
	if got.Bounds() != want.Bounds() {
		t.Fatalf("%s: bounds = %v, want %v", name, got.Bounds(), want.Bounds())
	}
	maxDiff, mae := diffStats(got, want)
	if maxDiff != 0 {
		t.Fatalf("%s: maxDiff = %d, mae = %.4f, want an exact match", name, maxDiff, mae)
	}
}

func assertMAE(t *testing.T, name string, got, want *image.NRGBA, bound float64) {
	t.Helper()
	if got.Bounds() != want.Bounds() {
		t.Fatalf("%s: bounds = %v, want %v", name, got.Bounds(), want.Bounds())
	}
	maxDiff, mae := diffStats(got, want)
	if mae > bound {
		t.Fatalf("%s: mae = %.4f, maxDiff = %d, want mae <= %.2f", name, mae, maxDiff, bound)
	}
	t.Logf("%s: mae = %.4f, maxDiff = %d (bound %.2f)", name, mae, maxDiff, bound)
}

func TestParityRotate(t *testing.T) {
	input := readFixture(t, "input_rect.png")
	for _, tc := range []struct {
		degrees int
		fixture string
	}{
		{90, "out_rotate90_rect.png"},
		{180, "out_rotate180_rect.png"},
		{270, "out_rotate270_rect.png"},
	} {
		gotBytes, err := RotateImage(input, tc.degrees)
		if err != nil {
			t.Fatalf("RotateImage(%d): %v", tc.degrees, err)
		}
		got := decodeAny(t, "go rotate", gotBytes)
		want := decodeAny(t, tc.fixture, readFixture(t, tc.fixture))
		assertExact(t, tc.fixture, got, want)
	}
}

func TestParityRotateZeroReturnsInputUnchanged(t *testing.T) {
	// rotateImage(0) is the identity and must not even decode.
	input := readFixture(t, "input_rect.png")
	got, err := RotateImage(input, 0)
	if err != nil {
		t.Fatalf("RotateImage(0): %v", err)
	}
	if &got[0] != &input[0] {
		t.Fatal("RotateImage(0) did not return the input buffer unchanged")
	}
}

func TestParityCrop(t *testing.T) {
	input := readFixture(t, "input_rect.png")
	gotBytes, err := CropImage(input, CropBox{X: 5, Y: 4, Width: 32, Height: 24})
	if err != nil {
		t.Fatalf("CropImage: %v", err)
	}
	got := decodeAny(t, "go crop", gotBytes)
	want := decodeAny(t, "out_crop_rect.png", readFixture(t, "out_crop_rect.png"))
	assertExact(t, "crop", got, want)
}

func TestParitySharpen(t *testing.T) {
	input := readFixture(t, "input_stripes.png")
	gotBytes, err := ApplySharpen(input, 25)
	if err != nil {
		t.Fatalf("ApplySharpen: %v", err)
	}
	got := decodeAny(t, "go sharpen", gotBytes)
	want := decodeAny(t, "out_sharpen_stripes.png", readFixture(t, "out_sharpen_stripes.png"))
	assertExact(t, "sharpen", got, want)
}

func TestParityBrightnessContrast(t *testing.T) {
	input := readFixture(t, "input_gradient.png")
	for _, tc := range []struct {
		name   string
		adjust Adjust
	}{
		{"both", Adjust{Brightness: 20, Contrast: 15}},
		{"brightness-only", Adjust{Brightness: 20}},
		{"contrast-only", Adjust{Contrast: 15}},
	} {
		gotBytes, err := ApplyBrightnessContrast(input, tc.adjust)
		if err != nil {
			t.Fatalf("ApplyBrightnessContrast(%s): %v", tc.name, err)
		}
		got := decodeAny(t, "go bc", gotBytes)
		var fixture string
		switch tc.name {
		case "both":
			fixture = "out_bc_gradient.png"
		case "brightness-only":
			fixture = "out_brightness_gradient.png"
		default:
			fixture = "out_contrast_gradient.png"
		}
		want := decodeAny(t, fixture, readFixture(t, fixture))
		assertMAE(t, tc.name, got, want, 1.0)
	}
}

func TestParityEncodeJpeg(t *testing.T) {
	input := readFixture(t, "input_gradient.png")
	gotBytes, err := EncodeJpeg(input, 85)
	if err != nil {
		t.Fatalf("EncodeJpeg: %v", err)
	}
	got := decodeAny(t, "go encodejpeg", gotBytes)
	want := decodeAny(t, "out_encode85_gradient.jpg", readFixture(t, "out_encode85_gradient.jpg"))
	assertMAE(t, "encodeJpeg", got, want, 6.0)
}

func TestParityNormalizeOrientation(t *testing.T) {
	for tag := 1; tag <= 8; tag++ {
		inputName := "input_exif_" + itoa(tag) + ".jpg"
		outName := "out_normalize_" + itoa(tag) + ".png"
		gotBytes, err := NormalizeOrientation(readFixture(t, inputName))
		if err != nil {
			t.Fatalf("NormalizeOrientation(%s): %v", inputName, err)
		}
		// The re-encoded PNG must carry no EXIF Orientation tag at all.
		if _, ok := exiforientation.ParseExifOrientation(gotBytes); ok {
			t.Fatalf("%s: normalized output still carries an EXIF orientation tag", inputName)
		}
		got := decodeAny(t, "go normalize", gotBytes)
		want := decodeAny(t, outName, readFixture(t, outName))
		assertMAE(t, outName, got, want, 5.0)
	}
}

func TestParityAutoLevels(t *testing.T) {
	for _, tc := range []struct {
		input   string
		fixture string
	}{
		{"input_gradient.png", "out_autolevels_gradient.json"},
		{"input_lowcontrast.png", "out_autolevels_lowcontrast.json"},
	} {
		brightness, contrast, err := ComputeAutoLevelsForImage(readFixture(t, tc.input))
		if err != nil {
			t.Fatalf("ComputeAutoLevelsForImage(%s): %v", tc.input, err)
		}
		var want struct {
			Brightness int `json:"brightness"`
			Contrast   int `json:"contrast"`
		}
		if err := json.Unmarshal(readFixture(t, tc.fixture), &want); err != nil {
			t.Fatalf("decode %s: %v", tc.fixture, err)
		}
		// x/image/draw and Skia resample differently, so allow a one-level slider difference.
		if absInt(brightness-want.Brightness) > 1 || absInt(contrast-want.Contrast) > 1 {
			t.Fatalf("%s: got brightness=%d contrast=%d, want brightness=%d contrast=%d (±1)",
				tc.input, brightness, contrast, want.Brightness, want.Contrast)
		}
	}
}

func TestParityLocalDocumentBox(t *testing.T) {
	var want map[string]struct {
		Kind string `json:"kind"`
		Box  *struct {
			X      float64 `json:"x"`
			Y      float64 `json:"y"`
			Width  float64 `json:"width"`
			Height float64 `json:"height"`
		} `json:"box"`
	}
	if err := json.Unmarshal(readFixture(t, "out_localbox.json"), &want); err != nil {
		t.Fatalf("decode out_localbox.json: %v", err)
	}
	for _, scene := range []struct {
		name                   string
		xTol, yTol, wTol, hTol float64
	}{
		{"doc", 5, 5, 8, 8},
		{"doc_big", 16, 16, 24, 24},
		{"uniform", 0, 0, 0, 0},
		{"fills_frame", 0, 0, 0, 0},
	} {
		expected := want[scene.name]
		got, err := DetectDocumentBoxLocally(readFixture(t, "input_"+scene.name+".png"))
		if err != nil {
			t.Fatalf("%s: DetectDocumentBoxLocally: %v", scene.name, err)
		}
		if string(got.Kind) != expected.Kind {
			t.Fatalf("%s: kind = %q, want %q", scene.name, got.Kind, expected.Kind)
		}
		if expected.Kind != string(LocalCropBox) {
			continue
		}
		if expected.Box == nil {
			t.Fatalf("%s: fixture kind=box but no box", scene.name)
		}
		b := got.Box
		if math.Abs(b.X-expected.Box.X) > scene.xTol ||
			math.Abs(b.Y-expected.Box.Y) > scene.yTol ||
			math.Abs(b.Width-expected.Box.Width) > scene.wTol ||
			math.Abs(b.Height-expected.Box.Height) > scene.hTol {
			t.Fatalf("%s: box = {%.0f %.0f %.0f %.0f}, want {%.0f %.0f %.0f %.0f} within tol",
				scene.name, b.X, b.Y, b.Width, b.Height,
				expected.Box.X, expected.Box.Y, expected.Box.Width, expected.Box.Height)
		}
	}
}

// TestDecodeFormats proves the x/image decoders are wired: webp is a canvas fixture; bmp and tiff
// are encoded here with x/image and round-tripped through decodeImage.
func TestDecodeFormats(t *testing.T) {
	webpBytes := readFixture(t, "input_rect.webp")
	img, err := decodeImage(webpBytes)
	if err != nil {
		t.Fatalf("decodeImage(webp): %v", err)
	}
	if img.Bounds().Dx() != 32 || img.Bounds().Dy() != 24 {
		t.Fatalf("webp dims = %dx%d, want 32x24", img.Bounds().Dx(), img.Bounds().Dy())
	}

	src := decodeAny(t, "rect", readFixture(t, "input_rect.png"))
	var bmpBuf, tiffBuf bytes.Buffer
	if err := bmp.Encode(&bmpBuf, src); err != nil {
		t.Fatalf("bmp.Encode: %v", err)
	}
	if err := tiff.Encode(&tiffBuf, src, nil); err != nil {
		t.Fatalf("tiff.Encode: %v", err)
	}
	for name, data := range map[string][]byte{"bmp": bmpBuf.Bytes(), "tiff": tiffBuf.Bytes()} {
		decoded, err := decodeImage(data)
		if err != nil {
			t.Fatalf("decodeImage(%s): %v", name, err)
		}
		if decoded.Bounds() != src.Bounds() {
			t.Fatalf("%s bounds = %v, want %v", name, decoded.Bounds(), src.Bounds())
		}
	}
}

// TestNormalizeOrientationDropsTagOnReencode is an explicit statement of Golden Rule 17's seam.
func TestNormalizeOrientationDropsTagOnReencode(t *testing.T) {
	for tag := 1; tag <= 8; tag++ {
		input := readFixture(t, "input_exif_"+itoa(tag)+".jpg")
		if _, ok := exiforientation.ParseExifOrientation(input); !ok {
			t.Fatalf("input_exif_%d.jpg unexpectedly has no orientation tag", tag)
		}
		got, err := NormalizeOrientation(input)
		if err != nil {
			t.Fatalf("NormalizeOrientation tag %d: %v", tag, err)
		}
		if _, ok := exiforientation.ParseExifOrientation(got); ok {
			t.Fatalf("normalized output for tag %d still carries an orientation tag", tag)
		}
		if _, err := png.Decode(bytes.NewReader(got)); err != nil {
			t.Fatalf("normalized output for tag %d is not a PNG: %v", tag, err)
		}
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// itoa avoids importing strconv just for the fixture names.
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	digits := ""
	for v > 0 {
		digits = string(rune('0'+v%10)) + digits
		v /= 10
	}
	return digits
}
