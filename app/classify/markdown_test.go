package classify

import (
	"strconv"
	"strings"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/extractionqualitygate"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
	"github.com/phamhung075/pdf-triage-pdf2w/markdowntables"
)

func multiChunkRawText(prefix string) string {
	lines := make([]string, 0, 60)
	for i := 1; i <= 60; i++ {
		lines = append(lines, prefix+itoa(i)+": padding content to push this document past the 1400-char chunk boundary for testing purposes.")
	}
	return strings.Join(lines, "\n")
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

// TS (micro-prompt-pipeline): chunkText splits long text into sequential chunks without losing data.
func TestChunkTextSplitsLongTextWithoutLosingData(t *testing.T) {
	lines := make([]string, 0, 100)
	for i := 1; i <= 100; i++ {
		lines = append(lines, "Line "+itoa(i)+": This is sample document content row for testing chunking.")
	}
	fullText := strings.Join(lines, "\n")
	if len(fullText) <= 3000 {
		t.Fatalf("test setup: fullText length = %d, want > 3000", len(fullText))
	}

	chunks := ChunkText(fullText, 1000)
	if len(chunks) <= 1 {
		t.Fatalf("chunks = %d, want > 1", len(chunks))
	}
	reassembled := strings.Join(chunks, "\n")
	if collapseWhitespace(reassembled) != collapseWhitespace(fullText) {
		t.Fatalf("reassembled differs after whitespace collapse")
	}
}

// TS: splits a single line that is longer than the chunk size.
func TestChunkTextSplitsSingleOverlongLine(t *testing.T) {
	oneLongLine := strings.TrimSpace(strings.Repeat("mot ", 4000))
	chunks := ChunkText(oneLongLine, 1400)
	if len(chunks) <= 1 {
		t.Fatalf("chunks = %d, want > 1", len(chunks))
	}
	for _, c := range chunks {
		if utf16Len(c) > 1400 {
			t.Fatalf("chunk length %d exceeds budget 1400", utf16Len(c))
		}
	}
}

// TS: loses no words when splitting a long line.
func TestChunkTextLosesNoWords(t *testing.T) {
	words := make([]string, 0, 900)
	for i := 0; i < 900; i++ {
		words = append(words, "w"+itoa(i))
	}
	chunks := ChunkText(strings.Join(words, " "), 1400)
	rejoined := strings.Fields(strings.Join(chunks, " "))
	if len(rejoined) != len(words) {
		t.Fatalf("rejoined %d words, want %d", len(rejoined), len(words))
	}
	for i := range words {
		if rejoined[i] != words[i] {
			t.Fatalf("word %d = %q, want %q", i, rejoined[i], words[i])
		}
	}
}

// TS: splits at whitespace rather than mid-word when it can.
func TestChunkTextSplitsAtWhitespace(t *testing.T) {
	lorems := make([]string, 0, 400)
	for i := 0; i < 400; i++ {
		lorems = append(lorems, "lorem")
	}
	chunks := ChunkText(strings.Join(lorems, " "), 200)
	for _, c := range chunks {
		if !strings.HasPrefix(c, "lorem") {
			t.Errorf("chunk does not start with a whole word: %q", c)
		}
		if !strings.HasSuffix(c, "lorem") {
			t.Errorf("chunk does not end with a whole word: %q", c)
		}
	}
}

// TS: still respects the budget for an unbroken run with no whitespace at all.
func TestChunkTextUnbrokenRunRespectsBudget(t *testing.T) {
	raw := strings.Repeat("x", 5000)
	chunks := ChunkText(raw, 1400)
	if len(chunks) <= 1 {
		t.Fatalf("chunks = %d, want > 1", len(chunks))
	}
	for _, c := range chunks {
		if utf16Len(c) > 1400 {
			t.Fatalf("chunk length %d exceeds budget 1400", utf16Len(c))
		}
	}
	if strings.Join(chunks, "") != raw {
		t.Fatalf("joining chunks did not reproduce the hard-cut input")
	}
}

// TS: leaves normal short-line text chunked exactly as before.
func TestChunkTextLeavesNormalTextIntact(t *testing.T) {
	lines := make([]string, 0, 60)
	for i := 0; i < 60; i++ {
		lines = append(lines, "Line "+itoa(i)+": ordinary content here.")
	}
	text := strings.Join(lines, "\n")
	chunks := ChunkText(text, 1400)
	for _, c := range chunks {
		if utf16Len(c) > 1400 {
			t.Fatalf("chunk length %d exceeds budget 1400", utf16Len(c))
		}
	}
	joined := strings.Join(chunks, "\n")
	for i := 0; i < 60; i++ {
		assertContains(t, joined, "Line "+itoa(i)+":")
	}
}

// TS (micro-prompt-pipeline): detectOpenTableTail cases.
func TestDetectOpenTableTailDetectsOpenTable(t *testing.T) {
	md := strings.Join([]string{
		"## Transactions",
		"",
		"| Date | Amount | Label |",
		"| --- | --- | --- |",
		"| 2024-05-01 | 100.00 | Salaire |",
		"| 2024-05-02 | 200.00 | Loyer |",
	}, "\n")

	result := DetectOpenTableTail(md)
	if result == nil {
		t.Fatal("expected an open table tail")
	}
	if result.Header != "| Date | Amount | Label |" {
		t.Errorf("header = %q", result.Header)
	}
	if result.Separator != "| --- | --- | --- |" {
		t.Errorf("separator = %q", result.Separator)
	}
}

func TestDetectOpenTableTailNullWhenNoTableRow(t *testing.T) {
	md := "## Section\n\nSome closing paragraph, not a table."
	if DetectOpenTableTail(md) != nil {
		t.Fatal("expected nil")
	}
}

func TestDetectOpenTableTailNullWhenLastLineIsSeparatorOnly(t *testing.T) {
	if DetectOpenTableTail("| --- | --- |") != nil {
		t.Fatal("expected nil")
	}
}

func TestDetectOpenTableTailNullWhenNoSeparatorRow(t *testing.T) {
	md := strings.Join([]string{
		"**Label:** value | still not a table",
		"| just one pipe-looking line |",
	}, "\n")
	if DetectOpenTableTail(md) != nil {
		t.Fatal("expected nil")
	}
}

func TestDetectOpenTableTailIgnoresTrailingBlankLines(t *testing.T) {
	md := strings.Join([]string{
		"| Date | Amount |",
		"| --- | --- |",
		"| 2024-05-01 | 100.00 |",
		"",
		"   ",
		"",
	}, "\n")
	result := DetectOpenTableTail(md)
	if result == nil {
		t.Fatal("expected an open table tail")
	}
	if result.Header != "| Date | Amount |" {
		t.Errorf("header = %q", result.Header)
	}
}

func TestDetectOpenTableTailNullForClosedTable(t *testing.T) {
	md := strings.Join([]string{
		"| Date | Amount |",
		"| --- | --- |",
		"| 2024-05-01 | 100.00 |",
		"",
		"End of section.",
	}, "\n")
	if DetectOpenTableTail(md) != nil {
		t.Fatal("expected nil for a table that closes with prose")
	}
}

// TS: threads the previous chunk's open-table header/separator into the next chunk's prompt as
// continuation context.
func TestConvertThreadsContinuationContext(t *testing.T) {
	rawText := multiChunkRawText("Line ")
	chunks := ChunkText(rawText, 1400)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want >= 2", len(chunks))
	}

	openTableMarkdown := strings.Join([]string{
		"## Transactions",
		"",
		"| Date | Amount | Label |",
		"| --- | --- | --- |",
		"| 2024-05-01 | 100.00 | Salaire |",
	}, "\n")

	e := newEnv()
	e.ollama.textScripts = []textScript{{res: ollama.TextCompletion{Response: openTableMarkdown, DoneReason: "stop"}}}
	for i := 1; i < len(chunks); i++ {
		e.ollama.textScripts = append(e.ollama.textScripts, textScript{res: ollama.TextCompletion{Response: "| 2024-05-02 | 200.00 | Loyer |", DoneReason: "stop"}})
	}

	if _, err := e.deps().ConvertRawTextToZeroLossMarkdown(rawText, "payroll.pdf"); err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(e.ollama.textCalls) != len(chunks) {
		t.Fatalf("text calls = %d, want %d", len(e.ollama.textCalls), len(chunks))
	}

	second := e.ollama.textCalls[1].user
	assertContains(t, second, "⚠️ CONTINUATION CONTEXT:")
	assertContains(t, second, "| Date | Amount | Label |")
	assertContains(t, second, "| --- | --- | --- |")
	if !strings.Contains(strings.ToLower(second), "do not repeat the header/separator row") {
		t.Fatalf("second prompt lacks the do-not-repeat instruction:\n%s", second)
	}
}

