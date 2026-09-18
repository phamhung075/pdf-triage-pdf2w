package pdfpagefit

import (
	"math"
	"testing"
)

// closeTo mirrors vitest's `expect(x).toBeCloseTo(y, 6)`: |x-y| < 0.5 * 10^-6.
func closeTo(got, want float64) bool {
	return math.Abs(got-want) < 0.5e-6
}

// All cases below are ported verbatim from pdf-triage's src/domain/pdf-page-fit.test.ts.

func TestFitImageToA4(t *testing.T) {
	t.Run("gives a portrait photo a portrait page", func(t *testing.T) {
		p := FitImageToA4(1154, 2051)
		if p.PageWidth != A4ShortSide {
			t.Fatalf("pageWidth = %v, want %v", p.PageWidth, A4ShortSide)
		}
		if p.PageHeight != A4LongSide {
			t.Fatalf("pageHeight = %v, want %v", p.PageHeight, A4LongSide)
		}
	})

	t.Run("gives a landscape photo a landscape page rather than letterboxing it", func(t *testing.T) {
		p := FitImageToA4(2227, 1253)
		if p.PageWidth != A4LongSide {
			t.Fatalf("pageWidth = %v, want %v", p.PageWidth, A4LongSide)
		}
		if p.PageHeight != A4ShortSide {
			t.Fatalf("pageHeight = %v, want %v", p.PageHeight, A4ShortSide)
		}
	})

	t.Run("preserves the aspect ratio", func(t *testing.T) {
		p := FitImageToA4(1000, 2000)
		got := p.DrawWidth / p.DrawHeight
		want := 1000.0 / 2000.0
		if !closeTo(got, want) {
			t.Fatalf("drawWidth/drawHeight = %v, want %v", got, want)
		}
	})

	t.Run("fits inside the page without cropping or stretching", func(t *testing.T) {
		for _, dims := range [][2]float64{{1154, 2051}, {2227, 1253}, {1000, 1000}, {4000, 300}} {
			p := FitImageToA4(dims[0], dims[1])
			if p.DrawWidth > p.PageWidth+1e-9 {
				t.Fatalf("drawWidth %v exceeds pageWidth %v", p.DrawWidth, p.PageWidth)
			}
			if p.DrawHeight > p.PageHeight+1e-9 {
				t.Fatalf("drawHeight %v exceeds pageHeight %v", p.DrawHeight, p.PageHeight)
			}
		}
	})

	t.Run("centres the image on the page", func(t *testing.T) {
		p := FitImageToA4(1000, 3000)
		if !closeTo(p.X, (p.PageWidth-p.DrawWidth)/2) {
			t.Fatalf("x = %v, want %v", p.X, (p.PageWidth-p.DrawWidth)/2)
		}
		if !closeTo(p.Y, (p.PageHeight-p.DrawHeight)/2) {
			t.Fatalf("y = %v, want %v", p.Y, (p.PageHeight-p.DrawHeight)/2)
		}
		if p.X < 0 {
			t.Fatalf("x = %v, want >= 0", p.X)
		}
		if p.Y < 0 {
			t.Fatalf("y = %v, want >= 0", p.Y)
		}
	})

	t.Run("touches the limiting edge exactly, so the page is actually filled in one dimension", func(t *testing.T) {
		// A very wide image is width-limited: it should span the full page width.
		wide := FitImageToA4(4000, 300)
		if !closeTo(wide.DrawWidth, wide.PageWidth) {
			t.Fatalf("wide.drawWidth = %v, want %v", wide.DrawWidth, wide.PageWidth)
		}
		// A very tall one is height-limited.
		tall := FitImageToA4(300, 4000)
		if !closeTo(tall.DrawHeight, tall.PageHeight) {
			t.Fatalf("tall.drawHeight = %v, want %v", tall.DrawHeight, tall.PageHeight)
		}
	})

	t.Run("is scale-invariant — the same photo at two resolutions lays out identically", func(t *testing.T) {
		small := FitImageToA4(1154, 2051)
		large := FitImageToA4(2308, 4102)
		if !closeTo(large.DrawWidth, small.DrawWidth) {
			t.Fatalf("large.drawWidth = %v, want %v", large.DrawWidth, small.DrawWidth)
		}
		if !closeTo(large.DrawHeight, small.DrawHeight) {
			t.Fatalf("large.drawHeight = %v, want %v", large.DrawHeight, small.DrawHeight)
		}
	})

	t.Run("falls back to a full portrait page on a degenerate size instead of producing NaN", func(t *testing.T) {
		for _, dims := range [][2]float64{{0, 100}, {100, 0}, {-5, 10}, {math.NaN(), 100}} {
			p := FitImageToA4(dims[0], dims[1])
			for _, v := range []float64{p.DrawWidth, p.DrawHeight, p.X, p.Y} {
				if math.IsNaN(v) || math.IsInf(v, 0) {
					t.Fatalf("got non-finite value %v for image %v", v, dims)
				}
			}
			if p.PageWidth != A4ShortSide {
				t.Fatalf("pageWidth = %v, want %v", p.PageWidth, A4ShortSide)
			}
			if p.PageHeight != A4LongSide {
				t.Fatalf("pageHeight = %v, want %v", p.PageHeight, A4LongSide)
			}
		}
	})
}
