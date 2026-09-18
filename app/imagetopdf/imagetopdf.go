// Package imagetopdf is a Go port of pdf-triage's src/application/image-to-pdf.ts (134 lines):
// the Vision Lab step functions RunOrientStep (image-to-pdf.ts:40), RunCropStep (:82) and
// RunEnhanceStep (:116).
//
// # Provenance
//
//	RunOrientStep       -> src/application/image-to-pdf.ts:40
//	RunCropStep         -> src/application/image-to-pdf.ts:82
//	RunEnhanceStep      -> src/application/image-to-pdf.ts:116
//	StepCandidate       -> src/application/image-to-pdf.ts:7-14
//	PipelineStepResult  -> src/application/image-to-pdf.ts:16-26
//
// Step 4 (extract) was retired upstream on 2026-09-17 and is intentionally absent here: text
// extraction now happens exclusively via pdf2w after the PDF is assembled, not per-image here. The
// TS file's closing comment (image-to-pdf.ts:132-134) is preserved verbatim:
//
//	// Step 4 (extract) retired 2026-09-17: text extraction now happens exclusively via pdf2w after the
//	// PDF is assembled, not per-image here. See
//	// docs/superpowers/specs/2026-09-17-pdf2w-extraction-swap-design.md ("Photo pipeline change").
//
// # Injection
//
// TS imported its neighbours (orientation-detector, crop-detector, image-processor, image-adjust,
// logger) directly and its unit test mocked those modules with vi.mock. Go has no module mocking, so
// every neighbour call is a field of Deps: production wires the already-ported infra packages
// (imageprocessor, orientation, crop, vision) through DefaultDeps, and tests pass fakes. The two
// model calls are injected at the cascade seams exactly as infra/orientation and infra/crop already
// define them (orientation.OrientationDetector / crop.ModelDetector), so the existing functions are
// imported rather than reimplemented.
//
// # Golden Rule 17 (EXIF orientation)
//
// RunOrientStep normalizes the upload FIRST and measures/applies orientation on that normalized
// buffer only. The normalized buffer is a PNG with no EXIF metadata, so the cascade reports
// exifDegrees == nil by design and no rotation is applied twice. The raw upload is never rotated.
// This is the exact behavior the upstream regression guard (image-to-pdf.test.ts:133-155) pins.
//
// # Deviations, all resolved in favor of matching the TS acceptance bar
//
//  1. Result shape. TS `candidates?: StepCandidate[]` is `undefined` when absent; Go uses a nil
//     slice. The "both crop boxes are null" case is a non-nil empty slice, matching TS `[]`.
//     `imageBase64?: string` / `text?: string` / `error?: string` are "" when absent.
//  2. Duration. TS `Date.now() - start` is integer milliseconds; Go uses
//     `now().Sub(start).Milliseconds()`.
//  3. Errors. TS returns `err.message` and never throws out of a step; Go returns `err.Error()` in
//     PipelineStepResult.Error for the same failures.
package imagetopdf

import (
	"context"
	"encoding/base64"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/imageadjust"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/crop"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/imageprocessor"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/orientation"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/vision"
)

// Step labels mirror the TS `PipelineStepResult['label']` union (image-to-pdf.ts:16).
const (
	LabelOriented = "oriented"
	LabelCropped  = "cropped"
	LabelEnhanced = "enhanced"
)

// StepCandidate mirrors the TS `StepCandidate` interface (image-to-pdf.ts:7). A nil Meta is the TS
// `undefined`; "" is the TS optional string.
type StepCandidate struct {
	Label       string         `json:"label"`
	Chosen      bool           `json:"chosen"`
	ImageBase64 string         `json:"imageBase64,omitempty"`
	Text        string         `json:"text,omitempty"`
	Meta        map[string]any `json:"meta,omitempty"`
	Error       string         `json:"error,omitempty"`
}

// PipelineStepResult mirrors the TS `PipelineStepResult` interface (image-to-pdf.ts:16). Step is
// 1, 2 or 3 — 4 (extracted) is retired. Candidates nil means the TS `undefined`.
type PipelineStepResult struct {
	Step        int             `json:"step"`
	Label       string          `json:"label"`
	DurationMS  int64           `json:"durationMs"`
	ImageBase64 string          `json:"imageBase64"`
	Markdown    string          `json:"markdown,omitempty"`
	ModelRaw    string          `json:"modelRaw,omitempty"`
	Meta        map[string]any  `json:"meta,omitempty"`
	Error       string          `json:"error,omitempty"`
	Candidates  []StepCandidate `json:"candidates,omitempty"`
}

