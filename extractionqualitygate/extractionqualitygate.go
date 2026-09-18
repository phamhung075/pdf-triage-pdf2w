// Package extractionqualitygate is a Go port of pdf-triage's
// src/domain/extraction-quality-gate.ts (257 lines): the QUALITY_GATE thresholds,
// countMalformedPipeLines, malformedPipeExamples, widestTableHeader, assessChunkMarkdown,
// describeTableRepairNote, assessExtractionQuality and the typed ExtractionQualityGateError, plus
// the QualityGateMetrics / QualityGateFailure / QualityGateReport / ChunkMarkdownReport shapes.
//
// The port reuses, rather than reimplements, the already-ported sibling packages: markdowntables
// (CountCells, AuditMarkdownTables, MeasureContentRecall) and pdftext (IsLikelyCorruptedText,
// ScoreTextQuality). The TypeScript source is the behavioral source of truth.
//
// The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/domain/extraction-quality-gate.test.ts` → 15 passed), so no upstream case is
// pinned red. The deviations below are pure language mechanics, each resolved in favor of matching
// TS exactly:
//
//  1. Whitespace and dots. `\s`/`\S` and `String.prototype.trim()` in the TS file use JavaScript's
//     WhiteSpace+LineTerminator set (NBSP U+00A0, the Unicode space separators, U+2028/U+2029, the
//     BOM U+FEFF, …). Go's regexp `\s` is ASCII-only and strings.TrimSpace follows
//     unicode.White_Space, so jsWhitespaceClass is injected into every regex and jsTrim replaces
//     strings.TrimSpace. For the same reason `.` is the explicit jsDotClass: JS `.` also excludes
//     U+2028/U+2029, while Go's `.` excludes only '\n' and would match a stray '\r'.
//  2. UTF-16 string length. JS `str.length` counts UTF-16 code units, and malformedPipeExamples
//     truncates at 70 by code unit. utf16Len / utf16Slice reproduce that count. The archive corpus
//     is BMP (French/European text), where a code unit is a rune; utf16Slice additionally matches
//     astral text for whole code points, and diverges from JS only for a surrogate pair straddling
//     the cut, which JS would leave as a lone surrogate — unrepresentable in a Go UTF-8 string and
//     unreachable for this corpus.
//  3. Math.round. JS Math.round is half-up-toward-+Infinity for every finite input; Go's
//     math.Round is half-away-from-zero, so the two disagree exactly at tie values. jsRound
//     reproduces the JS rule as floor(x + 0.5).
//  4. Number.prototype.toFixed. Go's strconv rounds exact ties to even, while the ECMAScript spec
//     chooses the larger integer at a tie. jsToFixed2 evaluates the spec on the float64's exact
//     rational value with math/big, so the score string matches Node for every representable
//     double.
//  5. Zero values, null and truthiness. TS `report.failures` / `malformedExamples` are arrays that
//     must stay arrays when empty, so they are initialized to empty slices (JSON `[]`, not null).
//     TS `contentRecall: number | null` maps to *float64 (nil = null). TS `describeTableRepairNote`
//     returning `string | null` maps to a string where "" is the null case (the function can never
//     return a legitimately empty non-null note), following the pdftext Reason-field precedent.
//     `(markdown || ”)` and `(rawText || ”)` collapse made no difference because these parameters
//     are already strings whose Go zero value is "".
//  6. The TS `QUALITY_GATE` object maps to the package-level constants below (the ported test
//     references MdMalformedMinLines instead of QUALITY_GATE.MD_MALFORMED_MIN_LINES).
//  7. The TS `ExtractionQualityGateError extends Error` class becomes an idiomatic Go error struct
//     that still carries Filename and the structured Report; `err instanceof Error` maps to
//     `errors.As(err, &*ExtractionQualityGateError)`.
package extractionqualitygate

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strings"

	"github.com/phamhung075/pdf-triage-pdf2w/markdowntables"
	"github.com/phamhung075/pdf-triage-pdf2w/pdftext"
)

