package extractionqualitygate

import (
	"errors"
	"strings"
	"testing"
)

// The cases below are ported verbatim from pdf-triage's
// src/domain/extraction-quality-gate.test.ts (15 `it` blocks). The upstream TypeScript suite is
// GREEN on the day of this port (`npx vitest run src/domain/extraction-quality-gate.test.ts` →
// 15 passed), so no red case is pinned here; the language-mechanics deviations are documented in
// the package comment of extractionqualitygate.go.

const cleanProse = "Bonjour Mademoiselle Palma, vous avez choisi la mensualisation pour régler vos factures " +
	"électricité et vous trouverez ci-joint votre facture de régularisation ainsi que votre bilan " +
	"personnalisé. Le montant total de votre facture correspond à la différence entre les montants " +
	"facturés et les prélèvements déjà effectués sur votre compte sur la période concernée."

var properTable = strings.Join([]string{
	"| Période | Prix | Montant | TVA |",
	"| --- | --- | --- | --- |",
	"| Base - du 17/05/25 au 31/07/25 | 8,60 | 21,49 | 5,5% |",
	"| Base - du 01/08/25 au 31/01/26 | 8,51 | 51,48 | 20,0% |",
}, "\n")

// The doc-5009 shape: rows that carry `|` cells but have no leading/trailing pipe.
var malformedRowsMD = strings.Join([]string{
	"## Détail de la facture",
	"",
	"| Période | Prix | Montant | TVA |",
	"| --- | --- | --- | --- |",
	"| Base - du 17/05/25 au 31/07/25 | 8,60 | 21,49 | 5,5% |",
	"Base - du 01/02/26 au 16/05/26 | 9,16 | 31,62 | 20,0%",
	"Base - du 17/05/26 au 15/06/26 | 9,16 | 9,16 | 20,0%",
	"Base - du 01/08/25 au 31/01/26 | 8,51 | 51,48 | 20,0%",
	"**Relevé fin**: Conso kWh | Prix €HT/kWh | Montant €HTTVA",
}, "\n")

// word reproduces the TS helper: a unique 6-letter content token ("mot" + a 3-letter base-26
// encoding of the index) — long enough for measureContentRecall's tokenizer, short enough to never
// look fused.
func word(i int) string {
	return "mot" +
		string(rune('a'+(i%26))) +
		string(rune('a'+((i/26)%26))) +
		string(rune('a'+((i/676)%26)))
}

func manyTokens(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = word(i)
	}
	return strings.Join(parts, " ")
}

func failureIDs(r QualityGateReport) []string {
	ids := make([]string, 0, len(r.Failures))
	for _, f := range r.Failures {
		ids = append(ids, f.ID)
	}
	return ids
}

func hasID(ids []string, id string) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("expected %q to contain %q", got, want)
	}
}

func TestCountMalformedPipeLines(t *testing.T) {
	t.Run("counts doc-5009-style rows that carry cells but are not well-formed GFM rows", func(t *testing.T) {
		if got := CountMalformedPipeLines(malformedRowsMD); got != 4 {
			t.Fatalf("CountMalformedPipeLines(malformedRowsMD) = %d, want 4", got)
		}
	})

	t.Run("ignores well-formed rows, separator rows and prose without pipes", func(t *testing.T) {
		if got := CountMalformedPipeLines(properTable); got != 0 {
			t.Fatalf("CountMalformedPipeLines(properTable) = %d, want 0", got)
		}
		if got := CountMalformedPipeLines("Just some prose without any pipes."); got != 0 {
			t.Fatalf("CountMalformedPipeLines(prose) = %d, want 0", got)
		}
		if got := CountMalformedPipeLines("| a | b |\n| --- | --- |\n| 1 | 2 |"); got != 0 {
			t.Fatalf("CountMalformedPipeLines(simple table) = %d, want 0", got)
		}
	})
}