// Logger is the subset of infra/logger used by the step functions. *logger.Logger satisfies it
// directly; nil skips logging.
type Logger interface {
	Info(moduleName, message string, meta any, filename ...string)
	Error(moduleName, message string, meta any, filename ...string)
}

// Deps is the injected neighbor set. Every field is required except Logger and Now.
//
// DetectOrientation / DetectCropBox close over a vision.Client in production (see DefaultDeps);
// tests substitute pure functions.
type Deps struct {
	NormalizeOrientation    func(imageBuffer []byte) ([]byte, error)
	DetectOrientation       func(ctx context.Context, imageBuffer []byte) (orientation.DetectionResult, error)
	RotateImage             func(imageBuffer []byte, degrees int) ([]byte, error)
	DetectCropBox           func(ctx context.Context, imageBuffer []byte) (crop.DetectionResult, error)
	CropImage               func(imageBuffer []byte, box imageprocessor.CropBox) ([]byte, error)
	ComputeAutoLevels       func(imageBuffer []byte) (brightness, contrast int, err error)
	ApplyBrightnessContrast func(imageBuffer []byte, adjust imageprocessor.Adjust) ([]byte, error)
	ApplySharpen            func(imageBuffer []byte, amount float64) ([]byte, error)
	Logger                  Logger
	Now                     func() time.Time
}

// DefaultDeps wires the already-ported infra packages. visionClient supplies both model calls; log
// may be nil.
func DefaultDeps(visionClient *vision.Client, log Logger) Deps {
	return Deps{
		NormalizeOrientation: imageprocessor.NormalizeOrientation,
		DetectOrientation: func(ctx context.Context, imageBuffer []byte) (orientation.DetectionResult, error) {
			return orientation.DetectOrientationCascade(ctx, imageBuffer, visionClient.DetectOrientation)
		},
		RotateImage: imageprocessor.RotateImage,
		DetectCropBox: func(ctx context.Context, imageBuffer []byte) (crop.DetectionResult, error) {
			return crop.DetectCropBoxCascade(ctx, imageBuffer, visionClient.DetectCropBox, imageprocessor.DetectDocumentBoxLocally)
		},
		CropImage:               imageprocessor.CropImage,
		ComputeAutoLevels:       imageprocessor.ComputeAutoLevelsForImage,
		ApplyBrightnessContrast: imageprocessor.ApplyBrightnessContrast,
		ApplySharpen:            imageprocessor.ApplySharpen,
		Logger:                  log,
		Now:                     time.Now,
	}
}

// Stepper runs the Vision Lab steps with one injected Deps set.
type Stepper struct {
	deps Deps
}

// NewStepper returns a Stepper. A nil Now defaults to time.Now.
func NewStepper(deps Deps) *Stepper {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Stepper{deps: deps}
}

// Deps returns the injected dependencies (useful for wrappers that re-derive a Stepper).
func (s *Stepper) Deps() Deps { return s.deps }

