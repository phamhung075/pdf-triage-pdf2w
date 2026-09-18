// Package exiforientation is a Go port of pdf-triage's src/domain/exif-orientation.ts:
// parseExifOrientation and exifOrientationToDegrees.
//
// Manually parses the EXIF Orientation tag (0x0112) from a JPEG's APP1 segment — no dependency
// needed for this one value.
//
// The TypeScript source is the behavioral source of truth. The only signature-level deviation is
// that TS's `number | null` inputs/outputs become a `*int` input and an `(int, bool)` result, Go's
// idiomatic nullable scalar forms; every bounds check and byte order branch is ported as written.
// Node's `buffer.toString('ascii', start, end)` silently clamps out-of-range bounds to the buffer,
// so the Go port guards the "Exif" slice with an explicit length check before comparing.
package exiforientation

import "encoding/binary"

// ParseExifOrientation returns the raw EXIF orientation tag (1-8), and whether one was found. It
// reports false if the file isn't a JPEG, has no APP1/EXIF segment, or the segment has no
// Orientation entry.
func ParseExifOrientation(buffer []byte) (int, bool) {
	if len(buffer) < 4 || buffer[0] != 0xff || buffer[1] != 0xd8 {
		return 0, false // not a JPEG (SOI marker)
	}

	offset := 2
	for offset < len(buffer)-1 {
		if buffer[offset] != 0xff {
			break
		}
		marker := buffer[offset+1]
		if marker == 0xd8 || marker == 0xd9 {
			offset += 2
			continue
		}
		if marker == 0xda {
			break // Start of Scan — no more metadata markers follow
		}
		if offset+4 > len(buffer) {
			break
		}
		segmentLength := int(binary.BigEndian.Uint16(buffer[offset+2 : offset+4]))

		if marker == 0xe1 {
			exifStart := offset + 4
			// Node clamps an over-long read, so an out-of-range APP1 payload simply cannot equal
			// "Exif" and falls through to the same skip the TS code performs.
			if exifStart+4 > len(buffer) || string(buffer[exifStart:exifStart+4]) != "Exif" {
				offset += 2 + segmentLength
				continue
			}
			tiffStart := exifStart + 6 // skip 'Exif\0\0'
			if tiffStart+8 > len(buffer) {
				return 0, false
			}
			byteOrder := string(buffer[tiffStart : tiffStart+2])
			isLittleEndian := byteOrder == "II"
			if !isLittleEndian && byteOrder != "MM" {
				return 0, false
			}
			readU16 := func(o int) int {
				if isLittleEndian {
					return int(binary.LittleEndian.Uint16(buffer[o : o+2]))
				}
				return int(binary.BigEndian.Uint16(buffer[o : o+2]))
			}
			readU32 := func(o int) uint32 {
				if isLittleEndian {
					return binary.LittleEndian.Uint32(buffer[o : o+4])
				}
				return binary.BigEndian.Uint32(buffer[o : o+4])
			}

			ifd0Offset := readU32(tiffStart + 4)
			ifd0Start := tiffStart + int(ifd0Offset)
			if ifd0Start+2 > len(buffer) {
				return 0, false
			}
			numEntries := readU16(ifd0Start)

			for i := 0; i < numEntries; i++ {
				entryOffset := ifd0Start + 2 + i*12
				if entryOffset+12 > len(buffer) {
					break
				}
				tag := readU16(entryOffset)
				if tag == 0x0112 {
					return readU16(entryOffset + 8), true
				}
			}
			return 0, false
		}

		offset += 2 + segmentLength
	}
	return 0, false
}

// ExifOrientationToDegrees maps the raw EXIF Orientation tag to the clockwise rotation (matching
// image-processor.ts's rotateImage semantics) needed to correct the image. Tags 2/4/5/7 involve a
// mirror flip (rare for camera photos) that isn't representable as a pure rotation, so they map to
// no result rather than silently dropping the mirror. A nil tag (no EXIF) also maps to no result.
func ExifOrientationToDegrees(tag *int) (int, bool) {
	if tag == nil {
		return 0, false
	}
	switch *tag {
	case 1:
		return 0, true
	case 3:
		return 180, true
	case 6:
		return 90, true
	case 8:
		return 270, true
	default:
		return 0, false
	}
}
