// Package pdftext is a Go port of the non-cleanExtractedText exports of pdf-triage's
// src/domain/pdf-text.ts: isUnitLikeToken, detectMidWordCapitalizationCorruption,
// isLikelyCorruptedText, detectThinTextLayer, scoreTextQuality and chooseBestExtraction, plus the
// CORRUPTION_* / THIN_TEXT_* constants and the CorruptionSignal / ThinTextLayerSignal /
// TextQualityMetrics / ExtractionChoice shapes. cleanExtractedText already lives in the sibling
// cleantext package and is deliberately not duplicated here.
//
// The TypeScript source is the behavioral source of truth. The corpus-calibration comments in
// this file (real document ids, measured ratios, calibration thresholds) are preserved verbatim
// from the TS source. Deviations are intentional and limited to language mechanics:
//
//  1. JS `\p{L}` / `\p{Lu}` regular-expression classes map to Go RE2's identically named Unicode
//     classes; Go's unicode.IsUpper is the same Lu category test used by `/\p{Lu}/gu`.
//  2. JS `String.prototype.length` counts UTF-16 code units. Every occurrence here ports to
//     utf8.RuneCountInString (equal for the BMP text this corpus contains) except where the
//     string has already been normalized to ASCII, where byte length is provably equal.
//  3. TS `string | null` reason fields use the empty string for null; TS `undefined`/`|| ”`
//     defaults use the Go zero value.
//  4. Line splitting uses Go's RE2 `\r?\n`, and the thin-text vocabulary regex is ported
//     literally as `[a-z\u00e0-\u00ff]{3,}` rather than a `\p{L}` class, exactly as the TS source
//     writes it.
package pdftext

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// --- Corrupted-text detection (bad embedded font / missing ToUnicode CMap) ---
//
// Symptom (confirmed on real data — doc id 2545, a Vietnamese balance sheet
// whose PDF font has no valid ToUnicode CMap): pdf-parse returns non-empty
// text that LOOKS like real words but individual characters have been
// substituted, producing a single random UPPERCASE letter in the middle of
// an otherwise-lowercase short word — e.g. "BANG cAN oor xf roAN" instead of
// "BẢNG CÂN ĐỐI KẾ TOÁN", "khAu" instead of "khấu", "NguyAn" instead of
// "Nguyễn". Normal French/English/Vietnamese prose essentially never produces
// this pattern except for rare brand names (iPhone, McDonald's) or CamelCase
// word-concatenation artifacts from column/cell extraction (e.g.
// "SociétéGénérale", "polystyrèneTache") — both of which are excluded below.
//
// Calibration (see scratch/calibrate-v6.cjs, run against all 662 documents
// in pdf_triage.db as of 2026-08-12):
//   - A naive whole-document ratio does NOT separate doc 2545 from clean
//     documents: the corruption is localized to a few pages (the balance-
//     sheet tables) inside a 78k-char / ~10.8k-word document, so it gets
//     diluted to a whole-document ratio of ~1%, well within the range of
//     ordinary documents (CamelCase-glued bank statement column headers sit
//     at 2-4%). A SLIDING WINDOW over the word stream is required to find
//     the localized burst.
//   - CamelCase multi-word concatenation (e.g. "polystyrèneTache",
//     "SociétéGénéraleBDDF") produces the same "uppercase not at position 0"
//     symptom but glues together multiple real (usually 8+ char) words.
//     Capping word length at 8 chars removes this false-positive class
//     almost entirely, since the per-character substitution corruption seen
//     in doc 2545 produces short monosyllabic-length garbled tokens.
//   - Requiring EXACTLY ONE uppercase letter (not two or more) additionally
//     filters CamelCase concatenations of 2+ words, which usually carry 2+
//     capitals.
//   - With word length capped at 2-8 chars, a 100-word sliding window
//     (50-word step), and a window flagged only once it has BOTH a ratio
//     >= 0.08 AND at least 6 matching words, doc 2545 comes out on top of
//     the entire corpus (window ratio 0.12, 12/100) with zero genuine false
//     positives among clean documents (their best window never reaches the
//     6-match floor at all). The next-highest scorers that do clear the bar
//     (a Bouygues Telecom invoice with "opÈrateur"/"payÈe" mojibake, and an
//     EDF invoice with letter-by-letter vertical-text extraction) are
//     themselves genuinely corrupted/garbled extractions that legitimately
//     benefit from the same OCR fallback chain.
//
//   - Unit-token exception (real data — doc id 5009, an EDF régularisation
//     invoice triaged 2026-09-03): "kW"/"kWh"/"kVA" legitimately carry exactly
//     one uppercase letter not at position 0 (lowercase SI prefix + uppercase
//     unit), and an energy bill packs them densely enough in its tariff tables
//     and footnotes to cross the window bar (14/100 = 14% in one window). The
//     digital layer of that invoice was CLEAN ("Mlle PALMA BRIGITTE", accents
//     intact), yet it was flagged corrupted, discarded, and replaced by a
//     full-page OCR pass that mangled names, addresses and accents ("MIe PALMA
//     BRI G TTE", "LE GALOI S", "Du lundi aū samedi"). Unlike brand names,
//     units are a repeatable vocabulary, so they are excluded via an explicit
//     allowlist (isUnitLikeToken) BEFORE the windowing step — frequency alone
//     cannot separate them, since a genuinely broken CMap also repeats tokens.

