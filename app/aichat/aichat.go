// Package aichat is a Go port of pdf-triage's src/application/ai-chat-assistant.ts (321 lines):
// searchRelevantDocuments, dedupeByPeriod, retrieveDocuments, buildPromptContext and
// processChatQuery. It is the retrieval layer behind the /api/chat route and the prepare_dossier
// MCP tool: FTS5 over documents_fts with the relax ladder, the token scorer as last resort,
// pay-slip period de-duplication, prompt building, the Ollama answer and citation pruning.
//
// Upstream status. `npx vitest run src/application/ai-chat-assistant.test.ts` -> 14 passed at port
// time, so no upstream case is pinned red. All 14 cases are ported, plus an end-to-end case over a
// real temp database (package comment deviation 1) and response-shape cases.
//
// Collaborators are injected, never globals, exactly where TS used module imports:
//
//   - DocumentStore is the database surface (ai-chat-assistant.ts:72, :189):
//     GetAllDocuments and SearchDocumentsFts. *store/database.Store satisfies it.
//   - TextChat is the Ollama text-chat call (ai-chat-assistant.ts:253). *infra/ollama.Client
//     satisfies it.
//   - PlanQuery is the planner call (ai-chat-assistant.ts:165); the real wiring is
//     chatplanner.PlanQuery. A nil PlanQuery is a planning failure and falls through to the token
//     scorer, the Go form of the TS mock returning undefined.
//   - Logger is the info/warn/error sink (ai-chat-assistant.ts:246, :195, :205, :301). Nil is silent.
//
// The TypeScript source is the behavioral source of truth. Deviations, all resolved in favor of
// matching TS:
//
//  1. Test scaffolding. The TS suite mocks getAllDocuments/searchDocumentsFts/planQuery with
//     vi.mock; the port injects the same seams and additionally runs one case against a real
//     *database.Store over a t.TempDir() file seeded through InsertDocumentRecord. The live
//     pdf_triage.db is never touched.
//  2. JS string primitives. `toLowerCase` is strings.ToLower; `String.prototype.trim` is jsTrim
//     (JS WhiteSpace+LineTerminator, which differs from strings.TrimSpace on U+0085/U+FEFF); the
//     scorer tokenizer and the requested-count tests reproduce JS `\s` and `.length` (UTF-16 code
//     units) exactly, as the chatquery port does. `Array.prototype.sort` is stable since ES2019, so
//     both sorts use sort.SliceStable.
//  3. JS Date parsing. `new Date('YYYY-MM-DD')` is UTC midnight and it validates month 01-12 and
//     day 01-31 while normalizing an over-long day into the next month (Feb 30 -> Mar 1); the port
//     reproduces that with time.Date in UTC, and an out-of-range month/day yields the TS `|| 0`.
//  4. `d.checksum || ”` and friends. Go's DocumentRecord text fields are already "" for SQL NULL,
//     so the `||` fallbacks collapse to the same values.
//  5. Optional result fields. TS `FileType` is absent from the catch-path object, so FormattedDocument
//     carries `json:"file_type,omitempty"`: the success path always sets it, the catch path never does.
//  6. Retrieval errors. TS `searchRelevantDocuments` rejects if the DB read fails, and
//     `retrieveDocuments` propagates that; Go returns the error. Planning failures and FTS failures
//     never surface, exactly as TS swallows them into the token scorer.
//  7. History. The TS `history` parameter is accepted but never read; the port keeps the parameter
//     for signature parity and ignores it the same way.
package aichat

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/chatquery"
	"github.com/phamhung075/pdf-triage-pdf2w/classification"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/logger"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
	"github.com/phamhung075/pdf-triage-pdf2w/taxonomy"
)

const (
	moduleName    = "CHAT_ASSISTANT"
	fetchHeadroom = 3
)

// DocumentStore is the slice of *store/database.Store the assistant needs.
type DocumentStore interface {
	GetAllDocuments() ([]database.DocumentRecord, error)
	SearchDocumentsFts(matchExpr string, filters database.FtsSearchFilters, limit int) ([]database.DocumentRecord, error)
}

