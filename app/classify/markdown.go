package classify

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/phamhung075/pdf-triage-pdf2w/extractionqualitygate"
	"github.com/phamhung075/pdf-triage-pdf2w/markdowntables"
	"github.com/phamhung075/pdf-triage-pdf2w/prompt"
)

// CONTENT_RECALL_WARN_THRESHOLD is preserved verbatim from classify-document.ts:84:
//
// Below this share of distinctive raw tokens surviving, the conversion has dropped real content.
// Set from the archive's own distribution: median recall is ~95%, and the documents with verified
// losses (bank statements missing payee names) sit at 53-69%. 0.80 flags those without firing on
// ordinary documents.
const contentRecallWarnThreshold = 0.80

var (
	// TABLE_ROW_PATTERN is /^\|.*\|\s*$/ with JavaScript's `.` and `\s` classes.
	tableRowPattern = regexp.MustCompile(`^\|` + jsDotClass + `*\|[` + jsWhitespaceClass + `]*$`)
	// TABLE_SEPARATOR_PATTERN is /^\|(?:\s*:?-{2,}:?\s*\|)+\s*$/.
	tableSeparatorPattern = regexp.MustCompile(`^\|(?:[` + jsWhitespaceClass + `]*:?-{2,}:?[` + jsWhitespaceClass + `]*\|)+[` + jsWhitespaceClass + `]*$`)
	// The continuation-head test in joinChunkMarkdown is /^\|.*\|/ (no end anchor).
	tableRowPrefixPattern = regexp.MustCompile(`^\|` + jsDotClass + `*\|`)
)

// splitOverlongLine ports splitOverlongLine (classify-document.ts:37).
//
// A line longer than the whole chunk budget used to be appended whole, so the "chunk" it produced
// blew straight past maxChunkSize. That is not hypothetical: an archived tax declaration extracted
// to 590,166 chars across just 320 lines — 153 of them over 1400 chars, the longest 13,860 — and
// the resulting ~14k-char chunk went to a model whose num_predict caps the RESPONSE at 4096 tokens.
// The reply came back truncated but non-empty and passed the `length > 10` success gate as
// "converted". (An earlier version of this comment claimed that had left the document's markdown at
// "6% of its raw text" — that ratio turned out to be a whitespace artefact, since these PDFs extract
// to text that is 92-99% whitespace. The over-long chunk is still a real bug; the 6% was not
// evidence of it.) Splitting on whitespace keeps words intact; an unbroken run with no whitespace is
// hard-cut, because exceeding the budget is worse than an ugly seam.
func splitOverlongLine(line string, maxChunkSize int) []string {
	pieces := []string{}
	rest := line

	for utf16Len(rest) > maxChunkSize {
		cut := lastIndexSpaceWithin(rest, maxChunkSize)
		// Ignore a boundary so early that the piece would be mostly empty (and guarantee progress:
		// cut must always be > 0, or this loop would never terminate).
		if float64(cut) < float64(maxChunkSize)/2 {
			cut = maxChunkSize
		}
		pieces = append(pieces, utf16Slice(rest, cut))
		rest = trimLeadingSpaces(utf16SliceFrom(rest, cut))
	}

	if utf16Len(rest) > 0 {
		pieces = append(pieces, rest)
	}
	return pieces
}

