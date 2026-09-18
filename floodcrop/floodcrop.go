// Package floodcrop is a Go port of pdf-triage's src/domain/flood-crop.ts (750 lines, zero
// imports): the "edge-snap" document crop detector plus its two admissibility statistics. The
// TypeScript file is pure image geometry with no I/O and no imports, so this port is
// function-for-function: BoxBlurRGB, DetectDocumentBox, IsAdmissibleCrop, SplitFrame,
// AnnulusImpurity and MaterialDistinctness, plus the exported constants WorkMaxDim, BlurFrac and
// MaxAnnulusImpurity.
//
// The TypeScript source is the behavioral source of truth. The deviations below are all resolved in
// favor of matching TS exactly:
//
//  1. Math.round. JS Math.round rounds half-values toward +Infinity; Go's math.Round rounds
//     half-values away from zero. jsRound reproduces the JS rule. Every call site in this file (the
//     barrier colour step and the matched filter's radius) receives a non-negative argument, where
//     the two rules agree, but the helper keeps the port faithful at the boundary.
//  2. Float32 storage. The working image is a Float32Array in TS and a []float32 here, and every
//     intermediate of boxBlurRGB is stored back into one. Reads are widened to float64 for
//     accumulation and narrowed to float32 on store, exactly as the typed-array code does, so the
//     quantisation of the working image has the same shape. Cost, histogram, prefix and profile
//     buffers are Float64Array in TS and []float64 here.
//  3. Division. Every fraction in the detector is a real division in TS: sum/win, c/span,
//     (prefix[hi]-prefix[lo])/(hi-lo), a/iN. Each is written with float64 operands here so Go's
//     integer division never silently truncates one.
//  4. JS null / Infinity. A null box is a nil *Rect; a JS Infinity is math.Inf(1).
//  5. JS `|0` truncation. `(i / w) | 0` on a non-negative index is floor division, which Go's int
//     division reproduces directly.
//  6. No string behaviour. The TS file has zero imports and touches no strings, so the JS
//     whitespace/regex classes, UTF-16 string length and NFD accent folding do not arise. The only
//     JS-vs-Go gaps that do are the numeric ones above.
//
// The upstream TS test suite (src/domain/flood-crop.test.ts, 15 `it` blocks) is GREEN: running
// `npx vitest run src/domain/flood-crop.test.ts --reporter=verbose` reports "Tests 15 passed (15)".
// There is no red case to pin. The ported Go test file also supplies a dependency-free stand-in for
// the @napi-rs/canvas subset the TS suite uses to render its scenes.
package floodcrop

