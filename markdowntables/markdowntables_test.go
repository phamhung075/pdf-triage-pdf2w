package markdowntables

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// The cases below are ported verbatim from pdf-triage's src/domain/markdown-tables.test.ts (42
// `it` blocks). One upstream case is RED and is handled explicitly; see
// TestRestoreMissingTableHeaderCells / "upstream-red trailing-width case" and the package comment
// for the evidence and reasoning.

const alpha = "abcdefghijklmnopqrstuvwxyz"

// many and distinct reproduce the TS test helpers of the same names exactly.
func many(n int, prefix string) string {
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = prefix + "ontent" +
			string(alpha[i%26]) +
			string(alpha[(i/26)%26]) +
			string(alpha[(i*7)%26])
	}
	return strings.Join(parts, " ")
}

func distinct(n int) string {
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = "libelle" +
			string(alpha[i%26]) +
			string(alpha[(i/26)%26]) +
			string(alpha[(i*5)%26])
	}
	return strings.Join(parts, " ")
}

func joinLines(lines ...string) string { return strings.Join(lines, "\n") }

func TestCountCells(t *testing.T) {
	t.Run("ignores the leading and trailing pipes", func(t *testing.T) {
		if got := CountCells("| a | b | c |"); got != 3 {
			t.Fatalf("CountCells(%q) = %d, want 3", "| a | b | c |", got)
		}
		if got := CountCells("| a |"); got != 1 {
			t.Fatalf("CountCells(%q) = %d, want 1", "| a |", got)
		}
	})
}

func TestAuditMarkdownTables(t *testing.T) {
	t.Run("reports a clean table as having no ragged rows", func(t *testing.T) {
		md := joinLines(
			"| Date | Coût |",
			"| --- | --- |",
			"| 12/08 | 1,00 |",
			"| 13/08 | 2,00 |",
		)
		r := AuditMarkdownTables(md)
		if r.Blocks != 1 {
			t.Fatalf("blocks = %d, want 1", r.Blocks)
		}
		if r.DataRows != 2 {
			t.Fatalf("dataRows = %d, want 2", r.DataRows)
		}
		if r.RaggedRows != 0 {
			t.Fatalf("raggedRows = %d, want 0", r.RaggedRows)
		}
		if r.WorstBlock != nil {
			t.Fatalf("worstBlock = %+v, want nil", r.WorstBlock)
		}
	})

	t.Run("catches the real Bouygues shape: 5-column header, 4-cell rows", func(t *testing.T) {
		md := joinLines(
			"| Date | Heure | Numéro appelé | Unité(s) décomptée(s) | Coût € TTC* |",
			"|:---|:---|:---|:---|:---|",
			"| 12/08 | 11:53:37 | 336528710 | 0,00 |",
			"| 12/08 | 11:54:33 | 336528710 | 0,00 |",
			"| 12/08 | 11:54:59 | 336528710 | 0,00 | 0,00 |",
		)
		r := AuditMarkdownTables(md)
		if r.DataRows != 3 {
			t.Fatalf("dataRows = %d, want 3", r.DataRows)
		}
		if r.RaggedRows != 2 {
			t.Fatalf("raggedRows = %d, want 2", r.RaggedRows)
		}
		if r.WorstBlock == nil {
			t.Fatalf("worstBlock is nil, want the Bouygues block")
		}
		if r.WorstBlock.HeaderCells != 5 {
			t.Fatalf("worstBlock.headerCells = %d, want 5", r.WorstBlock.HeaderCells)
		}
		if r.WorstBlock.RaggedRows != 2 {
			t.Fatalf("worstBlock.raggedRows = %d, want 2", r.WorstBlock.RaggedRows)
		}
	})

	t.Run("treats two separate tables as separate blocks, not as one ragged table", func(t *testing.T) {
		md := joinLines(
			"| A | B |",
			"| --- | --- |",
			"| 1 | 2 |",
			"",
			"| X | Y | Z |",
			"| --- | --- | --- |",
			"| 1 | 2 | 3 |",
		)
		r := AuditMarkdownTables(md)
		if r.Blocks != 2 {
			t.Fatalf("blocks = %d, want 2", r.Blocks)
		}
		// differing column counts across blocks is legitimate
		if r.RaggedRows != 0 {
			t.Fatalf("raggedRows = %d, want 0", r.RaggedRows)
		}
	})

	t.Run("counts a block with no separator as headerless rather than ragged", func(t *testing.T) {
		// A table continued across a chunk boundary: rows with no header above them.
		md := joinLines(
			"| 12/08 | 11:53 | 0,00 |",
			"| 13/08 | 12:01 | 0,00 |",
		)
		r := AuditMarkdownTables(md)
		if r.HeaderlessBlocks != 1 {
			t.Fatalf("headerlessBlocks = %d, want 1", r.HeaderlessBlocks)
		}
		if r.RaggedRows != 0 {
			t.Fatalf("raggedRows = %d, want 0", r.RaggedRows)
		}
		if r.DataRows != 0 {
			t.Fatalf("dataRows = %d, want 0", r.DataRows)
		}
	})

	t.Run("returns an empty report for markdown with no tables at all", func(t *testing.T) {
		r := AuditMarkdownTables("# Heading\n\nSome prose.\n")
		if r.Blocks != 0 {
			t.Fatalf("blocks = %d, want 0", r.Blocks)
		}
		if r.DataRows != 0 {
			t.Fatalf("dataRows = %d, want 0", r.DataRows)
		}
		if r.RaggedRows != 0 {
			t.Fatalf("raggedRows = %d, want 0", r.RaggedRows)
		}
	})

	t.Run("handles empty and undefined input", func(t *testing.T) {
		if got := AuditMarkdownTables("").Blocks; got != 0 {
			t.Fatalf("blocks = %d, want 0", got)
		}
		// TS passes `undefined as any`; a Go string has no undefined, and `(markdown || '')`
		// collapses it to "" — the same call.
		if got := AuditMarkdownTables("").Blocks; got != 0 {
			t.Fatalf("blocks = %d, want 0", got)
		}
	})
}