// Pre-registration extraction-quality gate + chunk-targeted repair signals.
//
// WHY THIS GATE EXISTS
// The triage pipeline used to register whatever extraction + Step C markdown produced. Doc 5009
// (2026-09-03) showed the failure mode: a table came back with rows outside the GFM pipes (values
// dropping out of the table entirely) and the pipeline stored it with no error anywhere — the
// integrity audit only sees lines that START with '|', so the malformed rows were invisible.
//
// WHY TWO LAYERS
// 1. While Step C converts, each chunk's output is screened (assessChunkMarkdown /
//    describeTableRepairNote). A chunk whose output has malformed table rows or an absurd column
//    blow-out is re-converted ONCE, alone, with a corrective note — the failing chunk is repaired,
//    the other chunks' output is untouched. Re-running the whole document would waste the model
//    round-trips of every chunk that converted fine, and would re-roll the dice on the healthy ones.
// 2. Before registration (in triage-scan, after classification), the COMPLETE document is assessed
//    (assessExtractionQuality). A hard failure throws ExtractionQualityGateError — a typed,
//    catchable error (mirroring OllamaUnavailableError) so a programmatic caller — the web scan
//    route, an MCP tool, or a local agent that wants to re-fix the file directly — can act on the
//    structured report instead of parsing log lines.
//
// Every threshold is deliberately CONSERVATIVE. The gate exists to stop *obviously unusable*
// content from entering the registry (where it silently corrupts FTS search, summaries and the
// chat assistant), not to reject documents with cosmetic quirks. Thresholds are calibrated on the
// real corpus (recent docs 4991-5009): healthy documents pass with margin, the known-bad shapes
// fail.

// QUALITY_GATE maps the TS `QUALITY_GATE` object (see package comment deviation 6).
const (
	// TextNoiseMinTokens: Text-noise check: ignore short docs; flag prose-quality scores at/below
	// this.
	TextNoiseMinTokens = 50
	TextNoiseMaxScore  = 1.0
	// MdMalformedMinLines: Malformed pipe rows (a line holding `|` cells but not a well-formed GFM
	// row).
	MdMalformedMinLines = 4
	// MdRaggedMinRows / MdRaggedMinDataRows / MdRaggedMaxRatio: Integrity audit: absolute ragged
	// rows OR a large share of a big-enough table.
	MdRaggedMinRows     = 4
	MdRaggedMinDataRows = 6
	MdRaggedMaxRatio    = 0.5
	// MdHeaderlessMinBlocks: Orphan table fragments with no header (tables that never re-joined
	// across chunk boundaries).
	MdHeaderlessMinBlocks = 2
	// MdContentRecallMin: Distinctive-token recall floor when the text is measurable (see
	// markdowntables.MeasureContentRecall).
	MdContentRecallMin = 0.55
	// ChunkWideHeaderCols: A header this wide is a chart/axis blow-out, not a data table (see
	// markdowntables.NeutralizeChartLikeTables).
	ChunkWideHeaderCols = 14
)

// QualityGateMetrics mirrors the TS `QualityGateMetrics` interface.
type QualityGateMetrics struct {
	TextTokens         int
	TextScore          float64
	TextCorrupted      bool
	TableBlocks        int
	DataRows           int
	RaggedRows         int
	HeaderlessBlocks   int
	MalformedPipeLines int
	// ContentRecall is nil where TS reports null (the raw text was not measurable).
	ContentRecall *float64
}

// QualityGateFailure mirrors the TS `QualityGateFailure` interface.
type QualityGateFailure struct {
	// ID is the stable machine-readable id (used in blocked-file records and by agents).
	ID      string
	Message string
}

// QualityGateReport mirrors the TS `QualityGateReport` interface. Failures is a non-nil empty slice
// when the gate passes, matching the TS `[]`.
type QualityGateReport struct {
	Pass     bool
	Failures []QualityGateFailure
	Metrics  QualityGateMetrics
}

// jsWhitespaceClass is the exact character set matched by JavaScript's `\s`: WhiteSpace plus
// LineTerminator, inlined into every regex below (see package comment deviation 1).
const jsWhitespaceClass = `\t\n\v\f\r \x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}`

// jsDotClass is JavaScript's `.`: any character except a line terminator (see package comment
// deviation 1).
const jsDotClass = `[^\n\r\x{2028}\x{2029}]`

