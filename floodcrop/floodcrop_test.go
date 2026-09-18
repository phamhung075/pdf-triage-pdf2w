package floodcrop

import (
	"math"
	"testing"
)

// All 15 cases below are ported verbatim from pdf-triage's src/domain/flood-crop.test.ts. Upstream
// is GREEN: `npx vitest run src/domain/flood-crop.test.ts --reporter=verbose` reports
// "Tests 15 passed (15)". There is no red upstream case to pin. The descriptive comments from the
// TS suite are preserved because they document the invariants under test.

func TestDetectDocumentBox(t *testing.T) {
	t.Run("finds a tight, admissible box for a document on a clearly different background", func(t *testing.T) {
		r := detectScene(400, 300, func(c *testCanvas, w, h int) {
			c.paint = solid(15, 15, 15)
			c.fillRect(0, 0, float64(w), float64(h))
			c.paint = solid(250, 250, 250)
			c.fillRect(80, 60, 240, 180)
		})

		if r.Box == nil {
			t.Fatal("box = nil, want non-nil")
		}
		if !IsAdmissibleCrop(r.DocumentBoxResult) {
			t.Fatal("isAdmissibleCrop = false, want true")
		}
		assertWithin(t, "left", float64(r.Box.Left), 80, 5)
		assertWithin(t, "top", float64(r.Box.Top), 60, 5)
		assertWithin(t, "right", float64(r.Box.Right), 319, 5)
		assertWithin(t, "bottom", float64(r.Box.Bottom), 239, 5)
	})

	// The case a single global brightness threshold fails on: the page is barely lighter than the
	// desk, and the desk is not even one colour. The barrier map handles it because a smooth
	// lighting gradient can be crossed without ever taking a large step, while the page edge
	// cannot.
	t.Run("finds the document on a gradient pale-on-pale background", func(t *testing.T) {
		r := detectScene(500, 400, func(c *testCanvas, w, h int) {
			grad := linearGrad{x0: 0, y0: 0, x1: float64(w), y1: float64(h)}
			grad.stops = []gradStop{
				{t: 0, c: testRGB{238, 225, 225}},
				{t: 1, c: testRGB{222, 222, 238}},
			}
			c.paint = grad
			c.fillRect(0, 0, float64(w), float64(h))
			c.paint = solid(248, 248, 246)
			c.fillRect(60, 50, 380, 300)
		})

		if r.Box == nil {
			t.Fatal("box = nil, want non-nil")
		}
		if !IsAdmissibleCrop(r.DocumentBoxResult) {
			t.Fatal("isAdmissibleCrop = false, want true")
		}
		assertWithin(t, "left", float64(r.Box.Left), 60, 5)
		assertWithin(t, "top", float64(r.Box.Top), 50, 5)
		assertWithin(t, "right", float64(r.Box.Right), 439, 5)
		assertWithin(t, "bottom", float64(r.Box.Bottom), 349, 5)
	})

	t.Run("is not admissible for a uniform image with no document at all", func(t *testing.T) {
		r := detectScene(200, 200, func(c *testCanvas, w, h int) {
			c.paint = solid(200, 200, 200)
			c.fillRect(0, 0, float64(w), float64(h))
		})

		if r.Box != nil {
			t.Fatalf("box = %+v, want nil", r.Box)
		}
		if IsAdmissibleCrop(r.DocumentBoxResult) {
			t.Fatal("isAdmissibleCrop = true, want false")
		}
	})

	// A real phone photo had a pen lying on the desk beside the document. Anything that unioned
	// every non-background region would drag the box out to swallow it. The centre-covering
	// component vote discards it instead, because the pen does not cover the middle of the frame.
	t.Run("excludes an unrelated object elsewhere in the frame via the centre vote", func(t *testing.T) {
		r := detectScene(400, 300, func(c *testCanvas, w, h int) {
			c.paint = solid(15, 15, 15)
			c.fillRect(0, 0, float64(w), float64(h))
			c.paint = solid(250, 250, 250)
			c.fillRect(80, 40, 240, 180) // the document
			c.paint = solid(200, 30, 30)
			c.fillRect(20, 260, 15, 15) // a small red "pen" object down in the corner
		})

		if r.Box == nil {
			t.Fatal("box = nil, want non-nil")
		}
		if !IsAdmissibleCrop(r.DocumentBoxResult) {
			t.Fatal("isAdmissibleCrop = false, want true")
		}
		// The document's own bounds — not stretched down/left toward the pen at (20,260).
		assertWithin(t, "left", float64(r.Box.Left), 80, 5)
		assertWithin(t, "top", float64(r.Box.Top), 40, 5)
		assertWithin(t, "right", float64(r.Box.Right), 319, 5)
		assertWithin(t, "bottom", float64(r.Box.Bottom), 219, 5)
	})

	// Recast of the scene that used to justify the deleted texture gate. A page whose paper is only
	// barely distinguishable from the desk (9 levels), carrying much stronger printed content
	// inside it. There is no texture gate any more, so the assertion is on the OUTCOME, and it has
	// two halves: the low-contrast page must not be swallowed into the background (the box must be
	// the page, not null and not the frame), and the box must not collapse onto the print block
	// inside it either.
	t.Run("does not swallow a low-colour-contrast printed page, nor collapse onto its print", func(t *testing.T) {
		r := detectScene(300, 300, func(c *testCanvas, w, h int) {
			c.paint = solid(205, 205, 205)
			c.fillRect(0, 0, float64(w), float64(h))
			c.paint = solid(196, 196, 196)
			c.fillRect(100, 100, 100, 100) // the page: 9 levels off the desk
			c.paint = solid(120, 120, 120)
			for i := 0; i < 8; i++ {
				c.fillRect(110, float64(112+i*11), 80, 3) // printed lines
			}
		})

		if r.Box == nil {
			t.Fatal("box = nil, want non-nil")
		}
		if !IsAdmissibleCrop(r.DocumentBoxResult) {
			t.Fatal("isAdmissibleCrop = false, want true")
		}
		assertWithin(t, "left", float64(r.Box.Left), 100, 5)
		assertWithin(t, "top", float64(r.Box.Top), 100, 5)
		assertWithin(t, "right", float64(r.Box.Right), 199, 5)
		assertWithin(t, "bottom", float64(r.Box.Bottom), 199, 5)
	})

	// THE test that would have caught the original bug. The deleted texture gate compared a
	// gradient magnitude against an ABSOLUTE floor, which is a resolution-dependent quantity: the
	// same scene photographed larger has gentler per-pixel gradients. Every decision in this
	// detector is instead a fraction of a dimension or a fraction of a population, so the same
	// scene at 600x450 and at 1800x1350 must produce the same box in normalized coordinates.
	t.Run("is scale invariant: the same scene at 600x450 and 1800x1350 gives the same normalized box", func(t *testing.T) {
		small := detectScene(600, 450, pageOnDarkDesk(1, 0))
		large := detectScene(1800, 1350, pageOnDarkDesk(1, 0))

		if !IsAdmissibleCrop(small.DocumentBoxResult) {
			t.Fatal("small: isAdmissibleCrop = false, want true")
		}
		if !IsAdmissibleCrop(large.DocumentBoxResult) {
			t.Fatal("large: isAdmissibleCrop = false, want true")
		}
		a := normalized(small)
		b := normalized(large)
		if math.Abs(a.x-b.x) >= 0.01 {
			t.Fatalf("x delta = %v, want < 0.01", math.Abs(a.x-b.x))
		}
		if math.Abs(a.y-b.y) >= 0.01 {
			t.Fatalf("y delta = %v, want < 0.01", math.Abs(a.y-b.y))
		}
		if math.Abs(a.w-b.w) >= 0.01 {
			t.Fatalf("w delta = %v, want < 0.01", math.Abs(a.w-b.w))
		}
		if math.Abs(a.h-b.h) >= 0.01 {
			t.Fatalf("h delta = %v, want < 0.01", math.Abs(a.h-b.h))
		}
	})

	// Every quantity the detector compares against is measured on the photo itself, and the two
	// searches that could drift under a tone change (the leak window and the scale-space edge
	// search) are defined multiplicatively. So flattening and lifting the whole image must not move
	// the box.
	t.Run("is intensity invariant: gain 1.0 and gain 0.6 + lift 60 give the same normalized box", func(t *testing.T) {
		bright := detectScene(600, 450, pageOnDarkDesk(1.0, 0))
		flat := detectScene(600, 450, pageOnDarkDesk(0.6, 60))

		if !IsAdmissibleCrop(bright.DocumentBoxResult) {
			t.Fatal("bright: isAdmissibleCrop = false, want true")
		}
		if !IsAdmissibleCrop(flat.DocumentBoxResult) {
			t.Fatal("flat: isAdmissibleCrop = false, want true")
		}
		a := normalized(bright)
		b := normalized(flat)
		if math.Abs(a.x-b.x) >= 0.01 {
			t.Fatalf("x delta = %v, want < 0.01", math.Abs(a.x-b.x))
		}
		if math.Abs(a.y-b.y) >= 0.01 {
			t.Fatalf("y delta = %v, want < 0.01", math.Abs(a.y-b.y))
		}
		if math.Abs(a.w-b.w) >= 0.01 {
			t.Fatalf("w delta = %v, want < 0.01", math.Abs(a.w-b.w))
		}
		if math.Abs(a.h-b.h) >= 0.01 {
			t.Fatalf("h delta = %v, want < 0.01", math.Abs(a.h-b.h))
		}
	})

	// THE BLOCKING DEFECT the admissibility guard exists for. A scan, a close-up capture, a PDF page
	// render, or a second pass over this pipeline's own output: the frame IS the page, so there is
	// no background and the only thing enclosed by a strong barrier is the PRINT. The pipeline still
	// returns a confident, well-formed box — around a text block — and cropping to it destroys the
	// page. Only the outside-the-box evidence can tell: the annulus is full of more print, and the
	// material either side of the "edge" is the same paper.
	t.Run("refuses a printed page that fills the entire frame edge to edge", func(t *testing.T) {
		fillsFrame := func(c *testCanvas, w, h int) {
			c.paint = solid(246, 246, 244)
			c.fillRect(0, 0, float64(w), float64(h))
			c.paint = solid(30, 30, 30)
			for i := 0; i < 14; i++ {
				c.fillRect(0.12*float64(w), (0.1+float64(i)*0.055)*float64(h), 0.72*float64(w), 0.02*float64(h))
			}
		}

		landscape := detectScene(400, 300, fillsFrame)
		// The detector DOES produce a box here — that is the whole problem, and why the guard cannot
		// be expressed as "did we find something".
		if landscape.Box == nil {
			t.Fatal("landscape: box = nil, want non-nil")
		}
		if IsAdmissibleCrop(landscape.DocumentBoxResult) {
			t.Fatal("landscape: isAdmissibleCrop = true, want false")
		}

		// Same verdict at a different aspect ratio and resolution, so this is not one lucky framing.
		portrait := detectScene(900, 1200, fillsFrame)
		if portrait.Box == nil {
			t.Fatal("portrait: box = nil, want non-nil")
		}
		if IsAdmissibleCrop(portrait.DocumentBoxResult) {
			t.Fatal("portrait: isAdmissibleCrop = true, want false")
		}
	})

	// Guards against re-introducing the deleted FLOODCROP_MIN_AREA_RATIO = 0.25, which rejected any
	// document under a quarter of the frame. No ground-truth box in the benchmark corpus is that
	// small (they run 57-86% of frame), so such a rule is invisible to the bench while silently
	// imposing a cliff on receipts, ID cards and business cards.
	t.Run("finds a small document occupying only ~8% of the frame", func(t *testing.T) {
		r := detectScene(600, 450, func(c *testCanvas, w, h int) {
			c.paint = solid(20, 22, 28)
			c.fillRect(0, 0, float64(w), float64(h))
			c.paint = solid(248, 248, 246)
			c.fillRect(0.35*float64(w), 0.367*float64(h), 0.3*float64(w), 0.267*float64(h)) // 0.3 * 0.267 = 8.0% of frame area
		})

		if r.Box == nil {
			t.Fatal("box = nil, want non-nil")
		}
		if !IsAdmissibleCrop(r.DocumentBoxResult) {
			t.Fatal("isAdmissibleCrop = false, want true")
		}
		n := normalized(r)
		if n.w*n.h >= 0.12 {
			t.Fatalf("normalized area = %v, want < 0.12 (still small — not blown out to the frame)", n.w*n.h)
		}
		assertWithin(t, "left", float64(r.Box.Left), 210, 5)
		assertWithin(t, "top", float64(r.Box.Top), 165, 5)
		assertWithin(t, "right", float64(r.Box.Right), 389, 5)
		assertWithin(t, "bottom", float64(r.Box.Bottom), 285, 5)
	})
}