func TestMeasureContentRecall(t *testing.T) {
	t.Run("scores full recall when every content token survives", func(t *testing.T) {
		raw := many(60, "se")
		r := MeasureContentRecall(raw, "# Heading\n\n"+raw)
		if !r.Measurable {
			t.Fatalf("measurable = false, want true")
		}
		if r.Recall != 1 {
			t.Fatalf("recall = %v, want 1", r.Recall)
		}
		if r.MissingTokens != 0 {
			t.Fatalf("missingTokens = %d, want 0", r.MissingTokens)
		}
	})

	t.Run("detects dropped tokens", func(t *testing.T) {
		kept := many(50, "se")
		// Under 15 chars each: long runs are treated as fusion artifacts and deliberately ignored.
		dropped := "consommateur resiliation penalite"
		r := MeasureContentRecall(kept+" "+dropped, kept)
		if !r.Measurable {
			t.Fatalf("measurable = false, want true")
		}
		if r.MissingTokens != 3 {
			t.Fatalf("missingTokens = %d, want 3", r.MissingTokens)
		}
		if r.Recall >= 1 {
			t.Fatalf("recall = %v, want < 1", r.Recall)
		}
	})

	t.Run("refuses to measure heavily fused raw text, where de-fusing is the right behaviour", func(t *testing.T) {
		// No spaces at all: raw tokens are unmatchable by construction, and the markdown is BETTER.
		fused := strings.Repeat("Jemepermetsdevousadressermacandidaturepourunstageauseindevotreentrepriseaveclobjectif", 4)
		r := MeasureContentRecall(fused, "Je me permets de vous adresser ma candidature pour un stage.")
		if r.AvgWordLength <= FusedTextAvgWordLen {
			t.Fatalf("avgWordLength = %v, want > %v", r.AvgWordLength, FusedTextAvgWordLen)
		}
		if r.Measurable {
			t.Fatalf("measurable = true, want false")
		}
	})

	t.Run("refuses to measure a document with too few tokens to be meaningful", func(t *testing.T) {
		r := MeasureContentRecall("Attestation de domicile signee", "Attestation")
		if r.Measurable {
			t.Fatalf("measurable = true, want false")
		}
	})

	t.Run("ignores markup added by the conversion", func(t *testing.T) {
		raw := many(60, "se")
		words := strings.Split(raw, " ")
		for i, w := range words {
			words[i] = fmt.Sprintf("| %s |", w)
		}
		md := strings.Join(words, "\n")
		r := MeasureContentRecall(raw, "| Header |\n| --- |\n"+md)
		if r.Recall != 1 {
			t.Fatalf("recall = %v, want 1 (pipes and dashes are not content tokens)", r.Recall)
		}
	})

	t.Run("handles empty input without throwing", func(t *testing.T) {
		r := MeasureContentRecall("", "")
		if r.Measurable {
			t.Fatalf("measurable = true, want false")
		}
		if r.Recall != 1 {
			t.Fatalf("recall = %v, want 1", r.Recall)
		}
	})
}

