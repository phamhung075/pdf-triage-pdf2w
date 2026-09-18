// Package imageprocessor is a Go port of pdf-triage's src/infrastructure/image-processor.ts (219
// lines): the decode / encode / rotate / crop / enhance seam of the photo pipeline.
//
// # Provenance
//
//	NormalizeOrientation        -> src/infrastructure/image-processor.ts:22
//	EncodeJpeg                  -> src/infrastructure/image-processor.ts:39
//	RotateImage                 -> src/infrastructure/image-processor.ts:46
//	LocalCropResult             -> src/infrastructure/image-processor.ts:69
//	DetectDocumentBoxLocally    -> src/infrastructure/image-processor.ts:81
//	DetectCropBoxLocally        -> src/infrastructure/image-processor.ts:134
//	CropImage                   -> src/infrastructure/image-processor.ts:139
//	ComputeAutoLevelsForImage   -> src/infrastructure/image-processor.ts:153
//	ApplyBrightnessContrast     -> src/infrastructure/image-processor.ts:173
//	ApplySharpen                -> src/infrastructure/image-processor.ts:188
//
// The WHY-comments from the TS source are preserved verbatim at the functions they explain.
//
// # Replacing @napi-rs/canvas
//
// Decoding and encoding are stdlib image/jpeg, image/png and image/draw plus golang.org/x/image
// (webp, bmp, tiff, and the scaling interpolators). The task that owns this port approved exactly
// that one module addition; see go.mod.
//
// # The EXIF invariant (Golden Rule 17)
//
// @napi-rs/canvas auto-applies a JPEG's EXIF Orientation tag on decode; Go's decoders do not.
// decodeImage is the single seam that restores that behavior: after a successful image.Decode it
// reads the raw tag with exiforientation.ParseExifOrientation and, when one is present, bakes the
// full eight-way orientation transform into the pixels. NormalizeOrientation then re-encodes as PNG,
// which carries no orientation metadata — this is what lets every later stage read identical pixels
// and lets the orientation cascade measure only the rotation the photograph genuinely needs.
// exifDegrees is null by design downstream; nothing may re-apply the tag.
//
// imageadjust supplies the auto-levels histogram math (ComputeAutoLevelsForImage) and SharpenPixel
// (ApplySharpen). applyBrightnessContrast has no imageadjust equivalent — TS drove canvas's CSS
// brightness()/contrast() filter — so that sRGB transfer is implemented here.
//
// exiforientation.ExifOrientationToDegrees intentionally exposes only the four pure rotations
// (1,3,6,8) that the orientation cascade can report; decodeImage needs the mirror tags (2,4,5,7)
// too, so it maps the raw tag itself (applyExifOrientation). The mapping is differential-tested
// against @napi-rs/canvas for all eight tags (testdata/parity).
//
// # Deviations, all resolved in favor of matching the TS acceptance bar
//
//  1. Resampling. Canvas drawImage uses Skia's smoothing; this port uses
//     golang.org/x/image/draw.ApproxBiLinear for the two downscales (detectDocumentBoxLocally's
//     WORK_MAX_DIM working image and computeAutoLevelsForImage's 400px histogram view). The
//     implementations cannot be bit-identical; parity is asserted on the detector's output box and
//     on the emitted auto-level sliders, with a stated tolerance (see the parity test).
//  2. JPEG quality. Go's image/jpeg treats quality <= 0 as "use the default 75"; @napi-rs/canvas
//     treats 0 as the lowest quality. EncodeJpeg clamps to [1,100] so a requested 0 does not
//     silently jump to 75. The caller in production passes 85.
//  3. Pixel rounding. Canvas round-trips through 8-bit premultiplied storage; this port works on
//     image.NRGBA (un-premultiplied), which is what ctx.getImageData exposes for the opaque photos
//     this pipeline handles. applySharpen therefore matches exactly; CSS brightness()/contrast()
//     are re-implemented in the sRGB domain and match within the parity test's MAE bound.
//  4. rotateImage only accepts 0/90/180/270. TS declared that as a union type; an out-of-set Go
//     value returns an error instead of rotating by an arbitrary angle.
//  5. Errors. TS throws; Go returns errors.
package imageprocessor

