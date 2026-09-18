// Package markdowntables is a Go port of pdf-triage's src/domain/markdown-tables.ts (619 lines,
// zero imports). It ports, function-for-function: countCells, auditMarkdownTables,
// measureContentRecall, neutralizeChartLikeTables, normalizeMalformedPipeRows,
// mergeHeaderlessContinuationBlocks, reattachHeadingSplitTableRows and
// restoreMissingTableHeaderCells, plus their exported constants.
//
// The TypeScript source is the behavioral source of truth. Two stdlib mismatches were resolved in
// favor of matching TS exactly rather than the obvious Go primitive:
//
//  1. Whitespace. Every `\s`, `\S` and `String.prototype.trim()` in the TS file uses JavaScript's
//     WhiteSpace+LineTerminator set, which includes NBSP (U+00A0), the Unicode space separators
//     (U+1680, U+2000-U+200A, U+202F, U+205F, U+3000), U+2028/U+2029 and the BOM (U+FEFF). Go's
//     regexp `\s`/`\S` is ASCII-only (omitting those) and strings.TrimSpace follows
//     unicode.White_Space (including U+0085 NEL and omitting U+FEFF). To keep the TS acceptance
//     bar exact, jsWhitespaceClass is injected into every regex and jsTrim replaces
//     strings.TrimSpace. The same reason makes `.*` an explicit jsDotClass: JS `.` also excludes
//     U+2028/U+2029, while Go `.` excludes only '\n' and would match a lone '\r'.
//  2. String length. JS `str.length` counts UTF-16 code units, not runes, and the recall
//     heuristics threshold token lengths at 15 and divide the raw length by the token count.
//     utf16Len reproduces that count so the measured shares match the TS result.
//
// One upstream test is RED and is handled deliberately, not silently adjusted. The TS case
// "leaves a block whose first cells are values alone (extra width is trailing, not a lost label
// column)" (src/domain/markdown-tables.test.ts:542-554) expects restoredHeaders == 0, but the TS
// implementation returns 1 and pads the header. Its fixture ("Salaire"/"Prime" under
// "| Libellé | Montant |") contradicts both the test title and the function's own documented
// signature ("whose FIRST cell (every row) is NOT a value, [whose] last cell (every row) is a pure
// value"), which the TS gates therefore accept. `vitest run src/domain/markdown-tables.test.ts`
// reports 41 passed | 1 failed on exactly that case. No deterministic local rule pads the intended
// doc-5009 shape while refusing this structurally identical one, so this port follows the TS
// IMPLEMENTATION (the behavioral source of truth, per the canonicalpath/taxonomy precedent) and
// documents the discrepancy in the ported test.
package markdowntables

import (
	"fmt"
	"regexp"
	"strings"
)

// Table integrity checks for Step C's assembled Markdown.
//
// A GFM row with fewer cells than its header does not leave the trailing column blank — it shifts
// every value one heading to the left. A Bouygues call-detail table came back as
//   | Date | Heure | Numéro appelé | Unité(s) décomptée(s) | Coût € TTC* |
//   | 12/08 | 11:53:37 | 336528710 | 0,00 |
// filing each call's cost under "Unité(s) décomptée(s)" for 33 of 35 rows. That is wrong data, not
// wrong formatting, and nothing in the pipeline noticed: the only way to find it was to audit the
// database afterwards.
//
// This module only MEASURES. It deliberately does not repair: which cell is missing is undecidable
// from the output alone, so padding by guess would assign a figure a meaning the source never gave
// it — precisely what rule 2b of prompts/micro_prompt_markdown.md forbids. Repair belongs to the
// model (rule 2c), and this is the check that says whether the model complied.

const (
	// jsWhitespaceClass is the exact character set matched by JavaScript's `\s`: WhiteSpace plus
	// LineTerminator. It is inlined into every regex below; Go's `\s` would silently drop NBSP,
	// U+2028/29, the Unicode spaces and U+FEFF (see the package comment).
	jsWhitespaceClass = `\t\n\v\f\r \x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}`
	// jsDotClass is JavaScript's `.`: any character except a line terminator. Go's `.` excludes
	// only '\n', so it would also match a lone '\r' or U+2028/U+2029 left inside a line.
	jsDotClass = `[^\n\r\x{2028}\x{2029}]`
)