// ===============================================================================================
// WHAT THIS FILE REPLACED, AND WHY — READ THIS BEFORE CHANGING ANYTHING BELOW
// ===============================================================================================
//
// This file used to hold a border-seeded flood fill (floodDocumentBox) gated by three tests: a
// local per-step colour tolerance, a global distance-from-border-average tolerance, and a TEXTURE
// gate built on box-blurred Sobel gradient magnitude. All of it is gone. The whole of it —
// FLOODCROP_LOCAL_TOL, FLOODCROP_GLOBAL_TOL, FLOODCROP_TEXTURE_K, FLOODCROP_TEXTURE_FLOOR,
// FLOODCROP_TEXTURE_BLUR_RADIUS, FLOODCROP_MIN_BLOB, FLOODCROP_MERGE_SIZE_RATIO,
// FLOODCROP_MERGE_X_OVERLAP, computeTextureMap, boxBlurFloat and floodDocumentBox — has been
// deleted and replaced by the edge-snap detector documented under PIPELINE below.
//
// THE BUG. The texture gate's threshold was `max(FLOODCROP_TEXTURE_FLOOR, mean + K*stddev)` of the
// border pixels' gradient magnitude, and FLOODCROP_TEXTURE_FLOOR was an ABSOLUTE gradient value: 6.
// Six sits BELOW the JPEG noise floor of an ordinarily flat region in a real phone photo. On such a
// photo the floor, not the adaptive term, is what binds — and it declares ordinary compression
// noise in the background to be "printed structural detail". The background therefore fails the
// gate immediately, the flood never leaves the frame border, almost nothing is marked background,
// and the "largest unreached component" is nearly the entire frame. Measured against hand-labelled
// ground truth this returned a near-full-frame box on 16 of 16 real photos: no crop at all, dressed
// up as a successful detection.
//
// WHY NO RE-CALIBRATION COULD EVER HAVE FIXED IT. The gate's premise was that the document is MORE
// textured than the background ("a printed page almost always has some structural detail; a
// legitimate background stays flat"). Direct measurement of the same 16-photo corpus says otherwise:
// on 9 of those 16 photos the document INTERIOR is LESS textured than the background it sits on —
// desk grain, wood, fabric, carpet and cloth are all busier than a mostly-white page. On more than
// half the corpus the gate's signal is therefore INVERTED IN SIGN, and a signal that points the
// wrong way on half its inputs cannot be rescued by moving its threshold: any value tight enough to
// catch the photos where it points the right way blocks the background on the photos where it
// points the wrong way, and vice versa. That is exactly the "either missed the passport's faint
// text page, or treated the payslip's textured desk as document" oscillation the old comments
// recorded as a tuning difficulty. It was not a tuning difficulty. It was the wrong feature.
//
// DO NOT REINTRODUCE A TEXTURE GATE. Not with a better blur, not with a percentile instead of a
// stddev, not with a per-photo adaptive floor. Texture does not separate document from background
// in this corpus, in either direction, and the detector below does not need it to: it never asks
// how busy a region is, only whether a path from the frame border to it has to cross a strong
// colour step (stage 3), and whether the two sides of a proposed edge are made of different
// material (stage 7's colour check and admissibility test B).
//
// DOMAIN CONTRACT (unchanged): pure logic, zero I/O, no canvas import. The decode / resample /
// pack half of the original detector's `prepare()` lives in src/infrastructure/image-processor.ts;
// this file exports WORK_MAX_DIM, BLUR_FRAC and boxBlurRGB so infrastructure can build exactly the
// working image the detector expects, and exports detectDocumentBox / isAdmissibleCrop as the two
// halves of the decision. Coordinates in and out are WORKING pixels; mapping back to original
// image pixels is infrastructure's job.
//
// ===============================================================================================
// "edge-snap" document crop detector.
//
// DESIGN CONTRACT: every value that decides "background vs document" is measured on the image
// being processed. The only literals in this file are (a) resource caps and structural constants
// that never participate in a classification, and (b) DIMENSIONLESS conventions - fractions of an
// image dimension, fractions of a population, or a rise-point convention. There is no absolute
// intensity, no absolute gradient magnitude and no absolute pixel count in any decision.
//
// PIPELINE (each stage calibrates itself from what the previous stage measured):
//
//   1. NORMALISE  Decode and resample so every image is analysed at one common working scale.
//                 This is what makes the "fraction of a dimension" radii below mean the same
//                 physical thing on a 12 MP phone photo and on a 0.5 MP scan.
//
//   2. SMOOTH     Box blur at a radius that is a fraction of the working dimension. Print, JPEG
//                 noise and desk grain are sub-radius and collapse into their local surface
//                 colour; a page boundary is a long discontinuity and survives.
//
//   3. BARRIER    The core measurement, and the reason no colour threshold is needed. For every
//                 pixel compute the MINIMAX BARRIER COST: the smallest possible value of "the
//                 largest colour step you must cross" over all paths from a seed border to that
//                 pixel (a priority flood with a bucket queue, popping in non-decreasing cost).
//                 Background - however textured, however gradient-lit, however multi-material,
//                 as long as it reaches the frame border - is reachable without crossing any
//                 strong step, so its cost is low. Anything enclosed by a real boundary has a
//                 cost equal to that boundary's strength. This turns "is it background" into a
//                 single scalar whose scale is set by the photo itself.
//
//   4. LEAK       Where the flood breaks OUT of the background is read off that cost map's own
//                 histogram: sweeping the cost upward, area is absorbed slowly while the flood is
//                 still wandering inside the background, and then the entire document arrives at
//                 once, at the strength of the boundary enclosing it. The threshold sits just
//                 below that arrival. The arrival is measured in a window one octave of cost wide,
//                 which is what makes the reading survive a tone change: brightening or flattening
//                 a photo multiplies every cost by roughly a constant.
//
//   5. ISOLATE    Reduce the above-threshold pixels to the single connected component that best
//                 covers the frame centre - the photographic fact that the subject is at frame
//                 centre, not brightness, hue, or size. This discards speckle, a pen beside the
//                 page, a bright patch of floor, before any of it can pollute a profile.
//
//   6. ARBITRATE  Steps 3-5 are run FIVE times over independent samples of the same photo: one
//                 flood seeded from the whole frame border, and one from each border line alone.
//                 No single seed is always right - a whole-frame seed is spoiled when the document
//                 itself runs off the frame, a single-side seed is spoiled when some other object
//                 covers that border - so the samples compete rather than being chosen by rule.
//
//   7. EDGE SNAP  Each of the four sides is refined independently. For a side, and for each of the
//                 five samples, build a profile of "what fraction of this row/column, across the
//                 current box's span, is document", scan inward from the frame border, and find
//                 the largest SUSTAINED step. Because that filter compares two equally deep bands,
//                 a thin printed rule - which has paper on both sides - produces no step at all,
//                 while a page boundary produces close to a full flip of the composition. The
//                 filter is run at successive octaves of depth, finest first, so a knife-edge and
//                 a long skew ramp are both resolvable without giving up localisation on the easy
//                 case. The side keeps the strongest reading over the five samples, then extends
//                 it outward by hysteresis to the extreme corner of a skewed sheet. A side whose
//                 profile never flips from majority-background to majority-document has no visible
//                 boundary - the document runs off frame there - and is left where it was.
//
//   8. RE-SNAP    Repeat step 7, now measuring each side's profile over the snapped extent rather
//                 than the coarse over-inclusion.
//
// Returns a null box only when no sample found anything covering the middle of the photograph.
// ===============================================================================================

import (
	"math"
	"sort"
)

// --- Resource cap. Never appears in a comparison that classifies a pixel. It bounds run time
// regardless of camera megapixels AND normalises every image to one working scale, which is what
// makes the dimensionless fractions below resolution-independent rather than resolution-dependent.
const WorkMaxDim = 768

// --- Structural constants describing the numeric domain of the barrier cost, not thresholds.
// Colour steps are measured on the SMOOTHED image, whose values are real-valued averages, so they
// are carried at sub-unit precision: COST_QUANT steps per unit of 8-bit intensity. Without this
// the whole low-contrast end of the scale - a pale page on a pale desk, or any photo whose
// contrast has been compressed - collapses into one or two integer levels and stops being
// measurable at all. MAX_COST is the resulting domain size (3 x 255 units, quantised).
const (
	costQuant = 16
	maxCost   = 765 * costQuant
)

// --- DIMENSIONLESS: smoothing radius as a fraction of the shorter working dimension. Sets the
// spatial scale below which detail is "surface texture" rather than "geometry".
const BlurFrac = 0.005

// --- DIMENSIONLESS: side of the central patch used for the "the subject is at frame centre"
// vote, as a fraction of each axis.
const centerFrac = 1.0 / 3.0

