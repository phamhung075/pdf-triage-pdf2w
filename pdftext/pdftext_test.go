package pdftext

import (
	"math"
	"strings"
	"testing"
	"unicode"
)

// All cases below are ported verbatim from pdf-triage's src/domain/pdf-text.test.ts (the
// cleanExtractedText describe block is not in the current file; that function already lives in
// the sibling cleantext package). The real corpus fixtures live in fixtures_test.go.
// The TypeScript source is the behavioral source of truth.

func closeTo(got, want float64, digits int) bool {
	return math.Abs(got-want) < 0.5*math.Pow(10, float64(-digits))
}

func repeatLines(line string, n int, sep string) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = line
	}
	return strings.Join(parts, sep)
}

func TestIsLikelyCorruptedTextRealDataCalibration(t *testing.T) {
	t.Run("flags the real garbled excerpt from doc 2545 (bad font ToUnicode CMap)", func(t *testing.T) {
		if !IsLikelyCorruptedText(corruptedBalanceSheetExcerpt) {
			t.Fatal("IsLikelyCorruptedText(corrupted excerpt) = false, want true")
		}
	})

	t.Run("does NOT flag a clean French administrative letter (doc 2503)", func(t *testing.T) {
		if IsLikelyCorruptedText(cleanFrenchLetterExcerpt) {
			t.Fatal("IsLikelyCorruptedText(clean French letter) = true, want false")
		}
	})

	t.Run("does NOT flag a real bank statement despite heavy CamelCase word-concatenation (doc 3080)", func(t *testing.T) {
		// This is the critical false-positive trap: pdf-parse glues table cells
		// together with no spaces ("SociétéGénérale", "RELEVÉDECOMPTE"), which
		// also produces "uppercase not at position 0" but is a completely
		// different, harmless extraction artifact that must not trigger OCR.
		if IsLikelyCorruptedText(cleanBankStatementExcerpt) {
			t.Fatal("IsLikelyCorruptedText(clean bank statement) = true, want false")
		}
	})

	t.Run("does NOT flag a clean short administrative certificate (doc 2548)", func(t *testing.T) {
		if IsLikelyCorruptedText(cleanCertificateExcerpt) {
			t.Fatal("IsLikelyCorruptedText(clean certificate) = true, want false")
		}
	})

	t.Run("does NOT flag a clean EDF invoice whose digital layer is dense with SI-unit tokens (doc 5009 regression)", func(t *testing.T) {
		// kW/kWh/kVA legitimately carry one uppercase not at position 0. Before the
		// isUnitLikeToken allowlist this CLEAN layer was flagged corrupted, the
		// digital text discarded and full-page OCR run over it — which mangled the
		// very content the layer had stored perfectly ("Mlle PALMA BRIGITTE" →
		// "MIe PALMA BRI G TTE", "Du lundi au samedi" → "aū samedi").
		if IsLikelyCorruptedText(cleanEdfUnitsExcerpt) {
			t.Fatal("IsLikelyCorruptedText(clean EDF units) = true, want false")
		}
	})

	t.Run("returns false for empty or very short text (insufficient signal, handled by the separate <10-char guard)", func(t *testing.T) {
		if IsLikelyCorruptedText("") {
			t.Fatal("IsLikelyCorruptedText(\"\") = true, want false")
		}
		if IsLikelyCorruptedText("short text") {
			t.Fatal("IsLikelyCorruptedText(\"short text\") = true, want false")
		}
	})

	t.Run("does not false-positive on a rare, isolated brand-name mid-capital dropped into real clean prose", func(t *testing.T) {
		// "iPhone" is itself a mid-word-capitalized token, but ONE mention inside
		// a large body of normal text must not push any 100-word window over the
		// ratio/absolute-count bar. (A brand name repeated unnaturally often in
		// a tight cluster legitimately would cross the bar — that's not the
		// "rare mention" case this exception is meant to protect.)
		text := cleanFrenchLetterExcerpt + " " + cleanCertificateExcerpt + " " +
			"Il a acheté un iPhone chez McDonald la semaine dernière."
		if IsLikelyCorruptedText(text) {
			t.Fatal("IsLikelyCorruptedText(clean prose + rare brand) = true, want false")
		}
	})
}