// RunOrientStep ports runOrientStep. The WHY-comments below are preserved verbatim from the TS
// source (image-to-pdf.ts:32-39 and :43-47).
//
// Step 1: orientation. Candidates cover every non-null signal the cascade considered (EXIF, vision
// model) — selecting one in the UI is purely visual; the cascade's own rotationDegrees always feeds
// step 2, regardless of what a developer looks at here.
//
// The `exif` candidate will normally be absent now: the buffer is EXIF-normalized before the
// cascade runs, so there is no orientation tag left to read and exifDegrees comes back null. That is
// deliberate, not a regression — see normalizeOrientation for why the tag must not be treated as a
// rotation still owed.
//
//	// Normalize BEFORE anything measures orientation. Our decoders (canvas here) already apply the
//	// EXIF Orientation tag, so the raw tag is not a rotation still owed — re-applying it turns an
//	// upright photo sideways. Normalizing first strips the tag and leaves every stage looking at
//	// identical pixels, so the cascade below measures only the rotation the photograph itself needs.
func (s *Stepper) RunOrientStep(ctx context.Context, imageBuffer []byte) PipelineStepResult {
	start := s.now()

	normalizedBuffer, err := s.deps.NormalizeOrientation(imageBuffer)
	if err != nil {
		return s.errorResult(1, LabelOriented, start, err)
	}
	detection, err := s.deps.DetectOrientation(ctx, normalizedBuffer)
	if err != nil {
		return s.errorResult(1, LabelOriented, start, err)
	}
	orientedBuffer, err := s.deps.RotateImage(normalizedBuffer, detection.RotationDegrees)
	if err != nil {
		return s.errorResult(1, LabelOriented, start, err)
	}

	type candidateDegrees struct {
		label   string
		degrees int
	}
	var degreeCandidates []candidateDegrees
	if detection.ExifDegrees != nil {
		degreeCandidates = append(degreeCandidates, candidateDegrees{label: "exif", degrees: *detection.ExifDegrees})
	}
	degreeCandidates = append(degreeCandidates, candidateDegrees{label: "model", degrees: detection.ModelDegrees})

	candidates := []StepCandidate{}
	for _, candidate := range degreeCandidates {
		isChosen := candidate.degrees == detection.RotationDegrees
		candidateMeta := map[string]any{"rotationDegrees": candidate.degrees}
		if isChosen {
			candidates = append(candidates, StepCandidate{
				Label:       candidate.label,
				Chosen:      true,
				ImageBase64: base64.StdEncoding.EncodeToString(orientedBuffer),
				Meta:        candidateMeta,
			})
			continue
		}
		// Rotate the NORMALIZED buffer, not the raw upload — otherwise the comparison views sit in a
		// different pixel space than the chosen one. (image-to-pdf.ts:60-61, preserved verbatim.)
		buf, rotateErr := s.deps.RotateImage(normalizedBuffer, candidate.degrees)
		if rotateErr != nil {
			candidates = append(candidates, StepCandidate{
				Label:  candidate.label,
				Chosen: false,
				Meta:   candidateMeta,
				Error:  rotateErr.Error(),
			})
			continue
		}
		candidates = append(candidates, StepCandidate{
			Label:       candidate.label,
			Chosen:      false,
			ImageBase64: base64.StdEncoding.EncodeToString(buf),
			Meta:        candidateMeta,
		})
	}

	var exifDegrees any
	if detection.ExifDegrees != nil {
		exifDegrees = *detection.ExifDegrees
	}
	meta := map[string]any{
		"rotationDegrees": detection.RotationDegrees,
		"exifDegrees":     exifDegrees,
		"modelDegrees":    detection.ModelDegrees,
		"source":          detection.Source,
	}
	durationMS := s.since(start)
	s.logInfo(metaWith(meta, "durationMs", durationMS), "Step 1 (oriented) succeeded")
	return PipelineStepResult{
		Step:        1,
		Label:       LabelOriented,
		ImageBase64: base64.StdEncoding.EncodeToString(orientedBuffer),
		DurationMS:  durationMS,
		ModelRaw:    detection.ModelRaw,
		Meta:        meta,
		Candidates:  candidates,
	}
}

// RunCropStep ports runCropStep. The WHY-comment below is preserved verbatim from the TS source
// (image-to-pdf.ts:80-81).
//
// Step 2: crop. Candidates cover the model box and the flood-fill box when each is non-null —
// selecting one is purely visual; the cascade's own cropBox always feeds step 3.
//
// There is deliberately NO texture/variance gate here (Golden Rule 17): whatever box the cascade
// returns is applied. The old crop detector's texture gate is not reintroduced.
func (s *Stepper) RunCropStep(ctx context.Context, orientedBuffer []byte) PipelineStepResult {
	start := s.now()

	detection, err := s.deps.DetectCropBox(ctx, orientedBuffer)
	if err != nil {
		return s.errorResult(2, LabelCropped, start, err)
	}

	var croppedBuffer []byte
	if detection.CropBox != nil {
		croppedBuffer, err = s.deps.CropImage(orientedBuffer, *detection.CropBox)
		if err != nil {
			return s.errorResult(2, LabelCropped, start, err)
		}
	} else {
		croppedBuffer = orientedBuffer
	}

	type candidateBox struct {
		label string
		box   imageprocessor.CropBox
	}
	var boxCandidates []candidateBox
	if detection.ModelCropBox != nil {
		boxCandidates = append(boxCandidates, candidateBox{label: "model", box: *detection.ModelCropBox})
	}
	if detection.FloodCropBox != nil {
		boxCandidates = append(boxCandidates, candidateBox{label: "flood", box: *detection.FloodCropBox})
	}

	candidates := []StepCandidate{}
	for _, candidate := range boxCandidates {
		isChosen := detection.CropBox != nil && candidate.box == *detection.CropBox
		candidateMeta := map[string]any{"box": cropBoxMeta(candidate.box)}
		if isChosen {
			candidates = append(candidates, StepCandidate{
				Label:       candidate.label,
				Chosen:      true,
				ImageBase64: base64.StdEncoding.EncodeToString(croppedBuffer),
				Meta:        candidateMeta,
			})
			continue
		}
		buf, cropErr := s.deps.CropImage(orientedBuffer, candidate.box)
		if cropErr != nil {
			candidates = append(candidates, StepCandidate{
				Label:  candidate.label,
				Chosen: false,
				Meta:   candidateMeta,
				Error:  cropErr.Error(),
			})
			continue
		}
		candidates = append(candidates, StepCandidate{
			Label:       candidate.label,
			Chosen:      false,
			ImageBase64: base64.StdEncoding.EncodeToString(buf),
			Meta:        candidateMeta,
		})
	}

	meta := map[string]any{
		"cropBox":      nullableCropBoxMeta(detection.CropBox),
		"modelCropBox": nullableCropBoxMeta(detection.ModelCropBox),
		"floodCropBox": nullableCropBoxMeta(detection.FloodCropBox),
		"source":       detection.Source,
	}
	durationMS := s.since(start)
	s.logInfo(metaWith(meta, "durationMs", durationMS), "Step 2 (cropped) succeeded")
	return PipelineStepResult{
		Step:        2,
		Label:       LabelCropped,
		ImageBase64: base64.StdEncoding.EncodeToString(croppedBuffer),
		DurationMS:  durationMS,
		ModelRaw:    detection.ModelRaw,
		Meta:        meta,
		Candidates:  candidates,
	}
}

