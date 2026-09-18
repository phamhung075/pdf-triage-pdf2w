package imagetopdf

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"reflect"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/infra/crop"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/imageprocessor"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/orientation"
)

// These cases are ported from pdf-triage's src/application/image-to-pdf.test.ts (11 cases: 5 for
// runOrientStep, 4 for runCropStep, 2 for runEnhanceStep). The upstream TypeScript suite is GREEN at
// port time (`npx vitest run src/application/image-to-pdf.test.ts` -> 11 passed), so no upstream
// case is pinned red. RunOrientStep carries the Golden Rule 17 regression guard (EXIF must be
// normalized before measuring, and the raw upload must never be rotated).

var (
	originalBuf   = []byte("original")
	normalizedBuf = []byte("normalized")
	orientedBuf   = []byte("oriented")
	croppedBuf    = []byte("cropped")
	leveledBuf    = []byte("leveled")
	finalBuf      = []byte("final")
)

func intPtr(v int) *int { return &v }

// baseDeps returns a Deps whose seams all have a safe default; unused seams fail loudly instead of
// silently succeeding. Individual cases override the seam under test.
func baseDeps() Deps {
	return Deps{
		NormalizeOrientation: func([]byte) ([]byte, error) { return normalizedBuf, nil },
		DetectOrientation: func(context.Context, []byte) (orientation.DetectionResult, error) {
			return orientation.DetectionResult{}, errors.New("unexpected DetectOrientation call")
		},
		RotateImage: func(buffer []byte, degrees int) ([]byte, error) {
			if degrees == 0 {
				return buffer, nil
			}
			return nil, fmt.Errorf("unexpected RotateImage(%d) call", degrees)
		},
		DetectCropBox: func(context.Context, []byte) (crop.DetectionResult, error) {
			return crop.DetectionResult{}, errors.New("unexpected DetectCropBox call")
		},
		CropImage:               func(buffer []byte, _ imageprocessor.CropBox) ([]byte, error) { return buffer, nil },
		ComputeAutoLevels:       func([]byte) (int, int, error) { return 0, 0, nil },
		ApplyBrightnessContrast: func(buffer []byte, _ imageprocessor.Adjust) ([]byte, error) { return buffer, nil },
		ApplySharpen:            func(buffer []byte, _ float64) ([]byte, error) { return buffer, nil },
	}
}

func findCandidate(candidates []StepCandidate, label string) (StepCandidate, bool) {
	for _, candidate := range candidates {
		if candidate.Label == label {
			return candidate, true
		}
	}
	return StepCandidate{}, false
}

func TestRunOrientStepRotatesByCascadeChosenDegreesAndIncludesEveryNonNullCandidate(t *testing.T) {
	deps := baseDeps()
	deps.DetectOrientation = func(context.Context, []byte) (orientation.DetectionResult, error) {
		return orientation.DetectionResult{
			RotationDegrees: 90,
			ExifDegrees:     intPtr(90),
			ModelDegrees:    0,
			ModelRaw:        `{"rotationDegrees":0}`,
			Source:          orientation.SourceModelOnly,
		}, nil
	}
	deps.RotateImage = func(_ []byte, degrees int) ([]byte, error) {
		if degrees == 90 {
			return orientedBuf, nil
		}
		return []byte(fmt.Sprintf("rotated-%d", degrees)), nil
	}

	result := NewStepper(deps).RunOrientStep(context.Background(), originalBuf)

	if result.Step != 1 {
		t.Fatalf("Step = %d, want 1", result.Step)
	}
	if result.Label != LabelOriented {
		t.Fatalf("Label = %q, want %q", result.Label, LabelOriented)
	}
	if result.ImageBase64 != base64.StdEncoding.EncodeToString(orientedBuf) {
		t.Fatalf("ImageBase64 = %q, want base64(oriented)", result.ImageBase64)
	}
	if result.ModelRaw != `{"rotationDegrees":0}` {
		t.Fatalf("ModelRaw = %q", result.ModelRaw)
	}
	wantMeta := map[string]any{"rotationDegrees": 90, "exifDegrees": 90, "modelDegrees": 0, "source": "model-only"}
	if !reflect.DeepEqual(result.Meta, wantMeta) {
		t.Fatalf("Meta = %#v, want %#v", result.Meta, wantMeta)
	}

	if len(result.Candidates) != 2 {
		t.Fatalf("len(Candidates) = %d, want 2", len(result.Candidates))
	}
	exif, ok := findCandidate(result.Candidates, "exif")
	if !ok {
		t.Fatal("missing exif candidate")
	}
	wantExif := StepCandidate{Label: "exif", Chosen: true, ImageBase64: base64.StdEncoding.EncodeToString(orientedBuf), Meta: map[string]any{"rotationDegrees": 90}}
	if !reflect.DeepEqual(exif, wantExif) {
		t.Fatalf("exif candidate = %#v, want %#v", exif, wantExif)
	}
	model, ok := findCandidate(result.Candidates, "model")
	if !ok {
		t.Fatal("missing model candidate")
	}
	wantModel := StepCandidate{Label: "model", Chosen: false, ImageBase64: base64.StdEncoding.EncodeToString([]byte("rotated-0")), Meta: map[string]any{"rotationDegrees": 0}}
	if !reflect.DeepEqual(model, wantModel) {
		t.Fatalf("model candidate = %#v, want %#v", model, wantModel)
	}
}