var (
	tableRowRe       = regexp.MustCompile(`^[` + jsWhitespaceClass + `]*\|` + jsDotClass + `*\|[` + jsWhitespaceClass + `]*$`)
	tableSeparatorRe = regexp.MustCompile(`^[` + jsWhitespaceClass + `]*\|[` + jsWhitespaceClass + `:|-]+\|[` + jsWhitespaceClass + `]*$`)
	lineSplitRe      = regexp.MustCompile(`\r?\n`)
	contentTokenRe   = regexp.MustCompile(`[a-zà-ÿ]{6,}|\d[\d.,]{2,}`)
	camelCaseRe      = regexp.MustCompile(`[a-zà-ÿ][A-ZÀ-Ý]`)
	jsWhitespaceRe   = regexp.MustCompile(`[` + jsWhitespaceClass + `]+`)
	wellFormedRowRe  = regexp.MustCompile(`^\|` + jsDotClass + `*\|[` + jsWhitespaceClass + `]*$`)
	// separatorRowRe is the normalizer's stricter separator check, distinct from tableSeparatorRe.
	separatorRowRe     = regexp.MustCompile(`^\|(?:[` + jsWhitespaceClass + `]*:?-{2,}:?[` + jsWhitespaceClass + `]*\|)+[` + jsWhitespaceClass + `]*$`)
	pipeMarkupPrefixRe = regexp.MustCompile(`^[>#*+\-]|^\d+\.`)
	headingLineRe      = regexp.MustCompile(`^#{1,6}[` + jsWhitespaceClass + `]+[^` + jsWhitespaceClass + `]`)
	headingPrefixRe    = regexp.MustCompile(`^#{1,6}[` + jsWhitespaceClass + `]+`)
	valueCellRe        = regexp.MustCompile(`^[-–—−]?[` + jsWhitespaceClass + `]*\(?\d[\d` + jsWhitespaceClass + `.,€%]*(?:\)[` + jsWhitespaceClass + `]*)?$`)
)

// TableBlockReport mirrors the TS `TableBlockReport` interface.
type TableBlockReport struct {
	// StartLine is the 0-based line index of the block's first row, for pointing a human at it.
	StartLine    int
	HeaderCells  int
	DataRows     int
	RaggedRows   int
	HasSeparator bool
}

// MarkdownTableReport mirrors the TS `MarkdownTableReport` interface. WorstBlock is nil where JS
// returns null.
type MarkdownTableReport struct {
	Blocks     int
	DataRows   int
	RaggedRows int
	// HeaderlessBlocks counts blocks with no separator row at all — a table continued across a
	// chunk boundary.
	HeaderlessBlocks int
	WorstBlock       *TableBlockReport
}

// CountCells returns the cell count of a GFM row, ignoring the leading and trailing pipes.
func CountCells(row string) int {
	s := jsTrim(row)
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	return len(strings.Split(s, "|"))
}

// AuditMarkdownTables ports auditMarkdownTables.
func AuditMarkdownTables(markdown string) MarkdownTableReport {
	lines := splitLines(markdown)

	// Contiguous runs of pipe rows. A blank line or prose ends a block, which is what separates two
	// genuinely different tables from one table that merely has different column counts.
	type pipeBlock struct {
		start int
		rows  []string
	}
	var blocks []pipeBlock
	var current []string
	start := -1
	for i, line := range lines {
		if tableRowRe.MatchString(line) {
			if current == nil {
				current = []string{}
				start = i
			}
			current = append(current, line)
		} else if current != nil {
			blocks = append(blocks, pipeBlock{start: start, rows: current})
			current = nil
		}
	}
	if current != nil {
		blocks = append(blocks, pipeBlock{start: start, rows: current})
	}

	report := MarkdownTableReport{Blocks: len(blocks)}

	for _, block := range blocks {
		separatorIdx := findSeparator(block.rows)
		if separatorIdx < 0 {
			report.HeaderlessBlocks++
			continue // no header to compare against; the orphan itself is the finding
		}

		headerCells := CountCells(rowAtOrBefore(block.rows, separatorIdx))
		dataRows := 0
		raggedRows := 0
		for i, row := range block.rows {
			if i == separatorIdx || i == separatorIdx-1 {
				continue
			}
			dataRows++
			if CountCells(row) != headerCells {
				raggedRows++
			}
		}

		report.DataRows += dataRows
		report.RaggedRows += raggedRows

		if raggedRows > 0 && (report.WorstBlock == nil || raggedRows > report.WorstBlock.RaggedRows) {
			report.WorstBlock = &TableBlockReport{
				StartLine:    block.start,
				HeaderCells:  headerCells,
				DataRows:     dataRows,
				RaggedRows:   raggedRows,
				HasSeparator: true,
			}
		}
	}

	return report
}

// Content preservation check.
//
// Step C's contract is ZERO CONTENT SKIPPING, but nothing verified it. Bank statements were losing
// transaction payee names — "JEFF DE BRUGES", "BURGER KING", "AUCHAN MARSEILLE" and a
// "SOLDE CREDITEUR" closing balance vanished while every card reference on the same rows survived —
// and the only way to notice was to diff the database by hand.
//
// The measure is token recall: of the distinctive tokens in the raw text (words of 6+ letters and
// numbers, which a summariser drops and markup cannot invent), how many survive into the Markdown?
//
// The blind spot, and why the fusion guard exists: PDF extraction sometimes yields text with no
// spaces at all ("Jemepermetsdevousadressermacandidature"). De-fusing that into real words is a
// legitimate and desirable transformation, but it makes raw tokens unmatchable, scoring ~15% recall
// on a document the pipeline handled *well*. Documents whose raw text is heavily fused are
// therefore not measurable this way and are skipped rather than reported as losses.

