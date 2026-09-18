package imageadjust

import "testing"

// All cases below are ported verbatim from pdf-triage's src/domain/image-adjust.test.ts, which in
// turn came from pdf-awesome's original validated cases.

func TestFindBlackWhitePoints(t *testing.T) {
	t.Run("clips outlier pixels and lands the black/white points on the real tonal extremes", func(t *testing.T) {
		// 3px each at 0/255 (outliers), 88 midtone at 128, 3px each at 10/245 (the "real" extremes)
		gray := make([]uint8, 100)
		i := 0
		for k := 0; k < 3; k++ {
			gray[i] = 0
			i++
		}
		for k := 0; k < 3; k++ {
			gray[i] = 10
			i++
		}
		for k := 0; k < 88; k++ {
			gray[i] = 128
			i++
		}
		for k := 0; k < 3; k++ {
			gray[i] = 245
			i++
		}
		for k := 0; k < 3; k++ {
			gray[i] = 255
			i++
		}
		black, white := FindBlackWhitePoints(gray, 0.03)
		if black != 10 {
			t.Fatalf("black = %d, want 10", black)
		}
		if white != 245 {
			t.Fatalf("white = %d, want 245", white)
		}
	})

	t.Run("a perfectly flat image has black point = white point", func(t *testing.T) {
		gray := make([]uint8, 50)
		for i := range gray {
			gray[i] = 128
		}
		black, white := FindBlackWhitePoints(gray, 0.01)
		if black != 128 {
			t.Fatalf("black = %d, want 128", black)
		}
		if white != 128 {
			t.Fatalf("white = %d, want 128", white)
		}
	})
}

func TestAutoLevelsFromBlackWhite(t *testing.T) {
	t.Run("already full-range (0..255) needs no adjustment", func(t *testing.T) {
		got := AutoLevelsFromBlackWhite(0, 255)
		if got != (Levels{Brightness: 0, Contrast: 0}) {
			t.Fatalf("got %+v, want {Brightness:0 Contrast:0}", got)
		}
	})

	t.Run("underexposed/low-contrast range brightens and boosts contrast to fill 0..255", func(t *testing.T) {
		got := AutoLevelsFromBlackWhite(26, 204)
		if got != (Levels{Brightness: 11, Contrast: 29}) {
			t.Fatalf("got %+v, want {Brightness:11 Contrast:29}", got)
		}
	})

	t.Run("near-flat range (black ≈ white) bails out rather than amplifying noise", func(t *testing.T) {
		got := AutoLevelsFromBlackWhite(120, 124)
		if got != (Levels{Brightness: 0, Contrast: 0}) {
			t.Fatalf("got %+v, want {Brightness:0 Contrast:0}", got)
		}
	})

	t.Run("an extreme stretch clamps brightness at +50", func(t *testing.T) {
		extreme := AutoLevelsFromBlackWhite(0, 26)
		if extreme.Brightness != 50 {
			t.Fatalf("brightness = %d, want 50", extreme.Brightness)
		}
		if extreme.Contrast != 0 {
			t.Fatalf("contrast = %d, want 0", extreme.Contrast)
		}
	})
}

func TestSharpenPixel(t *testing.T) {
	t.Run("amount=0 leaves the pixel unchanged", func(t *testing.T) {
		if got := SharpenPixel(100, 90, 90, 90, 90, 0); got != 100 {
			t.Fatalf("got %v, want 100", got)
		}
	})

	t.Run("amount=100 boosts a brighter-than-neighbors pixel", func(t *testing.T) {
		if got := SharpenPixel(100, 90, 90, 90, 90, 100); got != 140 {
			t.Fatalf("got %v, want 140", got)
		}
	})

	t.Run("amount=100 darkens a dimmer-than-neighbors pixel", func(t *testing.T) {
		if got := SharpenPixel(100, 110, 110, 110, 110, 100); got != 60 {
			t.Fatalf("got %v, want 60", got)
		}
	})

	t.Run("result clamps at 255", func(t *testing.T) {
		if got := SharpenPixel(250, 0, 0, 0, 0, 100); got != 255 {
			t.Fatalf("got %v, want 255", got)
		}
	})

	t.Run("result clamps at 0", func(t *testing.T) {
		if got := SharpenPixel(5, 255, 255, 255, 255, 100); got != 0 {
			t.Fatalf("got %v, want 0", got)
		}
	})
}

func TestAutoAdjustSharpness(t *testing.T) {
	t.Run("is the fixed default of 25 (no reliable single-photo blur measurement, same as pdf-awesome)", func(t *testing.T) {
		if AutoAdjustSharpness != 25 {
			t.Fatalf("AutoAdjustSharpness = %d, want 25", AutoAdjustSharpness)
		}
	})
}
