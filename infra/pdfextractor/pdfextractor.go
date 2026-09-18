// Package pdfextractor is a Go port of pdf-triage's src/infrastructure/pdf-extractor.ts (58 lines):
// the ExtractedPDF shape, sanitizeDocumentNoise and extractPDFContent.
//
// The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/pdf-extractor.test.ts` -> 3 passed and
// `npx vitest run src/application/micro-prompt-pipeline.test.ts` -> 10 passed, the latter carrying
// the only sanitizeDocumentNoise case). No upstream case is pinned red. All three extractor cases and
// that one sanitize case are ported; added cases cover the in-process-cleaning guarantee, HTTP-error
// and timeout propagation, the optional logger hook, and whitespace/case normalization.
//
// Extraction delegates to infra/pdf2w exactly as the TS module delegated to pdf2w-remote.ts. The
// clean-text step, however, is IN-PROCESS here: TS called cleanExtractedTextRemote (an HTTP POST to
// the pdf-triage-pdf2w service's /clean-text endpoint, src/infrastructure/clean-text-remote.ts),
// while this port imports the existing Go `cleantext` package directly. At cutover the /clean-text
// HTTP hop disappears and the Go server calls cleantext.CleanExtractedText in-process; this package
// is that call. The TS test mocked clean-text-remote with only a `\n{3,}` collapse, but the real
// service it talked to was already this same cleantext package, so using it here matches the TS
// runtime behavior exactly (including the "under 10 trimmed chars -> empty string" floor).
//
// Preserved verbatim from the TS source (pdf-extractor.ts:23-24):
//
//	// Strips the litter a browser PDF viewer / scanner leaves behind in extracted text. Kept exported
//	// for the callers and tests that still use it; the pdf2w service does its own path.
//
// Preserved verbatim from the TS source (pdf-extractor.ts:37-43):
//
//	// ---- pdf2w extraction -------------------------------------------------------------------------
//	//
//	// extractPDFContent() is the seam triage-scan, relocalize-document and repair-registry import.
//	// Every file — PDF, photo-derived PDF, office document — is delegated over HTTP to the required
//	// self-hosted markdown-extract-service (pdf2w) via pdf2w-remote.ts. There is no in-process
//	// fallback: an unreachable service is a hard error for that file (FILE_FAILED), by design (see
//	// docs/superpowers/specs/2026-09-17-pdf2w-extraction-swap-design.md).
//
// Preserved verbatim from the TS source (pdf-extractor.ts:49-51):
//
//	// Checksum computed locally, not trusted from the service: it is the dedupe key, and every
//	// transport must produce the SAME sha256 over the file bytes or the same physical file would
//	// be registered as different documents depending on transport.
//
// Deviations, all resolved in favor of matching the TypeScript acceptance bar:
//
//  1. Base URL and timeout are explicit Config fields instead of CONFIG.PDF2W_SERVICE_URL /
//     CONFIG.PDF2W_SERVICE_TIMEOUT_MS, because the settings port is a later migration phase.
//  2. The TS `logger.info('PDF_PARSER', ...)` call becomes an optional Logger interface; passing
//     nil skips it. `*logger.Logger` from infra/logger satisfies the interface, so production wires
//     the same line in.
//  3. JS String.prototype.trim() trims the ECMAScript WhiteSpace + LineTerminator set (which
//     includes U+FEFF but NOT U+0085), while Go's strings.TrimSpace uses unicode.IsSpace (which
//     includes U+0085 but not U+FEFF), so jsTrim below is a faithful port of the JS set.
//  4. JS `.replace(regex, ...)` with the /gim flags maps to Go regexp with `(?im)` and
//     ReplaceAllString; RE2 supports every construct these four patterns use (no lookaround) and
//     `\d` is [0-9] in both.
//  5. The local checksum reads the file bytes with os.ReadFile and hashes them with sha256 exactly
//     as crypto.createHash('sha256').update(fs.readFileSync(...)).digest('hex') did.
package pdfextractor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/phamhung075/pdf-triage-pdf2w/cleantext"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/pdf2w"
)