import (
	"bytes"
	"fmt"
	"image"
	imagedraw "image/draw"
	"image/jpeg"
	"image/png"
	"math"

	_ "golang.org/x/image/bmp"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/phamhung075/pdf-triage-pdf2w/exiforientation"
	"github.com/phamhung075/pdf-triage-pdf2w/floodcrop"
	"github.com/phamhung075/pdf-triage-pdf2w/imageadjust"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/vision"
)

// The x/image decoders register themselves with image.Decode in their init, so the accepted-format
// set (JPEG, PNG, webp, bmp, tiff) is wired by these blank imports. The tests additionally use
// bmp.Encode / tiff.Encode to build round-trip fixtures.

// CropBox is a type alias for vision.CropBox, mirroring the TS re-export at
// image-processor.ts:2 (`import type { CropBox } from './vision-client.js'`).
type CropBox = vision.CropBox

// LocalCropKind is the TS LocalCropResult discriminator (`kind`).
type LocalCropKind string

// The three possible answers the local, model-free detector can give. Two of them are NOT the same
// thing, and collapsing them into one `null` is what made the fills-frame guard inert:
//
//	'box'       a document with real background around it — crop here.
//	'no-signal' nothing covered the middle of the frame; the detector has no opinion, and some
//	            other signal (the vision model) may still be trusted.
//	'vetoed'    a box WAS found, and the admissibility evidence says the frame IS the document:
//	            there is no background to crop away, so cropping would destroy content. This is
//	            the detector's most confident possible statement, not an absence of one.
//
// (Comment preserved verbatim from image-processor.ts:60-68.)
const (
	LocalCropBox      LocalCropKind = "box"
	LocalCropNoSignal LocalCropKind = "no-signal"
	LocalCropVetoed   LocalCropKind = "vetoed"
)

// LocalCropResult is the Go form of the TS LocalCropResult tagged union. Box is meaningful only
// when Kind == LocalCropBox.
type LocalCropResult struct {
	Kind LocalCropKind
	Box  CropBox
}

// decodeImage decodes any accepted format (JPEG, PNG, webp, bmp, tiff) and reproduces
// @napi-rs/canvas's decode-time EXIF auto-application. See the package comment.
func decodeImage(buffer []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(buffer))
	if err != nil {
		return nil, err
	}
	if tag, ok := exiforientation.ParseExifOrientation(buffer); ok {
		img = applyExifOrientation(img, tag)
	}
	return img, nil
}

// applyExifOrientation bakes the EXIF Orientation tag (1-8) into the pixels, exactly as a decoder
// that honours the tag does. It is the full eight-way table: 1 identity, 2 flip horizontal,
// 3 rotate 180, 4 flip vertical, 5 transpose, 6 rotate 90 CW, 7 transverse, 8 rotate 270 CW.
func applyExifOrientation(src image.Image, tag int) image.Image {
	if tag == 1 {
		return src
	}
	nrgba := toNRGBA(src)
	w, h := nrgba.Bounds().Dx(), nrgba.Bounds().Dy()
	dw, dh := w, h
	if tag >= 5 && tag <= 8 {
		dw, dh = h, w
	}
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var sx, sy int
			switch tag {
			case 2:
				sx, sy = w-1-x, y
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sx, sy = x, h-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, h-1-x
			case 7:
				sx, sy = w-1-y, h-1-x
			case 8:
				sx, sy = w-1-y, x
			default:
				sx, sy = x, y
			}
			copyNRGBAPixel(dst, x, y, nrgba, sx, sy)
		}
	}
	return dst
}