// Fusion is detected from three angles because no single one is reliable. Average token length
// alone misses a document like RCHQ_101_..._20100506 — "DateNaturedesoperationsValeurDebitCredit
// 12RUEQUELQUEPART MRNOMPRENOM" scores only 18.9 while being obviously run-together, because the
// surviving spaces drag the mean down. Measured against known-fused and known-clean documents:
//
//	fused  -> longTokenShare 49-95%, camelShare 10-23%
//	clean  -> longTokenShare  7-20%, camelShare  0-2%   (corpus p90 of longTokenShare is 9.1%)
const (
	// FusedTextAvgWordLen is the average characters per whitespace-separated token; above this the
	// text is run-together.
	FusedTextAvgWordLen = 25
	// FusedTextLongTokenShare is the share of tokens 15+ chars long. Long account numbers push a
	// clean statement to ~20%.
	FusedTextLongTokenShare = 0.35
	// FusedTextCamelShare is the share of tokens containing a lowercase->uppercase seam, the
	// signature of glued-together words.
	FusedTextCamelShare = 0.05
)

// ContentRecallReport mirrors the TS `ContentRecallReport` interface.
type ContentRecallReport struct {
	// Measurable is false when the raw text is too fused for recall to mean anything.
	Measurable    bool
	Recall        float64
	TotalTokens   int
	MissingTokens int
	AvgWordLength float64
	// FusionSuspected is true when the raw text shows fusion indicators that fell short of the skip
	// thresholds. Recall is then only a HINT: on run-together text, de-fusing and genuine loss are
	// indistinguishable to any token-level measure, so a caller must present the number as
	// something to look at rather than as proof that content was dropped.
	FusionSuspected bool
}

func contentTokens(text string) []string {
	return contentTokenRe.FindAllString(strings.ToLower(text), -1)
}

// MeasureContentRecall ports measureContentRecall.
func MeasureContentRecall(rawText, markdown string) ContentRecallReport {
	trimmed := jsTrim(rawText)
	var tokens []string
	if trimmed != "" {
		for _, t := range jsWhitespaceRe.Split(trimmed, -1) {
			if t != "" {
				tokens = append(tokens, t)
			}
		}
	}
	words := len(tokens)
	avgWordLength := 0.0
	longTokenShare := 0.0
	camelShare := 0.0
	if words > 0 {
		avgWordLength = float64(utf16Len(trimmed)) / float64(words)
		longTokens := 0
		camelTokens := 0
		for _, t := range tokens {
			if utf16Len(t) >= 15 {
				longTokens++
			}
			if camelCaseRe.MatchString(t) {
				camelTokens++
			}
		}
		longTokenShare = float64(longTokens) / float64(words)
		camelShare = float64(camelTokens) / float64(words)
	}
	fused := avgWordLength >= FusedTextAvgWordLen ||
		longTokenShare >= FusedTextLongTokenShare ||
		camelShare >= FusedTextCamelShare

	raw := uniqueStrings(contentTokens(rawText))
	md := make(map[string]struct{})
	for _, t := range contentTokens(markdown) {
		md[t] = struct{}{}
	}

	// A "missing" token that is itself a fusion artifact is not evidence of loss: the model split it
	// into real words, which is the desired behaviour. Two unambiguous shapes — a run far longer than
	// any ordinary French word ("evolutionsmensuellesdevotrecomptecheques"), and a numeric run
	// carrying several decimal commas ("00,71039,92139,211"), which is multiple amounts glued
	// together and then sliced arbitrarily by the tokenizer.
	isFusionArtifact := func(t string) bool {
		return utf16Len(t) >= 15 || strings.Count(t, ",") >= 2
	}
	missing := 0
	for _, t := range raw {
		if _, ok := md[t]; !ok && !isFusionArtifact(t) {
			missing++
		}
	}

	// Too few tokens to say anything, or text so fused that recall measures de-fusing rather than loss.
	measurable := len(raw) >= 40 && !fused

	// Below the skip thresholds but still showing fusion: recall is a hint, not a verdict.
	fusionSuspected := !fused &&
		(longTokenShare >= FusedTextLongTokenShare/3 || camelShare > 0)

	recall := 1.0
	if len(raw) > 0 {
		recall = float64(len(raw)-missing) / float64(len(raw))
	}

	return ContentRecallReport{
		Measurable:      measurable,
		Recall:          recall,
		TotalTokens:     len(raw),
		MissingTokens:   missing,
		AvgWordLength:   avgWordLength,
		FusionSuspected: fusionSuspected,
	}
}

