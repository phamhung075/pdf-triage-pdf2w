package crop

// Ports src/infrastructure/crop-detector.test.ts case-for-case. The TS suite is GREEN at port time
// (`npx vitest run src/infrastructure/crop-detector.test.ts` -> 9 passed).
//
// The TS test mocked detectCropBox, detectDocumentBoxLocally and loadImage(1000x800). This port
// injects the two signals and supplies a real 1000x800 PNG so imagedimensions reads the same frame
// the TS mock returned.

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/infra/imageprocessor"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/vision"
)

func frame(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 1000, 800))
	for y := 0; y < 800; y++ {
		for x := 0; x < 1000; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 200, G: 200, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	return buf.Bytes()
}

func box(x, y, width, height float64) vision.CropBox {
	return vision.CropBox{X: x, Y: y, Width: width, Height: height}
}

func modelDetector(cropBox *vision.CropBox, raw string) ModelDetector {
	return func(context.Context, []byte) (vision.CropResult, error) {
		return vision.CropResult{CropBox: cropBox, Raw: raw}, nil
	}
}

func localBox(b vision.CropBox) LocalDetector {
	return func([]byte) (imageprocessor.LocalCropResult, error) {
		return imageprocessor.LocalCropResult{Kind: imageprocessor.LocalCropBox, Box: b}, nil
	}
}

func localNoSignal() LocalDetector {
	return func([]byte) (imageprocessor.LocalCropResult, error) {
		return imageprocessor.LocalCropResult{Kind: imageprocessor.LocalCropNoSignal}, nil
	}
}

func localVetoed() LocalDetector {
	return func([]byte) (imageprocessor.LocalCropResult, error) {
		return imageprocessor.LocalCropResult{Kind: imageprocessor.LocalCropVetoed}, nil
	}
}

func TestCascadeUsesModelBoxWhenItAgreesWithFlood(t *testing.T) {
	modelBox := box(100, 100, 700, 500)
	result, err := DetectCropBoxCascade(context.Background(), frame(t),
		modelDetector(&modelBox, `{"cropBox":{}}`), localBox(box(110, 110, 690, 490)))
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.Source != SourceModelFloodAgree {
		t.Errorf("source = %q, want %q", result.Source, SourceModelFloodAgree)
	}
	if result.CropBox == nil || *result.CropBox != modelBox {
		t.Errorf("cropBox = %+v, want %+v", result.CropBox, modelBox)
	}
}

func TestCascadeOverridesWithFloodWhenModelReturnsFullBounds(t *testing.T) {
	full := box(0, 0, 1000, 800)
	local := box(80, 60, 800, 600)
	result, err := DetectCropBoxCascade(context.Background(), frame(t),
		modelDetector(&full, `{"cropBox":{"x":0,"y":0,"width":1000,"height":800}}`), localBox(local))
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.Source != SourceFloodOverride {
		t.Errorf("source = %q, want %q", result.Source, SourceFloodOverride)
	}
	if result.CropBox == nil || *result.CropBox != local {
		t.Errorf("cropBox = %+v, want %+v", result.CropBox, local)
	}
}

func TestCascadeOverridesWithFloodWhenModelReturnsNull(t *testing.T) {
	local := box(80, 60, 800, 600)
	result, err := DetectCropBoxCascade(context.Background(), frame(t),
		modelDetector(nil, `{"cropBox":null}`), localBox(local))
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.Source != SourceFloodOverride {
		t.Errorf("source = %q, want %q", result.Source, SourceFloodOverride)
	}
	if result.CropBox == nil || *result.CropBox != local {
		t.Errorf("cropBox = %+v, want %+v", result.CropBox, local)
	}
}

func TestCascadeOverridesWithFloodOnStrongDisagreement(t *testing.T) {
	modelBox := box(0, 0, 400, 300)
	local := box(600, 500, 400, 300)
	result, err := DetectCropBoxCascade(context.Background(), frame(t),
		modelDetector(&modelBox, `{"cropBox":{}}`), localBox(local))
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.Source != SourceFloodOverride {
		t.Errorf("source = %q, want %q", result.Source, SourceFloodOverride)
	}
	if result.CropBox == nil || *result.CropBox != local {
		t.Errorf("cropBox = %+v, want %+v", result.CropBox, local)
	}
}

func TestCascadeFallsBackToModelWhenFloodInconclusive(t *testing.T) {
	modelBox := box(50, 50, 900, 700)
	result, err := DetectCropBoxCascade(context.Background(), frame(t),
		modelDetector(&modelBox, `{"cropBox":{}}`), localNoSignal())
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.Source != SourceModelOnly {
		t.Errorf("source = %q, want %q", result.Source, SourceModelOnly)
	}
	if result.CropBox == nil || *result.CropBox != modelBox {
		t.Errorf("cropBox = %+v, want %+v", result.CropBox, modelBox)
	}
}

func TestCascadeReturnsNilWhenNeitherFindsAnything(t *testing.T) {
	result, err := DetectCropBoxCascade(context.Background(), frame(t),
		modelDetector(nil, `{"cropBox":null}`), localNoSignal())
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.Source != SourceNone {
		t.Errorf("source = %q, want %q", result.Source, SourceNone)
	}
	if result.CropBox != nil {
		t.Errorf("cropBox = %+v, want nil", result.CropBox)
	}
}

func TestCascadeReportsNoCropWhenFloodInconclusiveAndModelDegenerate(t *testing.T) {
	full := box(0, 0, 1000, 800)
	result, err := DetectCropBoxCascade(context.Background(), frame(t),
		modelDetector(&full, `{"cropBox":{"x":0,"y":0,"width":1000,"height":800}}`), localNoSignal())
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.Source != SourceNone {
		t.Errorf("source = %q, want %q", result.Source, SourceNone)
	}
	if result.CropBox != nil {
		t.Errorf("cropBox = %+v, want nil", result.CropBox)
	}
}

func TestCascadeShortCircuitsToNoCropOnVeto(t *testing.T) {
	modelBox := box(120, 90, 500, 400)
	result, err := DetectCropBoxCascade(context.Background(), frame(t),
		modelDetector(&modelBox, `{"cropBox":{}}`), localVetoed())
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.Source != SourceFloodVeto {
		t.Errorf("source = %q, want %q", result.Source, SourceFloodVeto)
	}
	if result.CropBox != nil {
		t.Errorf("cropBox = %+v, want nil", result.CropBox)
	}
	if result.ModelCropBox == nil || *result.ModelCropBox != modelBox {
		t.Errorf("modelCropBox = %+v, want %+v (reported for diagnostics)", result.ModelCropBox, modelBox)
	}
	if result.FloodCropBox != nil {
		t.Errorf("floodCropBox = %+v, want nil", result.FloodCropBox)
	}
}

func TestCascadeVetoesRegardlessOfModel(t *testing.T) {
	full := box(0, 0, 1000, 800)
	result, err := DetectCropBoxCascade(context.Background(), frame(t),
		modelDetector(&full, `{"cropBox":{}}`), localVetoed())
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.Source != SourceFloodVeto {
		t.Errorf("source = %q, want %q", result.Source, SourceFloodVeto)
	}
	if result.CropBox != nil {
		t.Errorf("cropBox = %+v, want nil", result.CropBox)
	}
}