// --- DIMENSIONLESS: radius of the step-response matched filter, as a fraction of the axis being
// scanned. A boundary must separate two bands each this deep to count; anything thinner (printed
// rules, staple shadows, fold seams) averages out.
const stepFrac = 0.02

// --- DIMENSIONLESS: coarsest comparison depth in the scale-space search above, as a fraction of
// the axis. A quarter of the axis: beyond that the two bands can no longer both fit between the
// frame border and the middle of the frame, so a deeper filter would be measuring the frame
// rather than the boundary.
const maxStepFrac = 0.25

// --- DIMENSIONLESS: a side is only snapped when its profile step crosses the midpoint of the
// profile's own 0..1 range - i.e. when the row/column genuinely flips from majority-background to
// majority-document. The natural midpoint of a fraction, not a tuned level.
const minStep = 0.5

// --- DIMENSIONLESS: the ratio that defines "one octave", used both for the cost window that
// locates the leak and for the successive depths of the scale-space edge search. A doubling is
// the standard scale-space step; being a pure ratio, it is what carries the multiplicative
// invariance those two searches rely on.
const octave = 2

// --- DIMENSIONLESS: rise-point convention for extending the detected step outward to the extreme
// corner of a skewed sheet, as a fraction of this side's own measured low-to-high profile span.
// The standard 10% rise point.
const riseFrac = 0.1

// Rect is a box in WORKING pixel coordinates, inclusive on all four edges. It mirrors the TS
// `Rect` interface.
type Rect struct {
	Left, Top, Right, Bottom int
}

// Work is the prepared working image. The comment below is preserved verbatim from the TS source:
//
// The prepared working image: blurred RGB, 3 values/px, kept real-valued (see COST_QUANT).
// Built by infrastructure from WORK_MAX_DIM + BLUR_FRAC + boxBlurRGB.
//
// The field is RGB rather than rgb only to satisfy Go's exported-field convention.
type Work struct {
	RGB  []float32
	W, H int
}

// side is the TS `Side` string union.
type side int

const (
	sideTop side = iota
	sideBottom
	sideLeft
	sideRight
)

var allSides = []side{sideTop, sideBottom, sideLeft, sideRight}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// jsRound is JS Math.round: half-values go toward +Infinity. See the package comment.
func jsRound(v float64) float64 { return math.Floor(v + 0.5) }

func jsRoundInt(v float64) int { return int(jsRound(v)) }

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

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

// ---------------------------------------------------------------------------------------- smooth

// BoxBlurRGB ports boxBlurRGB.
func BoxBlurRGB(src []float32, w, h, r int) []float32 {
	tmp := make([]float32, w*h*3)
	out := make([]float32, w*h*3)
	win := float64(2*r + 1)
	for y := 0; y < h; y++ {
		row := y * w
		for c := 0; c < 3; c++ {
			sum := 0.0
			for x := -r; x <= r; x++ {
				sum += float64(src[(row+clampInt(x, 0, w-1))*3+c])
			}
			tmp[row*3+c] = float32(sum / win)
			for x := 1; x < w; x++ {
				sum += float64(src[(row+clampInt(x+r, 0, w-1))*3+c]) - float64(src[(row+clampInt(x-r-1, 0, w-1))*3+c])
				tmp[(row+x)*3+c] = float32(sum / win)
			}
		}
	}
	for x := 0; x < w; x++ {
		for c := 0; c < 3; c++ {
			sum := 0.0
			for y := -r; y <= r; y++ {
				sum += float64(tmp[(clampInt(y, 0, h-1)*w+x)*3+c])
			}
			out[x*3+c] = float32(sum / win)
			for y := 1; y < h; y++ {
				sum += float64(tmp[(clampInt(y+r, 0, h-1)*w+x)*3+c]) - float64(tmp[(clampInt(y-r-1, 0, h-1)*w+x)*3+c])
				out[(y*w+x)*3+c] = float32(sum / win)
			}
		}
	}
	return out
}

// -------------------------------------------------------------------------- minimax barrier map

// For every pixel: the smallest achievable "largest single colour step crossed" over all paths
// from the seeded border(s). Implemented as a priority flood with a bucket queue over the integer
// L1 colour-step domain, so pixels are settled in non-decreasing cost order in O(n) time.
//
// This is the whole background model. It needs no threshold, no reference colour and no texture
// statistic: whatever the background is made of, if it connects to the seeded border without
// crossing a strong colour step it settles cheaply; whatever is enclosed by a real boundary
// settles at that boundary's own strength, expressed in the same units as the image.
func barrierMap(work Work, sides []side) []int32 {
	rgb, w, h := work.RGB, work.W, work.H
	n := w * h
	cost := make([]int32, n)
	for i := range cost {
		cost[i] = maxCost + 1
	}
	buckets := make([][]int32, maxCost+1)

	push := func(c, i int32) {
		buckets[c] = append(buckets[c], i)
	}

	seed := func(i int32) {
		if cost[i] != 0 {
			cost[i] = 0
			push(0, i)
		}
	}
	w32 := int32(w)
	for _, s := range sides {
		switch s {
		case sideTop:
			for x := 0; x < w; x++ {
				seed(int32(x))
			}
		case sideBottom:
			for x := 0; x < w; x++ {
				seed(int32((h-1)*w + x))
			}
		case sideLeft:
			for y := 0; y < h; y++ {
				seed(int32(y * w))
			}
		case sideRight:
			for y := 0; y < h; y++ {
				seed(int32(y*w + w - 1))
			}
		}
	}

	step := func(a, b int32) int32 {
		ai, bi := a*3, b*3
		d := math.Abs(float64(rgb[ai])-float64(rgb[bi])) +
			math.Abs(float64(rgb[ai+1])-float64(rgb[bi+1])) +
			math.Abs(float64(rgb[ai+2])-float64(rgb[bi+2]))
		return int32(jsRound(float64(costQuant) * d))
	}

	for c := 0; c <= maxCost; c++ {
		cc := int32(c)
		relax := func(i, j int32) {
			nc := cc
			if s := step(i, j); s > nc {
				nc = s
			}
			if nc < cost[j] {
				cost[j] = nc
				push(nc, j)
			}
		}
		for k := 0; k < len(buckets[c]); k++ {
			i := buckets[c][k]
			if cost[i] != cc {
				continue
			}
			x := int(i) % w
			y := int(i) / w
			if x > 0 {
				relax(i, i-1)
			}
			if x < w-1 {
				relax(i, i+1)
			}
			if y > 0 {
				relax(i, i-w32)
			}
			if y < h-1 {
				relax(i, i+w32)
			}
		}
		buckets[c] = nil
	}
	return cost
}