// Chart/axis regions are not tables.
//
// When OCR or a confused markdown pass renders a bar chart's axis labels as a GFM table, the block
// has a huge header (one cell per bar/month) and near-empty data rows — doc 5009's
// "Evolution de votre consommation" chart came back as a 64-column table whose only data row was
// entirely empty cells. Nothing flagged it: the cells matched the header count, so the integrity
// audit above measures it as healthy. No document in this archive's use (invoices, payslips, bank
// statements, tax forms) needs 14+ columns, so a block whose header claims that many cells is
// definitionally not tabular data.
//
// auditMarkdownTables() only MEASURES, on purpose: repair belongs to the model because guessing
// which cell belongs where can fabricate data. This neutralizer is different — it changes nothing
// about the content, it only removes the TABLE STRUCTURE from a shape that never was one. Every
// cell is kept verbatim inside a blockquote (so auditMarkdownTables no longer counts it as a
// ragged/healthy table, and markdown renderers no longer draw a giant empty grid), and the whole
// block stays in markdown_content for FTS/search.

// ChartTableMaxHeaderCells mirrors the TS constant of the same name.
const ChartTableMaxHeaderCells = 14

// ChartLikeNeutralizeReport mirrors the TS `ChartLikeNeutralizeReport` interface.
type ChartLikeNeutralizeReport struct {
	Markdown string
	// NeutralizedBlocks is how many table blocks were converted to verbatim blockquotes.
	NeutralizedBlocks int
	// NeutralizedLines is how many pipe rows were converted (for the log line).
	NeutralizedLines int
}

// NeutralizeChartLikeTables ports neutralizeChartLikeTables.
func NeutralizeChartLikeTables(markdown string) ChartLikeNeutralizeReport {
	report := ChartLikeNeutralizeReport{Markdown: markdown}
	lines := splitLines(markdown)
	if len(lines) == 0 {
		return report
	}

	// Same contiguous-run detection as auditMarkdownTables: a run of pipe rows is one block.
	type chartBlock struct {
		start int
		rows  []string
	}
	var blocks []chartBlock
	var current []string
	start := -1
	for i, line := range lines {
		if tableRowRe.MatchString(line) {
			if current == nil {
				current = []string{}
				start = i
			}
			current = append(current, line)
		} else if current != nil {
			blocks = append(blocks, chartBlock{start: start, rows: current})
			current = nil
		}
	}
	if current != nil {
		blocks = append(blocks, chartBlock{start: start, rows: current})
	}

	out := make([]string, len(lines))
	copy(out, lines)
	// Convert from the last block backwards so earlier block line indices stay valid.
	for bi := len(blocks) - 1; bi >= 0; bi-- {
		block := blocks[bi]
		separatorIdx := findSeparator(block.rows)
		if separatorIdx < 0 {
			continue // no header → nothing to compare (headerless is audited separately)
		}
		headerCells := CountCells(rowAtOrBefore(block.rows, separatorIdx))
		if headerCells < ChartTableMaxHeaderCells {
			continue
		}

		marker := fmt.Sprintf("> ⚠️ [Wide non-tabular region (%d columns) — kept verbatim as text, not a data table]", headerCells)
		out = spliceInsert(out, block.start, marker)
		// Prefixed AFTER the marker insert so earlier rows still map 1:1 onto their block rows.
		insertAt := block.start + 1
		for rowOffset, row := range block.rows {
			out[insertAt+rowOffset] = "> " + row
		}
		report.NeutralizedBlocks++
		report.NeutralizedLines += len(block.rows)
	}

	report.Markdown = strings.Join(out, "\n")
	return report
}

// Well-formedness repair: table rows that lost their edge pipes.
//
// The model occasionally emits a table whose rows carry the cell pipes but drop the leading or
// trailing pipe — doc 5009's "Base - 03kVA - du 01/02/26 au 16/05/26 | 9,16 | 31,62 | 20,0%".
// Such a line is invisible to auditMarkdownTables (it only counts rows that START with '|'), so
// the whole row silently leaves the table. This pass re-inserts ONLY the missing edge pipes —
// adding "| " at the start and " |" at the end is a purely syntactic, reversible change that
// cannot fabricate or move a value, unlike the width repairs the model itself must make. Lines
// that are markup or prose (headings, blockquotes, lists, bold labels) are never touched.

// PipeRowNormalizeReport mirrors the TS `PipeRowNormalizeReport` interface.
type PipeRowNormalizeReport struct {
	Markdown string
	// FixedLines is how many rows got their missing edge pipes back.
	FixedLines int
}