func TestRunOrientStepOmitsExifCandidateWhenDegreesAreNull(t *testing.T) {
	deps := baseDeps()
	deps.DetectOrientation = func(context.Context, []byte) (orientation.DetectionResult, error) {
		return orientation.DetectionResult{RotationDegrees: 0, ExifDegrees: nil, ModelDegrees: 0, ModelRaw: `{"rotationDegrees":0}`, Source: orientation.SourceModelOnly}, nil
	}
	deps.RotateImage = func([]byte, int) ([]byte, error) { return orientedBuf, nil }

	result := NewStepper(deps).RunOrientStep(context.Background(), originalBuf)

	if len(result.Candidates) != 1 {
		t.Fatalf("len(Candidates) = %d, want 1", len(result.Candidates))
	}
	want := StepCandidate{Label: "model", Chosen: true, ImageBase64: base64.StdEncoding.EncodeToString(orientedBuf), Meta: map[string]any{"rotationDegrees": 0}}
	if !reflect.DeepEqual(result.Candidates[0], want) {
		t.Fatalf("candidate = %#v, want %#v", result.Candidates[0], want)
	}
}

func TestRunOrientStepRecordsErrorAndNoCandidatesWhenOrientationDetectionFails(t *testing.T) {
	deps := baseDeps()
	deps.DetectOrientation = func(context.Context, []byte) (orientation.DetectionResult, error) {
		return orientation.DetectionResult{}, errors.New("vision model unreachable")
	}

	result := NewStepper(deps).RunOrientStep(context.Background(), originalBuf)

	if result.Step != 1 {
		t.Fatalf("Step = %d, want 1", result.Step)
	}
	if result.Error != "vision model unreachable" {
		t.Fatalf("Error = %q", result.Error)
	}
	if result.Candidates != nil {
		t.Fatalf("Candidates = %#v, want nil (TS undefined)", result.Candidates)
	}
	if result.DurationMS < 0 {
		t.Fatalf("DurationMS = %d, want a number", result.DurationMS)
	}
}

func TestRunOrientStepIsolatesAFailingCandidateRenderToThatCandidate(t *testing.T) {
	deps := baseDeps()
	deps.DetectOrientation = func(context.Context, []byte) (orientation.DetectionResult, error) {
		return orientation.DetectionResult{RotationDegrees: 90, ExifDegrees: intPtr(90), ModelDegrees: 0, ModelRaw: `{"rotationDegrees":0}`, Source: orientation.SourceModelOnly}, nil
	}
	deps.RotateImage = func(_ []byte, degrees int) ([]byte, error) {
		if degrees == 90 {
			return orientedBuf, nil
		}
		return nil, errors.New("rotate failed for candidate")
	}

	result := NewStepper(deps).RunOrientStep(context.Background(), originalBuf)

	if result.Error != "" {
		t.Fatalf("Error = %q, want empty", result.Error)
	}
	if result.ImageBase64 != base64.StdEncoding.EncodeToString(orientedBuf) {
		t.Fatalf("ImageBase64 = %q", result.ImageBase64)
	}
	if len(result.Candidates) != 2 {
		t.Fatalf("len(Candidates) = %d, want 2", len(result.Candidates))
	}
	exif, _ := findCandidate(result.Candidates, "exif")
	if !reflect.DeepEqual(exif, StepCandidate{Label: "exif", Chosen: true, ImageBase64: base64.StdEncoding.EncodeToString(orientedBuf), Meta: map[string]any{"rotationDegrees": 90}}) {
		t.Fatalf("exif candidate = %#v", exif)
	}
	model, _ := findCandidate(result.Candidates, "model")
	wantModel := StepCandidate{Label: "model", Chosen: false, Meta: map[string]any{"rotationDegrees": 0}, Error: "rotate failed for candidate"}
	if !reflect.DeepEqual(model, wantModel) {
		t.Fatalf("model candidate = %#v, want %#v", model, wantModel)
	}
}