// TS: does not inject continuation context when the previous chunk does not end mid-table.
func TestConvertNoContinuationForProse(t *testing.T) {
	rawText := multiChunkRawText("Line ")
	chunks := ChunkText(rawText, 1400)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want >= 2", len(chunks))
	}

	e := newEnv()
	for i := range chunks {
		e.ollama.textScripts = append(e.ollama.textScripts, textScript{res: ollama.TextCompletion{Response: "## Section " + itoa(i+1) + "\n\nJust prose, no table here.", DoneReason: "stop"}})
	}

	if _, err := e.deps().ConvertRawTextToZeroLossMarkdown(rawText, "letter.pdf"); err != nil {
		t.Fatalf("convert: %v", err)
	}
	second := e.ollama.textCalls[1].user
	if strings.Contains(second, "⚠️ CONTINUATION CONTEXT:") {
		t.Fatalf("unexpected continuation context in the second prompt")
	}
}

// TS: logs a debug line identifying the chunk index and detected header when continuation context
// is passed forward.
func TestConvertLogsContinuationDebugLine(t *testing.T) {
	rawText := multiChunkRawText("Line ")
	chunks := ChunkText(rawText, 1400)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want >= 2", len(chunks))
	}

	openTableMarkdown := strings.Join([]string{
		"| Ref | Montant |",
		"| --- | --- |",
		"| A1 | 50.00 |",
	}, "\n")

	e := newEnv()
	e.ollama.textScripts = []textScript{{res: ollama.TextCompletion{Response: openTableMarkdown, DoneReason: "stop"}}}
	for i := 1; i < len(chunks); i++ {
		e.ollama.textScripts = append(e.ollama.textScripts, textScript{res: ollama.TextCompletion{Response: "| A2 | 75.00 |", DoneReason: "stop"}})
	}

	if _, err := e.deps().ConvertRawTextToZeroLossMarkdown(rawText, "notice.pdf"); err != nil {
		t.Fatalf("convert: %v", err)
	}

	record := e.log.find("DEBUG", "ends mid-table")
	if record == nil {
		t.Fatal("expected a DEBUG line about the open table")
	}
	assertContains(t, record.message, "Ref | Montant")
	if record.meta["filename"] != "notice.pdf" {
		t.Errorf("filename = %v", record.meta["filename"])
	}
	if record.meta["chunkIndex"] != 1 {
		t.Errorf("chunkIndex = %v, want 1", record.meta["chunkIndex"])
	}
	if record.meta["nextChunkIndex"] != 2 {
		t.Errorf("nextChunkIndex = %v, want 2", record.meta["nextChunkIndex"])
	}
}