// NormalizeMalformedPipeRows ports normalizeMalformedPipeRows.
func NormalizeMalformedPipeRows(markdown string) PipeRowNormalizeReport {
	out := splitLines(markdown)
	fixedLines := 0
	for i := 0; i < len(out); i++ {
		trimmed := jsTrim(out[i])
		if trimmed == "" {
			continue
		}
		if wellFormedRowRe.MatchString(trimmed) {
			continue // already a well-formed GFM row
		}
		if separatorRowRe.MatchString(trimmed) {
			continue // separator
		}
		if pipeMarkupPrefixRe.MatchString(trimmed) {
			continue // heading / blockquote / list / bold label
		}
		pipes := strings.Count(trimmed, "|")
		if pipes < 2 {
			continue
		}
		fixed := trimmed
		if !strings.HasPrefix(fixed, "|") {
			fixed = "| " + fixed
		}
		if !strings.HasSuffix(fixed, "|") {
			fixed += " |"
		}
		out[i] = fixed
		fixedLines++
	}
	return PipeRowNormalizeReport{Markdown: strings.Join(out, "\n"), FixedLines: fixedLines}
}

// Headerless continuation rows: merge them back into the table they belong to.
//
// Chunk boundaries split tables: the model continues a table across chunks by emitting ONLY the
// continuing `| row |` lines (no header — see the CONTINUATION CONTEXT note), and joinChunkMarkdown
// already avoids a blank line when the previous chunk's LAST line is a row. But a model that adds
// a trailing heading/text after its rows (or the raw-text fallback of a chunk) leaves the next
// chunk's rows separated by a blank line — a new "headerless" block containing rows whose header
// lives in the previous block. Rows like that are perfectly good data, just visually orphaned.
// When a headerless block DIRECTLY follows (blank-lines only between them) a table block whose
// header has the SAME column count, re-join them: the rows continue that table. Width must match
// exactly — a different width means a different table and is left alone.

// OrphanBlockMergeReport mirrors the TS `OrphanBlockMergeReport` interface.
type OrphanBlockMergeReport struct {
	Markdown     string
	MergedBlocks int
}

// MergeHeaderlessContinuationBlocks ports mergeHeaderlessContinuationBlocks.
func MergeHeaderlessContinuationBlocks(markdown string) OrphanBlockMergeReport {
	lines := splitLines(markdown)

	// Identify contiguous runs of pipe rows (blocks) and which have a header separator.
	type mergeBlock struct {
		start int
		rows  []string
	}
	var blocks []mergeBlock
	var current []string
	start := -1
	for i, line := range lines {
		if tableRowRe.MatchString(line) {
			if current == nil {
				current = []string{}
				start = i
			}
			current = append(current, line)
		} else if current != nil {
			blocks = append(blocks, mergeBlock{start: start, rows: current})
			current = nil
		}
	}
	if current != nil {
		blocks = append(blocks, mergeBlock{start: start, rows: current})
	}

	headerCellsOf := func(b mergeBlock) (int, bool) {
		sepIdx := findSeparator(b.rows)
		if sepIdx < 0 {
			return 0, false
		}
		return CountCells(rowAtOrBefore(b.rows, sepIdx)), true
	}
	onlyBlankBetween := func(aEnd, bStart int) bool {
		for i := aEnd + 1; i < bStart; i++ {
			if jsTrim(lines[i]) != "" {
				return false
			}
		}
		return true
	}

	merges := make(map[int]bool) // indexes of headerless blocks that get merged into the previous one
	for i := 1; i < len(blocks); i++ {
		prev := blocks[i-1]
		cur := blocks[i]
		prevCells, prevHasHeader := headerCellsOf(prev)
		_, curHasHeader := headerCellsOf(cur)
		if curHasHeader || !prevHasHeader {
			continue
		}
		if CountCells(cur.rows[0]) != prevCells {
			continue
		}
		if !onlyBlankBetween(prev.start+len(prev.rows)-1, cur.start) {
			continue
		}
		merges[i] = true
	}
	if len(merges) == 0 {
		return OrphanBlockMergeReport{Markdown: markdown, MergedBlocks: 0}
	}

	// Rebuild: drop the blank lines between merged pairs by removing the lines between the blocks.
	dropFrom := make(map[int]bool)
	for i := range merges {
		prevEnd := blocks[i-1].start + len(blocks[i-1].rows) - 1
		for j := prevEnd + 1; j < blocks[i].start; j++ {
			dropFrom[j] = true
		}
	}
	out := make([]string, 0, len(lines))
	for idx, line := range lines {
		if dropFrom[idx] {
			continue
		}
		out = append(out, line)
	}
	return OrphanBlockMergeReport{Markdown: strings.Join(out, "\n"), MergedBlocks: len(merges)}
}