// ExtractedPDF mirrors the TS `ExtractedPDF` interface.
type ExtractedPDF struct {
	Checksum string `json:"checksum"`
	RawText  string `json:"raw_text"`
	Numpages int    `json:"numpages"`
	Info     any    `json:"info"`
	// OCRDegraded is the legacy provenance flag from the deleted in-process PaddleOCR/Tesseract
	// chain. Kept because relocalize-document.ts still reads it; extractPDFContent() never sets it —
	// pdf2w performs its own vision-rescue server-side and reports no engine-degradation signal.
	OCRDegraded bool `json:"ocr_degraded,omitempty"`
	// Pdf2wMarkdown is present when the pdf2w extraction service returned its structured Markdown. A
	// caller that converts raw text to Markdown (the Step C pass in classify-document) can use this
	// directly instead of asking the LLM to rebuild structure it already has.
	Pdf2wMarkdown string `json:"pdf2w_markdown,omitempty"`
}

// Logger is the subset of infra/logger that this package uses. It is an interface so the extractor
// does not depend on the settings/logger wiring, and so tests can pass nil.
type Logger interface {
	Info(moduleName, message string, meta any, filename ...string)
}

// Config carries the connection parameters and the optional logger. BaseURL/Timeout are required
// for a real extraction; the settings port will supply CONFIG.PDF2W_SERVICE_URL and
// CONFIG.PDF2W_SERVICE_TIMEOUT_MS at composition time.
type Config struct {
	BaseURL string
	Timeout time.Duration
	Logger  Logger
}

// ExtractPDFContent is `extractPDFContent(filePath: string)`.
func ExtractPDFContent(filePath string, cfg Config) (ExtractedPDF, error) {
	filename := filepath.Base(filePath)
	pdf2wResult, err := pdf2w.ExtractContent(filePath, cfg.BaseURL, cfg.Timeout)
	if err != nil {
		return ExtractedPDF{}, err
	}

	if cfg.Logger != nil {
		cfg.Logger.Info(
			"PDF_PARSER",
			fmt.Sprintf("Extraction delegated to pdf2w (%d chars)", len(pdf2wResult.RawText)),
			map[string]any{"filename": filename},
		)
	}

	checksum := pdf2wResult.Checksum
	if checksum == "" {
		fileBytes, err := os.ReadFile(filePath)
		if err != nil {
			return ExtractedPDF{}, err
		}
		sum := sha256.Sum256(fileBytes)
		checksum = hex.EncodeToString(sum[:])
	}

	return ExtractedPDF{
		Checksum:      checksum,
		RawText:       cleantext.CleanExtractedText(pdf2wResult.RawText),
		Numpages:      pdf2wResult.Numpages,
		Info:          pdf2wResult.Info,
		Pdf2wMarkdown: pdf2wResult.Pdf2wMarkdown,
	}, nil
}

var (
	docPropertiesRe = regexp.MustCompile(`(?im)^\[Propriétés Document:[^\]]+\]`)
	ocrExtractedRe  = regexp.MustCompile(`(?im)^\[OCR Extracted Text\]`)
	qpTmpRe         = regexp.MustCompile(`(?im)^QPtmp\d+`)
	chromeExtRe     = regexp.MustCompile(`(?im)chrome-extension___[a-z0-9_]+`)
	trailingSpaceRe = regexp.MustCompile("[ \t]+\n")
	tripleBlankRe   = regexp.MustCompile(`\n{3,}`)
)

// SanitizeDocumentNoise is `sanitizeDocumentNoise(text: string): string`.
func SanitizeDocumentNoise(text string) string {
	if text == "" {
		return ""
	}
	cleaned := docPropertiesRe.ReplaceAllString(text, "")
	cleaned = ocrExtractedRe.ReplaceAllString(cleaned, "")
	cleaned = qpTmpRe.ReplaceAllString(cleaned, "")
	cleaned = chromeExtRe.ReplaceAllString(cleaned, "")
	cleaned = trailingSpaceRe.ReplaceAllString(cleaned, "\n")
	cleaned = tripleBlankRe.ReplaceAllString(cleaned, "\n\n")
	return jsTrim(cleaned)
}

// jsTrim is a port of JavaScript's String.prototype.trim(): the ECMAScript WhiteSpace set
// (TAB, VT, FF, SP, NBSP, ZWNBSP/U+FEFF and every Unicode Space_Separator) plus the LineTerminator
// set (LF, CR, LS/U+2028, PS/U+2029). Go's strings.TrimSpace differs on U+0085 (trimmed by Go, not
// by JS) and U+FEFF (trimmed by JS, not by Go), so the exact JS set is spelled out here.
func jsTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		switch r {
		case '\t', '\v', '\f', ' ', '\u00a0', '\ufeff', '\n', '\r', '\u2028', '\u2029':
			return true
		}
		return unicode.Is(unicode.Zs, r)
	})
}