// Corruption* constants are the thresholds calibrated against the 662-document corpus.
const (
	CorruptionWordMinLen     = 2
	CorruptionWordMaxLen     = 8
	CorruptionWindowSize     = 100
	CorruptionWindowStep     = 50
	CorruptionMinWindowWords = 60 // 60% of CorruptionWindowSize — ignore short trailing windows
	CorruptionMinAbsMatches  = 6
	CorruptionMinRatio       = 0.08
)

// unitLikeTokens ports UNIT_LIKE_TOKENS: tokens that legitimately look "mid-word-capitalized"
// but are real SI-unit abbreviations — a lowercase prefix letter (kilo, milli, hecto, …) followed
// by an uppercase unit symbol: kW, kWh, kVA, kPa, hPa, dBm, mA, mSv, …
//
// A genuine per-character CMap corruption never produces these exact words —
// its tokens are mangled source words ("khAu", "cAn", "roAN"), not units — so
// an explicit allowlist removes the energy-invoice false-positive class (EDF
// doc 5009, see calibration notes above) without weakening the symptom
// detector. Keep it exact: a structural rule (e.g. "lowercase prefix + one
// uppercase") would also swallow real corruption tokens like "cAn".
var unitLikeTokens = map[string]struct{}{
	"kW": {}, "kWh": {}, "kVA": {}, "kWc": {}, "kWp": {}, "kVAr": {}, "kVar": {}, "kV": {}, "kB": {},
	"kHz": {}, "kPa": {}, "hPa": {}, "cSt": {}, "dBm": {}, "cGy": {}, "mGy": {}, "mSv": {},
	"mV": {}, "mA": {}, "mW": {}, "mAh": {}, "mΩ": {}, "kΩ": {}, "μA": {}, "μW": {}, "µA": {}, "µW": {},
}

// CorruptionSignal mirrors the TS `CorruptionSignal` interface.
type CorruptionSignal struct {
	Corrupted bool
	// Ratio is the ratio of mid-word-capitalized tokens in the worst window found (0 if no window
	// qualifies).
	Ratio float64
	// MatchCount is the absolute count of mid-word-capitalized tokens in the worst window.
	MatchCount int
	// SampleWords is a few example tokens from the worst window, for debug logging.
	SampleWords []string
}

// IsUnitLikeToken ports isUnitLikeToken: true when `word` is a known unit abbreviation that only
// *looks* corrupted.
func IsUnitLikeToken(word string) bool {
	_, ok := unitLikeTokens[word]
	return ok
}

// letterRe is `/\p{L}+/gu` — the word stream the corruption and quality detectors run over.
var letterRe = regexp.MustCompile(`\p{L}+`)

// isMidWordCapitalized ports isMidWordCapitalized.
func isMidWordCapitalized(word string) bool {
	n := utf8.RuneCountInString(word)
	if n < CorruptionWordMinLen || n > CorruptionWordMaxLen {
		return false
	}
	upperMatches := 0
	firstIsUpper := false
	first := true
	for _, r := range word {
		if first {
			firstIsUpper = unicode.IsUpper(r)
			first = false
		}
		if unicode.IsUpper(r) {
			upperMatches++
		}
	}
	return upperMatches == 1 && !firstIsUpper
}