// Heading-split table rows: a row whose FIRST CELL escaped to a heading.
//
// A table split across a chunk boundary is usually continued as bare `| row |` lines (see the
// continuation context), but when prose — a footnote, a caption — sits between the chunks' rows,
// the model sometimes promotes each continuation row's first cell to a Markdown heading instead:
//   ## Base - 03kVA - du 01/02/26 au 16/05/26
//   | 9,16 | 31,62 | 20,0% |
// The passes above cannot see this damage: the value line is already a well-formed GFM row (edge
// pipes intact) whose width is one short of the header, so the orphan-merge pass refuses it, and
// the row now lives in a headerless block below a heading — doc 5009's "Grille tarifaire" came
// back exactly this way, 3 of its rows promoted to headings with their values left behind.
//
// The parent table is not always healthy either: on some runs the model ALSO dropped the leading
// header cell ("| Prix €HT/mois | Montant €HTTVA | TVA |" with no "Période" column), so its own
// rows are one cell wider than its header. A fold must then match the DATA width, not the header —
// otherwise the repair silently skips the very escape it exists for.
//
// This repair is deterministic and content-preserving: when a heading is IMMEDIATELY followed by a
// lone well-formed row, every cell of that row is a pure value (numeric/percent/€ — a description
// cell would carry words), and the nearest headed table above is exactly (row width + 1) columns
// wide — measured against the block's uniform data width when that is wider than its header — the
// heading text is that row's missing first cell. Re-fold it and move the row into the parent table
// so description and values are one row again. The gates are deliberately narrow — width match,
// all-numeric cells, a lone row, no intervening headed table, a bounded gap — so a heading that
// merely introduces its own small table, or sits far from any table, is left alone.

// HeadingSplitRowReport mirrors the TS `HeadingSplitRowReport` interface.
type HeadingSplitRowReport struct {
	Markdown string
	// ReattachedRows is how many heading-promoted rows were re-folded into their parent table.
	ReattachedRows int
}

// HeadingSplitMaxGapLines is the maximum non-blank lines of prose allowed between a table and its
// escaped rows (a footnote).
const HeadingSplitMaxGapLines = 8