// TextChat is the slice of *infra/ollama.Client the assistant needs.
type TextChat interface {
	RequestTextChatCompletion(system, user string) (ollama.TextCompletion, error)
}

// PlanQueryFunc plans a free-text request. The real wiring is chatplanner.PlanQuery; a nil value is
// a planning failure.
type PlanQueryFunc func(userMessage string, now time.Time) (chatquery.StructuredQuery, error)

// Logger is the slice of *infra/logger.Logger the assistant needs. Nil is silent.
type Logger interface {
	Info(moduleName, message string, meta any, filename ...string)
	Warn(moduleName, message string, meta any, filename ...string)
	Error(moduleName, message string, meta any, filename ...string)
}

// Deps carries the injected collaborators.
type Deps struct {
	Store     DocumentStore
	Ollama    TextChat
	PlanQuery PlanQueryFunc
	Log       Logger
}

// Compile-time proof that the real collaborators satisfy the injected seams the later wiring will
// pass in.
var (
	_ DocumentStore = (*database.Store)(nil)
	_ TextChat      = (*ollama.Client)(nil)
	_ Logger        = (*logger.Logger)(nil)
)

// ChatMessage mirrors the TS `ChatMessage` interface. The role is informational only.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatResponse mirrors the TS `ChatResponse` interface.
type ChatResponse struct {
	Answer           string              `json:"answer"`
	MatchedDocuments []FormattedDocument `json:"matchedDocuments"`
}

// FormattedDocument is one element of matchedDocuments. FileType is absent from the TS catch-path
// object, so it is omitempty (package comment deviation 5).
type FormattedDocument struct {
	ID               int64  `json:"id"`
	Checksum         string `json:"checksum"`
	Title            string `json:"title"`
	Date             string `json:"date"`
	Category         string `json:"category"`
	Subcategory      string `json:"subcategory"`
	FileType         string `json:"file_type,omitempty"`
	Summary          string `json:"summary"`
	TotalAmount      string `json:"total_amount"`
	OriginalFilename string `json:"original_filename"`
	NewPath          string `json:"new_path"`
}

var (
	// The four date patterns are the literal TS regular expressions (ai-chat-assistant.ts:27, :33, :39).
	docDateISORe  = regexp.MustCompile(`\b(20\d{2})[-/._]?(\d{2})[-/._]?(\d{2})\b`)
	docDateFRRe   = regexp.MustCompile(`\b(\d{2})[-/._]?(\d{2})[-/._]?(20\d{2})\b`)
	docDateYearRe = regexp.MustCompile(`\b(20\d{2})\b`)

	// extractRequestedCount patterns (ai-chat-assistant.ts:53-54, :61-63).
	requestedCountNamesRe = regexp.MustCompile(`(?i)\b(\d+)\s*(derniers?|dernieres?|fiches?|bulletins?|factures?|pay|slips?|statements?|relevés?|docs?|documents?)\b`)
	requestedCountLeadRe  = regexp.MustCompile(`(?i)\b(derniers?|dernieres?|last)\s*(\d+)\b`)
	threeRe               = regexp.MustCompile(`(?i)\b(trois|three)\b`)
	twoRe                 = regexp.MustCompile(`(?i)\b(deux|two)\b`)
	singleRe              = regexp.MustCompile(`(?i)\b(single|last|dernier|dernière)\b`)

	// tokenSplitRe is `/[\s,.;:!?/\\_-]+/` with JavaScript's `\s` set injected, the same tokenizer
	// the chatquery port uses.
	tokenSplitRe = regexp.MustCompile(`[` + jsWhitespaceClass + `,.;:!?/\\_-]+`)

	// citeRe is `/\[Doc #(\d+)/gi` (ai-chat-assistant.ts:258).
	citeRe = regexp.MustCompile(`(?i)\[Doc #(\d+)`)
)

// jsWhitespaceClass is the exact character set matched by JavaScript's `\s`: WhiteSpace plus
// LineTerminator. Go's `\s` is ASCII-only and would silently drop NBSP, U+2028/29, the Unicode
// spaces and U+FEFF.
const jsWhitespaceClass = `\t\n\v\f\r \x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}`