// TestRunOrientStepNormalizesExifBeforeMeasuringAndNeverRotatesTheRawUpload is the ported Golden
// Rule 17 regression guard for the bug that put 9 of 16 real photos upside down: our decoders
// already apply the EXIF Orientation tag, so orientation must be measured and applied on the
// NORMALIZED buffer. If anything here starts reading the raw upload again, the EXIF rotation gets
// applied a second time and upright documents come out sideways.
func TestRunOrientStepNormalizesExifBeforeMeasuringAndNeverRotatesTheRawUpload(t *testing.T) {
	var normalizeInput, detectInput []byte
	var rotateInputs [][]byte

	deps := baseDeps()
	deps.NormalizeOrientation = func(buffer []byte) ([]byte, error) {
		normalizeInput = append([]byte(nil), buffer...)
		return normalizedBuf, nil
	}
	deps.DetectOrientation = func(_ context.Context, buffer []byte) (orientation.DetectionResult, error) {
		detectInput = append([]byte(nil), buffer...)
		return orientation.DetectionResult{RotationDegrees: 90, ExifDegrees: nil, ModelDegrees: 0, ModelRaw: `{"rotationDegrees":0}`, Source: orientation.SourceModelOnly}, nil
	}
	deps.RotateImage = func(buffer []byte, _ int) ([]byte, error) {
		rotateInputs = append(rotateInputs, append([]byte(nil), buffer...))
		return orientedBuf, nil
	}

	NewStepper(deps).RunOrientStep(context.Background(), originalBuf)

	if !bytes.Equal(normalizeInput, originalBuf) {
		t.Fatalf("NormalizeOrientation input = %q, want original", normalizeInput)
	}
	if !bytes.Equal(detectInput, normalizedBuf) {
		t.Fatalf("DetectOrientation input = %q, want normalized", detectInput)
	}
	for _, input := range rotateInputs {
		if !bytes.Equal(input, normalizedBuf) {
			t.Fatalf("RotateImage called with %q, want the normalized buffer", input)
		}
	}
}

func TestRunCropStepCropsByChosenBoxAndIncludesEveryNonNullCandidate(t *testing.T) {
	modelBox := imageprocessor.CropBox{X: 1, Y: 2, Width: 3, Height: 4}
	floodBox := imageprocessor.CropBox{X: 9, Y: 9, Width: 9, Height: 9}

	deps := baseDeps()
	deps.DetectCropBox = func(context.Context, []byte) (crop.DetectionResult, error) {
		return crop.DetectionResult{CropBox: &modelBox, ModelCropBox: &modelBox, ModelRaw: `{"cropBox":{}}`, FloodCropBox: &floodBox, Source: crop.SourceModelFloodAgree}, nil
	}
	deps.CropImage = func(_ []byte, box imageprocessor.CropBox) ([]byte, error) {
		if box.X == 1 {
			return croppedBuf, nil
		}
		return []byte(fmt.Sprintf("cropped-%d", int(box.X))), nil
	}

	result := NewStepper(deps).RunCropStep(context.Background(), orientedBuf)

	if result.Step != 2 {
		t.Fatalf("Step = %d, want 2", result.Step)
	}
	if result.ImageBase64 != base64.StdEncoding.EncodeToString(croppedBuf) {
		t.Fatalf("ImageBase64 = %q", result.ImageBase64)
	}
	if len(result.Candidates) != 2 {
		t.Fatalf("len(Candidates) = %d, want 2", len(result.Candidates))
	}
	model, _ := findCandidate(result.Candidates, "model")
	wantModel := StepCandidate{Label: "model", Chosen: true, ImageBase64: base64.StdEncoding.EncodeToString(croppedBuf), Meta: map[string]any{"box": map[string]any{"x": 1.0, "y": 2.0, "width": 3.0, "height": 4.0}}}
	if !reflect.DeepEqual(model, wantModel) {
		t.Fatalf("model candidate = %#v, want %#v", model, wantModel)
	}
	flood, _ := findCandidate(result.Candidates, "flood")
	wantFlood := StepCandidate{Label: "flood", Chosen: false, ImageBase64: base64.StdEncoding.EncodeToString([]byte("cropped-9")), Meta: map[string]any{"box": map[string]any{"x": 9.0, "y": 9.0, "width": 9.0, "height": 9.0}}}
	if !reflect.DeepEqual(flood, wantFlood) {
		t.Fatalf("flood candidate = %#v, want %#v", flood, wantFlood)
	}
}

