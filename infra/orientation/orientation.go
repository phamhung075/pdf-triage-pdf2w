// Package orientation is a Go port of pdf-triage's src/infrastructure/orientation-detector.ts (33
// lines): DetectOrientationCascade (orientation-detector.ts:20), the cascade that combines the EXIF
// orientation signal with the pinned Ollama vision model.
//
// # Provenance
//
//	DetectOrientationCascade  -> src/infrastructure/orientation-detector.ts:20
//	DetectionResult           -> src/infrastructure/orientation-detector.ts:4
//
// The WHY-comment from the TS source is preserved verbatim below.
//
// # Golden Rule 17
//
// In the live pipeline the buffer handed here has already passed
// imageprocessor.NormalizeOrientation, so the EXIF tag is gone and ExifDegrees is nil by design —
// that is what stops the cascade from re-applying a rotation the decoder already performed. This
// package reads the tag from the raw bytes only to report it; the model is always the deciding
// signal (matching TS exactly). Nothing here rotates pixels.
//
// # Deviations, all resolved in favor of matching the TS acceptance bar
//
//  1. Injection. TS imported parseExifOrientation / exifOrientationToDegrees and detectOrientation
//     directly and its test mocked those modules. Go has no module mocking, so the model call is a
//     parameter (OrientationDetector) and the EXIF read is the real, already-ported exiforientation
//     package exercised with synthetic tagged buffers. Production passes vision.Client.DetectOrientation.
//  2. Nullable. TS's `0|90|180|270|null` exifDegrees is a *int here, nil meaning null.
package orientation

import (
	"context"

	"github.com/phamhung075/pdf-triage-pdf2w/exiforientation"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/vision"
)

// OrientationDetector is the model call the cascade depends on. vision.Client.DetectOrientation
// satisfies it.
type OrientationDetector func(ctx context.Context, imageBuffer []byte) (vision.OrientationResult, error)

// Source values for DetectionResult.Source.
const (
	SourceExifModelAgree = "exif+model-agree"
	SourceModelOnly      = "model-only"
)

// DetectionResult is the Go form of the TS `OrientationDetectionResult` interface
// (orientation-detector.ts:4). ExifDegrees is nil when EXIF is absent or carries only a mirror tag
// (exifOrientationToDegrees returns null for tags 2/4/5/7).
type DetectionResult struct {
	RotationDegrees int
	ExifDegrees     *int
	ModelDegrees    int
	ModelRaw        string
	Source          string
}

// DetectOrientationCascade ports detectOrientationCascade. The comment below is preserved verbatim
// from the TS source:
//
// Cascades two independent orientation signals: EXIF metadata (instant, but sometimes wrong or
// absent — a real phone photo surfaced exactly this) and the minicpm-v4.6 vision model (also
// sometimes wrong on its own, as the same photo proved). When they agree, that shared answer is
// used. Otherwise — including when EXIF is absent, since there's then nothing for the model to
// agree with — the vision model's answer is taken as final, with no independent tiebreaker: the
// app's only remaining OCR dependency is pdf2w, and pdf2w exposes no orientation-classification
// API. This is a deliberate, accepted loss of the OCR-verified tiebreaker this cascade used to
// have (previously PaddleOCR, with Tesseract OSD as its availability fallback).
func DetectOrientationCascade(ctx context.Context, imageBuffer []byte, detect OrientationDetector) (DetectionResult, error) {
	var exifDegrees *int
	if tag, ok := exiforientation.ParseExifOrientation(imageBuffer); ok {
		if degrees, ok := exiforientation.ExifOrientationToDegrees(&tag); ok {
			exifDegrees = &degrees
		}
	}

	model, err := detect(ctx, imageBuffer)
	if err != nil {
		return DetectionResult{}, err
	}

	agree := exifDegrees != nil && *exifDegrees == model.RotationDegrees
	source := SourceModelOnly
	if agree {
		source = SourceExifModelAgree
	}
	return DetectionResult{
		RotationDegrees: model.RotationDegrees,
		ExifDegrees:     exifDegrees,
		ModelDegrees:    model.RotationDegrees,
		ModelRaw:        model.Raw,
		Source:          source,
	}, nil
}
