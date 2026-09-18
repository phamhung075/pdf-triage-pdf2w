// Package pdf2w is a Go port of pdf-triage's src/infrastructure/pdf2w-remote.ts (54 lines):
// the Pdf2wExtractResult shape and extractPdf2wContent.
//
// The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/pdf2w-remote.test.ts` -> 3 passed), so no upstream case is
// pinned red. All three cases are ported; the suite mocks global.fetch, so the Go port pins the
// same contract against net/http/httptest and additionally asserts the method, path, headers and
// raw body bytes. Added cases cover the x-file-name percent-encoding, the non-OK error template,
// the missing-markdown guard, every field fallback in the result mapping, and the abort timeout.
//
// Preserved verbatim from the TS source (pdf2w-remote.ts:13-18):
//
//	HTTP client for the self-hosted markdown-extract-service (pdf2w). Required, not
//	optional-with-fallback: the caller (extractPDFContent in pdf-extractor.ts) treats any failure
//	as a hard error for that file — there is no in-process extraction left to fall back to.
//
// Deviations, all resolved in favor of matching the TypeScript acceptance bar:
//
//  1. fetch -> net/http. The TS `AbortSignal.timeout(timeoutMs)` becomes a
//     context.WithTimeout(context.Background(), timeout) when timeout > 0; timeout <= 0 means "no
//     deadline", exactly as `timeoutMs > 0 ? AbortSignal.timeout(...) : undefined`.
//  2. x-file-name. TS uses encodeURIComponent(path.basename(filePath)); Go's url.QueryEscape
//     encodes space as '+' and url.PathEscape leaves some delimiters alone, so encodeURIComponent
//     below is a literal port of the JS algorithm (A-Za-z0-9 and -_.!~*'() unescaped, every other
//     UTF-8 byte as %XX).
//  3. `res.statusText`. Go has no per-response reason phrase, so http.StatusText(res.status) is
//     used; the exact template is
//     `pdf2w service returned <status> <statusText> for '<basename>'`.
//  4. `data.info`. TS keeps `data.info` whenever it is truthy and typeof object (which includes
//     arrays); the field is therefore `any` here and keeps a JSON object (map[string]any) or array
//     ([]any), falling back to an empty map for null/scalars.
//  5. `data.numpages`. JSON numbers decode to float64; `typeof === 'number' && >= 1` maps to
//     float64 with the same >= 1 guard, converted to int. A non-integer number would be truncated
//     where TS keeps it; the service reports page counts, so no upstream case covers that.
//  6. A malformed JSON body (`res.json()` rejecting) and an unreadable file (`fs.readFileSync`
//     throwing) both surface as Go errors here.
//
// The base URL and timeout are explicit parameters (the settings port is a later migration phase),
// and the base URL's trailing slashes are stripped exactly as TS's `.replace(/\/+$/, ”)`.
package pdf2w

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ExtractResult mirrors the TS `Pdf2wExtractResult` interface.
type ExtractResult struct {
	Checksum      string `json:"checksum"`
	RawText       string `json:"raw_text"`
	Numpages      int    `json:"numpages"`
	Info          any    `json:"info"`
	Pdf2wMarkdown string `json:"pdf2w_markdown"`
}

// ExtractContent is `extractPdf2wContent(filePath: string)`. baseURL replaces
// CONFIG.PDF2W_SERVICE_URL and timeout replaces CONFIG.PDF2W_SERVICE_TIMEOUT_MS.
func ExtractContent(filePath, baseURL string, timeout time.Duration) (*ExtractResult, error) {
	if baseURL == "" {
		return nil, errors.New("PDF2W_SERVICE_URL is not configured")
	}
	base := strings.TrimRight(baseURL, "/")
	fileBytes, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	filename := encodeURIComponent(filepath.Base(filePath))

	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/convert", bytes.NewReader(fileBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/octet-stream")
	req.Header.Set("x-file-name", filename)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, fmt.Errorf(
			"pdf2w service returned %d %s for '%s'",
			res.StatusCode, http.StatusText(res.StatusCode), filepath.Base(filePath),
		)
	}

	var data map[string]any
	if err := json.NewDecoder(res.Body).Decode(&data); err != nil {
		return nil, err
	}
	markdown, ok := data["markdown"].(string)
	if !ok {
		return nil, errors.New("pdf2w service returned an unexpected response shape (missing markdown)")
	}

	checksum := ""
	if s, ok := data["checksum"].(string); ok {
		checksum = s
	}
	rawText := markdown
	if s, ok := data["text"].(string); ok {
		rawText = s
	}
	numpages := 1
	if n, ok := data["numpages"].(float64); ok && n >= 1 {
		numpages = int(n)
	}
	info := any(map[string]any{})
	if v, ok := data["info"]; ok && v != nil {
		switch v.(type) {
		case map[string]any, []any:
			info = v
		}
	}

	return &ExtractResult{
		Checksum:      checksum,
		RawText:       rawText,
		Numpages:      numpages,
		Info:          info,
		Pdf2wMarkdown: markdown,
	}, nil
}

// encodeURIComponent is a literal port of JavaScript's encodeURIComponent, which the TS source uses
// for the x-file-name header. Go's url.QueryEscape encodes space as '+' and url.PathEscape leaves
// characters such as '+' unescaped, so neither matches the wire contract. Iterating UTF-8 bytes and
// percent-encoding each unescaped byte is equivalent to encodeURIComponent for every valid Go
// string.
func encodeURIComponent(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z',
			c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '!', c == '~', c == '*', c == '\'', c == '(', c == ')':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}
