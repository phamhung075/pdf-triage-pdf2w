package floodcrop

// This file is test-only infrastructure: a minimal stand-in for the subset of @napi-rs/canvas that
// src/domain/flood-crop.test.ts uses (createCanvas, fillStyle/fillRect, createLinearGradient,
// drawImage downscale, getImageData). It exists so the ported Go tests can render the same scenes
// the TS suite renders, without adding a canvas dependency to a go.mod that must stay
// dependency-free. Only axis-aligned rectangle fills and linear gradients appear in that suite, so
// a small area-coverage rasteriser is sufficient; the downscale is an area average, which preserves
// the geometry every tolerance-based assertion depends on.

import (
	"math"
	"testing"
)

// testRGB is an un-premultiplied RGB triple in 0..255.
type testRGB struct{ r, g, b float64 }

// testPaint is the canvas fillStyle.
type testPaint interface {
	at(x, y float64) testRGB
}

// solidPaint reproduces a `rgb(r,g,b)` fillStyle.
type solidPaint struct{ c testRGB }

func (s solidPaint) at(_, _ float64) testRGB { return s.c }

// gradStop is one addColorStop.
type gradStop struct {
	t float64
	c testRGB
}

// linearGrad reproduces createLinearGradient + addColorStop.
type linearGrad struct {
	x0, y0, x1, y1 float64
	stops          []gradStop
}

func (g linearGrad) at(x, y float64) testRGB {
	dx, dy := g.x1-g.x0, g.y1-g.y0
	denom := dx*dx + dy*dy
	tt := 0.0
	if denom != 0 {
		tt = ((x-g.x0)*dx + (y-g.y0)*dy) / denom
	}
	if tt < 0 {
		tt = 0
	} else if tt > 1 {
		tt = 1
	}
	if len(g.stops) == 0 {
		return testRGB{}
	}
	if tt <= g.stops[0].t {
		return g.stops[0].c
	}
	for i := 1; i < len(g.stops); i++ {
		if tt <= g.stops[i].t {
			a, b := g.stops[i-1], g.stops[i]
			if b.t == a.t {
				return b.c
			}
			f := (tt - a.t) / (b.t - a.t)
			return testRGB{
				r: a.c.r + (b.c.r-a.c.r)*f,
				g: a.c.g + (b.c.g-a.c.g)*f,
				b: a.c.b + (b.c.b-a.c.b)*f,
			}
		}
	}
	return g.stops[len(g.stops)-1].c
}

// testCanvas stores composited RGBA channels in 0..255. Every scene fills the whole canvas with an
// opaque paint first, so alpha stays 255 and un-premultiplying in imageData is a no-op.
type testCanvas struct {
	w, h  int
	pix   []float64
	paint testPaint
}

func newTestCanvas(w, h int) *testCanvas {
	return &testCanvas{w: w, h: h, pix: make([]float64, w*h*4)}
}

func (c *testCanvas) fillRect(x, y, w, h float64) {
	if c.paint == nil {
		return
	}
	x0 := int(math.Floor(x))
	x1 := int(math.Ceil(x + w))
	y0 := int(math.Floor(y))
	y1 := int(math.Ceil(y + h))
	for py := y0; py < y1; py++ {
		for px := x0; px < x1; px++ {
			if px < 0 || px >= c.w || py < 0 || py >= c.h {
				continue
			}
			cov := edgeOverlap(px, x, w) * edgeOverlap(py, y, h)
			if cov <= 0 {
				continue
			}
			col := c.paint.at(float64(px)+0.5, float64(py)+0.5)
			i := (py*c.w + px) * 4
			c.pix[i] = col.r*cov + c.pix[i]*(1-cov)
			c.pix[i+1] = col.g*cov + c.pix[i+1]*(1-cov)
			c.pix[i+2] = col.b*cov + c.pix[i+2]*(1-cov)
			c.pix[i+3] = 255*cov + c.pix[i+3]*(1-cov)
		}
	}
}

// edgeOverlap is the fraction of pixel p covered by the interval [start, start+size).
func edgeOverlap(p int, start, size float64) float64 {
	lo := math.Max(float64(p), start)
	hi := math.Min(float64(p+1), start+size)
	if hi <= lo {
		return 0
	}
	return hi - lo
}

// drawImageScaled reproduces ctx.drawImage(src, 0, 0, dw, dh) as an area average of the source.
func (c *testCanvas) drawImageScaled(src *testCanvas, dw, dh int) {
	for dy := 0; dy < dh; dy++ {
		sy0 := float64(dy) * float64(src.h) / float64(dh)
		sy1 := float64(dy+1) * float64(src.h) / float64(dh)
		for dx := 0; dx < dw; dx++ {
			sx0 := float64(dx) * float64(src.w) / float64(dw)
			sx1 := float64(dx+1) * float64(src.w) / float64(dw)
			var sum [4]float64
			wsum := 0.0
			for sy := int(math.Floor(sy0)); sy < int(math.Ceil(sy1)); sy++ {
				if sy < 0 || sy >= src.h {
					continue
				}
				wy := math.Min(float64(sy+1), sy1) - math.Max(float64(sy), sy0)
				if wy <= 0 {
					continue
				}
				for sx := int(math.Floor(sx0)); sx < int(math.Ceil(sx1)); sx++ {
					if sx < 0 || sx >= src.w {
						continue
					}
					wx := math.Min(float64(sx+1), sx1) - math.Max(float64(sx), sx0)
					if wx <= 0 {
						continue
					}
					wgt := wx * wy
					si := (sy*src.w + sx) * 4
					for ch := 0; ch < 4; ch++ {
						sum[ch] += src.pix[si+ch] * wgt
					}
					wsum += wgt
				}
			}
			di := (dy*c.w + dx) * 4
			if wsum > 0 {
				for ch := 0; ch < 4; ch++ {
					c.pix[di+ch] = sum[ch] / wsum
				}
			}
		}
	}
}