func TestDetectMidWordCapitalizationCorruption(t *testing.T) {
	t.Run("reports a non-zero ratio, an absolute match count, and sample tokens for the corrupted excerpt", func(t *testing.T) {
		signal := DetectMidWordCapitalizationCorruption(corruptedBalanceSheetExcerpt)
		if !signal.Corrupted {
			t.Fatal("corrupted = false, want true")
		}
		if !(signal.Ratio >= 0.08) {
			t.Fatalf("ratio = %v, want >= 0.08", signal.Ratio)
		}
		if !(signal.MatchCount >= 6) {
			t.Fatalf("matchCount = %d, want >= 6", signal.MatchCount)
		}
		if len(signal.SampleWords) == 0 {
			t.Fatal("sampleWords = empty, want non-empty")
		}
		// Every sample word must itself carry exactly one uppercase letter not at position 0.
		for _, word := range signal.SampleWords {
			upperCount := 0
			for _, r := range word {
				if unicode.IsUpper(r) {
					upperCount++
				}
			}
			if upperCount != 1 {
				t.Fatalf("sample word %q has %d uppercase letters, want 1", word, upperCount)
			}
			first := firstRune(word)
			if unicode.IsUpper(first) {
				t.Fatalf("sample word %q starts uppercase, want not", word)
			}
		}
	})

	t.Run("reports corrupted: false with zero ratio/matchCount for clean text", func(t *testing.T) {
		signal := DetectMidWordCapitalizationCorruption(cleanFrenchLetterExcerpt)
		if signal.Corrupted {
			t.Fatal("corrupted = true, want false")
		}
		if signal.Ratio != 0 {
			t.Fatalf("ratio = %v, want 0", signal.Ratio)
		}
		if signal.MatchCount != 0 {
			t.Fatalf("matchCount = %d, want 0", signal.MatchCount)
		}
		if len(signal.SampleWords) != 0 {
			t.Fatalf("sampleWords = %v, want empty", signal.SampleWords)
		}
	})
}

func TestIsUnitLikeToken(t *testing.T) {
	t.Run("recognises lowercase-prefix SI unit abbreviations that only look mid-word-capitalized", func(t *testing.T) {
		for _, token := range []string{"kW", "kWh", "kVA", "kVAr", "dBm", "kPa", "hPa", "mA", "mV", "mSv", "kB"} {
			if !IsUnitLikeToken(token) {
				t.Fatalf("IsUnitLikeToken(%q) = false, want true", token)
			}
		}
	})

	t.Run("does not recognise real corruption tokens or brand names", func(t *testing.T) {
		// These must keep flowing into the corruption detector: a broken CMap
		// produces mangled source words, never exact unit abbreviations.
		for _, token := range []string{"khAu", "cAn", "roAN", "iPhone"} {
			if IsUnitLikeToken(token) {
				t.Fatalf("IsUnitLikeToken(%q) = true, want false", token)
			}
		}
	})
}

func TestScoreTextQuality(t *testing.T) {
	t.Run("scores clean prose far above OCR band/decoration noise", func(t *testing.T) {
		clean := ScoreTextQuality(cleanDigitalLayerExcerpt).Score
		noise := ScoreTextQuality(ocrBandNoise).Score
		if !(clean > noise+2) {
			t.Fatalf("clean score = %v, want > noise score %v + 2", clean, noise)
		}
	})

	t.Run("reports zeros for empty text", func(t *testing.T) {
		m := ScoreTextQuality("")
		if m.Tokens != 0 {
			t.Fatalf("tokens = %d, want 0", m.Tokens)
		}
		if m.Score != -1 {
			t.Fatalf("score = %v, want -1", m.Score)
		}
	})
}