func copyNRGBAPixel(dst *image.NRGBA, dx, dy int, src *image.NRGBA, sx, sy int) {
	si := src.PixOffset(sx, sy)
	di := dst.PixOffset(dx, dy)
	copy(dst.Pix[di:di+4], src.Pix[si:si+4])
}

// NormalizeOrientation ports normalizeOrientation. The comment below is preserved verbatim from
// the TS source:
//
// Bake the decoder's EXIF handling into the pixels and drop the metadata, producing one canonical
// buffer that every later stage is guaranteed to read the same way.
//
// WHY THIS EXISTS. A JPEG's EXIF Orientation tag is an instruction to the decoder, and decoders
// disagree about whether to honour it. Measured in this stack: @napi-rs/canvas DOES apply it (a
// photo stored 2051x1154 with tag 6 decodes as 1154x2051), and so does OpenCV, which is what the
// PaddleOCR service decodes with. So by the time any of our stages sees pixels, the EXIF rotation
// has ALREADY been applied — while parseExifOrientation, reading the raw bytes, still reports the
// tag as a rotation waiting to be performed. Treating that tag as a correction therefore rotates an
// already-upright image a second time.
//
// Rather than teach each stage which decoders auto-rotate — a fact that can change with a library
// version and is invisible when it does — this normalizes once, at the pipeline entry. The PNG it
// returns carries no orientation metadata at all, so there is nothing left for anything downstream
// to interpret differently, and the orientation cascade that follows measures only the rotation the
// PHOTOGRAPH genuinely needs.
func NormalizeOrientation(imageBuffer []byte) ([]byte, error) {
	img, err := decodeImage(imageBuffer)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// EncodeJpeg ports encodeJpeg. The comment below is preserved verbatim from the TS source:
//
// Re-encodes any decodable image as JPEG at the given quality.
//
// Two reasons this exists. pdf-lib can only embed JPEG and PNG, so .webp/.bmp/.tiff uploads have to
// be normalised before they can be placed on a page at all. And for a photograph JPEG is far
// smaller than PNG — the pipeline's intermediate buffers are PNG (lossless, right for repeated
// processing), but a full-resolution PNG page makes a multi-megabyte archive file for no benefit
// once processing is finished.
//
// NOTE the quality units: @napi-rs/canvas takes JPEG quality as 0-100, NOT 0-1. Passing 0.92 here
// silently encodes at quality 1 and produces unreadable garbage.
func EncodeJpeg(imageBuffer []byte, quality int) ([]byte, error) {
	img, err := decodeImage(imageBuffer)
	if err != nil {
		return nil, err
	}
	// See package deviation 2: Go's encoder would treat <=0 as "default 75", canvas does not.
	q := quality
	if q < 1 {
		q = 1
	} else if q > 100 {
		q = 100
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: q}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// RotateImage ports rotateImage. degrees is one of 0, 90, 180, 270 (clockwise). degrees == 0
// returns the input unchanged without decoding, exactly as TS does.
func RotateImage(imageBuffer []byte, degrees int) ([]byte, error) {
	if degrees == 0 {
		return imageBuffer, nil
	}
	if degrees != 90 && degrees != 180 && degrees != 270 {
		return nil, fmt.Errorf("imageprocessor: rotateImage: unsupported rotation %d (want 0, 90, 180 or 270)", degrees)
	}
	img, err := decodeImage(imageBuffer)
	if err != nil {
		return nil, err
	}
	src := toNRGBA(img)
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := w, h
	if degrees == 90 || degrees == 270 {
		dw, dh = h, w
	}
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var sx, sy int
			switch degrees {
			case 90: // src(sx,sy) -> dst(h-1-sy, sx)
				sx, sy = y, h-1-x
			case 180: // src(sx,sy) -> dst(w-1-sx, h-1-sy)
				sx, sy = w-1-x, h-1-y
			case 270: // src(sx,sy) -> dst(sy, w-1-sx)
				sx, sy = w-1-y, x
			}
			copyNRGBAPixel(dst, x, y, src, sx, sy)
		}
	}
	return encodePNG(dst)
}