// TS: logs an info line when a chunk's converted output contains the illegible-fragment marker.
func TestConvertLogsIllegibleFragmentMarker(t *testing.T) {
	e := newEnv()
	e.ollama.textScripts = text("> ⚠️ [Illegible fragment — preserved as-is]\nBANG cAN oor xf roAN garbled OCR noise")

	if _, err := e.deps().ConvertRawTextToZeroLossMarkdown("BANG cAN oor xf roAN garbled OCR noise", "garbled.pdf"); err != nil {
		t.Fatalf("convert: %v", err)
	}
	record := e.log.find("INFO", "illegible-fragment marker")
	if record == nil {
		t.Fatal("expected an INFO line about the illegible-fragment marker")
	}
	if record.meta["filename"] != "garbled.pdf" {
		t.Errorf("filename = %v", record.meta["filename"])
	}
	if record.meta["chunkIndex"] != 1 {
		t.Errorf("chunkIndex = %v, want 1", record.meta["chunkIndex"])
	}
}

// TS: keeps the raw chunk when the model call throws, instead of deleting it.
func TestConvertKeepsRawChunkWhenCallThrows(t *testing.T) {
	rawText := multiChunkRawText("SENTINEL ")
	chunks := ChunkText(rawText, 1400)
	if len(chunks) < 3 {
		t.Fatalf("chunks = %d, want >= 3", len(chunks))
	}

	e := newEnv()
	for i := range chunks {
		if i == 1 {
			e.ollama.textScripts = append(e.ollama.textScripts, textScript{err: errString("simulated ollama timeout")})
		} else {
			e.ollama.textScripts = append(e.ollama.textScripts, textScript{res: ollama.TextCompletion{Response: "## Converted chunk " + itoa(i+1), DoneReason: "stop"}})
		}
	}

	md, err := e.deps().ConvertRawTextToZeroLossMarkdown(rawText, "timeout.pdf")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	for _, line := range strings.Split(chunks[1], "\n") {
		if strings.TrimSpace(line) != "" {
			assertContains(t, md, strings.TrimSpace(line))
		}
	}
	firstFailedLine := strings.TrimSpace(strings.Split(chunks[1], "\n")[0])
	if strings.Index(md, "## Converted chunk 1") >= strings.Index(md, firstFailedLine) {
		t.Fatalf("converted chunk 1 should precede the raw fallback chunk")
	}
	assertContains(t, md, "## Converted chunk "+itoa(len(chunks)))
}

