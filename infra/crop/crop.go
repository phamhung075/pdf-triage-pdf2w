// Package crop is a Go port of pdf-triage's src/infrastructure/crop-detector.ts (99 lines):
// DetectCropBoxCascade (crop-detector.ts:71), the cascade that arbitrates the pinned Ollama vision
// model's crop box against the local, model-free edge-snap detector.
//
// # Provenance
//
//	DetectCropBoxCascade  -> src/infrastructure/crop-detector.ts:71
//	DetectionResult       -> src/infrastructure/crop-detector.ts:5
//	IOUAgreementThreshold -> src/infrastructure/crop-detector.ts:17
//	FullBoundsTolerance   -> src/infrastructure/crop-detector.ts:20
//
// The WHY-comment from the TS source is preserved verbatim below, including the three-valued local
// result contract and the veto short-circuit.
//
// # Deviations, all resolved in favor of matching the TS acceptance bar
//
//  1. Injection. TS imported detectCropBox and detectDocumentBoxLocally directly and its test
//     mocked those modules. Go takes the two signals as parameters; production passes
//     vision.Client.DetectCropBox and imageprocessor.DetectDocumentBoxLocally.
//  2. Dimensions. The degenerate-box test needs the image's pixel dimensions. TS read them with
//     @napi-rs/canvas loadImage; this port reads the encoded header with the already-ported
//     imagedimensions package (PNG and JPEG only), exactly as infra/vision does, and returns an
//     error for any other format. Every production caller passes the PNG produced by
//     NormalizeOrientation, so the dimensions agree there.
//  3. Nullable. TS's `CropBox | null` is a *vision.CropBox here, nil meaning null.
package crop

import (
	"context"
	"fmt"
	"math"

	"github.com/phamhung075/pdf-triage-pdf2w/imagedimensions"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/imageprocessor"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/vision"
)

// ModelDetector is the vision-model signal. vision.Client.DetectCropBox satisfies it.
type ModelDetector func(ctx context.Context, imageBuffer []byte) (vision.CropResult, error)

// LocalDetector is the local signal. imageprocessor.DetectDocumentBoxLocally satisfies it.
type LocalDetector func(imageBuffer []byte) (imageprocessor.LocalCropResult, error)

// Source values for DetectionResult.Source.
const (
	SourceModelFloodAgree = "model-flood-agree"
	SourceFloodOverride   = "flood-override"
	SourceModelOnly       = "model-only"
	SourceFloodVeto       = "flood-veto"
	SourceNone            = "none"
)

// IOUAgreementThreshold matches pdf-awesome's own quadRectIoU disagreement threshold
// (js/domain/autocrop.js) — its combination logic overrides its primary (global-threshold)
// detector with flood-fill whenever they disagree by more than this, because real-photo testing
// there found flood-fill the more trustworthy signal on low-contrast backgrounds, not the other
// way around.
const IOUAgreementThreshold = 0.5

// FullBoundsTolerance is a few pixels of slack for rounding — the model rarely echoes the exact
// image dimensions back.
const FullBoundsTolerance = 3

// DetectionResult is the Go form of the TS `CropDetectionResult` interface (crop-detector.ts:5).
type DetectionResult struct {
	CropBox      *vision.CropBox
	ModelCropBox *vision.CropBox
	ModelRaw     string
	FloodCropBox *vision.CropBox
	Source       string
}