// DetectDocumentBoxLocally ports detectDocumentBoxLocally. The comment below is preserved verbatim
// from the TS source:
//
// Local, model-free crop-boundary detection. Everything that decides "background vs document"
// lives in domain/flood-crop.ts (pure, zero I/O); this function is decode + prepare + map-back
// only:
//  1. decode and resample to the domain's common working scale (WORK_MAX_DIM),
//  2. pack RGBA into the 3-channel real-valued buffer the detector expects and box-blur it,
//  3. run detectDocumentBox / isAdmissibleCrop in WORKING pixel coordinates,
//  4. map the accepted box back to ORIGINAL image pixels.
func DetectDocumentBoxLocally(imageBuffer []byte) (LocalCropResult, error) {
	img, err := decodeImage(imageBuffer)
	if err != nil {
		return LocalCropResult{}, err
	}
	bounds := img.Bounds()
	ow, oh := bounds.Dx(), bounds.Dy()
	if ow == 0 || oh == 0 {
		return LocalCropResult{Kind: LocalCropNoSignal}, nil
	}
	ds := math.Min(math.Min(float64(floodcrop.WorkMaxDim)/float64(ow), float64(floodcrop.WorkMaxDim)/float64(oh)), 1)
	w := maxInt(1, jsRoundInt(float64(ow)*ds))
	h := maxInt(1, jsRoundInt(float64(oh)*ds))
	resized := resizeNRGBA(img, w, h)

	// The working buffer MUST stay Float32 all the way into the detector. Quantising the blurred
	// image back to 8-bit here collapses a contrast-compressed photo's page boundary into a single
	// integer level, so the barrier cost that boundary should carry rounds away and stops being
	// measurable at all. Measured cost of doing it: mean IoU 0.951 instead of 0.968, plus two
	// outright failures on the re-lit (gamma/gain) variants of the corpus.
	packed := make([]float32, w*h*3)
	for i, p, q := 0, 0, 0; i < w*h; i, p, q = i+1, p+4, q+3 {
		packed[q] = float32(resized.Pix[p])
		packed[q+1] = float32(resized.Pix[p+1])
		packed[q+2] = float32(resized.Pix[p+2])
	}
	radius := maxInt(1, jsRoundInt(floodcrop.BlurFrac*float64(minInt(w, h))))
	rgb := floodcrop.BoxBlurRGB(packed, w, h, radius)

	r := floodcrop.DetectDocumentBox(rgb, w, h)
	if r.Box == nil {
		return LocalCropResult{Kind: LocalCropNoSignal}, nil
	}
	if !floodcrop.IsAdmissibleCrop(r) {
		return LocalCropResult{Kind: LocalCropVetoed}, nil
	}

	// Working -> original pixels. No outward padding is applied: the 0.968 mean IoU this detector
	// was measured at was measured with none, and a pad is an unmeasured bias on every box.
	//
	// There is deliberately NO minimum-area check here either. The old detector rejected any box
	// under 25% of the frame (FLOODCROP_MIN_AREA_RATIO). Every ground-truth box in the benchmark
	// corpus is 57-86% of frame, so that check never once bound on the bench and was entirely
	// untested — while in production it would silently impose a small-document cliff on receipts,
	// ID cards and business cards. The isolation it was standing in for (a leak into an interior
	// fragment) is already done properly by the domain's centreComponent stage.
	scaleX := float64(ow) / float64(w)
	scaleY := float64(oh) / float64(h)
	return LocalCropResult{
		Kind: LocalCropBox,
		Box: CropBox{
			X:      jsRound(float64(r.Box.Left) * scaleX),
			Y:      jsRound(float64(r.Box.Top) * scaleY),
			Width:  jsRound(float64(r.Box.Right-r.Box.Left+1) * scaleX),
			Height: jsRound(float64(r.Box.Bottom-r.Box.Top+1) * scaleY),
		},
	}, nil
}