// TS: preserves every chunk even when every single model call throws.
func TestConvertPreservesEveryChunkWhenAllThrow(t *testing.T) {
	rawText := multiChunkRawText("SENTINEL ")
	chunks := ChunkText(rawText, 1400)

	e := newEnv()
	for range chunks {
		e.ollama.textScripts = append(e.ollama.textScripts, textScript{err: errString("ollama down")})
	}

	md, err := e.deps().ConvertRawTextToZeroLossMarkdown(rawText, "alldown.pdf")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	for i := 1; i <= 60; i++ {
		assertContains(t, md, "SENTINEL "+itoa(i)+":")
	}
}

// TS: warns rather than debug-logs when a chunk falls back, so the degradation is visible in the log.
func TestConvertWarnsWhenChunkFallsBack(t *testing.T) {
	rawText := multiChunkRawText("SENTINEL ")
	chunks := ChunkText(rawText, 1400)

	e := newEnv()
	for i := range chunks {
		if i == 1 {
			e.ollama.textScripts = append(e.ollama.textScripts, textScript{err: errString("simulated ollama timeout")})
		} else {
			e.ollama.textScripts = append(e.ollama.textScripts, textScript{res: ollama.TextCompletion{Response: "## Converted chunk " + itoa(i+1), DoneReason: "stop"}})
		}
	}

	if _, err := e.deps().ConvertRawTextToZeroLossMarkdown(rawText, "timeout.pdf"); err != nil {
		t.Fatalf("convert: %v", err)
	}
	record := e.log.find("WARN", "conversion failed")
	if record == nil {
		t.Fatal("expected a WARN about the failed conversion")
	}
	if record.meta["filename"] != "timeout.pdf" {
		t.Errorf("filename = %v", record.meta["filename"])
	}
	if record.meta["chunkIndex"] != 2 {
		t.Errorf("chunkIndex = %v, want 2", record.meta["chunkIndex"])
	}
}

// TS: keeps the raw chunk when generation stopped at the output limit.
func TestConvertKeepsRawChunkOnTruncation(t *testing.T) {
	rawText := multiChunkRawText("TRUNCMARK ")
	chunks := ChunkText(rawText, 1400)

	e := newEnv()
	for i := range chunks {
		if i == 1 {
			e.ollama.textScripts = append(e.ollama.textScripts, textScript{res: ollama.TextCompletion{Response: "## Converted but cut off mid-sen", DoneReason: "length"}})
		} else {
			e.ollama.textScripts = append(e.ollama.textScripts, textScript{res: ollama.TextCompletion{Response: "## Converted chunk " + itoa(i+1), DoneReason: "stop"}})
		}
	}

	md, err := e.deps().ConvertRawTextToZeroLossMarkdown(rawText, "truncated.pdf")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if strings.Contains(md, "cut off mid-sen") {
		t.Fatal("the truncated output must be discarded")
	}
	for _, line := range strings.Split(chunks[1], "\n") {
		if strings.TrimSpace(line) != "" {
			assertContains(t, md, strings.TrimSpace(line))
		}
	}
}