func TestRunCropStepUsesOrientedImageAsIsAndHasEmptyCandidatesWhenBothBoxesAreNull(t *testing.T) {
	deps := baseDeps()
	deps.DetectCropBox = func(context.Context, []byte) (crop.DetectionResult, error) {
		return crop.DetectionResult{CropBox: nil, ModelCropBox: nil, ModelRaw: `{"cropBox":null}`, FloodCropBox: nil, Source: crop.SourceNone}, nil
	}
	cropCalled := false
	deps.CropImage = func(buffer []byte, _ imageprocessor.CropBox) ([]byte, error) {
		cropCalled = true
		return buffer, nil
	}

	result := NewStepper(deps).RunCropStep(context.Background(), orientedBuf)

	if cropCalled {
		t.Fatal("CropImage called, want not called")
	}
	if result.ImageBase64 != base64.StdEncoding.EncodeToString(orientedBuf) {
		t.Fatalf("ImageBase64 = %q", result.ImageBase64)
	}
	if result.Candidates == nil {
		t.Fatal("Candidates = nil, want non-nil empty slice (TS [])")
	}
	if len(result.Candidates) != 0 {
		t.Fatalf("len(Candidates) = %d, want 0", len(result.Candidates))
	}
}

func TestRunCropStepRecordsErrorWhenCropDetectionFails(t *testing.T) {
	deps := baseDeps()
	deps.DetectCropBox = func(context.Context, []byte) (crop.DetectionResult, error) {
		return crop.DetectionResult{}, errors.New("malformed JSON from model")
	}

	result := NewStepper(deps).RunCropStep(context.Background(), orientedBuf)

	if result.Error != "malformed JSON from model" {
		t.Fatalf("Error = %q", result.Error)
	}
}

func TestRunCropStepIsolatesAFailingCandidateRenderToThatCandidate(t *testing.T) {
	modelBox := imageprocessor.CropBox{X: 1, Y: 2, Width: 3, Height: 4}
	floodBox := imageprocessor.CropBox{X: 9, Y: 9, Width: 9, Height: 9}

	deps := baseDeps()
	deps.DetectCropBox = func(context.Context, []byte) (crop.DetectionResult, error) {
		return crop.DetectionResult{CropBox: &modelBox, ModelCropBox: &modelBox, ModelRaw: `{"cropBox":{}}`, FloodCropBox: &floodBox, Source: crop.SourceModelFloodAgree}, nil
	}
	deps.CropImage = func(_ []byte, box imageprocessor.CropBox) ([]byte, error) {
		if box.X == 1 {
			return croppedBuf, nil
		}
		return nil, errors.New("crop failed for candidate")
	}

	result := NewStepper(deps).RunCropStep(context.Background(), orientedBuf)

	if result.Error != "" {
		t.Fatalf("Error = %q, want empty", result.Error)
	}
	if result.ImageBase64 != base64.StdEncoding.EncodeToString(croppedBuf) {
		t.Fatalf("ImageBase64 = %q", result.ImageBase64)
	}
	model, _ := findCandidate(result.Candidates, "model")
	wantModel := StepCandidate{Label: "model", Chosen: true, ImageBase64: base64.StdEncoding.EncodeToString(croppedBuf), Meta: map[string]any{"box": map[string]any{"x": 1.0, "y": 2.0, "width": 3.0, "height": 4.0}}}
	if !reflect.DeepEqual(model, wantModel) {
		t.Fatalf("model candidate = %#v, want %#v", model, wantModel)
	}
	flood, _ := findCandidate(result.Candidates, "flood")
	wantFlood := StepCandidate{Label: "flood", Chosen: false, Meta: map[string]any{"box": map[string]any{"x": 9.0, "y": 9.0, "width": 9.0, "height": 9.0}}, Error: "crop failed for candidate"}
	if !reflect.DeepEqual(flood, wantFlood) {
		t.Fatalf("flood candidate = %#v, want %#v", flood, wantFlood)
	}
}