// Where does the flood LEAK out of the background and into the document?
//
// Sweeping the barrier cost from zero, area is absorbed slowly while the flood is still wandering
// around inside the background, and then, the instant the cost reaches the strength of the page
// boundary, the ENTIRE document arrives at once - its interior is uniform, so every one of its
// pixels has the same barrier cost as the boundary that encloses it. That single largest arrival
// is the leak, and the threshold sits immediately below it. Nothing here is supplied by the
// author: the operating point is whatever cost this photo happens to leak at.
//
// The arrival is measured in a window that is one OCTAVE OF COST wide - [s, 2s) - slid over every
// possible start s, and the leak is the start whose window catches the most area. The octave width
// is the correct invariance: brightening, darkening or flattening a photo multiplies every colour
// step, and therefore every barrier cost, by roughly a constant. A window defined multiplicatively
// scales with the photo; a window defined in raw cost levels does not. Sliding it, rather than
// snapping it to fixed powers of two, keeps that invariance exact instead of letting a tone change
// shunt an arrival across a bin edge. The octave width also stops a noisy background, whose mass is
// smeared thinly over many adjacent low cost levels, from out-voting a document that arrives
// concentrated at one higher level.
//
// Ties resolve to the LARGEST qualifying start, which is the tightest background consistent with
// the evidence: the threshold ends up immediately below the arrival rather than a long way beneath
// it with empty cost levels in between.
//
// Cost zero is background by definition - reachable from the border across no colour step at all -
// and is therefore never a candidate leak.
func leakThreshold(cost []int32) int {
	hist := make([]float64, maxCost+2)
	for _, v := range cost {
		hist[v]++
	}
	cum := make([]float64, maxCost+3)
	for c := 0; c <= maxCost+1; c++ {
		cum[c+1] = cum[c] + hist[c]
	}
	upTo := func(c int) float64 {
		if c < 0 {
			c = 0
		} else if c > maxCost+2 {
			c = maxCost + 2
		}
		return cum[c]
	}

	bestMass := -1.0
	bestStart := 1
	for s := 1; s <= maxCost; s++ {
		mass := upTo(octave*s) - upTo(s)
		if mass >= bestMass {
			bestMass = mass
			bestStart = s
		}
	}
	return bestStart - 1
}

func maskFromCost(cost []int32, threshold int) []uint8 {
	m := make([]uint8, len(cost))
	for i, v := range cost {
		if int(v) > threshold {
			m[i] = 1
		}
	}
	return m
}

// ------------------------------------------------------------------------------ coarse anchoring

type centerWindowRect struct {
	x0, x1, y0, y1 int
}

func centreWindow(w, h int) centerWindowRect {
	return centerWindowRect{
		x0: int(math.Floor(float64(w) * (0.5 - centerFrac/2))),
		x1: int(math.Ceil(float64(w) * (0.5 + centerFrac/2))),
		y0: int(math.Floor(float64(h) * (0.5 - centerFrac/2))),
		y1: int(math.Ceil(float64(h) * (0.5 + centerFrac/2))),
	}
}

// The document is ONE connected thing that covers the middle of the photograph. Reducing each
// mask to the single component that best covers the frame centre discards everything else the
// leak threshold happened to pick up - speckle on a near-blank desk, a pen lying beside the page,
// a bright patch of floor - before any of it can pollute a row/column profile. It is also the
// only place the "subject is at frame centre" assumption is used, and it is used as a vote over
// an area rather than as a test on a single pixel.
//
// Returns the isolated component and its bounding box, or null when nothing covers the centre at
// all (which is the correct outcome for a seed line that started on the document itself: that
// sample simply has no opinion, and contributes nothing).
//
// The Go port expresses that "null" as ok == false.
func centreComponent(mask []uint8, w, h int) ([]uint8, Rect, bool) {
	n := w * h
	seen := make([]uint8, n)
	stack := make([]int32, n)
	label := make([]int32, n)
	for i := range label {
		label[i] = -1
	}
	win := centreWindow(w, h)

	best := -1
	bestCentre := 0
	var bestBox Rect
	id := 0
	for s := 0; s < n; s++ {
		if mask[s] == 0 || seen[s] != 0 {
			continue
		}
		hd, tl := 0, 0
		stack[tl] = int32(s)
		tl++
		seen[s] = 1
		ax, ay := w, h
		bx, by := -1, -1
		centre := 0
		for hd < tl {
			i := int(stack[hd])
			hd++
			label[i] = int32(id)
			x := i % w
			y := i / w
			if x < ax {
				ax = x
			}
			if x > bx {
				bx = x
			}
			if y < ay {
				ay = y
			}
			if y > by {
				by = y
			}
			if x >= win.x0 && x < win.x1 && y >= win.y0 && y < win.y1 {
				centre++
			}
			if x > 0 && mask[i-1] != 0 && seen[i-1] == 0 {
				seen[i-1] = 1
				stack[tl] = int32(i - 1)
				tl++
			}
			if x < w-1 && mask[i+1] != 0 && seen[i+1] == 0 {
				seen[i+1] = 1
				stack[tl] = int32(i + 1)
				tl++
			}
			if y > 0 && mask[i-w] != 0 && seen[i-w] == 0 {
				seen[i-w] = 1
				stack[tl] = int32(i - w)
				tl++
			}
			if y < h-1 && mask[i+w] != 0 && seen[i+w] == 0 {
				seen[i+w] = 1
				stack[tl] = int32(i + w)
				tl++
			}
		}
		if centre > bestCentre {
			bestCentre = centre
			best = id
			bestBox = Rect{Left: ax, Top: ay, Right: bx, Bottom: by}
		}
		id++
	}
	if best < 0 {
		return nil, Rect{}, false
	}
	out := make([]uint8, n)
	for i := 0; i < n; i++ {
		if int(label[i]) == best {
			out[i] = 1
		}
	}
	return out, bestBox, true
}