// DetectMidWordCapitalizationCorruption ports detectMidWordCapitalizationCorruption: detects the
// "bad embedded font / missing ToUnicode CMap" corruption symptom — a localized burst of short
// words with a single stray mid-word capital (e.g. "cAn", "roAN", "khAu"). See the calibration
// notes above.
func DetectMidWordCapitalizationCorruption(text string) CorruptionSignal {
	none := CorruptionSignal{Corrupted: false, Ratio: 0, MatchCount: 0, SampleWords: []string{}}
	if text == "" {
		return none
	}

	words := letterRe.FindAllString(text, -1)
	// Unit abbreviations (kW/kWh/kVA/…) are excluded up front so they count
	// neither as corruption evidence nor as window filler (see calibration notes).
	eligible := make([]string, 0, len(words))
	for _, w := range words {
		l := utf8.RuneCountInString(w)
		if l >= CorruptionWordMinLen && l <= CorruptionWordMaxLen && !IsUnitLikeToken(w) {
			eligible = append(eligible, w)
		}
	}
	if len(eligible) < CorruptionMinWindowWords {
		return none
	}

	best := none
	for i := 0; i < len(eligible); i += CorruptionWindowStep {
		end := i + CorruptionWindowSize
		if end > len(eligible) {
			end = len(eligible)
		}
		window := eligible[i:end]
		if len(window) < CorruptionMinWindowWords {
			continue
		}

		flagged := make([]string, 0, len(window))
		for _, w := range window {
			if isMidWordCapitalized(w) {
				flagged = append(flagged, w)
			}
		}
		ratio := float64(len(flagged)) / float64(len(window))
		if len(flagged) >= CorruptionMinAbsMatches && ratio >= CorruptionMinRatio && ratio > best.Ratio {
			sample := flagged
			if len(sample) > 5 {
				sample = sample[:5]
			}
			best = CorruptionSignal{Corrupted: true, Ratio: ratio, MatchCount: len(flagged), SampleWords: sample}
		}
	}
	return best
}

// IsLikelyCorruptedText ports isLikelyCorruptedText: true when `text` shows the localized
// mid-word-capitalization pattern typical of a PDF font with a broken/missing ToUnicode CMap
// (garbled-but-nonempty text that would otherwise sail past the "< 10 chars" empty-text guard).
func IsLikelyCorruptedText(text string) bool {
	return DetectMidWordCapitalizationCorruption(text).Corrupted
}

// A scanned PDF often still carries a *thin* digital text layer — the scanner app's watermark, a
// page number, a fax header. The extraction gate only asked whether the text was empty or shorter
// than 10 characters, so any of those counted as "usable digital text" and OCR never ran. A real
// example from the archive: an 8-page employment attestation whose entire raw_text was
// "Scanned with AnyScanner" repeated eight times — 198 characters, and none of the document's
// actual content in the registry, the classifier, or the Markdown.
//
// Two independent symptoms, both requiring at least 2 pages. Single-page documents are excluded on
// purpose: a certificate or a cover page is legitimately sparse, and forcing OCR on those would buy
// nothing but OCR time.
const (
	ThinTextMinCharsPerPage = 100
	ThinTextMinPages        = 2
	// ThinTextMaxDistinctLines: at or below this many distinct non-blank lines, a multi-page text
	// layer may be boilerplate.
	ThinTextMaxDistinctLines = 2
	// ThinTextMaxDistinctChars: ...but only if that distinct content is also SHORT. A watermark is a
	// handful of words; a document that genuinely repeats a long line on every page still carries
	// real text, and forcing it through OCR would buy nothing but OCR time.
	ThinTextMaxDistinctChars = 200
	// ThinTextMaxDistinctWordsPerPage: the density rule needs its own version of that guard, or it
	// fires on a document that is simply SHORT rather than un-extracted — and the penalty for a
	// false positive is real: a full pdfjs pass plus up to OCR_MAX_PAGES canvas renders and OCR
	// round-trips, all to re-derive text already in hand. Vocabulary is what separates the two: an
	// un-extracted scan leaks only page furniture (a watermark, "Page 3", a fax header), so its few
	// characters are also the same few words.
	//
	// Measured over the 274 archived documents: the two genuinely starved ones carry 0.4 and 4.8
	// distinct words per page, while normal multi-page documents sit at a 5th-percentile of 18.9.
	// 10 splits that gap with room on both sides.
	ThinTextMaxDistinctWordsPerPage = 10
)

// Thin-text reason values, mirroring the TS `reason` union member strings.
const (
	ReasonLowDensity          = "low-density"
	ReasonRepeatedBoilerplate = "repeated-boilerplate"
)

// ThinTextLayerSignal mirrors the TS `ThinTextLayerSignal` interface.
type ThinTextLayerSignal struct {
	Thin                 bool
	CharsPerPage         float64
	DistinctLines        int
	DistinctWordsPerPage float64
	// Reason says which symptom fired — for the log line, so a human can tell density from
	// boilerplate. Empty string is the Go equivalent of the TS null.
	Reason string
}