// imageData reproduces ctx.getImageData(0, 0, w, h).data: 8-bit un-premultiplied RGBA.
func (c *testCanvas) imageData() []uint8 {
	out := make([]uint8, c.w*c.h*4)
	for i, v := range c.pix {
		if v < 0 {
			v = 0
		} else if v > 255 {
			v = 255
		}
		out[i] = uint8(roundHalfUp(v))
	}
	return out
}

// drawFunc mirrors the TS test's `type Draw = (ctx, w, h) => void`.
type drawFunc func(c *testCanvas, w, h int)

// roundHalfUp is JS Math.round for the non-negative arguments the test uses.
func roundHalfUp(v float64) int { return int(math.Floor(v + 0.5)) }

func maxIntTest(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minIntTest(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// within reproduces the TS test helper of the same name.
func within(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// prepare renders a scene and prepares it EXACTLY the way infrastructure/image-processor.ts does:
// resample so the longer side is at most WorkMaxDim, pack RGBA into a 3-channel Float32 buffer,
// then box-blur at radius max(1, round(BlurFrac * min(w, h))). Keeping this identical to production
// is the point — the detector is only ever fed images built this way, and the Float32 buffer in
// particular is load-bearing (see the comment in image-processor.ts).
func prepare(nativeW, nativeH int, draw drawFunc) (rgb []float32, w, h int) {
	src := newTestCanvas(nativeW, nativeH)
	draw(src, nativeW, nativeH)

	ds := math.Min(math.Min(float64(WorkMaxDim)/float64(nativeW), float64(WorkMaxDim)/float64(nativeH)), 1)
	w = maxIntTest(1, roundHalfUp(float64(nativeW)*ds))
	h = maxIntTest(1, roundHalfUp(float64(nativeH)*ds))
	canvas := newTestCanvas(w, h)
	canvas.drawImageScaled(src, w, h)
	data := canvas.imageData()

	packed := make([]float32, w*h*3)
	for i, p, q := 0, 0, 0; i < w*h; i, p, q = i+1, p+4, q+3 {
		packed[q] = float32(data[p])
		packed[q+1] = float32(data[p+1])
		packed[q+2] = float32(data[p+2])
	}
	radius := maxIntTest(1, roundHalfUp(BlurFrac*float64(minIntTest(w, h))))
	return BoxBlurRGB(packed, w, h, radius), w, h
}

// sceneResult carries the detector result together with the working dimensions so the normalized
// box can be computed.
type sceneResult struct {
	DocumentBoxResult
	w, h int
}

func detectScene(nativeW, nativeH int, draw drawFunc) sceneResult {
	rgb, w, h := prepare(nativeW, nativeH, draw)
	return sceneResult{DocumentBoxResult: DetectDocumentBox(rgb, w, h), w: w, h: h}
}

// normBox is the detected box as fractions of the frame, so results at different resolutions are
// comparable.
type normBox struct{ x, y, w, h float64 }

func normalized(r sceneResult) normBox {
	b := r.Box
	return normBox{
		x: float64(b.Left) / float64(r.w),
		y: float64(b.Top) / float64(r.h),
		w: float64(b.Right-b.Left+1) / float64(r.w),
		h: float64(b.Bottom-b.Top+1) / float64(r.h),
	}
}

// grey puts level v through an exposure change, for the intensity-invariance test.
func grey(v, gain, lift float64) solidPaint {
	c := math.Max(0, math.Min(255, float64(roundHalfUp(v*gain+lift))))
	return solidPaint{testRGB{c, c, c}}
}

func solid(r, g, b float64) solidPaint { return solidPaint{testRGB{r, g, b}} }

// pageOnDarkDesk is a pale page occupying the middle 60% of the frame on a near-black background,
// expressed in FRACTIONS of the frame so it can be rendered at any resolution and any exposure.
func pageOnDarkDesk(gain, lift float64) drawFunc {
	return func(c *testCanvas, w, h int) {
		c.paint = grey(15, gain, lift)
		c.fillRect(0, 0, float64(w), float64(h))
		c.paint = grey(250, gain, lift)
		c.fillRect(0.2*float64(w), 0.2*float64(h), 0.6*float64(w), 0.6*float64(h))
	}
}

func assertWithin(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if !within(got, want, tol) {
		t.Fatalf("%s = %v, want within %v of %v", name, got, want, tol)
	}
}
