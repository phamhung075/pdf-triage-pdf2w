package orientation

// Ports src/infrastructure/orientation-detector.test.ts case-for-case. The TS suite is GREEN at
// port time (`npx vitest run src/infrastructure/orientation-detector.test.ts` -> 3 passed).
//
// The TS test mocked the exif-orientation module and vision-client. This port injects the model
// call the same way and exercises the REAL, already-ported exiforientation package with a synthetic
// JPEG carrying an APP1 Orientation tag, so the cascade's EXIF branch is still covered.

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/infra/vision"
)

// exifJPEG returns a byte slice shaped like a JPEG whose APP1 segment carries the given EXIF
// Orientation tag. ParseExifOrientation returns as soon as it finds the tag, so no scan data is
// needed.
func exifJPEG(tag int) []byte {
	tiff := make([]byte, 26)
	copy(tiff[0:2], "II")
	binary.LittleEndian.PutUint16(tiff[2:4], 42)
	binary.LittleEndian.PutUint32(tiff[4:8], 8)             // IFD0 offset
	binary.LittleEndian.PutUint16(tiff[8:10], 1)            // entry count
	binary.LittleEndian.PutUint16(tiff[10:12], 0x0112)      // Orientation tag
	binary.LittleEndian.PutUint16(tiff[12:14], 3)           // type SHORT
	binary.LittleEndian.PutUint32(tiff[14:18], 1)           // count
	binary.LittleEndian.PutUint16(tiff[18:20], uint16(tag)) // value
	binary.LittleEndian.PutUint32(tiff[22:26], 0)           // next IFD

	payload := append([]byte("Exif\x00\x00"), tiff...)
	out := []byte{0xff, 0xd8, 0xff, 0xe1, 0x00, 0x00}
	binary.BigEndian.PutUint16(out[4:6], uint16(len(payload)+2))
	return append(out, payload...)
}

func fakeDetector(result vision.OrientationResult) OrientationDetector {
	return func(context.Context, []byte) (vision.OrientationResult, error) { return result, nil }
}

func TestDetectOrientationCascadeUsesAgreedValue(t *testing.T) {
	detect := fakeDetector(vision.OrientationResult{RotationDegrees: 90, Raw: `{"rotationDegrees":90}`})
	result, err := DetectOrientationCascade(context.Background(), exifJPEG(6), detect)
	if err != nil {
		t.Fatalf("DetectOrientationCascade error = %v", err)
	}
	if result.RotationDegrees != 90 {
		t.Errorf("rotationDegrees = %d, want 90", result.RotationDegrees)
	}
	if result.Source != SourceExifModelAgree {
		t.Errorf("source = %q, want %q", result.Source, SourceExifModelAgree)
	}
	if result.ExifDegrees == nil || *result.ExifDegrees != 90 {
		t.Errorf("exifDegrees = %v, want 90", result.ExifDegrees)
	}
	if result.ModelDegrees != 90 {
		t.Errorf("modelDegrees = %d, want 90", result.ModelDegrees)
	}
	if result.ModelRaw != `{"rotationDegrees":90}` {
		t.Errorf("modelRaw = %q", result.ModelRaw)
	}
}

func TestDetectOrientationCascadeTakesModelOnDisagreement(t *testing.T) {
	detect := fakeDetector(vision.OrientationResult{RotationDegrees: 0, Raw: `{"rotationDegrees":0}`})
	result, err := DetectOrientationCascade(context.Background(), exifJPEG(3), detect)
	if err != nil {
		t.Fatalf("DetectOrientationCascade error = %v", err)
	}
	if result.Source != SourceModelOnly {
		t.Errorf("source = %q, want %q", result.Source, SourceModelOnly)
	}
	if result.RotationDegrees != 0 {
		t.Errorf("rotationDegrees = %d, want 0", result.RotationDegrees)
	}
	if result.ExifDegrees == nil || *result.ExifDegrees != 180 {
		t.Errorf("exifDegrees = %v, want 180", result.ExifDegrees)
	}
	if result.ModelDegrees != 0 {
		t.Errorf("modelDegrees = %d, want 0", result.ModelDegrees)
	}
}

func TestDetectOrientationCascadeTakesModelWhenExifAbsent(t *testing.T) {
	detect := fakeDetector(vision.OrientationResult{RotationDegrees: 180, Raw: `{"rotationDegrees":180}`})
	result, err := DetectOrientationCascade(context.Background(), []byte("not a jpeg"), detect)
	if err != nil {
		t.Fatalf("DetectOrientationCascade error = %v", err)
	}
	if result.Source != SourceModelOnly {
		t.Errorf("source = %q, want %q", result.Source, SourceModelOnly)
	}
	if result.RotationDegrees != 180 {
		t.Errorf("rotationDegrees = %d, want 180", result.RotationDegrees)
	}
	if result.ExifDegrees != nil {
		t.Errorf("exifDegrees = %v, want nil", *result.ExifDegrees)
	}
}

// Extra: a mirror-only EXIF tag (2/4/5/7) maps to no degrees, so the cascade is model-only — the
// same result exifOrientationToDegrees gives in TS.
func TestDetectOrientationCascadeIgnoresMirrorOnlyExifTag(t *testing.T) {
	detect := fakeDetector(vision.OrientationResult{RotationDegrees: 0, Raw: `{}`})
	result, err := DetectOrientationCascade(context.Background(), exifJPEG(2), detect)
	if err != nil {
		t.Fatalf("DetectOrientationCascade error = %v", err)
	}
	if result.ExifDegrees != nil {
		t.Errorf("exifDegrees = %v, want nil for a mirror-only tag", *result.ExifDegrees)
	}
	if result.Source != SourceModelOnly {
		t.Errorf("source = %q, want %q", result.Source, SourceModelOnly)
	}
}