// DetectCropBoxCascade ports detectCropBoxCascade. The comment below is preserved verbatim from the
// TS source:
//
// Cascades two independent crop-boundary signals: the minicpm-v4.6 vision model, and the local,
// deterministic edge-snap detector (detectDocumentBoxLocally) that measures how strong a colour
// barrier separates the frame border from each pixel — see domain/flood-crop.ts for why it
// tolerates a document photographed on a background close in brightness to the paper itself, a
// case the model has been observed to default to a lazy "the document fills the whole photo"
// answer on.
//
// The local detector is THREE-valued, and the distinction is load-bearing:
//
//   - 'vetoed' SHORT-CIRCUITS THE WHOLE CASCADE to no crop, whatever the model said. A veto is not
//     "I found nothing"; it is "I examined this image and there is NO BACKGROUND to crop — the frame
//     IS the document, so any crop destroys content". That happens on scans, close-up captures, PDF
//     page renders, screenshots, and on a second pass over this pipeline's own already-cropped
//     output. The model has no defence against exactly that input: it is where it most reliably
//     invents a box around an interior text block or echoes a lazy full-frame answer. Deferring to
//     it here would hand back the destructive crop the guard exists to prevent, on precisely the
//     inputs where the local detector's evidence is strongest — so the veto outranks the model
//     rather than competing with it.
//   - 'no-signal' means the detector genuinely has no opinion (nothing covered the middle of the
//     frame). Then, and only then, the model is the sole signal and the pre-existing behaviour
//     applies: trust a real, non-degenerate model box ('model-only'), or report no crop when the
//     model's box is also degenerate (full-image-bounds echo, or missing) rather than silently
//     passing through a no-op answer mislabeled as a successful detection.
//   - 'box' arbitrates against the model as before: if the model's box is degenerate, or disagrees
//     by IoU below IOU_AGREEMENT_THRESHOLD, the local box overrides it — matching pdf-awesome's own
//     tested preference for the local detector over threshold-based detection on disagreement.
//     Otherwise the two corroborate each other and the model's box is used, since it can draw on
//     semantic context (e.g. "this looks like a stamped document") the local detector can't.
func DetectCropBoxCascade(ctx context.Context, imageBuffer []byte, model ModelDetector, local LocalDetector) (DetectionResult, error) {
	modelResult, err := model(ctx, imageBuffer)
	if err != nil {
		return DetectionResult{}, err
	}
	modelCropBox := modelResult.CropBox

	localResult, err := local(imageBuffer)
	if err != nil {
		return DetectionResult{}, err
	}

	dims, ok := imagedimensions.ReadImageDimensions(imageBuffer)
	if !ok {
		return DetectionResult{}, fmt.Errorf("crop: detectCropBoxCascade: could not read image dimensions (only PNG/JPEG headers are recognized)")
	}
	modelDegenerate := isFullBoundsOrMissing(modelCropBox, dims.Width, dims.Height)

	if localResult.Kind == imageprocessor.LocalCropVetoed {
		return DetectionResult{
			CropBox:      nil,
			ModelCropBox: modelCropBox,
			ModelRaw:     modelResult.Raw,
			FloodCropBox: nil,
			Source:       SourceFloodVeto,
		}, nil
	}

	var floodCropBox *vision.CropBox
	if localResult.Kind == imageprocessor.LocalCropBox {
		box := localResult.Box
		floodCropBox = &box
	}

	if floodCropBox == nil {
		if modelDegenerate {
			return DetectionResult{
				CropBox:      nil,
				ModelCropBox: modelCropBox,
				ModelRaw:     modelResult.Raw,
				FloodCropBox: nil,
				Source:       SourceNone,
			}, nil
		}
		return DetectionResult{
			CropBox:      modelCropBox,
			ModelCropBox: modelCropBox,
			ModelRaw:     modelResult.Raw,
			FloodCropBox: nil,
			Source:       SourceModelOnly,
		}, nil
	}

	iou := 0.0
	if modelCropBox != nil {
		iou = rectIoU(*modelCropBox, *floodCropBox)
	}
	if modelDegenerate || iou < IOUAgreementThreshold {
		return DetectionResult{
			CropBox:      floodCropBox,
			ModelCropBox: modelCropBox,
			ModelRaw:     modelResult.Raw,
			FloodCropBox: floodCropBox,
			Source:       SourceFloodOverride,
		}, nil
	}
	return DetectionResult{
		CropBox:      modelCropBox,
		ModelCropBox: modelCropBox,
		ModelRaw:     modelResult.Raw,
		FloodCropBox: floodCropBox,
		Source:       SourceModelFloodAgree,
	}, nil
}

func isFullBoundsOrMissing(box *vision.CropBox, imgWidth, imgHeight int) bool {
	if box == nil {
		return true
	}
	return box.X <= FullBoundsTolerance &&
		box.Y <= FullBoundsTolerance &&
		math.Abs(box.X+box.Width-float64(imgWidth)) <= FullBoundsTolerance &&
		math.Abs(box.Y+box.Height-float64(imgHeight)) <= FullBoundsTolerance
}

func rectIoU(a, b vision.CropBox) float64 {
	ax2 := a.X + a.Width
	ay2 := a.Y + a.Height
	bx2 := b.X + b.Width
	by2 := b.Y + b.Height
	ix1 := math.Max(a.X, b.X)
	iy1 := math.Max(a.Y, b.Y)
	ix2 := math.Min(ax2, bx2)
	iy2 := math.Min(ay2, by2)
	iw := math.Max(0, ix2-ix1)
	ih := math.Max(0, iy2-iy1)
	inter := iw * ih
	union := a.Width*a.Height + b.Width*b.Height - inter
	if union > 0 {
		return inter / union
	}
	return 0
}
