// Package pdfpagefit is a Go port of fitImageToA4 from pdf-triage's src/domain/pdf-page-fit.ts.
//
// Pure page geometry for placing one photographed document onto a PDF page. Zero I/O.
//
// Archived photos share the shelf with ordinary PDFs, so they are laid out on a real A4 page rather
// than on a page cut to the photo's own pixel dimensions: a page whose size varies per camera
// prints unpredictably and looks wrong next to every other document in the archive.
//
// The TypeScript source is the behavioral source of truth and carries no behavior that Go's
// standard library changes: JS numbers and Go float64 are both IEEE-754 doubles, and the
// NaN/negative guards in fitImageToA4 are written with the same `!(x > 0)` form so NaN takes the
// fallback branch in both languages.
package pdfpagefit

import "math"

// A4 in PostScript points (1/72"), the unit pdf-lib works in.
const (
	A4ShortSide = 595.28
	A4LongSide  = 841.89
)

// PagePlacement is the Go equivalent of the TS `PagePlacement` interface.
type PagePlacement struct {
	PageWidth  float64
	PageHeight float64
	DrawWidth  float64
	DrawHeight float64
	X          float64
	Y          float64
}

// FitImageToA4 fits an image onto an A4 page, preserving its aspect ratio and centring it.
//
// The page takes the orientation of the image, so a landscape photo lands on a landscape page
// instead of being letterboxed into a portrait one and wasting half the sheet. The image is scaled
// to fit inside the page (never cropped, never stretched, and never enlarged beyond the page), so
// whichever dimension is proportionally larger becomes the limiting one.
func FitImageToA4(imageWidth, imageHeight float64) PagePlacement {
	// A degenerate or unreadable size would otherwise produce NaN offsets and a corrupt page; fall
	// back to a full portrait page so the document is still archived rather than lost.
	if !(imageWidth > 0) || !(imageHeight > 0) {
		return PagePlacement{
			PageWidth:  A4ShortSide,
			PageHeight: A4LongSide,
			DrawWidth:  A4ShortSide,
			DrawHeight: A4LongSide,
			X:          0,
			Y:          0,
		}
	}

	landscape := imageWidth > imageHeight
	pageWidth := A4ShortSide
	pageHeight := A4LongSide
	if landscape {
		pageWidth = A4LongSide
		pageHeight = A4ShortSide
	}

	scale := math.Min(pageWidth/imageWidth, pageHeight/imageHeight)
	drawWidth := imageWidth * scale
	drawHeight := imageHeight * scale

	return PagePlacement{
		PageWidth:  pageWidth,
		PageHeight: pageHeight,
		DrawWidth:  drawWidth,
		DrawHeight: drawHeight,
		X:          (pageWidth - drawWidth) / 2,
		Y:          (pageHeight - drawHeight) / 2,
	}
}