func TestChooseBestExtractionOCRMustNeverBlindlyOverwriteTheDigitalLayer(t *testing.T) {
	t.Run("keeps a layer the corruption detector clears, whatever OCR says (doc-5009 class)", func(t *testing.T) {
		choice := ChooseBestExtraction(cleanDigitalLayerExcerpt, "Mle PALMA BRI G TTE 14 boulevard Trufheme")
		if choice.Source != "digital" {
			t.Fatalf("source = %q, want digital", choice.Source)
		}
		if choice.Reason != ReasonCleanLayerKept {
			t.Fatalf("reason = %q, want %q", choice.Reason, ReasonCleanLayerKept)
		}
		if choice.Text != cleanDigitalLayerExcerpt {
			t.Fatalf("text = %q, want the original layer", choice.Text)
		}
	})

	t.Run("keeps the original when OCR is empty or too short to be useful", func(t *testing.T) {
		choice := ChooseBestExtraction(corruptedBalanceSheetExcerpt, "  ")
		if choice.Source != "digital" {
			t.Fatalf("source = %q, want digital", choice.Source)
		}
		if choice.Reason != ReasonOCRUnusable {
			t.Fatalf("reason = %q, want %q", choice.Reason, ReasonOCRUnusable)
		}
	})

	t.Run("keeps the original when the OCR pass itself is still corrupted", func(t *testing.T) {
		choice := ChooseBestExtraction(corruptedBalanceSheetExcerpt, corruptedBalanceSheetExcerpt)
		if choice.Source != "digital" {
			t.Fatalf("source = %q, want digital", choice.Source)
		}
		if choice.Reason != ReasonOCRUnusable {
			t.Fatalf("reason = %q, want %q", choice.Reason, ReasonOCRUnusable)
		}
	})

	t.Run("keeps the original when OCR recovered only band noise, not words", func(t *testing.T) {
		choice := ChooseBestExtraction(corruptedBalanceSheetExcerpt, ocrBandNoise)
		if choice.Source != "digital" {
			t.Fatalf("source = %q, want digital", choice.Source)
		}
		if choice.Reason != ReasonOCRNotProse {
			t.Fatalf("reason = %q, want %q", choice.Reason, ReasonOCRNotProse)
		}
	})

	t.Run("prefers OCR when the layer is genuinely corrupted and OCR recovered real prose", func(t *testing.T) {
		cleanOcr := "Recovered clean text from the rendered page facture de regularisation EDF"
		choice := ChooseBestExtraction(corruptedBalanceSheetExcerpt, cleanOcr)
		if choice.Source != "ocr" {
			t.Fatalf("source = %q, want ocr", choice.Source)
		}
		if choice.Reason != ReasonOCRCleanRecovery {
			t.Fatalf("reason = %q, want %q", choice.Reason, ReasonOCRCleanRecovery)
		}
		if choice.Text != cleanOcr {
			t.Fatalf("text = %q, want the OCR text", choice.Text)
		}
	})
}

func TestDetectThinTextLayer(t *testing.T) {
	t.Run("flags the real-world scanner-watermark case: 8 pages of one repeated line", func(t *testing.T) {
		text := repeatLines("Scanned with AnyScanner", 8, "\n\n")
		signal := DetectThinTextLayer(text, 8)
		if !signal.Thin {
			t.Fatal("thin = false, want true")
		}
		if signal.Reason != ReasonRepeatedBoilerplate {
			t.Fatalf("reason = %q, want %q", signal.Reason, ReasonRepeatedBoilerplate)
		}
		if signal.DistinctLines != 1 {
			t.Fatalf("distinctLines = %d, want 1", signal.DistinctLines)
		}
	})

	t.Run("flags a multi-page document whose text layer is far too sparse to be content", func(t *testing.T) {
		signal := DetectThinTextLayer("Page 1\nPage 2\nPage 3\nSome stray header text", 4)
		if !signal.Thin {
			t.Fatal("thin = false, want true")
		}
		if signal.Reason != ReasonLowDensity {
			t.Fatalf("reason = %q, want %q", signal.Reason, ReasonLowDensity)
		}
	})

	t.Run("leaves a normal multi-page document alone", func(t *testing.T) {
		page := strings.Repeat("Bulletin de salaire. ", 40) // ~840 chars of real content per page
		signal := DetectThinTextLayer(strings.Join([]string{page, page, page}, "\n"), 3)
		if signal.Thin {
			t.Fatal("thin = true, want false")
		}
		if signal.Reason != "" {
			t.Fatalf("reason = %q, want empty", signal.Reason)
		}
	})

	t.Run("never flags a single-page document — a sparse certificate is legitimate", func(t *testing.T) {
		if DetectThinTextLayer("Attestation", 1).Thin {
			t.Fatal("Attestation: thin = true, want false")
		}
		if DetectThinTextLayer("x", 1).Thin {
			t.Fatal("x: thin = true, want false")
		}
	})

	t.Run("does not flag empty text — the existing \"< 10 chars\" guard already covers that", func(t *testing.T) {
		if DetectThinTextLayer("", 5).Thin {
			t.Fatal("empty: thin = true, want false")
		}
		if DetectThinTextLayer("   \n  ", 5).Thin {
			t.Fatal("whitespace: thin = true, want false")
		}
	})

	t.Run("does not treat a few long distinct lines as boilerplate", func(t *testing.T) {
		// 2 pages, 3 distinct substantial lines — above the density floor, so real content.
		text := strings.Join([]string{strings.Repeat("A", 200), strings.Repeat("B", 200), strings.Repeat("C", 200)}, "\n")
		if DetectThinTextLayer(text, 2).Thin {
			t.Fatal("thin = true, want false")
		}
	})
}