// DetectCropBoxLocally ports detectCropBoxLocally. The comment below is preserved verbatim from the
// TS source:
//
// Two-valued convenience wrapper for callers that only want "a box or nothing" — the benchmark
// scripts (scripts/crop-score.ts, crop-bench.ts, crop-fillsframe-check.ts), which score a null as
// "no crop applied". Production must use detectDocumentBoxLocally instead: a veto and a shrug are
// very different answers and the cascade has to tell them apart.
func DetectCropBoxLocally(imageBuffer []byte) (*CropBox, error) {
	r, err := DetectDocumentBoxLocally(imageBuffer)
	if err != nil {
		return nil, err
	}
	if r.Kind == LocalCropBox {
		box := r.Box
		return &box, nil
	}
	return nil, nil
}

// CropImage ports cropImage. The box is rounded and clamped into the image exactly as TS does.
func CropImage(imageBuffer []byte, box CropBox) ([]byte, error) {
	img, err := decodeImage(imageBuffer)
	if err != nil {
		return nil, err
	}
	src := toNRGBA(img)
	b := src.Bounds()
	imgW, imgH := b.Dx(), b.Dy()
	x := maxInt(0, minInt(jsRoundInt(box.X), imgW-1))
	y := maxInt(0, minInt(jsRoundInt(box.Y), imgH-1))
	width := maxInt(1, minInt(jsRoundInt(box.Width), imgW-x))
	height := maxInt(1, minInt(jsRoundInt(box.Height), imgH-y))
	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	for dy := 0; dy < height; dy++ {
		for dx := 0; dx < width; dx++ {
			copyNRGBAPixel(dst, dx, dy, src, x+dx, y+dy)
		}
	}
	return encodePNG(dst)
}

// ComputeAutoLevelsForImage ports computeAutoLevelsForImage. The comment below is preserved
// verbatim from the TS source:
//
// A coarse downsampled view is plenty for a global histogram — mirrors pdf-awesome's
// auto-adjust.js maxDim=400 approach.
func ComputeAutoLevelsForImage(imageBuffer []byte) (brightness, contrast int, err error) {
	img, err := decodeImage(imageBuffer)
	if err != nil {
		return 0, 0, err
	}
	b := img.Bounds()
	ow, oh := b.Dx(), b.Dy()
	const maxDim = 400
	ds := math.Min(math.Min(float64(maxDim)/float64(ow), float64(maxDim)/float64(oh)), 1)
	w := maxInt(1, jsRoundInt(float64(ow)*ds))
	h := maxInt(1, jsRoundInt(float64(oh)*ds))
	resized := resizeNRGBA(img, w, h)

	gray := make([]uint8, w*h)
	for i, p := 0, 0; i < w*h; i, p = i+1, p+4 {
		gray[i] = uint8(int(float64(resized.Pix[p])*0.299 + float64(resized.Pix[p+1])*0.587 + float64(resized.Pix[p+2])*0.114))
	}
	black, white := imageadjust.FindBlackWhitePoints(gray, imageadjust.AutoLevelsClipPct)
	levels := imageadjust.AutoLevelsFromBlackWhite(black, white)
	return levels.Brightness, levels.Contrast, nil
}

// Adjust is the TS `{ brightness, contrast }` argument object.
type Adjust struct {
	Brightness int
	Contrast   int
}