func TestIsAdmissibleCrop(t *testing.T) {
	t.Run("rejects a null box", func(t *testing.T) {
		if IsAdmissibleCrop(DocumentBoxResult{Box: nil, Impurity: 0, Distinctness: 99}) {
			t.Fatal("isAdmissibleCrop = true, want false")
		}
	})

	t.Run("rejects a box whose annulus is not background", func(t *testing.T) {
		box := Rect{Left: 10, Top: 10, Right: 90, Bottom: 90}
		if IsAdmissibleCrop(DocumentBoxResult{Box: &box, Impurity: MaxAnnulusImpurity + 0.001, Distinctness: 99}) {
			t.Fatal("impurity just above limit: isAdmissibleCrop = true, want false")
		}
		if !IsAdmissibleCrop(DocumentBoxResult{Box: &box, Impurity: MaxAnnulusImpurity, Distinctness: 99}) {
			t.Fatal("impurity at limit: isAdmissibleCrop = false, want true")
		}
	})

	t.Run("rejects a box whose annulus is the same material as its interior", func(t *testing.T) {
		box := Rect{Left: 10, Top: 10, Right: 90, Bottom: 90}
		if IsAdmissibleCrop(DocumentBoxResult{Box: &box, Impurity: 0, Distinctness: 1}) {
			t.Fatal("distinctness = 1: isAdmissibleCrop = true, want false")
		}
		if !IsAdmissibleCrop(DocumentBoxResult{Box: &box, Impurity: 0, Distinctness: 1.001}) {
			t.Fatal("distinctness = 1.001: isAdmissibleCrop = false, want true")
		}
	})
}