func TestMeasureContentRecallFusionDetection(t *testing.T) {
	t.Run("skips a statement that is fused despite a modest average token length", func(t *testing.T) {
		// The real RCHQ_101 shape: surviving spaces keep the mean at ~19, well under the 25 cutoff,
		// while half the tokens are glued-together runs.
		raw := "DateNaturedesoperationsValeurDebitCredit 99999LIEUXXXX 12RUEQUELQUEPART " +
			"CHAMBRE1BATIMENTB MRNOMPRENOMX 0000000000 Agence: VILLERONDPO ELEVEDECOMPTECHEQUESR " +
			"du06avril2010au06mai2010 RIB: 00000000000000000000000 IBAN: FR7600000000000000000000000"
		r := MeasureContentRecall(raw, "## Relevé de compte\n\n17 avenue de Luminy")
		// the old guard would have missed it
		if r.AvgWordLength >= FusedTextAvgWordLen {
			t.Fatalf("avgWordLength = %v, want < %v", r.AvgWordLength, FusedTextAvgWordLen)
		}
		if r.Measurable {
			t.Fatalf("measurable = true, want false")
		}
	})

	t.Run("still measures a clean statement that merely contains long account numbers", func(t *testing.T) {
		// Alphabetic only: the tokenizer's letter class stops at a digit, so "libelleA0" would collapse.
		words := distinct(80)
		raw := words + " 00000000000000000000000 FR7600000000000000000000000"
		r := MeasureContentRecall(raw, raw)
		if !r.Measurable {
			t.Fatalf("measurable = false, want true")
		}
		if r.Recall != 1 {
			t.Fatalf("recall = %v, want 1", r.Recall)
		}
	})
}

func TestMeasureContentRecallFusionArtifacts(t *testing.T) {
	t.Run("ignores a missing token that is a run-together word", func(t *testing.T) {
		kept := distinct(60)
		// The model split this into real words, so the glued form legitimately disappears.
		raw := kept + " evolutionsmensuellesdevotrecomptecheques"
		r := MeasureContentRecall(raw, kept)
		if r.MissingTokens != 0 {
			t.Fatalf("missingTokens = %d, want 0", r.MissingTokens)
		}
		if r.Recall != 1 {
			t.Fatalf("recall = %v, want 1", r.Recall)
		}
	})

	t.Run("ignores a missing numeric run carrying several decimal commas", func(t *testing.T) {
		kept := distinct(60)
		raw := kept + " 00,71039,92139,211"
		r := MeasureContentRecall(raw, kept)
		if r.MissingTokens != 0 {
			t.Fatalf("missingTokens = %d, want 0", r.MissingTokens)
		}
	})

	t.Run("still counts an ordinary word that vanished", func(t *testing.T) {
		kept := distinct(60)
		raw := kept + " consommateur resiliation penalite"
		r := MeasureContentRecall(raw, kept)
		if r.MissingTokens != 3 {
			t.Fatalf("missingTokens = %d, want 3", r.MissingTokens)
		}
	})

	t.Run("flags fusion as suspected when the text has camel-case seams but is still measurable", func(t *testing.T) {
		kept := distinct(60)
		r := MeasureContentRecall(kept+" DateNature valeurDebit", kept+" DateNature valeurDebit")
		if !r.Measurable {
			t.Fatalf("measurable = false, want true")
		}
		if !r.FusionSuspected {
			t.Fatalf("fusionSuspected = false, want true")
		}
	})

	t.Run("does not suspect fusion in ordinary clean text", func(t *testing.T) {
		kept := distinct(60)
		r := MeasureContentRecall(kept, kept)
		if r.FusionSuspected {
			t.Fatalf("fusionSuspected = true, want false")
		}
	})
}