// ------------------------------------------------------------------------------------ edge snap

type snapResult struct {
	pos       int
	confident bool
	strength  float64
	depth     int
	stepPos   int
}

// Scans a 0..1 profile inward from index 0 and returns the boundary index. See stage 7 in the
// file header for why each part is shaped the way it is.
func snapSide(profile []float64, limit, axisLen int) snapResult {
	nProf := len(profile)
	prefix := make([]float64, nProf+1)
	for i := 0; i < nProf; i++ {
		prefix[i+1] = prefix[i] + profile[i]
	}
	bandMean := func(lo, hi int) float64 {
		if hi <= lo {
			return 0
		}
		return (prefix[hi] - prefix[lo]) / float64(hi-lo)
	}

	// Scale space, searched FINEST FIRST. A boundary can present as a knife edge (a sheet lying flat,
	// sharply photographed) or as a long ramp (a sheet skewed in plane, a rounded corner, a page
	// fading into a desk of nearly its own colour). One fixed comparison depth answers only one of
	// those. So the same matched filter is run at successive octaves of depth and the search stops
	// at the FIRST - shallowest - depth that resolves the boundary. Coarsening buys sensitivity at
	// the cost of localisation, so it is spent only when the finer scales found nothing, never as a
	// free upgrade on a case that was already sharp.
	bestPos := -1
	bestStep := math.Inf(-1)
	bestR := maxInt(1, jsRoundInt(stepFrac*float64(axisLen)))
	for r := maxInt(1, jsRoundInt(stepFrac*float64(axisLen))); float64(r) <= float64(axisLen)*maxStepFrac; r *= octave {
		hardLimit := minInt(limit, nProf-r)
		pos := -1
		step := math.Inf(-1)
		for i := r; i <= hardLimit; i++ {
			s := bandMean(i, i+r) - bandMean(i-r, i)
			if s > step {
				step = s
				pos = i
			}
		}
		if step > bestStep {
			bestStep = step
			bestPos = pos
			bestR = r
		}
		if step >= minStep {
			bestStep = step
			bestPos = pos
			bestR = r
			break
		}
	}
	if bestPos < 0 || bestStep < minStep {
		return snapResult{pos: 0, confident: false, strength: bestStep, depth: bestR, stepPos: 0}
	}

	lo := median(profile, 0, bestPos)
	hi := median(profile, bestPos, maxInt(bestPos+1, minInt(nProf, limit)))
	thr := lo + riseFrac*(hi-lo)

	pos := bestPos
	for pos > 0 && profile[pos-1] >= thr {
		pos--
	}
	return snapResult{pos: pos, confident: true, strength: bestStep, depth: bestR, stepPos: bestPos}
}

func median(arr []float64, lo, hi int) float64 {
	if hi <= lo {
		return 0
	}
	s := make([]float64, hi-lo)
	copy(s, arr[lo:hi])
	sort.Float64s(s)
	return s[len(s)>>1]
}

func rowProfile(mask []uint8, w, h, x0, x1 int) []float64 {
	out := make([]float64, h)
	span := float64(maxInt(1, x1-x0+1))
	for y := 0; y < h; y++ {
		c := 0
		for x := x0; x <= x1; x++ {
			c += int(mask[y*w+x])
		}
		out[y] = float64(c) / span
	}
	return out
}

func colProfile(mask []uint8, w, h, y0, y1 int) []float64 {
	out := make([]float64, w)
	span := float64(maxInt(1, y1-y0+1))
	for x := 0; x < w; x++ {
		c := 0
		for y := y0; y <= y1; y++ {
			c += int(mask[y*w+x])
		}
		out[x] = float64(c) / span
	}
	return out
}

func reversed(a []float64) []float64 {
	out := make([]float64, len(a))
	for i := range a {
		out[i] = a[len(a)-1-i]
	}
	return out
}

