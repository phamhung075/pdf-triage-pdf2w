// Package imageadjust is a Go port of pdf-triage's src/domain/image-adjust.ts, which was itself
// ported from pdf-awesome's js/domain/auto-adjust.js and js/domain/adjust.js — both already pure,
// framework-agnostic functions with no DOM or canvas dependency. The original validated cases live
// in pdf-awesome/tests/test.js.
//
// The TypeScript source is the behavioral source of truth. One deviation is intentional: TS uses
// Math.round, which rounds half-values toward +Infinity (Math.round(-2.5) === -2), while Go's
// math.Round rounds half-values away from zero (math.Round(-2.5) === -3). autoLevelsFromBlackWhite
// reproduces JS semantics with math.Floor(v+0.5) rather than math.Round so the emitted slider
// values match TS exactly at the .5 boundary.
package imageadjust

import "math"

// AutoAdjustSharpness is the fixed default strength, matching TS's AUTO_ADJUST_SHARPNESS.
const AutoAdjustSharpness = 25

// AutoLevelsClipPct is the fraction of pixels clipped as outliers at each end of the histogram
// before picking the black/white points, so a few stray dark/bright specks (shadows, glare) don't
// skew the stretch.
const AutoLevelsClipPct = 0.01

// AutoLevelsMinRange: black/white points closer than this (0..1 normalized) mean the image is close
// to a single flat tone — nothing safe to stretch, so bail out rather than amplifying noise.
const AutoLevelsMinRange = 0.05

// Levels is the Go equivalent of autoLevelsFromBlackWhite's `{ brightness, contrast }` result.
type Levels struct {
	Brightness int
	Contrast   int
}

// FindBlackWhitePoints scans a grayscale histogram from both ends and returns the value where the
// running pixel count first exceeds clipPct of the total — the darkest/lightest points once the
// tiny outlier tails are ignored. The `cum > clipCount` comparisons are strict in TS and strict
// here, which is what makes the flat-image case land on the single populated bin.
func FindBlackWhitePoints(gray []uint8, clipPct float64) (black, white int) {
	var hist [256]int
	for _, v := range gray {
		hist[v]++
	}
	total := len(gray)
	clipCount := float64(total) * clipPct

	cum := 0
	black = 0
	for v := 0; v < 256; v++ {
		cum += hist[v]
		if float64(cum) > clipCount {
			black = v
			break
		}
	}
	cum = 0
	white = 255
	for v := 255; v >= 0; v-- {
		cum += hist[v]
		if float64(cum) > clipCount {
			white = v
			break
		}
	}
	return black, white
}

// AutoLevelsFromBlackWhite solves for CSS brightness()/contrast() multipliers k1/k2 such that the
// composed transform contrast(brightness(x)) maps black->0 and white->1, then converts those
// multipliers into +/-50 delta sliders (brightness(1 + b/100) / contrast(1 + c/100)).
func AutoLevelsFromBlackWhite(black, white int) Levels {
	l := float64(black) / 255
	h := float64(white) / 255
	if h-l < AutoLevelsMinRange {
		return Levels{Brightness: 0, Contrast: 0}
	}

	sum := l + h
	rng := h - l
	k1 := 1.0
	k2 := 1.0
	if sum > 0.01 {
		k1 = 1 / sum
		k2 = sum / rng
	}

	return Levels{
		Brightness: jsRound(clamp((k1-1)*100, -50, 50)),
		Contrast:   jsRound(clamp((k2-1)*100, -50, 50)),
	}
}

// SharpenPixel applies the blended 3x3 Laplacian/unsharp kernel: identity at amount=0, full sharpen
// at amount=100. Returned as float64 because the composed value is not rounded in TS. The center
// never exceeds 255 or drops below 0.
func SharpenPixel(center, n, s, w, e, amount float64) float64 {
	t := amount / 100
	v := (1+4*t)*center - t*(n+s+w+e)
	return math.Max(0, math.Min(255, v))
}

func clamp(v, min, max float64) float64 {
	return math.Max(min, math.Min(max, v))
}

// jsRound reproduces JS Math.round: floor(v + 0.5). This differs from math.Round for negative
// half-values (see the package comment).
func jsRound(v float64) int {
	return int(math.Floor(v + 0.5))
}