// TS: warns so the truncation is visible in the log.
func TestConvertWarnsOnTruncation(t *testing.T) {
	rawText := multiChunkRawText("TRUNCMARK ")
	chunks := ChunkText(rawText, 1400)

	e := newEnv()
	for i := range chunks {
		if i == 0 {
			e.ollama.textScripts = append(e.ollama.textScripts, textScript{res: ollama.TextCompletion{Response: "## cut", DoneReason: "length"}})
		} else {
			e.ollama.textScripts = append(e.ollama.textScripts, textScript{res: ollama.TextCompletion{Response: "## ok " + itoa(i), DoneReason: "stop"}})
		}
	}

	if _, err := e.deps().ConvertRawTextToZeroLossMarkdown(rawText, "truncated.pdf"); err != nil {
		t.Fatalf("convert: %v", err)
	}
	record := e.log.find("WARN", "done_reason=length")
	if record == nil {
		t.Fatal("expected a WARN about done_reason=length")
	}
	if record.meta["filename"] != "truncated.pdf" {
		t.Errorf("filename = %v", record.meta["filename"])
	}
	if record.meta["chunkIndex"] != 1 {
		t.Errorf("chunkIndex = %v, want 1", record.meta["chunkIndex"])
	}
}

// TS: accepts a normal completion untouched.
func TestConvertAcceptsNormalCompletion(t *testing.T) {
	rawText := multiChunkRawText("TRUNCMARK ")
	chunks := ChunkText(rawText, 1400)

	e := newEnv()
	for i := range chunks {
		e.ollama.textScripts = append(e.ollama.textScripts, textScript{res: ollama.TextCompletion{Response: "## Converted chunk " + itoa(i+1), DoneReason: "stop"}})
	}

	md, err := e.deps().ConvertRawTextToZeroLossMarkdown(rawText, "fine.pdf")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	assertContains(t, md, "## Converted chunk 1")
	if strings.Contains(md, "TRUNCMARK") {
		t.Fatal("fully converted document should not contain raw fallback text")
	}
}

// TS: warns when the assembled markdown has rows short of their header.
func TestConvertWarnsRaggedRows(t *testing.T) {
	e := newEnv()
	e.ollama.textScripts = text(strings.Join([]string{
		"| Date | Heure | Numéro | Unités | Coût |",
		"|:---|:---|:---|:---|:---|",
		"| 12/08 | 11:53:37 | 336528710 | 0,00 |",
		"| 12/08 | 11:54:33 | 336528710 | 0,00 |",
	}, "\n"))

	if _, err := e.deps().ConvertRawTextToZeroLossMarkdown("short raw text", "facture.pdf"); err != nil {
		t.Fatalf("convert: %v", err)
	}
	record := e.log.find("WARN", "Table integrity problems")
	if record == nil {
		t.Fatal("expected a table integrity warning")
	}
	if record.meta["filename"] != "facture.pdf" {
		t.Errorf("filename = %v", record.meta["filename"])
	}
	if record.meta["raggedRows"] != 2 {
		t.Errorf("raggedRows = %v, want 2", record.meta["raggedRows"])
	}
	if record.meta["dataRows"] != 2 {
		t.Errorf("dataRows = %v, want 2", record.meta["dataRows"])
	}
}

// TS: stays quiet for a well-formed table.
func TestConvertQuietForWellFormedTable(t *testing.T) {
	e := newEnv()
	e.ollama.textScripts = text(strings.Join([]string{
		"| Date | Coût |",
		"| --- | --- |",
		"| 12/08 | 1,00 |",
	}, "\n"))

	if _, err := e.deps().ConvertRawTextToZeroLossMarkdown("short raw text", "clean.pdf"); err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got := e.log.messages("WARN", "Table integrity"); len(got) != 0 {
		t.Fatalf("unexpected table integrity warnings: %v", got)
	}
}