// Does a proposed boundary actually separate two DIFFERENT MATERIALS?
//
// Everything upstream of here reasons about the barrier MASK, and the mask can flip strongly at a
// feature that is not a page edge at all: when the sheet runs off the frame, the sample seeded
// from that side starts on the PAPER, floods up through the paper, and halts at the first strong
// barrier it meets - a stamp, a signature, a band of print. The mask flip there is near-total, so
// the profile filter accepts it, and the honest samples cannot outvote it because they abstain
// (their own profiles never flip at all). That is the bottom edge of 20260727_181149, 11.8% of the
// frame height inside the sheet, cutting through the municipal stamp and the registrar's signature.
//
// Colour is what tells the two apart, and it is the one thing the pipeline never re-checks. Across
// a real page edge the material changes - paper to desk - so the colour step is large. Across a
// phantom interior edge it is paper on both sides, so the colour step is near zero however
// decisively the mask flips.
//
// The scale to compare against is already measured and costs no new constant: leakThreshold is
// literally "the barrier strength of the boundary enclosing this document" in this photo's own
// units. A boundary that is genuinely the page edge steps by about that much; one inside uniform
// paper does not. Both sides of the comparison are in the same COST_QUANT units, so the test is a
// pure ratio and carries the same multiplicative tone-invariance as the leak reading itself.
//
// The step is summarised over the side's span with a MEDIAN, because that population is exactly
// the contaminated kind - part page edge, part whatever else lies along that line.
func boundaryColourStep(work Work, s side, box Rect, pos, depth int) float64 {
	rgb, w, h := work.RGB, work.W, work.H
	vertical := s == sideTop || s == sideBottom
	spanLo, spanHi := box.Left, box.Right
	if !vertical {
		spanLo, spanHi = box.Top, box.Bottom
	}
	axisLen := h
	if !vertical {
		axisLen = w
	}
	inward := 1
	if s == sideBottom || s == sideRight {
		inward = -1
	}
	at := pos
	if s == sideBottom || s == sideRight {
		at = axisLen - 1 - pos
	}

	px := func(line, k int) int {
		c := at + inward*k
		if c < 0 || c >= axisLen {
			return -1
		}
		if vertical {
			return c*w + line
		}
		return line*w + c
	}

	var steps []float64
	for line := spanLo; line <= spanHi; line++ {
		var ir, ig, ib, iN, o0, o1, o2, oN float64
		for k := 0; k < depth; k++ {
			if i := px(line, k); i >= 0 {
				ir += float64(rgb[i*3])
				ig += float64(rgb[i*3+1])
				ib += float64(rgb[i*3+2])
				iN++
			}
			if o := px(line, -1-k); o >= 0 {
				o0 += float64(rgb[o*3])
				o1 += float64(rgb[o*3+1])
				o2 += float64(rgb[o*3+2])
				oN++
			}
		}
		if iN == 0 || oN == 0 {
			continue
		}
		steps = append(steps, math.Abs(ir/iN-o0/oN)+math.Abs(ig/iN-o1/oN)+math.Abs(ib/iN-o2/oN))
	}
	if len(steps) == 0 {
		return 0
	}
	sort.Float64s(steps)
	return float64(costQuant) * steps[len(steps)>>1]
}

// One refinement pass.
//
// Every side is snapped against EVERY available background sample and keeps the reading with the
// strongest step. There is no single seed that is always right: a whole-frame seed is spoiled
// when the document itself runs off the frame (the flood then starts on the paper), and a
// single-side seed is spoiled when some other object happens to cover that border (a desk mat
// along the bottom of the frame). Both failures show up the same way - a weak, ambiguous step -
// while a sample that genuinely sees background right up to the page produces a near-total flip
// of the row/column composition. So the sides arbitrate on evidence rather than on a rule about
// which seed to trust, and each side may end up trusting a different sample.
func refine(work Work, masks [][]uint8, leaks []int, box Rect) Rect {
	w, h := work.W, work.H
	scale := 0
	for _, b := range leaks {
		if b > scale {
			scale = b
		}
	}
	midY := int(math.Floor(float64(box.Top+box.Bottom) / 2))
	midX := int(math.Floor(float64(box.Left+box.Right) / 2))

	// A side keeps the strongest reading among the samples, but only among readings that actually
	// separate two different materials - see boundaryColourStep. A reading whose colour step falls
	// short of the barrier strength enclosing this document is an interior feature, not a page edge.
	best := func(s side, profileOf func([]uint8) []float64, limit, axisLen int) snapResult {
		var out snapResult
		has := false
		for _, m := range masks {
			r := snapSide(profileOf(m), limit, axisLen)
			if r.confident && boundaryColourStep(work, s, box, r.stepPos, r.depth) < float64(scale) {
				continue
			}
			if !has || r.strength > out.strength {
				out = r
				has = true
			}
		}
		if !has {
			return snapResult{pos: 0, confident: false, strength: math.Inf(-1), depth: 1, stepPos: 0}
		}
		return out
	}

	top := best(sideTop, func(m []uint8) []float64 { return rowProfile(m, w, h, box.Left, box.Right) }, midY, h)
	bottom := best(sideBottom, func(m []uint8) []float64 { return reversed(rowProfile(m, w, h, box.Left, box.Right)) }, h-1-midY, h)
	left := best(sideLeft, func(m []uint8) []float64 { return colProfile(m, w, h, box.Top, box.Bottom) }, midX, w)
	right := best(sideRight, func(m []uint8) []float64 { return reversed(colProfile(m, w, h, box.Top, box.Bottom)) }, w-1-midX, w)

	res := box
	if top.confident {
		res.Top = top.pos
	}
	if bottom.confident {
		res.Bottom = h - 1 - bottom.pos
	}
	if left.confident {
		res.Left = left.pos
	}
	if right.confident {
		res.Right = w - 1 - right.pos
	}
	return res
}