// parseDocDate ports parseDocDate.
//
// Parses ISO or DD/MM/YYYY date strings into a comparable timestamp.
func parseDocDate(dateStr string) int64 {
	if dateStr == "" {
		return 0
	}
	str := jsTrim(dateStr)

	// ISO YYYY-MM-DD
	if m := docDateISORe.FindStringSubmatch(str); m != nil {
		ts, ok := jsDateUTC(m[1], m[2], m[3])
		if !ok {
			return 0
		}
		return ts
	}

	// French DD/MM/YYYY
	if m := docDateFRRe.FindStringSubmatch(str); m != nil {
		ts, ok := jsDateUTC(m[3], m[2], m[1])
		if !ok {
			return 0
		}
		return ts
	}

	// Year only
	if m := docDateYearRe.FindStringSubmatch(str); m != nil {
		if ts, ok := jsDateUTC(m[1], "01", "01"); ok {
			return ts
		}
	}
	return 0
}

// jsDateUTC is `new Date('YYYY-MM-DD').getTime()`: UTC midnight, valid only for month 01-12 and day
// 01-31. Go's time.Date normalizes an over-long day into the next month, matching V8.
func jsDateUTC(year, month, day string) (int64, bool) {
	y, err1 := strconv.Atoi(year)
	mo, err2 := strconv.Atoi(month)
	d, err3 := strconv.Atoi(day)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	if mo < 1 || mo > 12 || d < 1 || d > 31 {
		return 0, false
	}
	return time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC).UnixMilli(), true
}

// extractRequestedCount ports extractRequestedCount.
//
// Detects explicit quantity requested in prompt (e.g. "3 derniers", "last 2", "top 5").
func extractRequestedCount(userMessage string) *int {
	lower := strings.ToLower(userMessage)

	// `a || b`: the first matching pattern wins, even when its value is out of range.
	var groups []string
	if m := requestedCountNamesRe.FindStringSubmatch(lower); m != nil {
		groups = m
	} else if m := requestedCountLeadRe.FindStringSubmatch(lower); m != nil {
		groups = m
	}
	if groups != nil {
		// `numMatch[1] || numMatch[2]`: group 1 is the number for the first pattern and the word for
		// the second, so the second pattern parses a word to NaN and never yields a count — the
		// upstream behaviour, pinned rather than fixed.
		raw := groups[1]
		if raw == "" {
			raw = groups[2]
		}
		if val, ok := leadingInt(raw); ok && val > 0 && val <= 20 {
			return &val
		}
	}

	if threeRe.MatchString(lower) {
		v := 3
		return &v
	}
	if twoRe.MatchString(lower) {
		v := 2
		return &v
	}
	if singleRe.MatchString(lower) {
		v := 1
		return &v
	}
	return nil
}