// RunEnhanceStep ports runEnhanceStep. The WHY-comment below is preserved verbatim from the TS
// source (image-to-pdf.ts:114-115).
//
// Step 3: enhance. No alternate signal exists to compare (auto-levels + a fixed sharpen default), so
// this step never has a `candidates` field.
func (s *Stepper) RunEnhanceStep(ctx context.Context, croppedBuffer []byte) PipelineStepResult {
	_ = ctx
	start := s.now()

	brightness, contrast, err := s.deps.ComputeAutoLevels(croppedBuffer)
	if err != nil {
		return s.errorResult(3, LabelEnhanced, start, err)
	}
	leveledBuffer, err := s.deps.ApplyBrightnessContrast(croppedBuffer, imageprocessor.Adjust{Brightness: brightness, Contrast: contrast})
	if err != nil {
		return s.errorResult(3, LabelEnhanced, start, err)
	}
	finalBuffer, err := s.deps.ApplySharpen(leveledBuffer, float64(imageadjust.AutoAdjustSharpness))
	if err != nil {
		return s.errorResult(3, LabelEnhanced, start, err)
	}

	durationMS := s.since(start)
	meta := map[string]any{
		"brightness": brightness,
		"contrast":   contrast,
		"sharpness":  imageadjust.AutoAdjustSharpness,
	}
	s.logInfo(metaWith(meta, "durationMs", durationMS), "Step 3 (enhanced) succeeded")
	return PipelineStepResult{
		Step:        3,
		Label:       LabelEnhanced,
		ImageBase64: base64.StdEncoding.EncodeToString(finalBuffer),
		DurationMS:  durationMS,
		Meta:        meta,
	}
}

// errorResult is the shared failure shape: no base64, no candidates, the error text recorded.
func (s *Stepper) errorResult(step int, label string, start time.Time, err error) PipelineStepResult {
	durationMS := s.since(start)
	if s.deps.Logger != nil {
		s.deps.Logger.Error("VISION_LAB", stepFailureMessage(step), map[string]any{
			"error":      err.Error(),
			"durationMs": durationMS,
		})
	}
	return PipelineStepResult{
		Step:        step,
		Label:       label,
		ImageBase64: "",
		DurationMS:  durationMS,
		Error:       err.Error(),
	}
}

func stepFailureMessage(step int) string {
	switch step {
	case 1:
		return "Step 1 (oriented) failed"
	case 2:
		return "Step 2 (cropped) failed"
	default:
		return "Step 3 (enhanced) failed"
	}
}

func (s *Stepper) now() time.Time { return s.deps.Now() }

func (s *Stepper) since(start time.Time) int64 { return s.now().Sub(start).Milliseconds() }

func (s *Stepper) logInfo(meta map[string]any, message string) {
	if s.deps.Logger != nil {
		s.deps.Logger.Info("VISION_LAB", message, meta)
	}
}

// metaWith returns a shallow copy of meta with one extra key, so the logged object carries
// durationMs without adding it to the result's Meta (TS spread `{ ...meta, durationMs }`).
func metaWith(meta map[string]any, key string, value any) map[string]any {
	merged := make(map[string]any, len(meta)+1)
	for k, v := range meta {
		merged[k] = v
	}
	merged[key] = value
	return merged
}

func cropBoxMeta(box imageprocessor.CropBox) map[string]any {
	return map[string]any{"x": box.X, "y": box.Y, "width": box.Width, "height": box.Height}
}

func nullableCropBoxMeta(box *imageprocessor.CropBox) any {
	if box == nil {
		return nil
	}
	return cropBoxMeta(*box)
}
