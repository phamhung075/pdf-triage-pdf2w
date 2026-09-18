// Package imagedimensions is a Go port of readImageDimensions from pdf-triage's
// src/domain/image-dimensions.ts.
//
// Reads pixel dimensions straight out of an encoded image's header — no decode, no canvas, no I/O.
//
// Used to size the OCR timeout budget: PaddleOCR's runtime tracks how much there is to read on the
// page, and a page rendered at twice the area holds roughly twice as much to find. Byte length is
// NOT a usable proxy for that (a photo of a blank wall is large and instant; a dense text scan
// compresses well and is slow), so this reads the real geometry instead.
//
// Returns no result for anything it does not recognize. Callers must treat !ok as "no information"
// and fall back to their floor budget rather than guessing — a wrong guess here silently changes how
// long a document is allowed to take.
//
// The TypeScript source is the behavioral source of truth. The only signature-level deviation is
// Go's `(ImageDimensions, bool)` return, which is the idiomatic equivalent of TS's
// `ImageDimensions | null`; every byte-level branch and bound is ported as written.
package imagedimensions

import (
	"bytes"
	"encoding/binary"
)

// ImageDimensions is the Go equivalent of the TS `ImageDimensions` interface.
type ImageDimensions struct {
	Width  int
	Height int
}

var pngSignature = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

// ReadImageDimensions ports readImageDimensions: PNG first, then JPEG, else no result.
func ReadImageDimensions(buffer []byte) (ImageDimensions, bool) {
	if dims, ok := readPNG(buffer); ok {
		return dims, true
	}
	return readJPEG(buffer)
}

func readPNG(buffer []byte) (ImageDimensions, bool) {
	// IHDR is mandated to be the first chunk, so width/height sit at fixed offsets 16 and 20.
	if len(buffer) < 24 {
		return ImageDimensions{}, false
	}
	if !bytes.Equal(buffer[:8], pngSignature) {
		return ImageDimensions{}, false
	}
	if string(buffer[12:16]) != "IHDR" {
		return ImageDimensions{}, false
	}
	return sane(int(binary.BigEndian.Uint32(buffer[16:20])), int(binary.BigEndian.Uint32(buffer[20:24])))
}

// JPEG markers that carry no payload at all, so there is no length field to skip past.
func isJPEGStandalone(marker byte) bool {
	return marker == 0xd8 || marker == 0xd9 || marker == 0x01
}

func readJPEG(buffer []byte) (ImageDimensions, bool) {
	if len(buffer) < 4 || buffer[0] != 0xff || buffer[1] != 0xd8 {
		return ImageDimensions{}, false
	}

	offset := 2
	for offset+3 < len(buffer) {
		if buffer[offset] != 0xff {
			offset++ // fill byte or padding between segments
			continue
		}
		marker := buffer[offset+1]
		if marker == 0xff {
			offset++ // 0xFF is a legal pad byte before the real marker
			continue
		}
		if isJPEGStandalone(marker) {
			offset += 2
			continue
		}
		length := int(binary.BigEndian.Uint16(buffer[offset+2 : offset+4]))
		// SOFn carries the frame geometry. 0xC4 (DHT), 0xC8 (JPG) and 0xCC (DAC) sit inside the same
		// numeric range but are NOT frame headers — excluding them is what keeps this from reading
		// a Huffman table as a picture size.
		isFrameHeader := marker >= 0xc0 && marker <= 0xcf && marker != 0xc4 && marker != 0xc8 && marker != 0xcc
		if isFrameHeader {
			if offset+9 > len(buffer) {
				return ImageDimensions{}, false
			}
			return sane(
				int(binary.BigEndian.Uint16(buffer[offset+7:offset+9])),
				int(binary.BigEndian.Uint16(buffer[offset+5:offset+7])),
			)
		}
		if length < 2 {
			return ImageDimensions{}, false // malformed: a segment cannot be shorter than its own length field
		}
		offset += 2 + length
	}
	return ImageDimensions{}, false
}

func sane(width, height int) (ImageDimensions, bool) {
	// Number.isInteger is always true for the uint16/uint32 reads above, but the guard is kept
	// because TS has it and it protects the <= 0 rule below.
	if width <= 0 || height <= 0 {
		return ImageDimensions{}, false
	}
	return ImageDimensions{Width: width, Height: height}, true
}