// SearchRelevantDocuments ports searchRelevantDocuments.
//
// Searches local documents using exact intent, category filtering, and date sorting.
func SearchRelevantDocuments(deps Deps, userMessage string) ([]database.DocumentRecord, error) {
	if deps.Store == nil {
		return nil, errors.New("aichat: no document store configured")
	}
	allDocs, err := deps.Store.GetAllDocuments()
	if err != nil {
		return nil, err
	}
	if len(allDocs) == 0 {
		return []database.DocumentRecord{}, nil
	}

	lower := jsTrim(strings.ToLower(userMessage))
	requestedCount := extractRequestedCount(userMessage)

	// General Intent scoring
	queryTokens := []string{}
	for _, token := range tokenSplitRe.Split(lower, -1) {
		if utf16Len(token) > 2 {
			queryTokens = append(queryTokens, token)
		}
	}

	type scoredDoc struct {
		doc       database.DocumentRecord
		score     int
		timestamp int64
	}
	scored := make([]scoredDoc, len(allDocs))
	for i, doc := range allDocs {
		score := 0
		catLower := strings.ToLower(doc.Category)
		subLower := strings.ToLower(doc.Subcategory)
		titleLower := strings.ToLower(doc.Title)
		fileLower := strings.ToLower(doc.OriginalFilename)
		summaryLower := strings.ToLower(doc.Summary)

		if strings.Contains(lower, catLower) && utf16Len(catLower) > 2 {
			score += 10
		}
		if subLower != "" && strings.Contains(lower, subLower) && utf16Len(subLower) > 2 {
			score += 15
		}

		for _, token := range queryTokens {
			if strings.Contains(titleLower, token) {
				score += 6
			}
			if strings.Contains(fileLower, token) {
				score += 5
			}
			if strings.Contains(subLower, token) {
				score += 4
			}
			if strings.Contains(catLower, token) {
				score += 3
			}
			if strings.Contains(summaryLower, token) {
				score += 2
			}
		}

		scored[i] = scoredDoc{doc: doc, score: score, timestamp: parseDocDate(doc.Date)}
	}

	matches := make([]scoredDoc, 0, len(scored))
	for _, item := range scored {
		if item.score > 0 {
			matches = append(matches, item)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].timestamp > matches[j].timestamp
	})

	docs := make([]database.DocumentRecord, len(matches))
	for i, m := range matches {
		docs[i] = m.doc
	}

	if len(docs) == 0 {
		sortedAll := append([]database.DocumentRecord(nil), allDocs...)
		sort.SliceStable(sortedAll, func(i, j int) bool {
			return parseDocDate(sortedAll[i].Date) > parseDocDate(sortedAll[j].Date)
		})
		n := 5
		if requestedCount != nil {
			n = *requestedCount
		}
		return sliceDocs(sortedAll, n), nil
	}

	limit := 10
	if requestedCount != nil {
		limit = *requestedCount
	}
	return sliceDocs(docs, limit), nil
}

// paySlipPeriodKey ports paySlipPeriodKey.
//
// Pay-slip-only period key: nil for anything else, so DedupeByPeriod leaves non-pay-slip documents
// alone.
func paySlipPeriodKey(doc database.DocumentRecord) *string {
	if strings.ToLower(doc.Category) != "bulletin_salaire" {
		return nil
	}
	ts := parseDocDate(doc.Date)
	if ts != 0 {
		key := time.UnixMilli(ts).UTC().Format("2006-01")
		return &key
	}
	key := "no-date-" + strconv.FormatInt(doc.ID, 10)
	return &key
}

// DedupeByPeriod ports dedupeByPeriod.
//
// Collapses re-scanned copies of the same pay period, keeping the most recently imported.
//
// Two imports of the same month must not occupy two of the limited result slots and push a
// distinct, still-requested month out — the user-reported "only 2 of my 3 last months" bug.
//
// Scoped to pay slips on purpose: they are inherently one-per-month-per-employer, so two in one
// month means a re-scan. No other document type carries that guarantee — five invoices in March
// are five invoices, and collapsing them by month would be a worse bug than the one this fixes.
// Input order is preserved; callers rank before calling.
func DedupeByPeriod(docs []database.DocumentRecord) []database.DocumentRecord {
	winnerIDByPeriod := map[string]int64{}
	for _, doc := range docs {
		key := paySlipPeriodKey(doc)
		if key == nil {
			continue
		}
		current, ok := winnerIDByPeriod[*key]
		if !ok || doc.ID > current {
			winnerIDByPeriod[*key] = doc.ID
		}
	}
	out := make([]database.DocumentRecord, 0, len(docs))
	for _, doc := range docs {
		key := paySlipPeriodKey(doc)
		if key == nil || winnerIDByPeriod[*key] == doc.ID {
			out = append(out, doc)
		}
	}
	return out
}