// ReattachHeadingSplitTableRows ports reattachHeadingSplitTableRows.
func ReattachHeadingSplitTableRows(markdown string) HeadingSplitRowReport {
	lines := splitLines(markdown)
	if len(lines) == 0 {
		return HeadingSplitRowReport{Markdown: markdown}
	}

	// Pipe blocks (same contiguous-run detection as the audit) with their header width and, when the
	// block's data rows all share one width, that uniform data width.
	type headingBlock struct {
		start        int
		end          int
		headerCells  int
		hasHeader    bool
		dataWidth    int
		hasDataWidth bool
	}
	var blocks []headingBlock
	var current []string
	start := -1
	pushBlock := func() {
		if current == nil {
			return
		}
		sep := findSeparator(current)
		block := headingBlock{start: start, end: start + len(current) - 1}
		if sep >= 0 {
			block.headerCells = CountCells(rowAtOrBefore(current, sep))
			block.hasHeader = true
			var widths []int
			for i, row := range current {
				if i == sep || i == sep-1 {
					continue
				}
				widths = append(widths, CountCells(row))
			}
			if len(widths) > 0 {
				uniform := true
				for _, w := range widths {
					if w != widths[0] {
						uniform = false
						break
					}
				}
				if uniform {
					block.dataWidth = widths[0]
					block.hasDataWidth = true
				}
			}
		}
		blocks = append(blocks, block)
		current = nil
	}
	for i, line := range lines {
		if tableRowRe.MatchString(line) {
			if current == nil {
				current = []string{}
				start = i
			}
			current = append(current, line)
		} else {
			pushBlock()
		}
	}
	pushBlock()

	// Nearest headed table block ending above a given line (orphan headerless row blocks in between
	// are skipped — they are earlier escapes, not a real parent).
	type parentBlock struct {
		end          int
		headerCells  int
		dataWidth    int
		hasDataWidth bool
	}
	parentBlockAbove := func(lineIdx int) (parentBlock, bool) {
		for b := len(blocks) - 1; b >= 0; b-- {
			if blocks[b].end >= lineIdx {
				continue
			}
			if !blocks[b].hasHeader {
				continue
			}
			return parentBlock{
				end:          blocks[b].end,
				headerCells:  blocks[b].headerCells,
				dataWidth:    blocks[b].dataWidth,
				hasDataWidth: blocks[b].hasDataWidth,
			}, true
		}
		return parentBlock{}, false
	}
	nextNonBlank := func(from int) int {
		for i := from; i < len(lines); i++ {
			if jsTrim(lines[i]) != "" {
				return i
			}
		}
		return -1
	}

	deleteLines := make(map[int]bool)
	insertAfter := make(map[int][]string) // parent block last line -> folded rows

	for h := 0; h < len(lines); h++ {
		heading := jsTrim(lines[h])
		if !headingLineRe.MatchString(heading) {
			continue
		}

		r := nextNonBlank(h + 1)
		if r < 0 || deleteLines[r] {
			continue
		}
		row := jsTrim(lines[r])
		if !tableRowRe.MatchString(row) || tableSeparatorRe.MatchString(row) {
			continue
		}

		// The row must stand ALONE: a heading followed by several rows (or a row with more rows after
		// it) is a section title above its own small table, not a stolen first cell.
		after := nextNonBlank(r + 1)
		if after >= 0 && tableRowRe.MatchString(jsTrim(lines[after])) {
			continue
		}

		rowCells := strings.Split(strings.TrimSuffix(strings.TrimPrefix(row, "|"), "|"), "|")
		if len(rowCells) < 2 {
			continue
		}
		allValues := true
		for _, c := range rowCells {
			if !valueCellRe.MatchString(jsTrim(c)) {
				allValues = false
				break
			}
		}
		if !allValues {
			continue
		}

		parent, ok := parentBlockAbove(h)
		if !ok {
			continue
		}
		// Fold width must equal the parent's real column count. Prefer the block's uniform DATA width
		// when it is wider than the header — that is the signature of a run whose header lost its
		// leading label cell ("Prix €HT/mois | ..." under rows that carry a description column) — and
		// fall back to the header width otherwise.
		want := parent.headerCells
		if parent.hasDataWidth && parent.dataWidth > parent.headerCells {
			want = parent.dataWidth
		}
		if len(rowCells)+1 != want {
			continue
		}

		// The heading must sit near its table: a lone numeric row under a matching-width table found
		// hundreds of lines away is more likely an unrelated caption than an escaped row.
		gapLines := 0
		for i := parent.end + 1; i < h; i++ {
			if jsTrim(lines[i]) != "" {
				gapLines++
			}
		}
		if gapLines > HeadingSplitMaxGapLines {
			continue
		}

		desc := jsTrim(headingPrefixRe.ReplaceAllString(heading, ""))
		desc = jsWhitespaceRe.ReplaceAllString(desc, " ")
		if desc == "" || strings.HasSuffix(desc, ":") {
			continue
		}

		trimmedCells := make([]string, len(rowCells))
		for i, c := range rowCells {
			trimmedCells[i] = jsTrim(c)
		}
		restored := "| " + desc + " | " + strings.Join(trimmedCells, " | ") + " |"
		deleteLines[h] = true
		deleteLines[r] = true
		insertAfter[parent.end] = append(insertAfter[parent.end], restored)
	}

	if len(insertAfter) == 0 {
		return HeadingSplitRowReport{Markdown: markdown, ReattachedRows: 0}
	}

	var out []string
	reattached := 0
	for i := 0; i < len(lines); i++ {
		if deleteLines[i] {
			continue
		}
		out = append(out, lines[i])
		if appended := insertAfter[i]; appended != nil {
			out = append(out, appended...)
			reattached += len(appended)
		}
	}
	return HeadingSplitRowReport{Markdown: strings.Join(out, "\n"), ReattachedRows: reattached}
}

// Table headers one cell short of every one of their rows: restore the missing leading cell.
//
// The heading-reattach pass above folds escaped rows into a parent whose DATA width is wider than
// its header — but the header stays short, so the whole block still audits as ragged
// ("values shifted into the wrong columns") even though every value now sits under the right
// column. The same short-header state occurs without any heading escape: on some runs doc 5009's
// Grille tarifaire came back as ONE block whose header was "| Prix €HT/mois | Montant €HTTVA |
// TVA |" (the model dropped the leading "Période" column) while every data row still carried its
// description cell — a 4-cell row under a 3-cell header. Nothing folds there, so only this pass
// can see it.
//
// Signature: a headed block whose data rows are ALL exactly one cell wider than their header,
// whose last cell (every row) is a pure value, and whose FIRST cell (every row) is NOT a value —
// a descriptive/period label column that lost its header cell. The label itself is not derivable
// deterministically from the output (the model itself is inconsistent about it between runs), so
// the restored cell is EMPTY: width integrity is restored — the audit sees one healthy table and
// no value is ever shifted, invented or relabelled — and the column is self-describing from its
// rows. Blocks whose rows are ragged in any other way, or whose extra width comes from trailing
// junk, are left alone.

// HeaderRestoreReport mirrors the TS `HeaderRestoreReport` interface.
type HeaderRestoreReport struct {
	Markdown string
	// RestoredHeaders is how many table headers were given back their missing leading cell.
	RestoredHeaders int
}

// prependEmptyCell rebuilds a GFM row with one EMPTY cell prepended (header and separator alignment
// only).
func prependEmptyCell(row string) string {
	trimmed := jsTrim(row)
	trimmed = strings.TrimPrefix(trimmed, "|")
	trimmed = strings.TrimSuffix(trimmed, "|")
	cells := strings.Split(trimmed, "|")
	for i, c := range cells {
		cells[i] = jsTrim(c)
	}
	return "|  | " + strings.Join(cells, " | ") + " |"
}