func TestNeutralizeChartLikeTables(t *testing.T) {
	t.Run("leaves a real table untouched", func(t *testing.T) {
		md := joinLines(
			"| Date | Coût |",
			"| --- | --- |",
			"| 12/08 | 1,00 |",
		)
		r := NeutralizeChartLikeTables(md)
		if r.Markdown != md {
			t.Fatalf("markdown changed:\n%s", r.Markdown)
		}
		if r.NeutralizedBlocks != 0 {
			t.Fatalf("neutralizedBlocks = %d, want 0", r.NeutralizedBlocks)
		}
	})

	t.Run("converts the doc-5009 shape — a huge axis header with empty data rows — into a verbatim blockquote", func(t *testing.T) {
		// 20 "columns" of chart axis fragments; the single data row is all empty cells.
		headerParts := make([]string, 20)
		for i := 0; i < 20; i++ {
			switch i % 3 {
			case 0:
				headerParts[i] = fmt.Sprintf("Mar2%d", i)
			case 1:
				headerParts[i] = "de"
			default:
				headerParts[i] = "à"
			}
		}
		header := strings.Join(headerParts, " | ")
		emptyParts := make([]string, 20)
		for i := range emptyParts {
			emptyParts[i] = ""
		}
		emptyRow := strings.Join(emptyParts, " | ")
		dashParts := make([]string, 20)
		for i := range dashParts {
			dashParts[i] = "---"
		}
		dashes := strings.Join(dashParts, " | ")
		md := joinLines(
			"## Evolution de votre consommation",
			"| "+header+" |",
			"| "+dashes+" |",
			"| "+emptyRow+" |",
		)

		r := NeutralizeChartLikeTables(md)
		if r.NeutralizedBlocks != 1 {
			t.Fatalf("neutralizedBlocks = %d, want 1", r.NeutralizedBlocks)
		}
		if r.NeutralizedLines != 3 {
			t.Fatalf("neutralizedLines = %d, want 3", r.NeutralizedLines)
		}
		// Every original line survives — prefixed, never dropped (zero content skipping).
		if !strings.Contains(r.Markdown, "Mar20") {
			t.Fatalf("markdown lost Mar20:\n%s", r.Markdown)
		}
		if !strings.Contains(r.Markdown, "> ⚠️ [Wide non-tabular region (20 columns)") {
			t.Fatalf("markdown missing neutralizer marker:\n%s", r.Markdown)
		}
		// The blockquote lines are no longer audit-counted as a (healthy) table.
		if got := AuditMarkdownTables(r.Markdown).Blocks; got != 0 {
			t.Fatalf("audit blocks = %d, want 0", got)
		}
	})

	t.Run("keeps a block at or below the threshold as a real table", func(t *testing.T) {
		width := ChartTableMaxHeaderCells - 1
		headerParts := make([]string, width)
		for i := range headerParts {
			headerParts[i] = fmt.Sprintf("col%d", i)
		}
		header := strings.Join(headerParts, " | ")
		dashParts := make([]string, width)
		for i := range dashParts {
			dashParts[i] = "---"
		}
		dashes := strings.Join(dashParts, " | ")
		valueParts := make([]string, width)
		for i := range valueParts {
			valueParts[i] = fmt.Sprintf("v%d", i)
		}
		values := strings.Join(valueParts, " | ")
		md := joinLines(
			"| "+header+" |",
			"| "+dashes+" |",
			"| "+values+" |",
		)
		r := NeutralizeChartLikeTables(md)
		if r.NeutralizedBlocks != 0 {
			t.Fatalf("neutralizedBlocks = %d, want 0", r.NeutralizedBlocks)
		}
		if r.Markdown != md {
			t.Fatalf("markdown changed:\n%s", r.Markdown)
		}
	})

	t.Run("handles empty input", func(t *testing.T) {
		r := NeutralizeChartLikeTables("")
		if r.NeutralizedBlocks != 0 {
			t.Fatalf("neutralizedBlocks = %d, want 0", r.NeutralizedBlocks)
		}
		if r.Markdown != "" {
			t.Fatalf("markdown = %q, want empty", r.Markdown)
		}
	})
}

func TestNormalizeMalformedPipeRows(t *testing.T) {
	t.Run("restores the missing edge pipes on doc-5009-style rows and leaves well-formed rows alone", func(t *testing.T) {
		md := joinLines(
			"| Période | Prix | Montant | TVA |",
			"| --- | --- | --- | --- |",
			"| Base - du 17/05/25 au 31/07/25 | 8,60 | 21,49 | 5,5% |",
			"Base - du 01/02/26 au 16/05/26 | 9,16 | 31,62 | 20,0%",
			"Base - du 17/05/26 au 15/06/26 | 9,16 | 9,16 | 20,0%",
		)
		r := NormalizeMalformedPipeRows(md)
		if r.FixedLines != 2 {
			t.Fatalf("fixedLines = %d, want 2", r.FixedLines)
		}
		if !strings.Contains(r.Markdown, "| Base - du 01/02/26 au 16/05/26 | 9,16 | 31,62 | 20,0% |") {
			t.Fatalf("markdown missing repaired row:\n%s", r.Markdown)
		}
		if !strings.Contains(r.Markdown, "| Base - du 17/05/26 au 15/06/26 | 9,16 | 9,16 | 20,0% |") {
			t.Fatalf("markdown missing repaired row:\n%s", r.Markdown)
		}
		// The repaired rows now belong to the table as far as the audit is concerned.
		if got := AuditMarkdownTables(r.Markdown).RaggedRows; got != 0 {
			t.Fatalf("audit raggedRows = %d, want 0", got)
		}
	})

	t.Run("never touches headings, blockquotes, lists or bold labels that merely contain pipes", func(t *testing.T) {
		md := joinLines(
			"## Base - du 01/02/26 | suite",
			"> **Relevé fin**: Conso kWh | Prix €HT/kWh | Montant €HT",
			"- alpha | beta",
			"**Montant €HT** | 21,49 | TVA",
		)
		r := NormalizeMalformedPipeRows(md)
		if r.FixedLines != 0 {
			t.Fatalf("fixedLines = %d, want 0", r.FixedLines)
		}
		if r.Markdown != md {
			t.Fatalf("markdown changed:\n%s", r.Markdown)
		}
	})

	t.Run("returns the markdown unchanged when there is nothing to fix", func(t *testing.T) {
		md := "| A | B |\n| --- | --- |\n| 1 | 2 |"
		r := NormalizeMalformedPipeRows(md)
		if r.FixedLines != 0 {
			t.Fatalf("fixedLines = %d, want 0", r.FixedLines)
		}
		if r.Markdown != md {
			t.Fatalf("markdown changed:\n%s", r.Markdown)
		}
	})
}