// RetrieveDocuments ports retrieveDocuments.
//
// The retrieval entry point: plan the query, run it through BM25, relax it if it found nothing.
//
// The old token scorer survives only as the last resort — when FTS5 is unavailable, when the
// MATCH throws, or when the ladder is exhausted. It is no longer the primary path.
func RetrieveDocuments(deps Deps, userMessage string, now time.Time) ([]database.DocumentRecord, error) {
	plan, planErr := planFor(deps, userMessage, now)
	if planErr != nil {
		// processChatQuery does not wrap this call in its own try/catch, so anything thrown here
		// propagates straight to the HTTP route and surfaces as a search error to the user — exactly
		// what the "never surface a search error" constraint forbids. Degrade the same way an FTS5
		// failure does instead of letting a planning failure escape.
		deps.warn(fmt.Sprintf("Query planning failed (%s); using the token scorer.", planErr.Error()))
		plan = chatquery.StructuredQuery{}
	} else {
		requestedCount := extractRequestedCount(userMessage)
		// On an explicit count ("les 3 derniers"), citation pruning below re-derives the same value
		// from the same pure helper (extractRequestedCount), so the two cannot disagree. On the
		// no-count path retrieval falls back to the planner's own limit (or 10) while pruning uses its
		// own majority rule instead — independent by design there.
		limit := 10
		if requestedCount != nil {
			limit = *requestedCount
		} else if plan.Limit != nil {
			limit = *plan.Limit
		}

		filters := database.FtsSearchFilters{}
		if plan.Category != nil {
			filters.Category = *plan.Category
		}
		if plan.Subcategory != nil {
			filters.Subcategory = *plan.Subcategory
		}
		if plan.DateFrom != nil {
			filters.DateFrom = *plan.DateFrom
		}
		if plan.DateTo != nil {
			filters.DateTo = *plan.DateTo
		}

		// Over-fetch before de-duplicating: dedupe must run on the candidate set, never on an
		// already-truncated one. Collapsing after the LIMIT is what let two scans of the same month
		// eat two of three slots and push a distinct, still-requested month out of the answer.

		current := &plan
		for current != nil {
			matchExpr := chatquery.BuildFtsMatchExpression(*current)
			if matchExpr == nil {
				break
			}
			hits, err := deps.Store.SearchDocumentsFts(*matchExpr, filters, limit*fetchHeadroom)
			if err != nil {
				deps.warn(fmt.Sprintf("FTS5 search failed (%s); using the token scorer.", err.Error()))
				break
			}
			if len(hits) > 0 {
				kept := DedupeByPeriod(hits)
				if len(kept) > 0 {
					return sliceDocs(kept, limit), nil
				}
			}
			current = chatquery.RelaxQuery(*current)
		}
	}

	// This last-resort path can under-fill relative to the requested count: searchRelevantDocuments
	// slices to its own limit internally, before dedupe ever runs, so a collapsed duplicate here is
	// not backfilled from beyond that slice. Acceptable — this path is only reached when FTS5 itself
	// is unavailable, and searchRelevantDocuments's exported signature is not changed to fix it.
	fallback, err := SearchRelevantDocuments(deps, userMessage)
	if err != nil {
		return nil, err
	}
	return DedupeByPeriod(fallback), nil
}