func TestRunEnhanceStepAppliesAutoLevelsAndSharpenWithNoCandidates(t *testing.T) {
	var contrastArg imageprocessor.Adjust
	var contrastInput []byte
	var sharpenArg float64
	var sharpenInput []byte

	deps := baseDeps()
	deps.ComputeAutoLevels = func([]byte) (int, int, error) { return 5, 6, nil }
	deps.ApplyBrightnessContrast = func(buffer []byte, adjust imageprocessor.Adjust) ([]byte, error) {
		contrastInput = append([]byte(nil), buffer...)
		contrastArg = adjust
		return leveledBuf, nil
	}
	deps.ApplySharpen = func(buffer []byte, amount float64) ([]byte, error) {
		sharpenInput = append([]byte(nil), buffer...)
		sharpenArg = amount
		return finalBuf, nil
	}

	result := NewStepper(deps).RunEnhanceStep(context.Background(), croppedBuf)

	if result.Step != 3 {
		t.Fatalf("Step = %d, want 3", result.Step)
	}
	if result.ImageBase64 != base64.StdEncoding.EncodeToString(finalBuf) {
		t.Fatalf("ImageBase64 = %q", result.ImageBase64)
	}
	wantMeta := map[string]any{"brightness": 5, "contrast": 6, "sharpness": 25}
	if !reflect.DeepEqual(result.Meta, wantMeta) {
		t.Fatalf("Meta = %#v, want %#v", result.Meta, wantMeta)
	}
	if result.Candidates != nil {
		t.Fatalf("Candidates = %#v, want nil", result.Candidates)
	}
	if !bytes.Equal(contrastInput, croppedBuf) || contrastArg != (imageprocessor.Adjust{Brightness: 5, Contrast: 6}) {
		t.Fatalf("ApplyBrightnessContrast called with (%q, %#v)", contrastInput, contrastArg)
	}
	if !bytes.Equal(sharpenInput, leveledBuf) || sharpenArg != 25 {
		t.Fatalf("ApplySharpen called with (%q, %v)", sharpenInput, sharpenArg)
	}
}

func TestRunEnhanceStepRecordsErrorWhenEnhancementFails(t *testing.T) {
	deps := baseDeps()
	deps.ComputeAutoLevels = func([]byte) (int, int, error) { return 0, 0, nil }
	deps.ApplyBrightnessContrast = func([]byte, imageprocessor.Adjust) ([]byte, error) { return leveledBuf, nil }
	deps.ApplySharpen = func([]byte, float64) ([]byte, error) { return nil, errors.New("canvas encode failed") }

	result := NewStepper(deps).RunEnhanceStep(context.Background(), croppedBuf)

	if result.Error != "canvas encode failed" {
		t.Fatalf("Error = %q", result.Error)
	}
}

// TestGoldenRule17CropStepHasNoTextureGate pins the inverted-signal invariant: whatever box the
// cascade returns is applied. The old crop detector's texture/variance gate is not reintroduced, so
// even a low-contrast synthetic page with a returned box gets cropped.
func TestGoldenRule17CropStepHasNoTextureGate(t *testing.T) {
	box := imageprocessor.CropBox{X: 2, Y: 2, Width: 4, Height: 4}
	deps := baseDeps()
	deps.DetectCropBox = func(context.Context, []byte) (crop.DetectionResult, error) {
		return crop.DetectionResult{CropBox: &box, ModelCropBox: &box, ModelRaw: "{}", Source: crop.SourceModelOnly}, nil
	}
	cropCalled := false
	deps.CropImage = func(_ []byte, _ imageprocessor.CropBox) ([]byte, error) {
		cropCalled = true
		return croppedBuf, nil
	}

	flat := makeSolidPNG(20, 20)
	result := NewStepper(deps).RunCropStep(context.Background(), flat)

	if !cropCalled {
		t.Fatal("CropImage not called: a texture gate has been reintroduced")
	}
	if result.Error != "" {
		t.Fatalf("Error = %q, want empty", result.Error)
	}
}

// makeSolidPNG is a deliberately low-contrast image: the kind a texture/variance gate would reject.
func makeSolidPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{R: 200, G: 200, B: 200, A: 255}), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

var _ = imageprocessor.CropBox{}
