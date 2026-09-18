package imagetopdf

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/infra/crop"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/imageprocessor"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/orientation"
)

// Ported from pdf-triage's src/application/image-to-pdf.integration.test.ts (1 case, GREEN
// upstream: `npx vitest run src/application/image-to-pdf.integration.test.ts` -> 1 passed).
//
// Unlike imagetopdf_test.go (which fakes every neighbor), this file fakes ONLY the two cascades
// (which themselves wrap real model calls) and lets the REAL imageprocessor operations run against a
// real synthetic PNG, to prove the step functions actually compose with real rotate/crop/enhance,
// not just with mocks that happen to satisfy the interface.

func makeTestPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{R: 200, G: 200, B: 200, A: 255}), image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(0, 0, maxInt(1, w/4), maxInt(1, h/4)), image.NewUniform(color.RGBA{R: 20, G: 20, B: 20, A: 255}), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func mustDecodeBase64(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	return decoded
}

func pngDims(t *testing.T, buffer []byte) (int, int) {
	t.Helper()
	cfg, err := png.DecodeConfig(bytes.NewReader(buffer))
	if err != nil {
		t.Fatalf("decode image config: %v", err)
	}
	return cfg.Width, cfg.Height
}

func TestVisionLabStepFunctionsComposeWithRealImageProcessor(t *testing.T) {
	deps := Deps{
		NormalizeOrientation: imageprocessor.NormalizeOrientation,
		DetectOrientation: func(context.Context, []byte) (orientation.DetectionResult, error) {
			return orientation.DetectionResult{
				RotationDegrees: 90,
				ExifDegrees:     nil,
				ModelDegrees:    90,
				ModelRaw:        `{"rotationDegrees":90}`,
				Source:          orientation.SourceExifModelAgree,
			}, nil
		},
		RotateImage: imageprocessor.RotateImage,
		DetectCropBox: func(context.Context, []byte) (crop.DetectionResult, error) {
			box := imageprocessor.CropBox{X: 5, Y: 5, Width: 50, Height: 40}
			return crop.DetectionResult{
				CropBox:      &box,
				ModelCropBox: &box,
				ModelRaw:     `{"cropBox":{"x":5,"y":5,"width":50,"height":40}}`,
				FloodCropBox: nil,
				Source:       crop.SourceModelFloodAgree,
			}, nil
		},
		CropImage:               imageprocessor.CropImage,
		ComputeAutoLevels:       imageprocessor.ComputeAutoLevelsForImage,
		ApplyBrightnessContrast: imageprocessor.ApplyBrightnessContrast,
		ApplySharpen:            imageprocessor.ApplySharpen,
	}
	stepper := NewStepper(deps)
	buf := makeTestPNG(100, 80)

	orientResult := stepper.RunOrientStep(context.Background(), buf)
	if orientResult.Error != "" {
		t.Fatalf("RunOrientStep error = %q", orientResult.Error)
	}
	oriented := mustDecodeBase64(t, orientResult.ImageBase64)
	orientedW, orientedH := pngDims(t, oriented)
	if orientedW != 80 || orientedH != 100 {
		t.Fatalf("oriented dims = %dx%d, want 80x100", orientedW, orientedH)
	}

	cropResult := stepper.RunCropStep(context.Background(), oriented)
	if cropResult.Error != "" {
		t.Fatalf("RunCropStep error = %q", cropResult.Error)
	}
	cropped := mustDecodeBase64(t, cropResult.ImageBase64)
	croppedW, croppedH := pngDims(t, cropped)
	if croppedW != 50 || croppedH != 40 {
		t.Fatalf("cropped dims = %dx%d, want 50x40", croppedW, croppedH)
	}

	enhanceResult := stepper.RunEnhanceStep(context.Background(), cropped)
	if enhanceResult.Error != "" {
		t.Fatalf("RunEnhanceStep error = %q", enhanceResult.Error)
	}
	if len(enhanceResult.ImageBase64) == 0 {
		t.Fatal("enhanced ImageBase64 is empty")
	}
	enhancedW, enhancedH := pngDims(t, mustDecodeBase64(t, enhanceResult.ImageBase64))
	if enhancedW <= 0 || enhancedH <= 0 {
		t.Fatalf("enhanced dims = %dx%d, want positive", enhancedW, enhancedH)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