// BuildPromptContext ports buildPromptContext.
//
// Builds system prompt for Qwen 3.5 AI with local document context.
func BuildPromptContext(userMessage string, documents []database.DocumentRecord, now time.Time) (system, userPrompt string) {
	summaries := make([]string, 0, len(documents))
	for _, d := range documents {
		sub := d.Category
		if d.Subcategory != "" {
			sub = d.Category + "/" + d.Subcategory
		}
		fType := d.FileType
		if fType == "" {
			filename := d.OriginalFilename
			if filename == "" {
				filename = d.Title
			}
			fType = string(taxonomy.DetectFileType(filename))
		}
		date := d.Date
		if date == "" {
			date = "N/A"
		}
		amount := d.TotalAmount
		if amount == "" {
			amount = "N/A"
		}
		summary := d.Summary
		if summary == "" {
			summary = "No summary"
		}
		summaries = append(summaries, fmt.Sprintf(
			"[Doc #%d] Title: \"%s\" | Format: %s | Category: %s | Date: %s | Amount: %s\nSummary: %s",
			d.ID, d.Title, fType, sub, date, amount, summary))
	}
	docSummaries := strings.Join(summaries, "\n\n")

	system = "Tu es un assistant archiviste IA local expert pour le tri et la préparation de dossiers administratifs (via outils MCP locaux).\n" +
		"Ta mission est de répondre à la demande de l'utilisateur de manière synthétique et professionnelle en français.\n" +
		"\n" +
		"Nous sommes le " + classification.FormatLocalDate(now) + ". Utilise cette date comme \"aujourd'hui\" pour toute expression temporelle relative dans la demande de l'utilisateur (ex: \"les 3 derniers mois\", \"cette année\", \"le mois dernier\") — calcule la période exacte à partir de cette date avant de comparer aux dates des documents fournis.\n" +
		"\n" +
		"Consignes strictes:\n" +
		"1. Analyse les documents fournis ci-dessous (triés par pertinence et par date via les outils MCP).\n" +
		"2. Sélectionne et liste EXACTEMENT les documents nécessaires qui répondent à la demande.\n" +
		"3. Pour CHAQUE document listé ci-dessous qui fait partie de ta réponse, mentionne obligatoirement son identifiant sous la forme [Doc #ID: Titre] (exemple: [Doc #1810: Bulletin de Salaire - Décembre 2024]) — n'omets aucune citation, un document non cité sera considéré comme non inclus dans la réponse.\n" +
		"4. Si des pièces obligatoires pour le dossier sont absentes de l'archive, indique-les sous \"⚠️ Documents manquants suggérés\".\n" +
		"5. Réponds directement en texte clair Markdown sans balises JSON raw."

	if docSummaries == "" {
		docSummaries = "Aucun document spécifique trouvé."
	}
	userPrompt = "Documents disponibles dans la base locale (via MCP):\n" + docSummaries +
		"\n\nDemande de l'utilisateur: \"" + userMessage + "\""
	return system, userPrompt
}

// ProcessChatQuery ports processChatQuery.
//
// Main chat handler: processes user query with MCP tool document retrieval & Qwen 3.5 completion.
// The history parameter is accepted for signature parity and never read, exactly as in TS.
func ProcessChatQuery(deps Deps, userMessage string, history []ChatMessage, now time.Time) (ChatResponse, error) {
	deps.info(fmt.Sprintf("Processing web chat query via MCP tools: \"%s\"", userMessage))

	docs, err := RetrieveDocuments(deps, userMessage, now)
	if err != nil {
		return ChatResponse{}, err
	}
	matchedDocs := DedupeByPeriod(docs)

	system, userPrompt := BuildPromptContext(userMessage, matchedDocs, now)

	if deps.Ollama == nil {
		return catchResponse(deps, matchedDocs, errors.New("ollama client not configured")), nil
	}
	aiResult, err := deps.Ollama.RequestTextChatCompletion(system, userPrompt)
	if err != nil {
		return catchResponse(deps, matchedDocs, err), nil
	}
	answerText := jsTrim(aiResult.Response)

	// Extract all [Doc #ID] numbers cited in the AI answer text
	citedDocIDs := map[int64]struct{}{}
	for _, m := range citeRe.FindAllStringSubmatch(answerText, -1) {
		if id, convErr := strconv.ParseInt(m[1], 10, 64); convErr == nil {
			citedDocIDs[id] = struct{}{}
		}
	}

	// Filter matchedDocuments to ONLY return documents cited by AI, or fallback to top candidates.
	// Trust the AI's narrower citation-based selection only when it cites at least half of what
	// it was actually given (or, when the user asked for an explicit count, at least that many) —
	// a small citation count against a much larger fuzzy-matched candidate set is deliberate
	// curation, but under-citing an already-precise, small retrieval (e.g. a "3 last pay slips"
	// query) is far more likely the model simply forgot to tag one in its prose than a deliberate
	// exclusion. Silently dropping correctly-retrieved documents in that case was the root cause
	// of a user-reported bug where a chat answer showed fewer documents than were actually found.
	finalDocs := matchedDocs
	if len(citedDocIDs) > 0 && len(matchedDocs) > 0 {
		cited := make([]database.DocumentRecord, 0, len(matchedDocs))
		for _, d := range matchedDocs {
			if _, ok := citedDocIDs[d.ID]; ok {
				cited = append(cited, d)
			}
		}
		requestedCount := extractRequestedCount(userMessage)
		minExpected := 0
		if requestedCount != nil {
			minExpected = *requestedCount
			if len(matchedDocs) < minExpected {
				minExpected = len(matchedDocs)
			}
		} else {
			minExpected = (len(matchedDocs) + 1) / 2
		}
		if len(cited) >= minExpected {
			finalDocs = cited
		}
	}

	formattedDocs := formatDocuments(finalDocs, true)
	answer := answerText
	if answer == "" {
		answer = fmt.Sprintf("Voici les %d document(s) sélectionné(s) correspondant à votre demande :", len(formattedDocs))
	}
	return ChatResponse{Answer: answer, MatchedDocuments: formattedDocs}, nil
}