// The two degeneracies are deliberately asymmetric, and getting them the wrong way round waves
// through the most destructive answer the detector can give.
func TestMaterialDistinctnessDegeneracy(t *testing.T) {
	work := Work{RGB: make([]float32, 4*3), W: 2, H: 2}
	for i := range work.RGB {
		work.RGB[i] = 100
	}

	t.Run("fails CLOSED (returns 0) when the box interior is empty — a collapsed box must never pass", func(t *testing.T) {
		got := MaterialDistinctness(work, FrameSplit{Inside: nil, Outside: []int{0, 1}}, []int{100})
		if got != 0 {
			t.Fatalf("materialDistinctness = %v, want 0", got)
		}
	})

	t.Run("fails OPEN (returns Infinity) when the annulus is empty — the box is the whole frame, so cropping is already a no-op", func(t *testing.T) {
		got := MaterialDistinctness(work, FrameSplit{Inside: []int{0, 1}, Outside: nil}, []int{100})
		if !math.IsInf(got, 1) {
			t.Fatalf("materialDistinctness = %v, want +Inf", got)
		}
	})

	t.Run("returns Infinity when no sample measured any barrier strength at all", func(t *testing.T) {
		got := MaterialDistinctness(work, FrameSplit{Inside: []int{0}, Outside: []int{1}}, []int{0})
		if !math.IsInf(got, 1) {
			t.Fatalf("materialDistinctness = %v, want +Inf", got)
		}
	})
}