func TestMergeHeaderlessContinuationBlocks(t *testing.T) {
	t.Run("re-joins headerless rows that directly follow a matching-width table across a blank line", func(t *testing.T) {
		md := joinLines(
			"| Date | Montant | Label |",
			"| --- | --- | --- |",
			"| 2024-05-01 | 100.00 | Salaire |",
			"",
			"| 2024-05-02 | 200.00 | Loyer |",
			"| 2024-05-03 | 300.00 | EDF |",
		)
		r := MergeHeaderlessContinuationBlocks(md)
		if r.MergedBlocks != 1 {
			t.Fatalf("mergedBlocks = %d, want 1", r.MergedBlocks)
		}
		audit := AuditMarkdownTables(r.Markdown)
		if audit.HeaderlessBlocks != 0 {
			t.Fatalf("audit headerlessBlocks = %d, want 0", audit.HeaderlessBlocks)
		}
		if audit.Blocks != 1 {
			t.Fatalf("audit blocks = %d, want 1", audit.Blocks)
		}
		if audit.DataRows != 3 {
			t.Fatalf("audit dataRows = %d, want 3", audit.DataRows)
		}
	})

	t.Run("does not merge when the widths differ (a genuinely different table)", func(t *testing.T) {
		md := joinLines(
			"| Date | Montant |",
			"| --- | --- |",
			"| 2024-05-01 | 100.00 |",
			"",
			"| 2024-05-02 | 200.00 | 300.00 | Autre table |",
		)
		r := MergeHeaderlessContinuationBlocks(md)
		if r.MergedBlocks != 0 {
			t.Fatalf("mergedBlocks = %d, want 0", r.MergedBlocks)
		}
	})

	t.Run("leaves markdown without orphan row blocks untouched", func(t *testing.T) {
		md := "| A | B |\n| --- | --- |\n| 1 | 2 |"
		r := MergeHeaderlessContinuationBlocks(md)
		if r.MergedBlocks != 0 {
			t.Fatalf("mergedBlocks = %d, want 0", r.MergedBlocks)
		}
		if r.Markdown != md {
			t.Fatalf("markdown changed:\n%s", r.Markdown)
		}
	})
}