// catchResponse is the TS catch branch (ai-chat-assistant.ts:300-320). Its formatted documents
// deliberately omit file_type, matching the TS object literal.
func catchResponse(deps Deps, matchedDocs []database.DocumentRecord, err error) ChatResponse {
	deps.errorf(fmt.Sprintf("Failed to generate AI response: %s", err.Error()))
	formattedDocs := formatDocuments(matchedDocs, false)
	return ChatResponse{
		Answer:           fmt.Sprintf("Voici les %d document(s) trouvé(s) dans vos archives pour votre demande :", len(formattedDocs)),
		MatchedDocuments: formattedDocs,
	}
}

func formatDocuments(docs []database.DocumentRecord, includeFileType bool) []FormattedDocument {
	out := make([]FormattedDocument, 0, len(docs))
	for _, d := range docs {
		fd := FormattedDocument{
			ID:               d.ID,
			Checksum:         d.Checksum,
			Title:            d.Title,
			Date:             d.Date,
			Category:         d.Category,
			Subcategory:      d.Subcategory,
			Summary:          d.Summary,
			TotalAmount:      d.TotalAmount,
			OriginalFilename: d.OriginalFilename,
			NewPath:          d.NewPath,
		}
		if fd.Subcategory == "" {
			fd.Subcategory = "general"
		}
		if fd.OriginalFilename == "" {
			fd.OriginalFilename = d.Title
		}
		if fd.NewPath == "" {
			fd.NewPath = d.OriginalPath
		}
		if includeFileType {
			ft := d.FileType
			if ft == "" {
				filename := d.OriginalFilename
				if filename == "" {
					filename = d.Title
				}
				ft = string(taxonomy.DetectFileType(filename))
			}
			fd.FileType = ft
		}
		out = append(out, fd)
	}
	return out
}

func planFor(deps Deps, userMessage string, now time.Time) (chatquery.StructuredQuery, error) {
	if deps.PlanQuery == nil {
		return chatquery.StructuredQuery{}, errors.New("no query planner configured")
	}
	return deps.PlanQuery(userMessage, now)
}

// sliceDocs is `docs.slice(0, limit)`.
func sliceDocs(docs []database.DocumentRecord, limit int) []database.DocumentRecord {
	if limit > len(docs) {
		limit = len(docs)
	}
	if limit < 0 {
		limit = 0
	}
	return docs[:limit]
}

func (d Deps) info(message string) {
	if d.Log != nil {
		d.Log.Info(moduleName, message, nil)
	}
}

func (d Deps) warn(message string) {
	if d.Log != nil {
		d.Log.Warn(moduleName, message, nil)
	}
}

func (d Deps) errorf(message string) {
	if d.Log != nil {
		d.Log.Error(moduleName, message, nil)
	}
}

// leadingInt is JavaScript parseInt(value, 10) for the token shapes extractRequestedCount can
// produce: an optional sign followed by a run of ASCII digits, else "not a number".
func leadingInt(s string) (int, bool) {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == start {
		return 0, false
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil {
		return 0, false
	}
	return n, true
}

// jsTrim is String.prototype.trim(): JS's WhiteSpace+LineTerminator set. Go's strings.TrimSpace
// differs on U+0085 (stripped) and U+FEFF (not stripped).
func jsTrim(s string) string {
	return strings.TrimFunc(s, isJSWhitespace)
}

func isJSWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x00A0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// utf16Len is JavaScript's `str.length`: the number of UTF-16 code units, not runes.
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