// TS: does not mention shifted values when no row is ragged.
func TestConvertTableWarningStatesOnlyHeaderless(t *testing.T) {
	e := newEnv()
	e.ollama.textScripts = text(strings.Join([]string{
		"| 12/08 | 11:53 | 0,00 |",
		"| 13/08 | 12:01 | 0,00 |",
	}, "\n"))

	if _, err := e.deps().ConvertRawTextToZeroLossMarkdown("short raw text", "orphan.pdf"); err != nil {
		t.Fatalf("convert: %v", err)
	}
	record := e.log.find("WARN", "Table integrity")
	if record == nil {
		t.Fatal("expected a table integrity warning")
	}
	assertContains(t, record.message, "no header at all")
	if strings.Contains(record.message, "shifted into the wrong columns") {
		t.Error("warning must not claim shifted values with zero ragged rows")
	}
	if strings.Contains(record.message, "(0%)") {
		t.Error("warning must not print a 0% ragged clause")
	}
}

// TS: does not mention headerless blocks when there are none.
func TestConvertTableWarningStatesOnlyRagged(t *testing.T) {
	e := newEnv()
	e.ollama.textScripts = text(strings.Join([]string{
		"| A | B | C |",
		"| --- | --- | --- |",
		"| 1 | 2 |",
	}, "\n"))

	if _, err := e.deps().ConvertRawTextToZeroLossMarkdown("short raw text", "ragged.pdf"); err != nil {
		t.Fatalf("convert: %v", err)
	}
	record := e.log.find("WARN", "Table integrity")
	if record == nil {
		t.Fatal("expected a table integrity warning")
	}
	assertContains(t, record.message, "shifted into the wrong columns")
	if strings.Contains(record.message, "no header at all") {
		t.Error("warning must not mention headerless blocks when there are none")
	}
}

// TS: warns when a large share of raw content tokens is absent from the markdown.
func TestConvertWarnsLowContentRecall(t *testing.T) {
	raw := distinctWords(60)
	e := newEnv()
	e.ollama.textScripts = text(strings.Join(strings.Split(raw, " ")[:10], " "))

	if _, err := e.deps().ConvertRawTextToZeroLossMarkdown(raw, "statement.pdf"); err != nil {
		t.Fatalf("convert: %v", err)
	}
	record := e.log.find("WARN", "Content preservation below threshold")
	if record == nil {
		t.Fatal("expected a content preservation warning")
	}
	if record.meta["filename"] != "statement.pdf" {
		t.Errorf("filename = %v", record.meta["filename"])
	}
}

// TS: stays quiet when the markdown keeps everything.
func TestConvertQuietWhenContentKept(t *testing.T) {
	raw := distinctWords(60)
	e := newEnv()
	e.ollama.textScripts = text("# Title\n\n" + raw)

	if _, err := e.deps().ConvertRawTextToZeroLossMarkdown(raw, "clean.pdf"); err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got := e.log.messages("WARN", "Content preservation"); len(got) != 0 {
		t.Fatalf("unexpected content preservation warnings: %v", got)
	}
}

// TS: does not warn on run-together raw text even when recall is low, because the claim cannot be
// made (it stays a DEBUG line).
func TestConvertRunTogetherTextLogsDebugNotWarn(t *testing.T) {
	raw := distinctWords(60) + " DateNature valeurDebit soldeCrediteur"
	e := newEnv()
	e.ollama.textScripts = text(strings.Join(strings.Split(raw, " ")[:10], " "))

	if _, err := e.deps().ConvertRawTextToZeroLossMarkdown(raw, "fused-statement.pdf"); err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got := e.log.messages("WARN", "Content preservation"); len(got) != 0 {
		t.Fatalf("unexpected content preservation warnings: %v", got)
	}
	if got := e.log.messages("DEBUG", "Low token recall"); len(got) == 0 {
		t.Fatal("expected a DEBUG low-token-recall line")
	}
}

// TS: does not warn on fused raw text, where de-fusing legitimately loses raw tokens.
func TestConvertFusedTextDoesNotWarn(t *testing.T) {
	fused := strings.Repeat("Jemepermetsdevousadressermacandidature ", 30)
	e := newEnv()
	e.ollama.textScripts = text("Je me permets de vous adresser ma candidature.")

	if _, err := e.deps().ConvertRawTextToZeroLossMarkdown(fused, "lettre.pdf"); err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got := e.log.messages("WARN", "Content preservation"); len(got) != 0 {
		t.Fatalf("unexpected content preservation warnings: %v", got)
	}
}