// ApplyBrightnessContrast ports applyBrightnessContrast. Zero deltas return the input unchanged
// without decoding, exactly as TS does. The CSS brightness()/contrast() chain is re-implemented in
// the sRGB domain; see package deviation 3.
func ApplyBrightnessContrast(imageBuffer []byte, adjust Adjust) ([]byte, error) {
	if adjust.Brightness == 0 && adjust.Contrast == 0 {
		return imageBuffer, nil
	}
	img, err := decodeImage(imageBuffer)
	if err != nil {
		return nil, err
	}
	src := toNRGBA(img)
	dst := image.NewNRGBA(src.Bounds())
	copy(dst.Pix, src.Pix)
	brightness := 1 + float64(adjust.Brightness)/100
	contrast := 1 + float64(adjust.Contrast)/100
	for i := 0; i+3 < len(dst.Pix); i += 4 {
		for ch := 0; ch < 3; ch++ {
			v := float64(dst.Pix[i+ch]) / 255
			if adjust.Brightness != 0 {
				v = clamp01(v * brightness)
			}
			if adjust.Contrast != 0 {
				v = clamp01(v*contrast + 0.5*(1-contrast))
			}
			dst.Pix[i+ch] = clampByte(v * 255)
		}
	}
	return encodePNG(dst)
}

// ApplySharpen ports applySharpen. The comment below is preserved verbatim from the TS source:
//
// Runs on raw canvas pixels since CSS/canvas filters have no sharpen primitive — same approach
// as pdf-awesome's js/domain/adjust.js applySharpen, ported to operate on a Buffer in/out.
func ApplySharpen(imageBuffer []byte, amount float64) ([]byte, error) {
	if amount == 0 {
		return imageBuffer, nil
	}
	img, err := decodeImage(imageBuffer)
	if err != nil {
		return nil, err
	}
	src := toNRGBA(img)
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	if w < 3 || h < 3 {
		return imageBuffer, nil
	}
	dst := image.NewNRGBA(src.Bounds())
	for y := 0; y < h; y++ {
		yUp := maxInt(y-1, 0)
		yDown := minInt(y+1, h-1)
		for x := 0; x < w; x++ {
			xLeft := maxInt(x-1, 0)
			xRight := minInt(x+1, w-1)
			si := src.PixOffset(x, y)
			di := dst.PixOffset(x, y)
			for ch := 0; ch < 3; ch++ {
				v := imageadjust.SharpenPixel(
					float64(src.Pix[si+ch]),
					float64(src.Pix[src.PixOffset(x, yUp)+ch]),
					float64(src.Pix[src.PixOffset(x, yDown)+ch]),
					float64(src.Pix[src.PixOffset(xLeft, y)+ch]),
					float64(src.Pix[src.PixOffset(xRight, y)+ch]),
					amount,
				)
				// Canvas stores into a Uint8ClampedArray, whose ToUint8Clamp rounds half to even.
				dst.Pix[di+ch] = toUint8Clamp(v)
			}
			dst.Pix[di+3] = src.Pix[si+3]
		}
	}
	return encodePNG(dst)
}

// toNRGBA returns src as an un-premultiplied NRGBA image, copying when needed.
func toNRGBA(src image.Image) *image.NRGBA {
	if n, ok := src.(*image.NRGBA); ok {
		return n
	}
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	imagedraw.Draw(dst, dst.Bounds(), src, b.Min, imagedraw.Src)
	return dst
}

// resizeNRGBA scales src to exactly w x h using the bilinear interpolator. See package deviation 1.
func resizeNRGBA(src image.Image, w, h int) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Src, nil)
	return dst
}

func encodePNG(img image.Image) ([]byte, error) {
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// jsRound is JS Math.round: half-values go toward +Infinity.
func jsRound(v float64) float64 { return math.Floor(v + 0.5) }

func jsRoundInt(v float64) int { return int(jsRound(v)) }

// clampByte is the float->8-bit conversion canvas performs on its filter output.
func clampByte(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(math.Round(v))
}

// toUint8Clamp is ECMAScript ToUint8Clamp: clamp to [0,255], then round to nearest with ties to
// even. It is what assigning a float into a Uint8ClampedArray does.
func toUint8Clamp(v float64) uint8 {
	if math.IsNaN(v) || v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	f := math.Floor(v)
	if f+0.5 < v {
		return uint8(f + 1)
	}
	if v < f+0.5 {
		return uint8(f)
	}
	if int64(f)%2 == 1 {
		return uint8(f + 1)
	}
	return uint8(f)
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