func TestAssessChunkMarkdownDescribeTableRepairNote(t *testing.T) {
	t.Run("describes malformed rows so the retry prompt can correct them", func(t *testing.T) {
		note := DescribeTableRepairNote(malformedRowsMD)
		mustContain(t, note, "NOT well-formed GFM rows")
		mustContain(t, note, "Base - du 01/02/26 au 16/05/26")
		mustContain(t, note, "start with `|`")
	})

	t.Run("returns null for structurally clean chunk output", func(t *testing.T) {
		if got := DescribeTableRepairNote(properTable); got != "" {
			t.Fatalf("DescribeTableRepairNote(properTable) = %q, want \"\" (TS null)", got)
		}
		if got := DescribeTableRepairNote("# Heading\n\nSome prose."); got != "" {
			t.Fatalf("DescribeTableRepairNote(heading+prose) = %q, want \"\" (TS null)", got)
		}
	})

	t.Run("flags a chart/axis header blow-out as not-a-table", func(t *testing.T) {
		headers := make([]string, 30)
		for i := range headers {
			headers[i] = "c" + itoa(i)
		}
		separators := make([]string, 30)
		for i := range separators {
			separators[i] = "---"
		}
		wide := "| " + strings.Join(headers, " | ") + " |\n| " + strings.Join(separators, " | ") + " |"
		if got := WidestTableHeader(wide); got != 30 {
			t.Fatalf("WidestTableHeader(wide) = %d, want 30", got)
		}
		mustContain(t, DescribeTableRepairNote(wide), "chart axis")
	})

	t.Run("exposes per-chunk metrics", func(t *testing.T) {
		report := AssessChunkMarkdown(malformedRowsMD)
		if report.MalformedPipeLines != 4 {
			t.Fatalf("malformedPipeLines = %d, want 4", report.MalformedPipeLines)
		}
		if len(report.MalformedExamples) <= 0 {
			t.Fatalf("malformedExamples length = %d, want > 0", len(report.MalformedExamples))
		}
	})
}