// RestoreMissingTableHeaderCells ports restoreMissingTableHeaderCells.
func RestoreMissingTableHeaderCells(markdown string) HeaderRestoreReport {
	lines := splitLines(markdown)
	if len(lines) == 0 {
		return HeaderRestoreReport{Markdown: markdown, RestoredHeaders: 0}
	}

	type headerBlock struct {
		start int
		rows  []string
		sep   int
	}
	var blocks []headerBlock
	var current []string
	start := -1
	pushBlock := func() {
		if current == nil {
			return
		}
		sep := findSeparator(current)
		if sep >= 0 {
			blocks = append(blocks, headerBlock{start: start, rows: current, sep: sep})
		}
		current = nil
	}
	for i, line := range lines {
		if tableRowRe.MatchString(line) {
			if current == nil {
				current = []string{}
				start = i
			}
			current = append(current, line)
		} else {
			pushBlock()
		}
	}
	pushBlock()

	padLine := make(map[int]bool) // line indexes of header/separator rows to pad
	for _, block := range blocks {
		sep := block.sep
		headerCells := CountCells(rowAtOrBefore(block.rows, sep))
		if headerCells < 2 {
			continue
		}
		var dataRows []string
		for i, row := range block.rows {
			if i == sep || i == sep-1 {
				continue
			}
			dataRows = append(dataRows, row)
		}
		if len(dataRows) == 0 {
			continue
		}

		// Every data row exactly one cell wider than the header…
		allOneWider := true
		for _, row := range dataRows {
			if CountCells(row) != headerCells+1 {
				allOneWider = false
				break
			}
		}
		if !allOneWider {
			continue
		}
		// …with the extra width on the LEFT (descriptive first cells, pure-value last cells). A block
		// whose rows are one cell wider because of trailing filler is a different damage, not this one.
		allLastValues := true
		allFirstNonValues := true
		for _, row := range dataRows {
			trimmed := jsTrim(row)
			first := jsTrim(strings.Split(strings.TrimPrefix(trimmed, "|"), "|")[0])
			cells := strings.Split(strings.TrimSuffix(strings.TrimPrefix(trimmed, "|"), "|"), "|")
			last := jsTrim(cells[len(cells)-1])
			if !valueCellRe.MatchString(last) {
				allLastValues = false
			}
			if valueCellRe.MatchString(first) {
				allFirstNonValues = false
			}
		}
		if !allLastValues {
			continue
		}
		if !allFirstNonValues {
			continue
		}

		padLine[block.start+sep-1] = true // header row (always exists above a separator)
		padLine[block.start+sep] = true   // separator row, kept aligned with the padded header
	}

	if len(padLine) == 0 {
		return HeaderRestoreReport{Markdown: markdown, RestoredHeaders: 0}
	}
	out := make([]string, len(lines))
	copy(out, lines)
	for i := range padLine {
		out[i] = prependEmptyCell(out[i])
	}
	return HeaderRestoreReport{Markdown: strings.Join(out, "\n"), RestoredHeaders: len(padLine) / 2}
}

// splitLines reproduces `(markdown || ”).split(/\r?\n/)`. A Go string has no `undefined`, so the
// `|| ”` collapse is the caller's plain string.
func splitLines(markdown string) []string {
	return lineSplitRe.Split(markdown, -1)
}

// findSeparator is `rows.findIndex(r => TABLE_SEPARATOR.test(r))`: the first separator row, or -1.
func findSeparator(rows []string) int {
	for i, row := range rows {
		if tableSeparatorRe.MatchString(row) {
			return i
		}
	}
	return -1
}

// rowAtOrBefore reproduces `rows[idx - 1] ?? rows[idx]`: JS reads rows[-1] as undefined and the `??`
// then falls back to the separator row itself.
func rowAtOrBefore(rows []string, idx int) string {
	if idx-1 >= 0 {
		return rows[idx-1]
	}
	return rows[idx]
}

// spliceInsert reproduces `out.splice(index, 0, value)`.
func spliceInsert(s []string, index int, value string) []string {
	s = append(s, "")
	copy(s[index+1:], s[index:])
	s[index] = value
	return s
}

// uniqueStrings reproduces `[...new Set(values)]`: same-value de-duplication preserving first-seen
// order.
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

// utf16Len is JavaScript's `str.length`: the number of UTF-16 code units, not runes (see the package
// comment). Go source strings are valid UTF-8, so each rune is one or two code units.
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

// jsTrim is `String.prototype.trim()`: it strips exactly the JavaScript whitespace set below. Go's
// strings.TrimSpace would additionally strip U+0085 NEL and would leave U+FEFF (see the package
// comment).
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