func TestDetectThinTextLayerBoilerplateRequiresShortRepeatedContent(t *testing.T) {
	t.Run("does not flag a document that repeats a long line on every page", func(t *testing.T) {
		longLine := strings.Repeat("Conditions generales de vente applicables au present contrat. ", 6) // ~370 chars
		signal := DetectThinTextLayer(strings.Join([]string{longLine, longLine, longLine}, "\n"), 3)
		if signal.Thin {
			t.Fatal("thin = true, want false")
		}
	})

	t.Run("still flags a short watermark repeated on every page", func(t *testing.T) {
		signal := DetectThinTextLayer(repeatLines("Scanned with CamScanner", 12, "\n"), 12)
		if !signal.Thin {
			t.Fatal("thin = false, want true")
		}
		if signal.Reason != ReasonRepeatedBoilerplate {
			t.Fatalf("reason = %q, want %q", signal.Reason, ReasonRepeatedBoilerplate)
		}
	})
}

func TestDetectThinTextLayerDensityRequiresVocabularyPoorText(t *testing.T) {
	t.Run("spares a short but genuinely real 2-page document", func(t *testing.T) {
		// Under the density floor over 2 pages, but carrying the varied vocabulary of a real document
		// (~16 distinct words per page, comfortably above the 5th-percentile of 18.9 measured across
		// the archive's normal multi-page documents). Running OCR here would cost minutes to re-derive
		// text already in hand.
		text := "attestation employeur salarie poste technicien contrat duree signature adresse " +
			"siret ville fonction brute nette essai avenant cadre statut heures jours conges " +
			"prime bureau"
		signal := DetectThinTextLayer(text, 2) // 85.5 chars/page, 11.5 distinct words/page
		if !(signal.CharsPerPage < 100) {      // density alone would have flagged it
			t.Fatalf("charsPerPage = %v, want < 100", signal.CharsPerPage)
		}
		if signal.Thin {
			t.Fatal("thin = true, want false")
		}
	})

	t.Run("still flags a sparse page-furniture-only text layer", func(t *testing.T) {
		signal := DetectThinTextLayer("Page 1\n\nPage 2\n\nPage 3\n\nPage 4", 4)
		if !signal.Thin {
			t.Fatal("thin = false, want true")
		}
		if signal.Reason != ReasonLowDensity {
			t.Fatalf("reason = %q, want %q", signal.Reason, ReasonLowDensity)
		}
	})

	t.Run("reports distinctWordsPerPage so the log line can explain the decision", func(t *testing.T) {
		signal := DetectThinTextLayer(repeatLines("Scanned with AnyScanner", 8, "\n"), 8)
		if !closeTo(signal.DistinctWordsPerPage, 3.0/8.0, 5) {
			t.Fatalf("distinctWordsPerPage = %v, want %v", signal.DistinctWordsPerPage, 3.0/8.0)
		}
	})
}

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}