var (
	// lineSplitRe is `/\r?\n/`.
	lineSplitRe = regexp.MustCompile(`\r?\n`)
	// gfmRowRe is the TS well-formed-row test `/^\|.*\|\s*$/`.
	gfmRowRe = regexp.MustCompile(`^\|` + jsDotClass + `*\|[` + jsWhitespaceClass + `]*$`)
	// separatorRowRe is the TS separator test `/^\|(?:\s*:?-{2,}:?\s*\|)+\s*$/`, shared by
	// countMalformedPipeLines and widestTableHeader exactly as in the TS source.
	separatorRowRe = regexp.MustCompile(`^\|(?:[` + jsWhitespaceClass + `]*:?-{2,}:?[` + jsWhitespaceClass + `]*\|)+[` + jsWhitespaceClass + `]*$`)
)

// CountMalformedPipeLines counts lines that clearly came from (or should be inside) a GFM table but
// are not well-formed table rows — e.g. doc 5009's
// `Base - 03kVA - du 01/02/26 au 16/05/26 | 9,16 | 31,62 | 20,0%` (pipe cells but no
// leading/trailing pipe) and `**Relevé fin**: Conso kWh | Prix €HT/kWh | ...` (markup glued into a
// header). auditMarkdownTables() cannot see these, yet each one is a row whose values are about to
// drop out of the table.
func CountMalformedPipeLines(markdown string) int {
	lines := splitLines(markdown)
	count := 0
	for _, line := range lines {
		trimmed := jsTrim(line)
		if trimmed == "" {
			continue
		}
		if gfmRowRe.MatchString(trimmed) {
			continue // well-formed GFM row
		}
		if separatorRowRe.MatchString(trimmed) {
			continue // separator row
		}
		if strings.Count(trimmed, "|") >= 2 {
			count++
		}
	}
	return count
}

// MalformedPipeExamples: first example lines of a chunk's malformed output, for the corrective note
// sent back to the model (short — never the whole chunk). TS's `max = 2` default is the explicit
// second argument here; pass 2 for the default behaviour.
func MalformedPipeExamples(markdown string, max int) []string {
	lines := splitLines(markdown)
	seen := []string{}
	for _, line := range lines {
		trimmed := jsTrim(line)
		if trimmed == "" {
			continue
		}
		if gfmRowRe.MatchString(trimmed) {
			continue
		}
		if strings.Count(trimmed, "|") >= 2 {
			if utf16Len(trimmed) > 70 {
				seen = append(seen, utf16Slice(trimmed, 70)+"…")
			} else {
				seen = append(seen, trimmed)
			}
			if len(seen) >= max {
				break
			}
		}
	}
	return seen
}

// WidestTableHeader returns the largest header width among well-formed table blocks (0 when none).
func WidestTableHeader(markdown string) int {
	lines := splitLines(markdown)
	widest := 0
	for i := 0; i < len(lines); i++ {
		line := jsTrim(lines[i])
		if !separatorRowRe.MatchString(line) {
			continue // separator row below a header
		}
		header := ""
		if i > 0 {
			header = jsTrim(lines[i-1])
		}
		if !gfmRowRe.MatchString(header) {
			continue
		}
		cells := markdowntables.CountCells(header)
		if cells > widest {
			widest = cells
		}
	}
	return widest
}

// ChunkMarkdownReport mirrors the TS `ChunkMarkdownReport` interface.
type ChunkMarkdownReport struct {
	MalformedPipeLines int
	MalformedExamples  []string
	WidestTableHeader  int
}

// AssessChunkMarkdown is the per-chunk structural screen run as each chunk converts (cheap, no
// document context needed).
func AssessChunkMarkdown(markdown string) ChunkMarkdownReport {
	return ChunkMarkdownReport{
		MalformedPipeLines: CountMalformedPipeLines(markdown),
		MalformedExamples:  MalformedPipeExamples(markdown, 2),
		WidestTableHeader:  WidestTableHeader(markdown),
	}
}