// -------------------------------------------------------------- boundary-evidence admissibility
//
// THE FAILURE THIS GUARDS AGAINST
//
// Everything above assumes the photograph contains background. When it does not - a scan, a
// close-up capture, a PDF page render, a screenshot, or simply a second pass over this detector's
// own output - the frame is document edge to edge. The flood is then seeded ON THE PAPER, the page
// interior settles at zero cost, and the only thing left that costs anything to reach is the
// PRINTED CONTENT. The leak reading, the centre component and the per-side profile flips all still
// produce confident, well-formed answers; they are simply answering a different question, and the
// box comes back around an interior text block. That destroys content, and it is not idempotent:
// running the detector over its own output eats the page.
//
// The pipeline cannot see this from the inside, because every quantity it uses is self-scaling.
// When print becomes the boundary, the leak threshold becomes print strength, and every ratio
// measured against it looks exactly as healthy as it did on a real page edge. The evidence has to
// come from the two things the pipeline never asks about: what lies OUTSIDE the proposed box, and
// whether the material out there is actually different from the material inside.
//
// TWO ADMISSIBILITY TESTS. Both are ratios of quantities this photograph has already measured.
// Neither introduces an absolute intensity, an absolute gradient or a pixel count.
//
//   A. BACKGROUND PURITY. If the region outside the box is background then, by the definition of
//      the barrier map, it is what the flood reached CHEAPLY - so almost none of it may sit above
//      that sample's own leak threshold. Whatever is up there is a second enclosed object, and when
//      the frame is full of document that is exactly what the annulus is full of: more print.
//      Read off the RAW above-threshold masks, before centre-component isolation, which exists to
//      throw the annulus away and would destroy this evidence.
//
//   B. MATERIAL DISTINCTNESS. A page edge separates two materials; a printed rule has paper on both
//      sides. So the BULK colour difference between the inside and the annulus must exceed the
//      barrier strength that supposedly separates them. Both sides of that comparison already
//      exist, in the same units, so this test costs no constant at all: `scale` is the leak
//      strength in COST_QUANT units and the bulk difference is a difference of medians in raw
//      intensity, which COST_QUANT converts. A boundary STRONGER than the material difference it
//      claims to divide is not an edge, it is print. Medians on both sides, because both
//      populations are contaminated - the inside by its own print, the outside by whatever else
//      happens to be lying on the desk.
//
// Failing either test means this photograph offers no evidence of a document sitting on anything.
// The honest answer is then no crop at all.

// --- DIMENSIONLESS: the only constant this guard adds. The largest fraction of the region outside
// the box that may sit above the leak threshold and still be called background. A fraction of a
// population, so it is invariant to resolution, tone, exposure and camera; it is not a pixel count,
// an intensity or a gradient.
//
// Calibrated against the STRESSED populations, not the identity ones: measured over a 12-point
// photometric/scale grid (gamma 0.80-0.95, gain 0.85-1.08, downscale 0.4x/0.6x) across both the
// genuine and the fills-frame corpora. Genuine documents-on-a-desk peak at 0.1490 (one photo with a
// second sheet and its shadow left in frame, at gamma 0.85); frames that are entirely document and
// that test B does not already catch bottom out at 0.1850. This value is the geometric centre of
// that window.
//
// Calibrating on the identity images alone gives 0.125, which sits BELOW the genuine population's
// own exposure excursion - at gamma 0.90 or gain 0.85 the guard then fires on a real photo and the
// crop benchmark falls from 0.968/16 to 0.949/15. A threshold must clear the spread the statistic
// shows under ordinary exposure variation, not merely the spread one fixed set of exposures shows.
const MaxAnnulusImpurity = 0.165

// DocumentBoxResult is the Go form of the TS `DocumentBoxResult` interface. The comment below is
// preserved verbatim from the TS source:
//
// The ungated box in WORKING pixel coordinates, plus the two admissibility statistics. Kept
// separate from the decision (isAdmissibleCrop) so the operating point can be swept on the crop and
// fills-frame benches without editing the detector.
//
// A null box is a nil *Rect.
type DocumentBoxResult struct {
	Box          *Rect
	Impurity     float64
	Distinctness float64
}

// DetectDocumentBox is the detector proper. The comment below is preserved verbatim from the TS
// source:
//
// The detector proper. `rgb` is the already-prepared working image: decoded, resampled to at most
// WORK_MAX_DIM on its longer side, packed 3 real-valued channels per pixel, and box-blurred at
// radius max(1, round(BLUR_FRAC * min(w, h))). Infrastructure builds that; see image-processor.ts.
func DetectDocumentBox(rgb []float32, w, h int) DocumentBoxResult {
	none := DocumentBoxResult{Box: nil, Impurity: 0, Distinctness: 0}
	if w < 1 || h < 1 {
		return none
	}
	work := Work{RGB: rgb, W: w, H: h}

	// Five independent background samples of the same photo: one seeded from the whole frame border,
	// and one from each border line on its own. Each is thresholded at its OWN measured leak point
	// and reduced to its own centre-covering component. The RAW above-threshold masks are kept as
	// well - see test A.
	var masks [][]uint8
	var raws [][]uint8
	var boxes []Rect
	var leaks []int
	seedSets := [][]side{allSides, {sideTop}, {sideBottom}, {sideLeft}, {sideRight}}
	for _, seeds := range seedSets {
		c := barrierMap(work, seeds)
		t := leakThreshold(c)
		raw := maskFromCost(c, t)
		cc, box, ok := centreComponent(raw, w, h)
		if ok {
			masks = append(masks, cc)
			raws = append(raws, raw)
			boxes = append(boxes, box)
			leaks = append(leaks, t)
		}
	}
	if len(masks) == 0 {
		return none // nothing covers the middle of the photograph: no crop
	}

	// Anchor: the tightest of the samples' boxes. Each box already contains the frame centre, so the
	// smallest of them is the sample that resolved the most boundary; the sides then refine from
	// there and are free to push any side back out to the frame border.
	coarse := boxes[0]
	for _, b := range boxes[1:] {
		if (b.Right-b.Left)*(b.Bottom-b.Top) < (coarse.Right-coarse.Left)*(coarse.Bottom-coarse.Top) {
			coarse = b
		}
	}

	box := refine(work, masks, leaks, coarse)
	box = refine(work, masks, leaks, box)

	left := clampInt(box.Left, 0, w-1)
	top := clampInt(box.Top, 0, h-1)
	right := clampInt(box.Right, left, w-1)
	bottom := clampInt(box.Bottom, top, h-1)
	rect := Rect{Left: left, Top: top, Right: right, Bottom: bottom}

	// Both admissibility tests read the same interior/annulus partition; building it once halves the
	// guard's cost (it walks the whole frame).
	split := SplitFrame(work, rect)

	return DocumentBoxResult{
		Box:          &rect,
		Impurity:     AnnulusImpurity(split, raws),
		Distinctness: MaterialDistinctness(work, split, leaks),
	}
}