var malformedChunk = strings.Join([]string{
	"| Période | Prix | Montant | TVA |",
	"| --- | --- | --- | --- |",
	"| Base - du 17/05/25 au 31/07/25 | 8,60 | 21,49 | 5,5% |",
	"Base - du 01/02/26 au 16/05/26 | 9,16 | 31,62 | 20,0%",
	"Base - du 17/05/26 au 15/06/26 | 9,16 | 9,16 | 20,0%",
}, "\n")

var fixedChunk = strings.Join([]string{
	"| Période | Prix | Montant | TVA |",
	"| --- | --- | --- | --- |",
	"| Base - du 17/05/25 au 31/07/25 | 8,60 | 21,49 | 5,5% |",
	"| Base - du 01/02/26 au 16/05/26 | 9,16 | 31,62 | 20,0% |",
	"| Base - du 17/05/26 au 15/06/26 | 9,16 | 9,16 | 20,0% |",
}, "\n")

// TS: re-converts ONLY the broken chunk (chunks + 1 generate calls), leaving others untouched.
func TestConvertTargetedRepairReconvertsOnlyBrokenChunk(t *testing.T) {
	rawText := multiChunkRawText("Line ")
	chunks := ChunkText(rawText, 1400)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want >= 2", len(chunks))
	}

	const target = 1
	e := newEnv()
	for i := range chunks {
		if i == target {
			e.ollama.textScripts = append(e.ollama.textScripts, textScript{res: ollama.TextCompletion{Response: malformedChunk, DoneReason: "stop"}})
		} else {
			e.ollama.textScripts = append(e.ollama.textScripts, textScript{res: ollama.TextCompletion{Response: "## Section " + itoa(i+1) + "\n\nClean prose without any pipes.", DoneReason: "stop"}})
		}
	}
	e.ollama.textScripts = append(e.ollama.textScripts, textScript{res: ollama.TextCompletion{Response: fixedChunk, DoneReason: "stop"}})

	markdown, err := e.deps().ConvertRawTextToZeroLossMarkdown(rawText, "edf.pdf")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(e.ollama.textCalls) != len(chunks)+1 {
		t.Fatalf("text calls = %d, want %d", len(e.ollama.textCalls), len(chunks)+1)
	}
	repairPrompt := e.ollama.textCalls[len(chunks)].user
	assertContains(t, repairPrompt, "⚠️ REPAIR REQUEST")
	assertContains(t, repairPrompt, "NOT well-formed GFM rows")
	assertContains(t, markdown, "| Base - du 01/02/26 au 16/05/26 | 9,16 | 31,62 | 20,0% |")
	if got := extractionqualitygate.CountMalformedPipeLines(markdown); got != 0 {
		t.Fatalf("malformed pipe lines = %d, want 0", got)
	}
}

// TS: keeps the first (content-preserving) output and stops when the repair still fails.
func TestConvertTargetedRepairKeepsFirstOutputWhenStillBroken(t *testing.T) {
	rawText := multiChunkRawText("Line ")
	chunks := ChunkText(rawText, 1400)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want >= 2", len(chunks))
	}

	const target = 1
	e := newEnv()
	for i := range chunks {
		if i == target {
			e.ollama.textScripts = append(e.ollama.textScripts, textScript{res: ollama.TextCompletion{Response: malformedChunk, DoneReason: "stop"}})
		} else {
			e.ollama.textScripts = append(e.ollama.textScripts, textScript{res: ollama.TextCompletion{Response: "## Section " + itoa(i+1) + "\n\nClean prose without any pipes.", DoneReason: "stop"}})
		}
	}
	e.ollama.textScripts = append(e.ollama.textScripts, textScript{res: ollama.TextCompletion{Response: malformedChunk, DoneReason: "stop"}})

	markdown, err := e.deps().ConvertRawTextToZeroLossMarkdown(rawText, "edf-still-broken.pdf")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(e.ollama.textCalls) != len(chunks)+1 {
		t.Fatalf("text calls = %d, want %d (one retry per broken chunk)", len(e.ollama.textCalls), len(chunks)+1)
	}
	if e.log.find("WARN", "still fails the structural screen") == nil {
		t.Fatal("expected a WARN that the repair still fails")
	}
	assertContains(t, markdown, "Base - du 01/02/26 au 16/05/26 | 9,16 | 31,62 | 20,0%")
}