var (
	// lineSplitRe is `/\r?\n/`.
	lineSplitRe = regexp.MustCompile(`\r?\n`)
	// distinctWordRe is `/[a-zà-ÿ]{3,}/g` (accent-aware so French text counts as words rather than
	// fragments, and ≥3 letters so page furniture ("p", "de", digits) does not inflate the
	// vocabulary).
	distinctWordRe = regexp.MustCompile("[a-z\u00e0-\u00ff]{3,}")
)

// DetectThinTextLayer ports detectThinTextLayer.
func DetectThinTextLayer(text string, numpages int) ThinTextLayerSignal {
	clean := strings.TrimSpace(text)
	pages := numpages
	if pages < 1 {
		pages = 1
	}
	rawLines := lineSplitRe.Split(clean, -1)
	lines := make([]string, 0, len(rawLines))
	for _, l := range rawLines {
		l = strings.TrimSpace(l)
		if l != "" {
			lines = append(lines, l)
		}
	}
	distinctLines := len(uniqueStrings(lines))
	charsPerPage := float64(utf8.RuneCountInString(clean)) / float64(pages)
	// Accent-aware so French text ("société", "prénom") counts as words rather than fragments, and
	// ≥3 letters so page furniture ("p", "de", digits) does not inflate the vocabulary.
	distinctWords := len(uniqueStrings(distinctWordRe.FindAllString(strings.ToLower(clean), -1)))
	distinctWordsPerPage := float64(distinctWords) / float64(pages)
	none := ThinTextLayerSignal{Thin: false, CharsPerPage: charsPerPage, DistinctLines: distinctLines, DistinctWordsPerPage: distinctWordsPerPage, Reason: ""}

	if pages < ThinTextMinPages {
		return none
	}
	if clean == "" { // empty text is already handled by the plain "< 10 chars" guard
		return none
	}

	// Every page repeating the same one or two SHORT lines is a watermark/header, not content.
	distinctChars := 0
	for _, l := range uniqueStrings(lines) {
		distinctChars += utf8.RuneCountInString(l)
	}
	if lines != nil && len(lines) >= pages &&
		distinctLines <= ThinTextMaxDistinctLines &&
		distinctChars <= ThinTextMaxDistinctChars {
		return ThinTextLayerSignal{Thin: true, CharsPerPage: charsPerPage, DistinctLines: distinctLines, DistinctWordsPerPage: distinctWordsPerPage, Reason: ReasonRepeatedBoilerplate}
	}

	// Sparse AND vocabulary-poor. Both halves are required: a short-but-real document has few
	// characters yet varied words, and dragging it through OCR would cost minutes to learn nothing.
	if charsPerPage < ThinTextMinCharsPerPage &&
		distinctWordsPerPage < ThinTextMaxDistinctWordsPerPage {
		return ThinTextLayerSignal{Thin: true, CharsPerPage: charsPerPage, DistinctLines: distinctLines, DistinctWordsPerPage: distinctWordsPerPage, Reason: ReasonLowDensity}
	}

	return none
}

// --- OCR vs digital-layer arbitration ("keep the better candidate") ---
//
// The corruption guard above routes a flagged digital layer through OCR because OCR is the right
// remedy for a GENUINELY broken ToUnicode CMap. But the guard can misfire (doc id 5009: a clean
// EDF layer dense with SI units), and when it does, blindly overwriting the layer with OCR output
// destroys text that was already perfect — "Mlle PALMA BRIGITTE" became "MIe PALMA BRI G TTE".
// So whenever OCR runs on a corruption-triggered path, the caller arbitrates between the two
// candidates instead of assuming OCR won. These helpers are that arbitration, kept pure so the
// decision is unit-testable without any OCR round-trip.

// TextQualityMetrics mirrors the TS `TextQualityMetrics` interface.
type TextQualityMetrics struct {
	// Score is higher = more prose-like. Roughly average token length minus short-token penalties.
	Score float64
	// Tokens is the number of letter tokens the score was computed over (0 for empty text).
	Tokens         int
	AvgTokenLength float64
	// ShortTokenShare is the share of tokens 1-2 letters long. OCR band/decoration noise is almost
	// all such tokens.
	ShortTokenShare float64
	// SingleLetterShare is the share of tokens that are a single isolated letter.
	SingleLetterShare float64
}