// IsAdmissibleCrop is the admissibility decision. The comment below is preserved verbatim from the
// TS source:
//
// The admissibility decision, kept in one place so production and the unit tests apply exactly the
// same rule. A false verdict on a NON-null box is not "I found nothing" — it is the much stronger
// "this frame IS the document, cropping it would destroy content". Callers must not treat the two
// the same way; see infrastructure/crop-detector.ts.
func IsAdmissibleCrop(r DocumentBoxResult) bool {
	if r.Box == nil {
		return false
	}
	if r.Impurity > MaxAnnulusImpurity {
		return false // the annulus is not background
	}
	if r.Distinctness <= 1 {
		return false // the annulus is the same material as the box
	}
	return true
}

// FrameSplit is the box interior and the annulus outside it. It mirrors the TS anonymous
// `{ inside, outside }` returned by splitFrame.
type FrameSplit struct {
	Inside, Outside []int
}

// SplitFrame splits the frame into the box interior and the annulus outside it. The comment below is
// preserved verbatim from the TS source:
//
// Split the frame into the box interior and the annulus outside it, both eroded by the matched
// filter's own radius so the boundary transition belongs to neither.
func SplitFrame(work Work, box Rect) FrameSplit {
	w, h := work.W, work.H
	band := maxInt(1, jsRoundInt(stepFrac*float64(minInt(w, h))))
	var inside, outside []int
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			inBand :=
				x >= box.Left-band && x <= box.Right+band && y >= box.Top-band && y <= box.Bottom+band &&
					minInt(minInt(absInt(x-box.Left), absInt(x-box.Right)), minInt(absInt(y-box.Top), absInt(y-box.Bottom))) < band
			if inBand {
				continue
			}
			isIn := x >= box.Left && x <= box.Right && y >= box.Top && y <= box.Bottom
			if isIn {
				inside = append(inside, y*w+x)
			} else {
				outside = append(outside, y*w+x)
			}
		}
	}
	return FrameSplit{Inside: inside, Outside: outside}
}

// TEST A. What fraction of the annulus is NOT background?
//
// Every sample is a background hypothesis and they disagree - a seed line that started on the
// document produces a mask that is above threshold nearly everywhere. So the annulus is read off
// the sample that best supports the proposed box: the one whose above-threshold mask most cleanly
// fills the inside and vacates the outside. That is the same evidence-not-rule arbitration the side
// snapping already uses, applied once more to the box as a whole. A single spoiled seed therefore
// cannot condemn a good photograph - and no seed can rescue a bad one, because in a frame that is
// entirely document EVERY seed starts on the paper and every annulus is full of print.
func AnnulusImpurity(split FrameSplit, raws [][]uint8) float64 {
	inside, outside := split.Inside, split.Outside
	if len(outside) == 0 {
		return 0 // the box is the whole frame: cropping is already a no-op
	}
	impurity := 1.0
	bestSupport := math.Inf(-1)
	for _, m := range raws {
		a, b := 0, 0
		for _, i := range inside {
			a += int(m[i])
		}
		for _, i := range outside {
			b += int(m[i])
		}
		rIn := 0.0
		if len(inside) > 0 {
			rIn = float64(a) / float64(len(inside))
		}
		rOut := float64(b) / float64(len(outside))
		if rIn-rOut > bestSupport {
			bestSupport = rIn - rOut
			impurity = rOut
		}
	}
	return impurity
}

// TEST B. How many barrier strengths apart are the two materials?
//
// The bulk colour difference between the inside and the annulus, expressed as a multiple of this
// photograph's own leak strength. Above 1 the two regions differ by more than the boundary that
// divides them - a page on a desk. Below 1 the "boundary" is stronger than the difference it claims
// to separate, which is what a printed line on uniform paper looks like.
func MaterialDistinctness(work Work, split FrameSplit, leaks []int) float64 {
	scale := 0
	for _, b := range leaks {
		if b > scale {
			scale = b
		}
	}
	if scale <= 0 {
		return math.Inf(1)
	}
	inside, outside := split.Inside, split.Outside
	// Fail CLOSED on an empty interior and OPEN on an empty annulus. These two degeneracies are not
	// symmetric: an empty annulus means the box is the whole frame, so cropping is already a no-op and
	// there is nothing to veto; an empty interior means the box collapsed to nothing, the most
	// destructive answer available, and a test that returned Infinity there would wave it through.
	if len(inside) == 0 {
		return 0
	}
	if len(outside) == 0 {
		return math.Inf(1)
	}
	medianColour := func(idx []int) [3]float64 {
		var out [3]float64
		for c := 0; c < 3; c++ {
			a := make([]float64, len(idx))
			for j, i := range idx {
				a[j] = float64(work.RGB[i*3+c])
			}
			sort.Float64s(a)
			out[c] = a[len(a)>>1]
		}
		return out
	}
	mi := medianColour(inside)
	mo := medianColour(outside)
	sep := math.Abs(mi[0]-mo[0]) + math.Abs(mi[1]-mo[1]) + math.Abs(mi[2]-mo[2])
	return (float64(costQuant) * sep) / float64(scale)
}