func TestReattachHeadingSplitTableRows(t *testing.T) {
	// doc 5009's real shape: the Grille tarifaire table's continuation rows came back with their
	// descriptions promoted to headings and only the value cells left as a 3-cell row, with the EDF
	// footnote paragraph sitting between the table's first two rows and the escaped rows.
	doc5009Shape := func() string {
		return joinLines(
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
		)
	}

	t.Run("re-folds heading-promoted rows back into their parent table across a footnote (doc-5009 shape)", func(t *testing.T) {
		r := ReattachHeadingSplitTableRows(doc5009Shape())
		if r.ReattachedRows != 3 {
			t.Fatalf("reattachedRows = %d, want 3", r.ReattachedRows)
		}

		audit := AuditMarkdownTables(r.Markdown)
		if audit.Blocks != 1 { // the footnote no longer splits one table into blocks
			t.Fatalf("audit blocks = %d, want 1", audit.Blocks)
		}
		if audit.HeaderlessBlocks != 0 {
			t.Fatalf("audit headerlessBlocks = %d, want 0", audit.HeaderlessBlocks)
		}
		if audit.RaggedRows != 0 {
			t.Fatalf("audit raggedRows = %d, want 0", audit.RaggedRows)
		}
		if audit.DataRows != 5 {
			t.Fatalf("audit dataRows = %d, want 5", audit.DataRows)
		}

		// The escaped rows are now full rows inside the table, description restored as first cell.
		if !strings.Contains(r.Markdown, "| Base - 03kVA - du 01/02/26 au 16/05/26 | 9,16 | 31,62 | 20,0% |") {
			t.Fatalf("markdown missing re-folded row:\n%s", r.Markdown)
		}
		if !strings.Contains(r.Markdown, "| Base - 03kVA - du 17/05/26 au 15/06/26 | 9,16 | 9,16 | 20,0% |") {
			t.Fatalf("markdown missing re-folded row:\n%s", r.Markdown)
		}
		if !strings.Contains(r.Markdown, "| Déduction - Base - 03kVA - du 17/05/25 au 15/06/25 | 8,60 | -8,60 | 5,5% |") {
			t.Fatalf("markdown missing re-folded row:\n%s", r.Markdown)
		}
		// No heading promotion remains for those rows.
		if strings.Contains(r.Markdown, "## Base - 03kVA - du 01/02/26") {
			t.Fatalf("heading promotion still present:\n%s", r.Markdown)
		}
		// The footnote stays in the document, AFTER the whole table (content is moved, never dropped).
		if !(strings.Index(r.Markdown, "| Déduction - Base - 03kVA - du 17/05/25 au 15/06/25") <
			strings.Index(r.Markdown, "--- habituellement 2 fois par an")) {
			t.Fatalf("deduction row did not move before the footnote:\n%s", r.Markdown)
		}
		if !strings.Contains(r.Markdown, "Document à conserver 5 ans") {
			t.Fatalf("markdown lost trailing prose:\n%s", r.Markdown)
		}
	})

	// doc 5009's Grille tarifaire on a run where the model ALSO dropped the leading "Période" header
	// cell: the header is "| Prix €HT/mois | Montant €HTTVA | TVA |" (3 cells) while every data row
	// carries its description cell (4 cells). The fold must match the DATA width, not the header —
	// a strict header match would silently skip the escape.
	doc5009HeaderLostShape := func() string {
		return joinLines(
			"| Prix €HT/mois | Montant €HTTVA | TVA |",
			"| :--- | :--- | :--- |",
			"| Abonnement<br>Base - 03kVA - du 17/05/25 au 31/07/25 | 8,60 | 21,49 | 5,5% |",
			"| Base - 03kVA - du 01/08/25 au 31/01/26 | 8,51 | 51,48 | 20,0% |",
			"",
			"## Base - 03kVA - du 01/02/26 au 16/05/26",
			"| 9,16 | 31,62 | 20,0% |",
			"",
			"## Base - 03kVA - du 17/05/26 au 15/06/26",
			"| 9,16 | 9,16 | 20,0% |",
			"",
			"## Déduction - Base - 03kVA - du 17/05/25 au 15/06/25",
			"| 8,60 | -8,60 | 5,5% |",
		)
	}

	t.Run("re-folds escaped rows even when the parent header lost its leading column (doc-5009 run variant)", func(t *testing.T) {
		r := ReattachHeadingSplitTableRows(doc5009HeaderLostShape())
		if r.ReattachedRows != 3 {
			t.Fatalf("reattachedRows = %d, want 3", r.ReattachedRows)
		}

		audit := AuditMarkdownTables(r.Markdown)
		if audit.Blocks != 1 {
			t.Fatalf("audit blocks = %d, want 1", audit.Blocks)
		}
		if audit.HeaderlessBlocks != 0 {
			t.Fatalf("audit headerlessBlocks = %d, want 0", audit.HeaderlessBlocks)
		}
		if audit.DataRows != 5 {
			t.Fatalf("audit dataRows = %d, want 5", audit.DataRows)
		}

		// The escaped rows are back inside the table, description restored as first cell.
		if !strings.Contains(r.Markdown, "| Base - 03kVA - du 01/02/26 au 16/05/26 | 9,16 | 31,62 | 20,0% |") {
			t.Fatalf("markdown missing re-folded row:\n%s", r.Markdown)
		}
		if !strings.Contains(r.Markdown, "| Déduction - Base - 03kVA - du 17/05/25 au 15/06/25 | 8,60 | -8,60 | 5,5% |") {
			t.Fatalf("markdown missing re-folded row:\n%s", r.Markdown)
		}
		if strings.Contains(r.Markdown, "## Base - 03kVA - du 01/02/26") {
			t.Fatalf("heading promotion still present:\n%s", r.Markdown)
		}
		// Content that was never part of an escape is untouched (first two rows keep their spacing).
		if !strings.Contains(r.Markdown, "| Abonnement<br>Base - 03kVA - du 17/05/25 au 31/07/25 | 8,60 | 21,49 | 5,5% |") {
			t.Fatalf("markdown altered untouched row:\n%s", r.Markdown)
		}
	})

	t.Run("leaves a heading that introduces its own small textual table alone", func(t *testing.T) {
		md := joinLines(
			"| Date | Montant | Label |",
			"| --- | --- | --- |",
			"| 2024-05-01 | 100.00 | Salaire |",
			"",
			"## Récapitulatif",
			"| Total | 1 234,56 € |", // width matches the 3-col table above, but cells are NOT all values
		)
		r := ReattachHeadingSplitTableRows(md)
		if r.ReattachedRows != 0 {
			t.Fatalf("reattachedRows = %d, want 0", r.ReattachedRows)
		}
		if r.Markdown != md {
			t.Fatalf("markdown changed:\n%s", r.Markdown)
		}
	})

	t.Run("leaves a heading followed by several rows alone (a real section + table)", func(t *testing.T) {
		md := joinLines(
			"| Date | Montant |",
			"| --- | --- |",
			"| 2024-05-01 | 100.00 |",
			"",
			"## Paiements suivants",
			"| 2024-05-02 | 200.00 |",
			"| 2024-05-03 | 300.00 |",
		)
		r := ReattachHeadingSplitTableRows(md)
		if r.ReattachedRows != 0 {
			t.Fatalf("reattachedRows = %d, want 0", r.ReattachedRows)
		}
		if r.Markdown != md {
			t.Fatalf("markdown changed:\n%s", r.Markdown)
		}
	})

	t.Run("leaves a lone numeric row with no matching-width table above untouched", func(t *testing.T) {
		md := joinLines(
			"## Récapitulatif",
			"| 9,16 | 31,62 | 20,0% |",
		)
		r := ReattachHeadingSplitTableRows(md)
		if r.ReattachedRows != 0 {
			t.Fatalf("reattachedRows = %d, want 0", r.ReattachedRows)
		}
		if r.Markdown != md {
			t.Fatalf("markdown changed:\n%s", r.Markdown)
		}
	})

	t.Run("does not reach across a far-away matching table to steal an unrelated row", func(t *testing.T) {
		gapParts := make([]string, 15)
		for i := range gapParts {
			gapParts[i] = "Paragraphe intermediaire numero " + strconv.Itoa(i) + "."
		}
		md := joinLines(
			"| A | B | C | D |",
			"| --- | --- | --- | --- |",
			"| 1 | 2 | 3 | 4 |",
			"",
			strings.Join(gapParts, "\n"),
			"",
			"## Base - 03kVA - du 01/02/26 au 16/05/26",
			"| 9,16 | 31,62 | 20,0% |",
		)
		r := ReattachHeadingSplitTableRows(md)
		if r.ReattachedRows != 0 {
			t.Fatalf("reattachedRows = %d, want 0", r.ReattachedRows)
		}
		if r.Markdown != md {
			t.Fatalf("markdown changed:\n%s", r.Markdown)
		}
	})

	t.Run("handles empty and clean input as a no-op", func(t *testing.T) {
		if got := ReattachHeadingSplitTableRows("").ReattachedRows; got != 0 {
			t.Fatalf("reattachedRows = %d, want 0", got)
		}
		clean := "| A | B |\n| --- | --- |\n| 1 | 2 |"
		r := ReattachHeadingSplitTableRows(clean)
		if r.ReattachedRows != 0 {
			t.Fatalf("reattachedRows = %d, want 0", r.ReattachedRows)
		}
		if r.Markdown != clean {
			t.Fatalf("markdown changed:\n%s", r.Markdown)
		}
	})
}

