package imagedimensions

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"testing"
)

// The TS test renders fixtures with @napi-rs/canvas and then reads back their headers. Go's
// standard library is the dependency-free equivalent, so the fixtures are real PNG/JPEG bytes
// produced by image/png and image/jpeg. Inputs and expected outputs are otherwise identical to
// pdf-triage's src/domain/image-dimensions.test.ts.

// render mirrors the TS test's white canvas with black "SAMPLE TEXT", in the sense that it produces
// a genuine encoded image of the requested size. The pixel content is irrelevant to a header read.
func render(width, height int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(20, height/2-20, 20+11*40, height/2+20), image.NewUniform(color.Black), image.Point{}, draw.Src)
	return img
}

func encodePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, render(width, height)); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func encodeJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, render(width, height), nil); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

func assertDims(t *testing.T, buf []byte, wantWidth, wantHeight int) {
	t.Helper()
	got, ok := ReadImageDimensions(buf)
	if !ok {
		t.Fatalf("ReadImageDimensions returned no result, want {%d %d}", wantWidth, wantHeight)
	}
	if got.Width != wantWidth || got.Height != wantHeight {
		t.Fatalf("got {%d %d}, want {%d %d}", got.Width, got.Height, wantWidth, wantHeight)
	}
}

func TestReadImageDimensions(t *testing.T) {
	t.Run("reads the geometry of a real PNG", func(t *testing.T) {
		// The production path: ocrPageBuffer hands over canvas.toBuffer('image/png').
		assertDims(t, encodePNG(t, 1190, 1684), 1190, 1684)
	})

	t.Run("reads the geometry of a real JPEG", func(t *testing.T) {
		// extractPDFContent's image branch hands over the original photo, usually a JPEG.
		assertDims(t, encodeJPEG(t, 800, 600), 800, 600)
	})

	t.Run("reads a non-square JPEG the right way round", func(t *testing.T) {
		// JPEG stores height BEFORE width in SOFn — transposing them is the classic bug here, and a
		// square fixture cannot catch it.
		assertDims(t, encodeJPEG(t, 1000, 400), 1000, 400)
	})

	t.Run("returns null for a format it does not understand", func(t *testing.T) {
		if _, ok := ReadImageDimensions([]byte("GIF89a and then some bytes")); ok {
			t.Fatalf("expected no result for a GIF")
		}
	})

	t.Run("returns null rather than throwing on a truncated PNG header", func(t *testing.T) {
		buf := encodePNG(t, 100, 100)[:12]
		if _, ok := ReadImageDimensions(buf); ok {
			t.Fatalf("expected no result for a truncated PNG header")
		}
	})

	t.Run("returns null rather than throwing on a JPEG that is only its start-of-image marker", func(t *testing.T) {
		if _, ok := ReadImageDimensions([]byte{0xff, 0xd8, 0xff, 0xe0}); ok {
			t.Fatalf("expected no result for a bare SOI marker")
		}
	})

	t.Run("returns null on an empty buffer", func(t *testing.T) {
		if _, ok := ReadImageDimensions([]byte{}); ok {
			t.Fatalf("expected no result for an empty buffer")
		}
	})
}