// TS: keeps continuation rows in the same table block as their header (no blank-line split).
func TestConvertJoinsContinuedTableRows(t *testing.T) {
	rawText := multiChunkRawText("Line ")
	chunks := ChunkText(rawText, 1400)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want >= 2", len(chunks))
	}

	headerAndRows := strings.Join([]string{
		"## Transactions",
		"",
		"| Date | Montant | Label |",
		"| --- | --- | --- |",
		"| 2024-05-01 | 100.00 | Salaire |",
		"| 2024-05-02 | 200.00 | Loyer |",
	}, "\n")

	e := newEnv()
	e.ollama.textScripts = []textScript{{res: ollama.TextCompletion{Response: headerAndRows, DoneReason: "stop"}}}
	for i := 1; i < len(chunks); i++ {
		e.ollama.textScripts = append(e.ollama.textScripts, textScript{res: ollama.TextCompletion{Response: "| 2024-05-0" + itoa(i+2) + " | 300.00 | Facture |", DoneReason: "stop"}})
	}

	markdown, err := e.deps().ConvertRawTextToZeroLossMarkdown(rawText, "joined.pdf")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	audit := markdowntables.AuditMarkdownTables(markdown)
	if audit.HeaderlessBlocks != 0 {
		t.Errorf("headerlessBlocks = %d, want 0", audit.HeaderlessBlocks)
	}
	if audit.Blocks != 1 {
		t.Errorf("blocks = %d, want 1", audit.Blocks)
	}
	assertContains(t, markdown, "2024-05-0")
}

// TS: re-folds rows whose description escaped to a heading into one healthy table and keeps the
// footnote after it.
func TestConvertReattachesHeadingSplitRows(t *testing.T) {
	response := strings.Join([]string{
		"| Période | Prix €HT/mois | Montant €HT | TVA |",
		"| :--- | :---: | :---: | :---: |",
		"| **Base - 03kVA** du 17/05/25 au 31/07/25 | 8,60 | 21,49 | 5,5% |",
		"| **Base - 03kVA** du 01/08/25 au 31/01/26 | 8,51 | 51,48 | 20,0% |",
		"",
		"--- habituellement 2 fois par an, au 1er février et au 1er août. Vous pouvez retrouver la grille tarifaire en vigueur sur notre site : https://particulier.edf.fr/fr/accueil/electricite-gaz/tarif-bleu.html",
		"Nous vous rappelons que vous restez libre de changer de contrat à tout moment et sans frais.",
		"",
		"Document à conserver 5 ans",
		"",
		"## Base - 03kVA - du 01/02/26 au 16/05/26",
		"| 9,16 | 31,62 | 20,0% |",
		"",
		"## Base - 03kVA - du 17/05/26 au 15/06/26",
		"| 9,16 | 9,16 | 20,0% |",
		"",
		"## Déduction - Base - 03kVA - du 17/05/25 au 15/06/25",
		"| 8,60 | -8,60 | 5,5% |",
	}, "\n")

	e := newEnv()
	e.ollama.textScripts = []textScript{{res: ollama.TextCompletion{Response: response, DoneReason: "stop"}}}

	markdown, err := e.deps().ConvertRawTextToZeroLossMarkdown("Détail de la facture du 19/05/2026", "edf-5009.pdf")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	audit := markdowntables.AuditMarkdownTables(markdown)
	if audit.HeaderlessBlocks != 0 {
		t.Errorf("headerlessBlocks = %d, want 0", audit.HeaderlessBlocks)
	}
	if audit.RaggedRows != 0 {
		t.Errorf("raggedRows = %d, want 0", audit.RaggedRows)
	}
	if audit.Blocks != 1 {
		t.Errorf("blocks = %d, want 1", audit.Blocks)
	}
	if audit.DataRows != 5 {
		t.Errorf("dataRows = %d, want 5", audit.DataRows)
	}
	assertContains(t, markdown, "| Base - 03kVA - du 01/02/26 au 16/05/26 | 9,16 | 31,62 | 20,0% |")
	assertContains(t, markdown, "| Déduction - Base - 03kVA - du 17/05/25 au 15/06/25 | 8,60 | -8,60 | 5,5% |")
	if strings.Index(markdown, "| Déduction - Base - 03kVA - du 17/05/25 au 15/06/25") >= strings.Index(markdown, "habituellement 2 fois par an") {
		t.Fatal("the footnote must come after the complete table")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