func TestRestoreMissingTableHeaderCells(t *testing.T) {
	// doc 5009's Grille tarifaire on a run where the model dropped the leading header cell: the
	// header claims only the numeric columns ("| Prix €HT/mois | Montant €HTTVA | TVA |") while every
	// data row still carries its description cell. No heading escaped here, so only this pass can
	// restore the width.
	shortHeaderShape := func() string {
		return joinLines(
			"| Prix €HT/mois | Montant €HTTVA | TVA |",
			"| :--- | :--- | :--- |",
			"| Abonnement<br>Base - 03kVA - du 17/05/25 au 31/07/25 | 8,60 | 21,49 | 5,5% |",
			"| Base - 03kVA - du 01/08/25 au 31/01/26 | 8,51 | 51,48 | 20,0% |",
			"| Base - 03kVA - du 01/02/26 au 16/05/26 | 9,16 | 31,62 | 20,0% |",
			"| Base - 03kVA - du 17/05/26 au 15/06/26 | 9,16 | 9,16 | 20,0% |",
			"| Déduction - Base - 03kVA - du 17/05/25 au 15/06/25 | 8,60 | -8,60 | 5,5% |",
		)
	}

	t.Run("gives a header that is one cell short of every row its missing leading cell back (doc-5009 shape)", func(t *testing.T) {
		r := RestoreMissingTableHeaderCells(shortHeaderShape())
		if r.RestoredHeaders != 1 {
			t.Fatalf("restoredHeaders = %d, want 1", r.RestoredHeaders)
		}

		audit := AuditMarkdownTables(r.Markdown)
		if audit.Blocks != 1 {
			t.Fatalf("audit blocks = %d, want 1", audit.Blocks)
		}
		if audit.HeaderlessBlocks != 0 {
			t.Fatalf("audit headerlessBlocks = %d, want 0", audit.HeaderlessBlocks)
		}
		if audit.RaggedRows != 0 {
			t.Fatalf("audit raggedRows = %d, want 0", audit.RaggedRows)
		}
		if audit.DataRows != 5 {
			t.Fatalf("audit dataRows = %d, want 5", audit.DataRows)
		}

		if !strings.Contains(r.Markdown, "|  | Prix €HT/mois | Montant €HTTVA | TVA |") {
			t.Fatalf("markdown missing padded header:\n%s", r.Markdown)
		}
		// Separator is padded in step with the header so the block stays one aligned table.
		if !strings.Contains(r.Markdown, "|  | :--- | :--- | :--- |") {
			t.Fatalf("markdown missing padded separator:\n%s", r.Markdown)
		}
		// Data rows are untouched — only the header/separator width changes.
		if !strings.Contains(r.Markdown, "| Base - 03kVA - du 17/05/26 au 15/06/26 | 9,16 | 9,16 | 20,0% |") {
			t.Fatalf("markdown altered a data row:\n%s", r.Markdown)
		}
	})

	t.Run("leaves healthy tables whose rows match their header alone", func(t *testing.T) {
		md := joinLines(
			"| Période | Prix €HT/mois | Montant €HT | TVA |",
			"| :--- | :--- | :--- | :--- |",
			"| Base - 03kVA | 8,60 | 21,49 | 5,5% |",
			"| Base - 03kVA | 8,51 | 51,48 | 20,0% |",
		)
		r := RestoreMissingTableHeaderCells(md)
		if r.RestoredHeaders != 0 {
			t.Fatalf("restoredHeaders = %d, want 0", r.RestoredHeaders)
		}
		if r.Markdown != md {
			t.Fatalf("markdown changed:\n%s", r.Markdown)
		}
	})

	t.Run("leaves a block whose rows are ragged in a different way alone", func(t *testing.T) {
		// One row is one cell short of the header, not one cell wider — a shifted-value shape the
		// model must fix, not a missing header label.
		md := joinLines(
			"| A | B | C |",
			"| --- | --- | --- |",
			"| 1 | 2 | 3 |",
			"| 4 | 5 |",
		)
		r := RestoreMissingTableHeaderCells(md)
		if r.RestoredHeaders != 0 {
			t.Fatalf("restoredHeaders = %d, want 0", r.RestoredHeaders)
		}
		if r.Markdown != md {
			t.Fatalf("markdown changed:\n%s", r.Markdown)
		}
	})

	// UPSTREAM-RED CASE — ported for completeness, not silently dropped.
	//
	// The TS test of this name (src/domain/markdown-tables.test.ts:542-554) expects
	// restoredHeaders == 0, but the TS implementation returns 1 and pads the header. The fixture
	// contradicts BOTH the test title ("whose first cells are VALUES") and the function's own
	// documented signature (FIRST cell NOT a value, last cell a pure value): "Salaire"/"Prime" are
	// not value cells and "5,5%" is, so the TS gates accept the block. The upstream suite confirms
	// the test is red:
	//
	//	src/domain/markdown-tables.test.ts (42 tests | 1 failed)
	//	× leaves a block whose first cells are values alone (extra width is trailing, not a lost label column)
	//	  AssertionError: expected 1 to be +0
	//
	// No deterministic local rule pads the intended doc-5009 shape while refusing this structurally
	// identical one, so the port resolves in favor of the TS IMPLEMENTATION (the behavioral source
	// of truth) and locks its actual output here. See the package comment.
	t.Run("upstream-red trailing-width case: matches the TS implementation, not the TS test", func(t *testing.T) {
		md := joinLines(
			"| Libellé | Montant |",
			"| --- | --- |",
			"| Salaire | 1 200,00 | 5,5% |",
			"| Prime | 300,00 | 5,5% |",
		)
		r := RestoreMissingTableHeaderCells(md)
		// The TS test asserts 0 here; the TS code returns 1. The port follows the code.
		if r.RestoredHeaders != 1 {
			t.Fatalf("restoredHeaders = %d, want 1 (the TS implementation's actual output)", r.RestoredHeaders)
		}
		want := joinLines(
			"|  | Libellé | Montant |",
			"|  | --- | --- |",
			"| Salaire | 1 200,00 | 5,5% |",
			"| Prime | 300,00 | 5,5% |",
		)
		if r.Markdown != want {
			t.Fatalf("markdown = %q, want %q", r.Markdown, want)
		}
	})

	t.Run("handles empty and headerless input as a no-op", func(t *testing.T) {
		if got := RestoreMissingTableHeaderCells("").RestoredHeaders; got != 0 {
			t.Fatalf("restoredHeaders = %d, want 0", got)
		}
		headerless := "| 1 | 2 |\n| 3 | 4 |" // no separator -> no header to compare
		r := RestoreMissingTableHeaderCells(headerless)
		if r.RestoredHeaders != 0 {
			t.Fatalf("restoredHeaders = %d, want 0", r.RestoredHeaders)
		}
		if r.Markdown != headerless {
			t.Fatalf("markdown changed:\n%s", r.Markdown)
		}
	})
}
