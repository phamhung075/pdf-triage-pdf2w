package exiforientation

import (
	"encoding/binary"
	"testing"
)

// All cases below are ported verbatim from pdf-triage's src/domain/exif-orientation.test.ts,
// including its hand-built minimal EXIF fixtures.

func buildJpegWithExifOrientation(orientationTag int, byteOrder string) []byte {
	isLE := byteOrder == "II"

	putU16 := func(buf []byte, offset int, val uint16) {
		if isLE {
			binary.LittleEndian.PutUint16(buf[offset:], val)
		} else {
			binary.BigEndian.PutUint16(buf[offset:], val)
		}
	}
	putU32 := func(buf []byte, offset int, val uint32) {
		if isLE {
			binary.LittleEndian.PutUint32(buf[offset:], val)
		} else {
			binary.BigEndian.PutUint32(buf[offset:], val)
		}
	}

	ifd0 := make([]byte, 2+12+4)
	putU16(ifd0, 0, 1)                       // numEntries = 1
	putU16(ifd0, 2, 0x0112)                  // tag = Orientation
	putU16(ifd0, 4, 3)                       // type = SHORT
	putU32(ifd0, 6, 1)                       // count = 1
	putU16(ifd0, 10, uint16(orientationTag)) // value, left-justified in the 4-byte value field
	putU32(ifd0, 12, 0)                      // next IFD offset = 0 (none)

	tiffHeader := make([]byte, 8)
	copy(tiffHeader[0:2], byteOrder)
	putU16(tiffHeader, 2, 0x002a) // TIFF magic
	putU32(tiffHeader, 4, 8)      // offset to IFD0, right after this header

	exifBlock := append([]byte("Exif\x00\x00"), tiffHeader...)
	exifBlock = append(exifBlock, ifd0...)

	app1Length := make([]byte, 2)
	binary.BigEndian.PutUint16(app1Length, uint16(len(exifBlock)+2)) // length field includes itself, always big-endian

	out := []byte{0xff, 0xd8, 0xff, 0xe1} // SOI + APP1 marker
	out = append(out, app1Length...)
	out = append(out, exifBlock...)
	out = append(out, 0xff, 0xd9) // EOI
	return out
}

func buildJpegWithoutExif() []byte {
	return []byte{0xff, 0xd8, 0xff, 0xd9} // SOI + EOI, no APP1
}

func buildJpegWithNonExifApp1() []byte {
	payload := []byte("JFIF\x00test-payload")
	app1Length := make([]byte, 2)
	binary.BigEndian.PutUint16(app1Length, uint16(len(payload)+2))
	out := []byte{0xff, 0xd8, 0xff, 0xe1}
	out = append(out, app1Length...)
	out = append(out, payload...)
	out = append(out, 0xff, 0xd9)
	return out
}

func TestParseExifOrientation(t *testing.T) {
	t.Run("reads the Orientation tag from a little-endian (Intel/II) EXIF block", func(t *testing.T) {
		got, ok := ParseExifOrientation(buildJpegWithExifOrientation(6, "II"))
		if !ok || got != 6 {
			t.Fatalf("got (%d, %v), want (6, true)", got, ok)
		}
	})

	t.Run("reads the Orientation tag from a big-endian (Motorola/MM) EXIF block", func(t *testing.T) {
		got, ok := ParseExifOrientation(buildJpegWithExifOrientation(3, "MM"))
		if !ok || got != 3 {
			t.Fatalf("got (%d, %v), want (3, true)", got, ok)
		}
	})

	t.Run("reads orientation tag 1 (normal, no correction needed)", func(t *testing.T) {
		got, ok := ParseExifOrientation(buildJpegWithExifOrientation(1, "II"))
		if !ok || got != 1 {
			t.Fatalf("got (%d, %v), want (1, true)", got, ok)
		}
	})

	t.Run("reads orientation tag 8", func(t *testing.T) {
		got, ok := ParseExifOrientation(buildJpegWithExifOrientation(8, "II"))
		if !ok || got != 8 {
			t.Fatalf("got (%d, %v), want (8, true)", got, ok)
		}
	})

	t.Run("returns null when the JPEG has no APP1/EXIF segment at all", func(t *testing.T) {
		if _, ok := ParseExifOrientation(buildJpegWithoutExif()); ok {
			t.Fatalf("expected no result")
		}
	})

	t.Run("returns null when APP1 is present but is not an EXIF block", func(t *testing.T) {
		if _, ok := ParseExifOrientation(buildJpegWithNonExifApp1()); ok {
			t.Fatalf("expected no result")
		}
	})

	t.Run("returns null for a non-JPEG buffer", func(t *testing.T) {
		if _, ok := ParseExifOrientation([]byte("not a jpeg at all")); ok {
			t.Fatalf("expected no result")
		}
	})

	t.Run("returns null for a buffer too short to contain a JPEG header", func(t *testing.T) {
		if _, ok := ParseExifOrientation([]byte{0xff}); ok {
			t.Fatalf("expected no result")
		}
	})
}

func TestExifOrientationToDegrees(t *testing.T) {
	intp := func(v int) *int { return &v }

	t.Run("maps tag 1 to 0 degrees", func(t *testing.T) {
		if got, ok := ExifOrientationToDegrees(intp(1)); !ok || got != 0 {
			t.Fatalf("got (%d, %v), want (0, true)", got, ok)
		}
	})

	t.Run("maps tag 3 to 180 degrees", func(t *testing.T) {
		if got, ok := ExifOrientationToDegrees(intp(3)); !ok || got != 180 {
			t.Fatalf("got (%d, %v), want (180, true)", got, ok)
		}
	})

	t.Run("maps tag 6 to 90 degrees", func(t *testing.T) {
		if got, ok := ExifOrientationToDegrees(intp(6)); !ok || got != 90 {
			t.Fatalf("got (%d, %v), want (90, true)", got, ok)
		}
	})

	t.Run("maps tag 8 to 270 degrees", func(t *testing.T) {
		if got, ok := ExifOrientationToDegrees(intp(8)); !ok || got != 270 {
			t.Fatalf("got (%d, %v), want (270, true)", got, ok)
		}
	})

	t.Run("maps null (no EXIF) to null", func(t *testing.T) {
		if got, ok := ExifOrientationToDegrees(nil); ok {
			t.Fatalf("got (%d, %v), want (0, false)", got, ok)
		}
	})

	t.Run("maps a mirrored tag (2, 4, 5, 7) to null rather than guessing a rotation", func(t *testing.T) {
		for _, tag := range []int{2, 4, 5, 7} {
			if got, ok := ExifOrientationToDegrees(intp(tag)); ok {
				t.Fatalf("tag %d => (%d, true), want (0, false)", tag, got)
			}
		}
	})
}
