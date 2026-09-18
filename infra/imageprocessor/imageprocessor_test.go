package imageprocessor

// Ports src/infrastructure/image-processor.test.ts case-for-case. The TS suite is GREEN at port
// time (`npx vitest run src/infrastructure/image-processor.test.ts` -> 32 passed); there is no red
// case to pin. The synthetic scenes are rebuilt here with Go's stdlib image package exactly as the
// TS test builds them with @napi-rs/canvas (a fractional fill is area-coverage blended, so the
// encoded PNG matches what canvas would produce). The TS suite had no case for normalizeOrientation
// or encodeJpeg; those two are covered by parity_test.go and by the explicit EXIF-tag test there.

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"testing"
)

func newTestImage(w, h int) *image.NRGBA {
	return image.NewNRGBA(image.Rect(0, 0, w, h))
}

// fillRectCoverage reproduces canvas fillRect with fractional edges as an area-coverage blend.
func fillRectCoverage(img *image.NRGBA, x, y, w, h float64, c color.NRGBA) {
	x0 := int(math.Floor(x))
	x1 := int(math.Ceil(x + w))
	y0 := int(math.Floor(y))
	y1 := int(math.Ceil(y + h))
	for py := y0; py < y1; py++ {
		for px := x0; px < x1; px++ {
			if px < 0 || px >= img.Bounds().Dx() || py < 0 || py >= img.Bounds().Dy() {
				continue
			}
			cov := edgeOverlapFrac(px, x, w) * edgeOverlapFrac(py, y, h)
			if cov <= 0 {
				continue
			}
			blendPixel(img, px, py, c, cov)
		}
	}
}

func edgeOverlapFrac(p int, start, size float64) float64 {
	lo := math.Max(float64(p), start)
	hi := math.Min(float64(p+1), start+size)
	if hi <= lo {
		return 0
	}
	return hi - lo
}

func blendPixel(img *image.NRGBA, x, y int, c color.NRGBA, cov float64) {
	i := img.PixOffset(x, y)
	img.Pix[i] = uint8(math.Round(float64(c.R)*cov + float64(img.Pix[i])*(1-cov)))
	img.Pix[i+1] = uint8(math.Round(float64(c.G)*cov + float64(img.Pix[i+1])*(1-cov)))
	img.Pix[i+2] = uint8(math.Round(float64(c.B)*cov + float64(img.Pix[i+2])*(1-cov)))
	img.Pix[i+3] = uint8(math.Round(float64(c.A)*cov + float64(img.Pix[i+3])*(1-cov)))
}

func encodeTestPNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func decodeTestImage(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("image.Decode: %v", err)
	}
	return img
}

func makeTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := newTestImage(w, h)
	fillRectCoverage(img, 0, 0, float64(w), float64(h), color.NRGBA{R: 200, G: 200, B: 200, A: 255})
	fillRectCoverage(img, 0, 0, float64(maxInt(1, w/4)), float64(maxInt(1, h/4)), color.NRGBA{R: 20, G: 20, B: 20, A: 255})
	return encodeTestPNG(t, img)
}

func docOnDeskPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := newTestImage(w, h)
	fillRectCoverage(img, 0, 0, float64(w), float64(h), color.NRGBA{R: 15, G: 15, B: 15, A: 255})
	fillRectCoverage(img, 0.2*float64(w), 0.2*float64(h), 0.6*float64(w), 0.6*float64(h), color.NRGBA{R: 250, G: 250, B: 250, A: 255})
	return encodeTestPNG(t, img)
}

func fillsFramePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := newTestImage(w, h)
	fillRectCoverage(img, 0, 0, float64(w), float64(h), color.NRGBA{R: 246, G: 246, B: 244, A: 255})
	for i := 0; i < 14; i++ {
		fillRectCoverage(img,
			0.12*float64(w), (0.1+float64(i)*0.055)*float64(h),
			0.72*float64(w), 0.02*float64(h),
			color.NRGBA{R: 30, G: 30, B: 30, A: 255})
	}
	return encodeTestPNG(t, img)
}

func uniformPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := newTestImage(w, h)
	fillRectCoverage(img, 0, 0, float64(w), float64(h), color.NRGBA{R: 200, G: 200, B: 200, A: 255})
	return encodeTestPNG(t, img)
}

func TestRotateImageReturnsInputUnchangedForZero(t *testing.T) {
	buf := makeTestPNG(t, 10, 6)
	result, err := RotateImage(buf, 0)
	if err != nil {
		t.Fatalf("RotateImage(0): %v", err)
	}
	if &result[0] != &buf[0] {
		t.Fatal("RotateImage(0) did not return the input buffer")
	}
}

func TestRotateImageSwapsWidthAndHeightFor90(t *testing.T) {
	result, err := RotateImage(makeTestPNG(t, 10, 6), 90)
	if err != nil {
		t.Fatalf("RotateImage(90): %v", err)
	}
	img := decodeTestImage(t, result)
	if img.Bounds().Dx() != 6 || img.Bounds().Dy() != 10 {
		t.Fatalf("dims = %dx%d, want 6x10", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

func TestRotateImageSwapsWidthAndHeightFor270(t *testing.T) {
	result, err := RotateImage(makeTestPNG(t, 10, 6), 270)
	if err != nil {
		t.Fatalf("RotateImage(270): %v", err)
	}
	img := decodeTestImage(t, result)
	if img.Bounds().Dx() != 6 || img.Bounds().Dy() != 10 {
		t.Fatalf("dims = %dx%d, want 6x10", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

func TestRotateImageKeepsDimensionsFor180(t *testing.T) {
	result, err := RotateImage(makeTestPNG(t, 10, 6), 180)
	if err != nil {
		t.Fatalf("RotateImage(180): %v", err)
	}
	img := decodeTestImage(t, result)
	if img.Bounds().Dx() != 10 || img.Bounds().Dy() != 6 {
		t.Fatalf("dims = %dx%d, want 10x6", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

func TestCropImageProducesExactDimensionsInBounds(t *testing.T) {
	result, err := CropImage(makeTestPNG(t, 100, 80), CropBox{X: 10, Y: 10, Width: 50, Height: 40})
	if err != nil {
		t.Fatalf("CropImage: %v", err)
	}
	img := decodeTestImage(t, result)
	if img.Bounds().Dx() != 50 || img.Bounds().Dy() != 40 {
		t.Fatalf("dims = %dx%d, want 50x40", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

func TestCropImageClampsBoxPastBounds(t *testing.T) {
	result, err := CropImage(makeTestPNG(t, 100, 80), CropBox{X: 90, Y: 70, Width: 50, Height: 40})
	if err != nil {
		t.Fatalf("CropImage: %v", err)
	}
	img := decodeTestImage(t, result)
	if img.Bounds().Dx() > 10 || img.Bounds().Dy() > 10 {
		t.Fatalf("dims = %dx%d, want <= 10x10", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

func TestComputeAutoLevelsBoostsContrastForLowContrastDocument(t *testing.T) {
	img := newTestImage(100, 100)
	fillRectCoverage(img, 0, 0, 100, 100, color.NRGBA{R: 200, G: 200, B: 200, A: 255})
	fillRectCoverage(img, 0, 0, 15, 15, color.NRGBA{R: 60, G: 60, B: 60, A: 255})
	brightness, contrast, err := ComputeAutoLevelsForImage(encodeTestPNG(t, img))
	if err != nil {
		t.Fatalf("ComputeAutoLevelsForImage: %v", err)
	}
	if contrast <= 20 {
		t.Fatalf("contrast = %d, want > 20", contrast)
	}
	if absInt(brightness) >= 20 {
		t.Fatalf("brightness = %d, want |brightness| < 20", brightness)
	}
}

func TestApplyBrightnessContrastReturnsInputUnchangedWhenZero(t *testing.T) {
	buf := makeTestPNG(t, 10, 10)
	result, err := ApplyBrightnessContrast(buf, Adjust{})
	if err != nil {
		t.Fatalf("ApplyBrightnessContrast: %v", err)
	}
	if &result[0] != &buf[0] {
		t.Fatal("ApplyBrightnessContrast with zero deltas did not return the input buffer")
	}
}

func TestApplyBrightnessContrastPreservesDimensionsAndChangesPixels(t *testing.T) {
	buf := makeTestPNG(t, 10, 10)
	result, err := ApplyBrightnessContrast(buf, Adjust{Brightness: 20, Contrast: 15})
	if err != nil {
		t.Fatalf("ApplyBrightnessContrast: %v", err)
	}
	img := decodeTestImage(t, result)
	if img.Bounds().Dx() != 10 || img.Bounds().Dy() != 10 {
		t.Fatalf("dims = %dx%d, want 10x10", img.Bounds().Dx(), img.Bounds().Dy())
	}
	if bytes.Equal(result, buf) {
		t.Fatal("adjusted output equals the input, want different bytes")
	}
}

func TestApplySharpenReturnsInputUnchangedWhenZero(t *testing.T) {
	buf := makeTestPNG(t, 10, 10)
	result, err := ApplySharpen(buf, 0)
	if err != nil {
		t.Fatalf("ApplySharpen: %v", err)
	}
	if &result[0] != &buf[0] {
		t.Fatal("ApplySharpen(0) did not return the input buffer")
	}
}

func TestApplySharpenChangesPixelsAtHardEdge(t *testing.T) {
	buf := makeTestPNG(t, 10, 10)
	result, err := ApplySharpen(buf, 25)
	if err != nil {
		t.Fatalf("ApplySharpen: %v", err)
	}
	if bytes.Equal(result, buf) {
		t.Fatal("sharpened output equals the input, want different bytes")
	}
}

func TestApplySharpenPreservesDimensions(t *testing.T) {
	result, err := ApplySharpen(makeTestPNG(t, 10, 10), 25)
	if err != nil {
		t.Fatalf("ApplySharpen: %v", err)
	}
	img := decodeTestImage(t, result)
	if img.Bounds().Dx() != 10 || img.Bounds().Dy() != 10 {
		t.Fatalf("dims = %dx%d, want 10x10", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

// TestEncodeJpegQualitySemantics pins the one documented quality deviation: @napi-rs/canvas takes
// 0-100, while Go's encoder treats <=0 as "default 75". A requested 0 must therefore still produce
// the LOWEST quality JPEG rather than silently jumping to 75, and >100 clamps to 100.
func TestEncodeJpegQualitySemantics(t *testing.T) {
	input := makeTestPNG(t, 64, 48)

	low, err := EncodeJpeg(input, 0)
	if err != nil {
		t.Fatalf("EncodeJpeg(0): %v", err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(low)); err != nil {
		t.Fatalf("EncodeJpeg(0) produced an undecodable JPEG: %v", err)
	}

	high, err := EncodeJpeg(input, 100)
	if err != nil {
		t.Fatalf("EncodeJpeg(100): %v", err)
	}
	over, err := EncodeJpeg(input, 1000)
	if err != nil {
		t.Fatalf("EncodeJpeg(1000): %v", err)
	}
	if !bytes.Equal(high, over) {
		t.Fatal("EncodeJpeg(1000) differs from EncodeJpeg(100), want the >100 clamp")
	}
	if len(low) >= len(high) {
		t.Fatalf("EncodeJpeg(0) is %d bytes and EncodeJpeg(100) is %d bytes; want the requested 0 to be smaller (it must not fall back to Go's default 75)", len(low), len(high))
	}

	img := decodeTestImage(t, low)
	if img.Bounds().Dx() != 64 || img.Bounds().Dy() != 48 {
		t.Fatalf("dims = %dx%d, want 64x48", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

func TestDetectDocumentBoxLocallyReturnsOriginalPixelBox(t *testing.T) {
	w, h := 400, 300 // below WORK_MAX_DIM: working pixels map 1:1 to original pixels
	r, err := DetectDocumentBoxLocally(docOnDeskPNG(t, w, h))
	if err != nil {
		t.Fatalf("DetectDocumentBoxLocally: %v", err)
	}
	if r.Kind != LocalCropBox {
		t.Fatalf("kind = %q, want %q", r.Kind, LocalCropBox)
	}
	assertWithin(t, "box.x", r.Box.X, 80, 5)
	assertWithin(t, "box.y", r.Box.Y, 60, 5)
	assertWithin(t, "box.width", r.Box.Width, 240, 8)
	assertWithin(t, "box.height", r.Box.Height, 180, 8)
}

func TestDetectDocumentBoxLocallyMapsBackFromWorkingScale(t *testing.T) {
	w, h := 1600, 1200 // downscaled to WORK_MAX_DIM internally, then mapped back
	r, err := DetectDocumentBoxLocally(docOnDeskPNG(t, w, h))
	if err != nil {
		t.Fatalf("DetectDocumentBoxLocally: %v", err)
	}
	if r.Kind != LocalCropBox {
		t.Fatalf("kind = %q, want %q", r.Kind, LocalCropBox)
	}
	assertWithin(t, "box.x", r.Box.X, 320, 16)
	assertWithin(t, "box.y", r.Box.Y, 240, 16)
	assertWithin(t, "box.width", r.Box.Width, 960, 24)
	assertWithin(t, "box.height", r.Box.Height, 720, 24)
}

func TestDetectDocumentBoxLocallyReportsNoSignalForUniformImage(t *testing.T) {
	r, err := DetectDocumentBoxLocally(uniformPNG(t, 200, 200))
	if err != nil {
		t.Fatalf("DetectDocumentBoxLocally: %v", err)
	}
	if r.Kind != LocalCropNoSignal {
		t.Fatalf("kind = %q, want %q", r.Kind, LocalCropNoSignal)
	}
}

func TestDetectDocumentBoxLocallyReportsVetoedForFullFramePage(t *testing.T) {
	r, err := DetectDocumentBoxLocally(fillsFramePNG(t, 400, 300))
	if err != nil {
		t.Fatalf("DetectDocumentBoxLocally: %v", err)
	}
	if r.Kind != LocalCropVetoed {
		t.Fatalf("kind = %q, want %q", r.Kind, LocalCropVetoed)
	}
}

func TestDetectCropBoxLocallyUnwrapsBox(t *testing.T) {
	box, err := DetectCropBoxLocally(docOnDeskPNG(t, 400, 300))
	if err != nil {
		t.Fatalf("DetectCropBoxLocally: %v", err)
	}
	if box == nil {
		t.Fatal("box = nil, want a box")
	}
	assertWithin(t, "box.x", box.X, 80, 5)
	assertWithin(t, "box.y", box.Y, 60, 5)
}

func TestDetectCropBoxLocallyReturnsNilForUniformImage(t *testing.T) {
	box, err := DetectCropBoxLocally(uniformPNG(t, 200, 200))
	if err != nil {
		t.Fatalf("DetectCropBoxLocally: %v", err)
	}
	if box != nil {
		t.Fatalf("box = %+v, want nil", box)
	}
}

func TestDetectCropBoxLocallyReturnsNilForFillsFramePage(t *testing.T) {
	box, err := DetectCropBoxLocally(fillsFramePNG(t, 400, 300))
	if err != nil {
		t.Fatalf("DetectCropBoxLocally: %v", err)
	}
	if box != nil {
		t.Fatalf("box = %+v, want nil", box)
	}
}

func assertWithin(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s = %v, want within %v of %v", name, got, want, tol)
	}
}