// lastIndexSpaceWithin is `str.lastIndexOf(' ', fromIndex)`: the UTF-16 index of the last ASCII
// space at or before fromIndex, or -1.
func lastIndexSpaceWithin(s string, fromIndex int) int {
	found := -1
	units := 0
	for _, r := range s {
		if units > fromIndex {
			break
		}
		if r == ' ' {
			found = units
		}
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	return found
}

// trimLeadingSpaces is `.replace(/^ +/, ”)`: it removes leading ASCII spaces only.
func trimLeadingSpaces(s string) string {
	return strings.TrimLeft(s, " ")
}

// ChunkText ports chunkText (classify-document.ts:54). maxChunkSize defaults to 1400 as in TS.
func ChunkText(rawText string, maxChunkSizeOpt ...int) []string {
	maxChunkSize := 1400
	if len(maxChunkSizeOpt) > 0 {
		maxChunkSize = maxChunkSizeOpt[0]
	}

	if rawText == "" || utf16Len(rawText) <= maxChunkSize {
		return []string{rawText}
	}

	rawLines := lineSplitRe.Split(rawText, -1)
	lines := make([]string, 0, len(rawLines))
	for _, line := range rawLines {
		if utf16Len(line) > maxChunkSize {
			lines = append(lines, splitOverlongLine(line, maxChunkSize)...)
		} else {
			lines = append(lines, line)
		}
	}

	chunks := []string{}
	currentChunk := ""
	for _, line := range lines {
		if utf16Len(currentChunk)+utf16Len(line)+1 > maxChunkSize && utf16Len(currentChunk) > 0 {
			chunks = append(chunks, jsTrim(currentChunk))
			currentChunk = ""
		}
		currentChunk += line + "\n"
	}

	if utf16Len(jsTrim(currentChunk)) > 0 {
		chunks = append(chunks, jsTrim(currentChunk))
	}
	return chunks
}

// DetectOpenTableTail ports detectOpenTableTail (classify-document.ts:95).
//
// Problem B (chunk-blind table splitting): detects whether a chunk's converted Markdown output
// ends "mid-table" — i.e. its last non-blank line is a GFM table data row with a header +
// separator row above it in the same chunk. When found, that header/separator is threaded into
// the next chunk's prompt as continuation context (see MarkdownContinuationContext in prompt.ts)
// so the model continues the same table instead of opening a disconnected new one.
func DetectOpenTableTail(markdown string) *prompt.MarkdownContinuationContext {
	lines := lineSplitRe.Split(markdown, -1)

	end := len(lines) - 1
	for end >= 0 && jsTrim(lines[end]) == "" {
		end--
	}
	if end < 0 {
		return nil
	}

	lastLine := jsTrim(lines[end])
	if !tableRowPattern.MatchString(lastLine) || tableSeparatorPattern.MatchString(lastLine) {
		return nil
	}

	// Walk upward through the contiguous run of table rows looking for the separator row.
	i := end
	separatorIdx := -1
	for i >= 0 && tableRowPattern.MatchString(jsTrim(lines[i])) {
		if tableSeparatorPattern.MatchString(jsTrim(lines[i])) {
			separatorIdx = i
			break
		}
		i--
	}
	if separatorIdx <= 0 {
		return nil // no separator found, or nothing above it to be the header
	}

	header := jsTrim(lines[separatorIdx-1])
	separator := jsTrim(lines[separatorIdx])
	if !tableRowPattern.MatchString(header) {
		return nil
	}

	return &prompt.MarkdownContinuationContext{Header: header, Separator: separator}
}

// JoinChunkMarkdown ports joinChunkMarkdown (classify-document.ts:134).
//
// Chunks are joined with a blank line EXCEPT across a chunk boundary where a table continues: the
// continuation mechanism tells the model to output ONLY the continuing `| row |` lines (no header),
// and a blank line between the previous chunk's table and those rows would split one table into a
// header block + an orphan "headerless" block. When the previous snippet ends on a GFM row and the
// next snippet starts with a GFM row, join them with a single newline so the rows stay in the same
// table block (the header lives in the earlier chunk).
func JoinChunkMarkdown(chunks []string) string {
	var out strings.Builder
	for i, curr := range chunks {
		if i == 0 {
			out.Reset()
			out.WriteString(curr)
			continue
		}
		prev := chunks[i-1]
		prevTail := lastLine(jsTrimEnd(prev))
		currHead := firstLine(jsTrimStart(curr))
		continuesTable := tableRowPattern.MatchString(prevTail) && tableRowPrefixPattern.MatchString(currHead)
		if continuesTable {
			out.WriteString("\n")
		} else {
			out.WriteString("\n\n")
		}
		out.WriteString(curr)
	}
	return out.String()
}

func lastLine(s string) string {
	lines := lineSplitRe.Split(s, -1)
	return lines[len(lines)-1]
}

func firstLine(s string) string {
	lines := lineSplitRe.Split(s, -1)
	return lines[0]
}

// ConvertRawTextToZeroLossMarkdown ports convertRawTextToZeroLossMarkdown
// (classify-document.ts:151). The error return is always nil today (every per-chunk failure is
// caught and degrades to the raw-text fallback); it exists so a future failure is not swallowed.
func (d Deps) ConvertRawTextToZeroLossMarkdown(rawText, filename string) (string, error) {
	chunks := ChunkText(rawText, 1400)
	convertedChunks := make([]string, 0, len(chunks))
	successCount := 0
	fallbackCount := 0
	var pendingContinuation *prompt.MarkdownContinuationContext
	// True per chunk when the model's conversion was used (false = raw-text fallback). Kept parallel
	// to convertedChunks so the targeted-retry pass below knows which chunks are re-convertible.
	convertedFlags := make([]bool, 0, len(chunks))

	for i, chunk := range chunks {
		built := d.Prompts.BuildMarkdownConversionPrompt(chunk, pendingContinuation, "")
		// Step C converts raw text into free-form GFM Markdown (headers, tables, bold) — not JSON.
		// requestClassificationCompletion forces format:'json' grammar-constrained decoding, which
		// starves/garbles Markdown output and was silently degrading most chunks to the raw-text
		// fallback below. requestTextChatCompletion has no format constraint.
		res, err := d.Ollama.RequestTextChatCompletion(built.System, built.User)
		if err != nil {
			// The push is the whole point of this branch: Step C's contract with the model is ZERO
			// CONTENT SKIPPING (prompts/micro_prompt_markdown.md rule 1), so a chunk the model could not
			// convert must still reach the output as raw text. Omitting it here silently deleted the
			// chunk from markdown_content — the caller sees a shorter document and no error at all.
			convertedChunks = append(convertedChunks, chunk)
			convertedFlags = append(convertedFlags, false)
			fallbackCount++
			pendingContinuation = nil
			d.warn(moduleOllamaAI, fmt.Sprintf("[STEP C] Chunk %d/%d conversion failed (%s), keeping raw text chunk", i+1, len(chunks), err.Error()), map[string]any{
				"filename": filename, "chunkIndex": i + 1, "totalChunks": len(chunks), "error": err.Error(),
			})
			continue
		}

		mdSnippet := jsTrim(res.Response)
		// done_reason 'length' means generation stopped at num_predict, not because the model was
		// finished — the markdown is cut off mid-document. It is still long and plausible-looking, so
		// the length check below would happily accept it and drop whatever came after the cut. The
		// raw chunk is worse-formatted but complete, and completeness is Step C's actual contract.
		if res.DoneReason == "length" {
			convertedChunks = append(convertedChunks, chunk)
			convertedFlags = append(convertedFlags, false)
			fallbackCount++
			pendingContinuation = nil
			d.warn(moduleOllamaAI, fmt.Sprintf("[STEP C] Chunk %d/%d hit the model's output limit (done_reason=length) — its markdown was truncated mid-chunk, keeping raw text instead", i+1, len(chunks)), map[string]any{
				"filename": filename, "chunkIndex": i + 1, "totalChunks": len(chunks), "chunkChars": utf16Len(chunk), "truncatedChars": utf16Len(mdSnippet),
			})
			continue
		}

		if mdSnippet != "" && utf16Len(mdSnippet) > 10 {
			convertedChunks = append(convertedChunks, mdSnippet)
			convertedFlags = append(convertedFlags, true)
			successCount++

			if strings.Contains(mdSnippet, prompt.IllegibleFragmentMarker) {
				d.info(moduleOllamaAI, fmt.Sprintf("[STEP C] Chunk %d/%d output contains an illegible-fragment marker (preserved instead of fabricated)", i+1, len(chunks)), map[string]any{
					"filename": filename, "chunkIndex": i + 1, "totalChunks": len(chunks),
				})
			}

			var openTail *prompt.MarkdownContinuationContext
			if i < len(chunks)-1 {
				openTail = DetectOpenTableTail(mdSnippet)
			}
			if openTail != nil {
				d.debug(fmt.Sprintf("[STEP C] Chunk %d/%d ends mid-table — passing continuation context (header: \"%s\") to chunk %d", i+1, len(chunks), openTail.Header, i+2), map[string]any{
					"filename": filename, "chunkIndex": i + 1, "nextChunkIndex": i + 2, "detectedHeader": openTail.Header,
				})
				pendingContinuation = openTail
			} else {
				pendingContinuation = nil
			}
		} else {
			convertedChunks = append(convertedChunks, chunk)
			convertedFlags = append(convertedFlags, false)
			fallbackCount++
			pendingContinuation = nil
			d.warn(moduleOllamaAI, fmt.Sprintf("[STEP C] Chunk %d/%d returned empty/too-short markdown, keeping raw text chunk", i+1, len(chunks)), map[string]any{
				"filename": filename, "chunkIndex": i + 1, "totalChunks": len(chunks),
			})
		}
	}

	// Per-step decision logging (finding E): lets a human later see, without re-running anything,
	// how much of a document's markdown came from the model vs the raw-text fallback.
	d.info(moduleOllamaAI, fmt.Sprintf("[STEP C] Markdown conversion complete for '%s': %d/%d chunk(s) converted, %d/%d kept as raw-text fallback", filenameOrDocument(filename), successCount, len(chunks), fallbackCount, len(chunks)), map[string]any{
		"filename": filename, "totalChunks": len(chunks), "successCount": successCount, "fallbackCount": fallbackCount,
	})

	// Table integrity is checked on the ASSEMBLED markdown, not per chunk, because a table split
	// across a chunk boundary is only visible once the pieces are joined. Measured, never repaired:
	// a row short of the header has shifted its values one column left, and which cell went missing
	// cannot be recovered from the output, so guessing would give a figure a meaning the source never
	// gave it. Without this line the damage was invisible — the only way to find it was to audit the
	// database after the fact, which is how the Bouygues call-detail tables were caught filing each
	// call's cost under "Unité(s) décomptée(s)".
	// ── Targeted single-chunk repair ─────────────────────────────────────────────
	// A chunk whose markdown came back structurally broken (table rows outside the GFM pipes, an
	// absurd header blow-out) is re-converted ONCE and ALONE with a corrective note. The other
	// chunks' output is untouched: re-running the whole document would re-spend every healthy
	// chunk's model round-trip. Only chunks that (a) converted successfully (not raw fallback) and
	// (b) fail the structural screen are retried, at most once each, so a still-broken chunk keeps
	// its first (content-preserving) output instead of looping forever.
	brokenChunks := []int{}
	for ci := range convertedChunks {
		if !convertedFlags[ci] {
			continue
		}
		if extractionqualitygate.DescribeTableRepairNote(convertedChunks[ci]) != "" {
			brokenChunks = append(brokenChunks, ci)
		}
	}
	if len(brokenChunks) > 0 {
		d.warn(moduleOllamaAI, fmt.Sprintf("[STEP C] Targeted repair: re-converting %d structurally broken chunk(s) in place (chunk %s) — other chunks are untouched", len(brokenChunks), joinIntsPlusOne(brokenChunks)), map[string]any{
			"filename": filename, "brokenChunks": plusOne(brokenChunks),
		})
		for _, ci := range brokenChunks {
			originalSnippet := convertedChunks[ci]
			repairNote := extractionqualitygate.DescribeTableRepairNote(originalSnippet)
			if repairNote == "" {
				continue
			}
			rebuilt := d.Prompts.BuildMarkdownConversionPrompt(chunks[ci], nil, repairNote)
			resR, errR := d.Ollama.RequestTextChatCompletion(rebuilt.System, rebuilt.User)
			if errR != nil {
				d.warn(moduleOllamaAI, fmt.Sprintf("[STEP C] Targeted re-conversion of chunk %d failed (%s) — keeping the first output", ci+1, errR.Error()), map[string]any{
					"filename": filename, "chunkIndex": ci + 1, "error": errR.Error(),
				})
				continue
			}
			if resR.DoneReason != "length" {
				fixed := jsTrim(resR.Response)
				if fixed != "" && utf16Len(fixed) > 10 && extractionqualitygate.DescribeTableRepairNote(fixed) == "" {
					convertedChunks[ci] = fixed
					d.info(moduleOllamaAI, fmt.Sprintf("[STEP C] Chunk %d/%d repaired by targeted re-conversion", ci+1, len(chunks)), map[string]any{
						"filename": filename, "chunkIndex": ci + 1, "totalChunks": len(chunks), "reason": utf16Slice(repairNote, 120),
					})
				} else {
					d.warn(moduleOllamaAI, fmt.Sprintf("[STEP C] Targeted re-conversion of chunk %d still fails the structural screen — keeping the first output (content-preserving)", ci+1), map[string]any{
						"filename": filename, "chunkIndex": ci + 1,
					})
				}
			}
		}
	}

	assembled := JoinChunkMarkdown(convertedChunks)

	// Chart/axis regions that a confused model rendered as GFM tables (doc 5009's consumption chart
	// became a 64-column table) are converted back to verbatim blockquotes BEFORE the integrity
	// audit below, so the audit measures only real tables. Content is never dropped — every cell
	// stays in the output inside a blockquote. See neutralizeChartLikeTables in markdown-tables.ts.
	neutralized := markdowntables.NeutralizeChartLikeTables(assembled)
	if neutralized.NeutralizedBlocks > 0 {
		d.warn(moduleOllamaAI, fmt.Sprintf("[STEP C] Converted %d chart/axis-like table block(s) (>= %d columns) in '%s' back to verbatim text — kept for search, not rendered as a data table", neutralized.NeutralizedBlocks, markdowntables.ChartTableMaxHeaderCells, filenameOrDocument(filename)), map[string]any{
			"filename": filename, "neutralizedBlocks": neutralized.NeutralizedBlocks, "neutralizedLines": neutralized.NeutralizedLines,
		})
	}
	assembledMarkdown := neutralized.Markdown

	// Well-formedness repair: table rows that lost their edge pipes ("Base ... | 9,16 | 31,62 |
	// 20,0%" without the leading/trailing '|') get them back. Purely syntactic — no cell is moved,
	// merged or invented — so a row that the model dropped out of its table returns to it.
	normalized := markdowntables.NormalizeMalformedPipeRows(assembledMarkdown)
	if normalized.FixedLines > 0 {
		d.warn(moduleOllamaAI, fmt.Sprintf("[STEP C] Restored missing edge pipes on %d table row(s) in '%s' (rows had dropped out of their GFM table)", normalized.FixedLines, filenameOrDocument(filename)), map[string]any{
			"filename": filename, "fixedLines": normalized.FixedLines,
		})
	}
	// Headerless continuation rows: rows the model continued from a previous chunk (no repeated
	// header) that a blank line separated into their own block. When they directly follow a table
	// whose header has the same column count, they are the same table's rows — re-join them so the
	// integrity audit (and the pre-registration quality gate) sees one healthy table.
	merged := markdowntables.MergeHeaderlessContinuationBlocks(normalized.Markdown)
	if merged.MergedBlocks > 0 {
		d.warn(moduleOllamaAI, fmt.Sprintf("[STEP C] Re-joined %d headerless continuation row block(s) into their parent table in '%s'", merged.MergedBlocks, filenameOrDocument(filename)), map[string]any{
			"filename": filename, "mergedBlocks": merged.MergedBlocks,
		})
	}
	// Heading-split rows: a row whose description the model promoted to a heading ("## Base - 03kVA -
	// du 01/02/26 au 16/05/26" above a lone "| 9,16 | 31,62 | 20,0% |") — doc 5009's Grille tarifaire
	// came back with 3 of its 5 rows in that shape, each one cell short of its header. The edge-pipe
	// and blank-line passes above cannot see it (the value line is already a well-formed row), so fold
	// the heading back into the row as its first cell and move the row into the parent table.
	reattached := markdowntables.ReattachHeadingSplitTableRows(merged.Markdown)
	if reattached.ReattachedRows > 0 {
		d.warn(moduleOllamaAI, fmt.Sprintf("[STEP C] Re-folded %d table row(s) whose description had escaped to a heading back into their parent table in '%s'", reattached.ReattachedRows, filenameOrDocument(filename)), map[string]any{
			"filename": filename, "reattachedRows": reattached.ReattachedRows,
		})
	}
	// Headers one cell short of every one of their rows: on some runs the model ALSO drops the
	// table's leading label column (a "| Prix €HT/mois | Montant €HTTVA | TVA |" header above rows
	// that each carry a description cell), which reattaching alone cannot fix because there is no
	// heading to fold. Give the header back its missing leading cell (empty — the label is not
	// derivable) so the table audits as one healthy block with no shifted values.
	restored := markdowntables.RestoreMissingTableHeaderCells(reattached.Markdown)
	if restored.RestoredHeaders > 0 {
		d.warn(moduleOllamaAI, fmt.Sprintf("[STEP C] Restored %d table header(s) that were one cell short of their rows (missing leading label cell) in '%s'", restored.RestoredHeaders, filenameOrDocument(filename)), map[string]any{
			"filename": filename, "restoredHeaders": restored.RestoredHeaders,
		})
	}
	finalMarkdown := restored.Markdown
	tables := markdowntables.AuditMarkdownTables(finalMarkdown)
	if tables.RaggedRows > 0 || tables.HeaderlessBlocks > 0 {
		// Only state the symptoms that actually occurred. The two are independent — a document can have
		// headerless blocks and no ragged rows — and an earlier version always led with the ragged
		// clause, so a document with 0 ragged rows was told "those rows' values are shifted into the
		// wrong columns" about no rows at all. A warning that overstates what it found is a warning
		// people learn to skim past.
		parts := []string{}
		if tables.RaggedRows > 0 {
			pct := 0
			if tables.DataRows > 0 {
				pct = jsRound((100 * float64(tables.RaggedRows)) / float64(tables.DataRows))
			}
			parts = append(parts, fmt.Sprintf("%d/%d data row(s) (%d%%) have a different cell count than their header, so those rows' values are shifted into the wrong columns", tables.RaggedRows, tables.DataRows, pct))
		}
		if tables.HeaderlessBlocks > 0 {
			parts = append(parts, fmt.Sprintf("%d table block(s) have no header at all (a table split across a chunk boundary)", tables.HeaderlessBlocks))
		}
		var worstBlockLine, worstBlockHeaderCells any
		if tables.WorstBlock != nil {
			worstBlockLine = tables.WorstBlock.StartLine
			worstBlockHeaderCells = tables.WorstBlock.HeaderCells
		}
		d.warn(moduleOllamaAI, fmt.Sprintf("[STEP C] Table integrity problems in '%s': %s", filenameOrDocument(filename), strings.Join(parts, "; ")), map[string]any{
			"filename":              filename,
			"tableBlocks":           tables.Blocks,
			"dataRows":              tables.DataRows,
			"raggedRows":            tables.RaggedRows,
			"headerlessBlocks":      tables.HeaderlessBlocks,
			"worstBlockLine":        worstBlockLine,
			"worstBlockHeaderCells": worstBlockHeaderCells,
		})
	}

	// Step C's contract is ZERO CONTENT SKIPPING; this is the only thing that checks it. Bank
	// statements were dropping transaction payee names and a closing balance while every card
	// reference on the same rows survived, and nothing anywhere said so. Skipped for heavily fused
	// raw text, where unmatchable raw tokens mean the model de-fused correctly rather than lost
	// anything — see measureContentRecall.
	recall := markdowntables.MeasureContentRecall(rawText, finalMarkdown)
	if recall.Measurable && recall.Recall < contentRecallWarnThreshold && recall.FusionSuspected {
		// Fused source text: de-fusing and genuine loss are indistinguishable to any token measure, so
		// this cannot be asserted. It stays at DEBUG rather than becoming a WARN nobody can act on —
		// a family of BNP RLV_CHQ_* statements produced ~10 such warnings an hour, every one a false
		// alarm on markdown that was correct. Still recorded, so an audit can find it.
		d.debug(fmt.Sprintf("[STEP C] Low token recall for '%s' (%s%%), but the raw text is run-together — most likely the model split fused words correctly rather than dropping content", filenameOrDocument(filename), jsToFixed0(recall.Recall*100)), map[string]any{
			"filename": filename, "recallPct": jsRound(recall.Recall * 100), "missingTokens": recall.MissingTokens, "totalTokens": recall.TotalTokens, "fusionSuspected": true,
		})
	} else if recall.Measurable && recall.Recall < contentRecallWarnThreshold {
		d.warn(moduleOllamaAI, fmt.Sprintf("[STEP C] Content preservation below threshold for '%s': %s%% of distinctive raw-text tokens survived into the markdown (%d/%d missing) — values present in the source are absent from markdown_content", filenameOrDocument(filename), jsToFixed0(recall.Recall*100), recall.MissingTokens, recall.TotalTokens), map[string]any{
			"filename": filename, "recallPct": jsRound(recall.Recall * 100), "missingTokens": recall.MissingTokens, "totalTokens": recall.TotalTokens, "fusionSuspected": recall.FusionSuspected,
		})
	}

	return finalMarkdown, nil
}

func plusOne(values []int) []int {
	out := make([]int, len(values))
	for i, v := range values {
		out[i] = v + 1
	}
	return out
}

func joinIntsPlusOne(values []int) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = fmt.Sprintf("%d", v+1)
	}
	return strings.Join(parts, ", ")
}