func TestAssessExtractionQuality(t *testing.T) {
	t.Run("passes healthy text with a well-formed table", func(t *testing.T) {
		report := AssessExtractionQuality(cleanProse, cleanProse+"\n\n"+properTable)
		if !report.Pass {
			t.Fatalf("pass = false, want true (failures: %v)", failureIDs(report))
		}
		if len(report.Failures) != 0 {
			t.Fatalf("failures = %v, want []", failureIDs(report))
		}
	})

	t.Run("flags pure OCR/decoration noise text", func(t *testing.T) {
		noiseParts := make([]string, 60)
		for i := range noiseParts {
			noiseParts[i] = "S"
		}
		noise := strings.Join(noiseParts, " ")
		report := AssessExtractionQuality(noise, "")
		if report.Pass {
			t.Fatalf("pass = true, want false")
		}
		if !hasID(failureIDs(report), "text-ocr-noise") {
			t.Fatalf("failures = %v, want to contain text-ocr-noise", failureIDs(report))
		}
	})

	t.Run("flags markdown whose table rows are malformed (doc-5009 gate case)", func(t *testing.T) {
		report := AssessExtractionQuality(cleanProse, malformedRowsMD)
		if report.Pass {
			t.Fatalf("pass = true, want false")
		}
		if !hasID(failureIDs(report), "markdown-malformed-rows") {
			t.Fatalf("failures = %v, want to contain markdown-malformed-rows", failureIDs(report))
		}
	})

	t.Run("flags a table whose rows are ragged against their header", func(t *testing.T) {
		md := strings.Join([]string{
			"| Date | Heure | Numéro | Coût |",
			"| --- | --- | --- | --- |",
			"| 12/08 | 11:53 | 0612345678 | 0,00 |",
			"| 13/08 | 12:01 | 0612345679 | 1,00 |",
			"| 14/08 | 09:00 | 0612345680 |",
			"| 15/08 | 10:00 | 0612345681 |",
			"| 16/08 | 11:00 | 0612345682 |",
			"| 17/08 | 12:00 | 0612345683 |",
		}, "\n")
		report := AssessExtractionQuality(cleanProse, md)
		if report.Pass {
			t.Fatalf("pass = true, want false")
		}
		if !hasID(failureIDs(report), "markdown-ragged-table") {
			t.Fatalf("failures = %v, want to contain markdown-ragged-table", failureIDs(report))
		}
	})

	t.Run("flags orphan table blocks that lost their header across chunks", func(t *testing.T) {
		md := strings.Join([]string{
			"| 12/08 | 11:53 | 0,00 |",
			"| 13/08 | 12:01 | 0,00 |",
			"",
			"| 14/08 | 09:00 | 0,00 |",
			"| 15/08 | 10:00 | 0,00 |",
		}, "\n")
		report := AssessExtractionQuality(cleanProse, md)
		if report.Pass {
			t.Fatalf("pass = true, want false")
		}
		if !hasID(failureIDs(report), "markdown-headerless-table") {
			t.Fatalf("failures = %v, want to contain markdown-headerless-table", failureIDs(report))
		}
	})

	t.Run("flags heavy content loss into the markdown", func(t *testing.T) {
		// 80 distinctive source tokens, only the first 40 of which survive into the markdown.
		report := AssessExtractionQuality(manyTokens(80), manyTokens(40))
		if report.Metrics.ContentRecall == nil {
			t.Fatalf("metrics.contentRecall = nil, want a number")
		}
		if report.Pass {
			t.Fatalf("pass = true, want false")
		}
		if !hasID(failureIDs(report), "markdown-content-loss") {
			t.Fatalf("failures = %v, want to contain markdown-content-loss", failureIDs(report))
		}
	})

	t.Run("flags text that is still mid-word-capitalization corrupted", func(t *testing.T) {
		fillerParts := make([]string, 120)
		for i := range fillerParts {
			fillerParts[i] = "mot" + itoa(i%7)
		}
		filler := strings.Join(fillerParts, " ")
		corruptParts := make([]string, 12)
		for i := range corruptParts {
			corruptParts[i] = "kA" + itoa(i)
		}
		corrupt := filler + " " + strings.Join(corruptParts, " ")
		report := AssessExtractionQuality(corrupt, "")
		if report.Pass {
			t.Fatalf("pass = true, want false")
		}
		if !hasID(failureIDs(report), "text-still-corrupted") {
			t.Fatalf("failures = %v, want to contain text-still-corrupted", failureIDs(report))
		}
	})

	t.Run("remembers the malformed-row threshold for future calibration", func(t *testing.T) {
		if MdMalformedMinLines < 1 {
			t.Fatalf("MdMalformedMinLines = %d, want >= 1", MdMalformedMinLines)
		}
	})
}

func TestExtractionQualityGateError(t *testing.T) {
	t.Run("carries the structured report and a direct re-fix hint for agents", func(t *testing.T) {
		report := AssessExtractionQuality(cleanProse, malformedRowsMD)
		err := NewExtractionQualityGateError("facture.pdf", report)

		// TS `expect(err).toBeInstanceOf(Error)`.
		var asError error = err
		if asError == nil {
			t.Fatalf("ExtractionQualityGateError must implement error")
		}
		var as *ExtractionQualityGateError
		if !errors.As(err, &as) {
			t.Fatalf("errors.As failed for *ExtractionQualityGateError")
		}
		if as.Filename != "facture.pdf" {
			t.Fatalf("filename = %q, want facture.pdf", as.Filename)
		}
		if as.Report.Pass {
			t.Fatalf("report.pass = true, want false")
		}
		if len(as.Report.Failures) <= 0 {
			t.Fatalf("report.failures length = %d, want > 0", len(as.Report.Failures))
		}
		msg := as.Error()
		mustContain(t, msg, "markdown-malformed-rows")
		mustContain(t, msg, "'facture.pdf'")
		mustContain(t, msg, "Re-fix this file locally")
	})
}

// itoa is a tiny local integer formatter used only to build the TS test fixtures verbatim without
// importing strconv into the test's hot path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