// ScoreTextQuality ports scoreTextQuality: a cheap, language-agnostic "is this text real prose or
// OCR noise?" estimate. It exists only to reject obviously degraded OCR (line noise, isolated
// letters, single-character runs), not to judge translation quality: clean prose scores high,
// while band/decoration noise like
// "S S8 S 5 T S S S8 S S S8 S - 8 Sd S te 0-Z S0 2 S0 e de e" collapses to near zero.
func ScoreTextQuality(text string) TextQualityMetrics {
	tokens := letterRe.FindAllString(text, -1)
	count := len(tokens)
	if count == 0 {
		return TextQualityMetrics{Score: -1, Tokens: 0, AvgTokenLength: 0, ShortTokenShare: 0, SingleLetterShare: 0}
	}
	sum := 0
	for _, t := range tokens {
		sum += utf8.RuneCountInString(t)
	}
	avgTokenLength := float64(sum) / float64(count)
	short := 0
	single := 0
	for _, t := range tokens {
		l := utf8.RuneCountInString(t)
		if l <= 2 {
			short++
		}
		if l == 1 {
			single++
		}
	}
	shortTokenShare := float64(short) / float64(count)
	singleLetterShare := float64(single) / float64(count)
	// Weights calibrated on real samples: clean French prose ~5.0+, the doc-5009 OCR output ~2.5,
	// pure band noise below 1.0.
	score := avgTokenLength - 1.5*singleLetterShare - 0.4*shortTokenShare
	return TextQualityMetrics{
		Score:             score,
		Tokens:            count,
		AvgTokenLength:    avgTokenLength,
		ShortTokenShare:   shortTokenShare,
		SingleLetterShare: singleLetterShare,
	}
}

// ExtractionChoiceReason mirrors the TS `ExtractionChoiceReason` string union.
type ExtractionChoiceReason string

const (
	// ReasonCleanLayerKept: the digital layer passes the corruption detector — never replace it.
	ReasonCleanLayerKept ExtractionChoiceReason = "clean-layer-kept"
	// ReasonOCRUnusable: OCR empty/too short, or still corrupted — keep the original.
	ReasonOCRUnusable ExtractionChoiceReason = "ocr-unusable"
	// ReasonOCRNotProse: OCR output looks like noise, not words — keep the original.
	ReasonOCRNotProse ExtractionChoiceReason = "ocr-not-prose"
	// ReasonOCRCleanRecovery: the layer was genuinely corrupted and OCR recovered prose — prefer OCR.
	ReasonOCRCleanRecovery ExtractionChoiceReason = "ocr-clean-recovery"
)

// ExtractionChoice mirrors the TS `ExtractionChoice` interface.
type ExtractionChoice struct {
	Text   string
	Source string // "digital" or "ocr"
	Reason ExtractionChoiceReason
}

// ChooseBestExtraction ports chooseBestExtraction: decide between the digital text layer and a
// full-page OCR pass of the same document.
//
// The default bias is toward the LAYER: OCR only wins when (a) the layer is actually flagged
// corrupted, (b) the OCR output is not itself corrupted, and (c) the OCR output looks like real
// words rather than noise. Any other outcome keeps the original text — the harm of keeping a
// genuinely corrupted layer (a later repair can re-OCR it) is smaller than the harm of replacing
// a readable layer with OCR garbage, which is unrecoverable without the source file.
func ChooseBestExtraction(originalText, ocrText string) ExtractionChoice {
	original := strings.TrimSpace(originalText)
	ocr := strings.TrimSpace(ocrText)

	if ocr == "" || utf8.RuneCountInString(ocr) < 10 {
		return ExtractionChoice{Text: original, Source: "digital", Reason: ReasonOCRUnusable}
	}
	if !IsLikelyCorruptedText(original) {
		// The (unit-aware) detector cleared the layer. Whatever OCR says, a layer this clean must not
		// be replaced — this is the doc-5009 false-positive class caught at the source.
		return ExtractionChoice{Text: original, Source: "digital", Reason: ReasonCleanLayerKept}
	}
	if IsLikelyCorruptedText(ocr) {
		return ExtractionChoice{Text: original, Source: "digital", Reason: ReasonOCRUnusable}
	}

	quality := ScoreTextQuality(ocr)
	proseLike := quality.AvgTokenLength >= 3.0 &&
		quality.SingleLetterShare <= 0.4 &&
		quality.ShortTokenShare <= 0.6
	if !proseLike {
		return ExtractionChoice{Text: original, Source: "digital", Reason: ReasonOCRNotProse}
	}
	return ExtractionChoice{Text: ocr, Source: "ocr", Reason: ReasonOCRCleanRecovery}
}

// uniqueStrings reproduces `Array.from(new Set(values))`: same-value de-duplication that preserves
// first-seen order.
func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