// DescribeTableRepairNote builds the corrective note for a chunk whose output failed the structural
// screen. It returns "" when the chunk output is structurally fine (nothing to repair); TS returns
// null there (see package comment deviation 5). This note is the single source of truth for both
// the per-chunk retry and the human/agent message.
func DescribeTableRepairNote(markdown string) string {
	report := AssessChunkMarkdown(markdown)
	parts := []string{}
	if report.MalformedPipeLines > 0 {
		examples := ""
		if len(report.MalformedExamples) > 0 {
			examples = ", e.g. `" + strings.Join(report.MalformedExamples, "` / `") + "`"
		}
		parts = append(parts,
			fmt.Sprintf("%d line(s) carry table cells but are NOT well-formed GFM rows%s. ", report.MalformedPipeLines, examples)+
				"Every row of a table must start with `|`, end with `|`, and have EXACTLY as many cells as its header. "+
				"Do not merge separate columns into one cell and do not leave a column label dangling inside a bold line.")
	}
	if report.WidestTableHeader >= ChunkWideHeaderCols {
		parts = append(parts,
			fmt.Sprintf("A table header has %d columns — that is a chart axis or decorative row, not data. ", report.WidestTableHeader)+
				"Do NOT emit it as a table; keep its labels as plain text lines.")
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	return ""
}

// AssessExtractionQuality ports assessExtractionQuality: the full-document pre-registration gate.
func AssessExtractionQuality(rawText string, markdown string) QualityGateReport {
	raw := rawText
	md := markdown

	textQuality := pdftext.ScoreTextQuality(raw)
	textCorrupted := pdftext.IsLikelyCorruptedText(raw)
	audit := markdowntables.AuditMarkdownTables(md)
	recall := markdowntables.MeasureContentRecall(raw, md)
	malformedPipeLines := CountMalformedPipeLines(md)

	failures := []QualityGateFailure{}

	if textQuality.Tokens >= TextNoiseMinTokens && textQuality.Score <= TextNoiseMaxScore {
		failures = append(failures, QualityGateFailure{
			ID: "text-ocr-noise",
			Message: fmt.Sprintf("Extracted text looks like OCR/decoration noise, not words (%d tokens, prose score %s — a real document scores ~4+).",
				textQuality.Tokens, jsToFixed2(textQuality.Score)),
		})
	}
	if textCorrupted {
		failures = append(failures, QualityGateFailure{
			ID:      "text-still-corrupted",
			Message: "Extracted text still shows the mid-word-capitalization corruption symptom of a broken embedded font (ToUnicode CMap).",
		})
	}
	if malformedPipeLines >= MdMalformedMinLines {
		examples := MalformedPipeExamples(md, 2)
		exampleText := ""
		if len(examples) > 0 {
			exampleText = " (e.g. `" + strings.Join(examples, "` / `") + "`)"
		}
		failures = append(failures, QualityGateFailure{
			ID:      "markdown-malformed-rows",
			Message: fmt.Sprintf("%d line(s) carry table cells but are not well-formed GFM rows — their values drop out of the table%s.", malformedPipeLines, exampleText),
		})
	}
	raggedRatio := 0.0
	if audit.DataRows > 0 {
		raggedRatio = float64(audit.RaggedRows) / float64(audit.DataRows)
	}
	if audit.RaggedRows >= MdRaggedMinRows ||
		(audit.DataRows >= MdRaggedMinDataRows && raggedRatio >= MdRaggedMaxRatio) {
		failures = append(failures, QualityGateFailure{
			ID:      "markdown-ragged-table",
			Message: fmt.Sprintf("%d/%d table row(s) (%d%%) have a different cell count than their header — values are shifted into the wrong columns.", audit.RaggedRows, audit.DataRows, jsRound(raggedRatio*100)),
		})
	}
	if audit.HeaderlessBlocks >= MdHeaderlessMinBlocks {
		failures = append(failures, QualityGateFailure{
			ID:      "markdown-headerless-table",
			Message: fmt.Sprintf("%d table block(s) have no header at all (tables that never re-joined across chunk boundaries).", audit.HeaderlessBlocks),
		})
	}
	if recall.Measurable && recall.Recall < MdContentRecallMin {
		failures = append(failures, QualityGateFailure{
			ID:      "markdown-content-loss",
			Message: fmt.Sprintf("Only %d%% of the distinctive source tokens survived into the markdown (%d/%d missing) — values present in the source are absent from the output.", jsRound(recall.Recall*100), recall.MissingTokens, recall.TotalTokens),
		})
	}

	var contentRecall *float64
	if recall.Measurable {
		value := recall.Recall
		contentRecall = &value
	}

	return QualityGateReport{
		Pass:     len(failures) == 0,
		Failures: failures,
		Metrics: QualityGateMetrics{
			TextTokens:         textQuality.Tokens,
			TextScore:          textQuality.Score,
			TextCorrupted:      textCorrupted,
			TableBlocks:        audit.Blocks,
			DataRows:           audit.DataRows,
			RaggedRows:         audit.RaggedRows,
			HeaderlessBlocks:   audit.HeaderlessBlocks,
			MalformedPipeLines: malformedPipeLines,
			ContentRecall:      contentRecall,
		},
	}
}

// ExtractionQualityGateError is thrown when the full-document quality gate fails AFTER the
// automatic single-chunk repair pass. A programmatic caller (web scan route, MCP tool, or a local
// agent re-fixing the source file directly) can catch it and act on Report.Failures / Report.Metrics.
type ExtractionQualityGateError struct {
	Filename string
	Report   QualityGateReport
}

// NewExtractionQualityGateError mirrors the TS `new ExtractionQualityGateError(filename, report)`.
func NewExtractionQualityGateError(filename string, report QualityGateReport) *ExtractionQualityGateError {
	return &ExtractionQualityGateError{Filename: filename, Report: report}
}

// Error renders the same human/agent message as the TS class constructor did.
func (e *ExtractionQualityGateError) Error() string {
	details := make([]string, 0, len(e.Report.Failures))
	for _, f := range e.Report.Failures {
		details = append(details, "- "+f.ID+": "+f.Message)
	}
	return fmt.Sprintf(
		"⛔ Quality gate blocked '%s' before registration (%d issue(s)) after automatic single-chunk repair:\n%s\n"+
			"Re-fix this file locally (extraction quality, OCR or markdown of the offending chunk) and re-scan.",
		e.Filename, len(e.Report.Failures), strings.Join(details, "\n"),
	)
}

// ExtractionQualityGateError implements the error interface (TS `extends Error`).
var _ error = (*ExtractionQualityGateError)(nil)

// splitLines reproduces `(markdown || ”).split(/\r?\n/)`.
func splitLines(markdown string) []string {
	return lineSplitRe.Split(markdown, -1)
}

// jsTrim is `String.prototype.trim()`: it strips exactly the JavaScript whitespace set. Go's
// strings.TrimSpace would additionally strip U+0085 NEL and would leave U+FEFF (see package
// comment deviation 1).
func jsTrim(s string) string {
	return strings.TrimFunc(s, isJSWhitespace)
}

// isJSWhitespace reports whether r is in JavaScript's WhiteSpace+LineTerminator set.
func isJSWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x00A0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// utf16Len is JavaScript's `str.length`: the number of UTF-16 code units, not runes (see package
// comment deviation 2).
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// utf16Slice reproduces `str.slice(0, n)`: the first n UTF-16 code units. A rune whose code units
// would straddle the cut is dropped; JS would keep a lone surrogate there, which a Go UTF-8 string
// cannot represent (see package comment deviation 2).
func utf16Slice(s string, n int) string {
	if n <= 0 {
		return ""
	}
	units := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}

// jsRound reproduces JavaScript's `Math.round`: round half toward +Infinity, i.e. floor(x + 0.5)
// (Go's math.Round is half away from zero — see package comment deviation 3).
func jsRound(x float64) int {
	return int(math.Floor(x + 0.5))
}

// jsToFixed2 reproduces JavaScript's `Number.prototype.toFixed(2)` for the score interpolation. The
// ECMAScript spec picks the larger integer n at an exact tie; evaluating that on the float64's exact
// rational value with math/big matches Node for every representable double, where strconv's
// round-half-to-even would not (see package comment deviation 4).
func jsToFixed2(x float64) string {
	switch {
	case math.IsNaN(x):
		return "NaN"
	case math.IsInf(x, 1):
		return "Infinity"
	case math.IsInf(x, -1):
		return "-Infinity"
	}
	scaled := new(big.Rat).SetFloat64(x)
	scaled.Mul(scaled, big.NewRat(100, 1))
	scaled.Add(scaled, big.NewRat(1, 2))
	// big.Int.Div is Euclidean division (floor for a positive denominator), which is the spec's
	// "pick the larger n" at a tie.
	n := new(big.Int).Div(scaled.Num(), scaled.Denom())
	neg := n.Sign() < 0
	n.Abs(n)
	q, r := new(big.Int).QuoRem(n, big.NewInt(100), new(big.Int))
	out := q.String() + "." + fmt.Sprintf("%02d", r.Int64())
	if neg {
		out = "-" + out
	}
	return out
}
