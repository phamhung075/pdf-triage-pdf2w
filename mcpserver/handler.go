package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/phamhung075/pdf-triage-pdf2w/app/guards"
	"github.com/phamhung075/pdf-triage-pdf2w/app/scanlock"
	"github.com/phamhung075/pdf-triage-pdf2w/app/triagescan"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/osopen"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/zipbuilder"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
	"github.com/phamhung075/pdf-triage-pdf2w/store/manualdecisions"
	"github.com/phamhung075/pdf-triage-pdf2w/taxonomy"
)

// Handler implements listMcpTools() and handleMcpToolCall() (mcp-server.ts:26, :138) with no
// transport dependency. *mcp.Server is wired to it in server.go.
type Handler struct {
	deps Deps
}

// NewHandler builds a Handler over the injected collaborators.
func NewHandler(deps Deps) *Handler { return &Handler{deps: deps} }

// ListTools is listMcpTools().
func (h *Handler) ListTools() *mcp.ListToolsResult { return ListTools() }

// ToolHandler adapts CallTool to the SDK's low-level mcp.ToolHandler. Arguments arrive as raw
// JSON (server.AddTool performs no unmarshalling) and are decoded into the map shape the direct
// tests use.
func (h *Handler) ToolHandler() mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := map[string]any{}
		if req != nil && req.Params != nil && len(req.Params.Arguments) > 0 {
			dec := json.NewDecoder(bytes.NewReader(req.Params.Arguments))
			dec.UseNumber()
			if err := dec.Decode(&args); err != nil {
				return errorResult(fmt.Sprintf("Error executing %s: %v", req.Params.Name, err)), nil
			}
		}
		name := ""
		if req != nil && req.Params != nil {
			name = req.Params.Name
		}
		return h.CallTool(ctx, name, args), nil
	}
}

// CallTool is handleMcpToolCall(name, args) (mcp-server.ts:138). It never returns an error: every
// failure is packed into the tool result, exactly as the TypeScript outer try/catch does. A panic
// from a collaborator is caught and reported the same way.
func (h *Handler) CallTool(ctx context.Context, name string, args map[string]any) (result *mcp.CallToolResult) {
	if args == nil {
		args = map[string]any{}
	}
	defer func() {
		if r := recover(); r != nil {
			result = errorResult(fmt.Sprintf("Error executing %s: %v", name, r))
		}
	}()

	res, err := h.dispatch(ctx, name, args)
	if err != nil {
		return errorResult(fmt.Sprintf("Error executing %s: %s", name, err.Error()))
	}
	if res == nil {
		return errorResult(fmt.Sprintf("Error executing %s: no result", name))
	}
	return res
}

func (h *Handler) dispatch(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
	switch name {
	case "search_documents":
		return h.searchDocuments(args)
	case "get_full_document_text":
		return h.getFullDocumentText(args)
	case "update_document_metadata":
		return h.updateDocumentMetadata(args)
	case "trigger_triage":
		return h.triggerTriage(ctx)
	case "list_categories":
		return h.listCategories()
	case "prepare_dossier":
		return h.prepareDossier(args)
	case "get_document_markdown":
		return h.getDocumentMarkdown(args)
	case "open_document_folder":
		return h.openDocumentFolder(args)
	case "package_documents":
		return h.packageDocuments(args)
	}
	return errorResult("Unknown tool name: " + name), nil
}

// --- search_documents (mcp-server.ts:140-186) ---

type searchPayload struct {
	Count   int                `json:"count"`
	Results []searchResultItem `json:"results"`
}

type searchResultItem struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Registre    string `json:"registre"`
	Date        string `json:"date"`
	Category    string `json:"category"`
	Subcategory string `json:"subcategory"`
	FileType    string `json:"file_type"`
	Summary     string `json:"summary"`
	NewPath     string `json:"new_path"`
	Status      string `json:"status"`
}

func (h *Handler) searchDocuments(args map[string]any) (*mcp.CallToolResult, error) {
	query := strings.ToLower(asString(args["query"]))
	category := strings.ToLower(asString(args["category"]))
	subcategory := strings.ToLower(asString(args["subcategory"]))
	fileType := strings.ToUpper(asString(args["fileType"]))

	// `(args?.limit as number) || 20`: a zero/absent/NaN limit falls back to 20.
	limit := 20
	if f, ok := toFloat(args["limit"]); ok && !math.IsNaN(f) && f != 0 {
		limit = int(f)
	}

	docs, err := h.deps.DB.GetAllDocuments()
	if err != nil {
		return nil, err
	}

	results := []searchResultItem{}
	for _, d := range docs {
		if !searchDocMatches(d, query, category, subcategory, fileType) {
			continue
		}
		results = append(results, searchResultItem{
			ID:          d.ID,
			Title:       d.Title,
			Registre:    d.Registre,
			Date:        d.Date,
			Category:    d.Category,
			Subcategory: d.Subcategory,
			FileType:    fileTypeOrDetect(d),
			Summary:     d.Summary,
			NewPath:     d.NewPath,
			Status:      d.Status,
		})
	}

	// `.slice(0, limit)` with JavaScript's negative-index semantics.
	if end := jsSliceEnd(limit, len(results)); end < len(results) {
		results = results[:end]
	}

	return jsonResult(searchPayload{Count: len(results), Results: results}), nil
}

func searchDocMatches(d database.DocumentRecord, query, category, subcategory, fileType string) bool {
	if category != "" && strings.ToLower(d.Category) != category {
		return false
	}
	if subcategory != "" && strings.ToLower(d.Subcategory) != subcategory {
		return false
	}
	docFileType := strings.ToUpper(fileTypeOrDetect(d))
	if fileType != "" && docFileType != fileType {
		return false
	}
	if query == "" {
		return true
	}
	return strings.Contains(strings.ToLower(d.Title), query) ||
		strings.Contains(strings.ToLower(d.Summary), query) ||
		strings.Contains(strings.ToLower(d.Registre), query) ||
		strings.Contains(strings.ToLower(d.Tags), query) ||
		strings.Contains(strings.ToLower(d.Subcategory), query) ||
		strings.Contains(strings.ToLower(d.RawText), query)
}

// jsSliceEnd is `Math.min(length, limit < 0 ? length+limit : limit)` clamped to >= 0.
func jsSliceEnd(limit, length int) int {
	end := limit
	if end < 0 {
		end = length + end
	}
	if end < 0 {
		end = 0
	}
	if end > length {
		end = length
	}
	return end
}

// --- get_full_document_text (mcp-server.ts:188-215) ---

type fullTextPayload struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Registre string `json:"registre"`
	Date     string `json:"date"`
	Category string `json:"category"`
	Summary  string `json:"summary"`
	FilePath string `json:"file_path"`
	RawText  string `json:"raw_text"`
}

func (h *Handler) getFullDocumentText(args map[string]any) (*mcp.CallToolResult, error) {
	docID, display := docIDArg(args)
	doc, err := h.deps.DB.GetDocumentByID(docID)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return errorResult(fmt.Sprintf("Error: Document ID %s not found", display)), nil
	}
	return jsonResult(fullTextPayload{
		ID:       doc.ID,
		Title:    doc.Title,
		Registre: doc.Registre,
		Date:     doc.Date,
		Category: doc.Category,
		Summary:  doc.Summary,
		FilePath: filePathOrOriginal(*doc),
		RawText:  doc.RawText,
	}), nil
}

// --- update_document_metadata (mcp-server.ts:217-280) ---

func (h *Handler) updateDocumentMetadata(args map[string]any) (*mcp.CallToolResult, error) {
	rawDocID, present := args["docId"]
	f, isNumber := toFloat(rawDocID)
	if !present || !isNumber || math.IsNaN(f) || math.Trunc(f) != f || f <= 0 {
		return errorResult(fmt.Sprintf("Error: docId must be a positive integer, got: %s", jsJSONStringify(rawDocID))), nil
	}
	docID := int64(f)

	// UpdateDocumentSchema.safeParse(args) (Zod strips unknown keys such as docId).
	rawJSON, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	updates, err := documentschema.ParseUpdateDocument(rawJSON)
	if err != nil {
		return errorResult("Error: invalid arguments — " + err.Error()), nil
	}

	// Golden Rule #4: reject an explicit forbidden subcategory before writing anything.
	explicitSubcategory := updates.Subcategory
	if explicitSubcategory == nil {
		explicitSubcategory = updates.Subcategorie
	}
	if explicitSubcategory != nil {
		if violation := guards.ForbiddenSubcategoryViolation(*explicitSubcategory); violation != nil {
			return errorResult("Error: " + violation.Message), nil
		}
	}

	docBefore, err := h.deps.DB.GetDocumentByID(docID)
	if err != nil {
		return nil, err
	}
	if docBefore == nil {
		return errorResult(fmt.Sprintf("Error: Document ID %d not found", docID)), nil
	}

	success, err := h.deps.DB.UpdateDocumentRecord(docID, documentUpdatesFrom(updates))
	if err != nil {
		return nil, err
	}
	if !success {
		return errorResult(fmt.Sprintf("Error: Document ID %d not found", docID)), nil
	}

	targetCategory := coalesceString(updates.Category, updates.Categorie, docBefore.Category)
	targetSubcategory := coalesceString(updates.Subcategory, updates.Subcategorie, docBefore.Subcategory)

	// Relocalize the physical file if category/subcategory changed, mirroring
	// PUT /api/documents/:id — this tool previously left the DB and disk out of sync.
	if docBefore.NewPath != "" && fileExists(docBefore.NewPath) {
		// Golden Rule #5: register the target taxonomy BEFORE the physical move.
		if h.deps.Categories != nil {
			if err := guards.EnsureCategoryAndSubcategoryExist(h.deps.Categories, targetCategory, targetSubcategory); err != nil {
				return nil, err
			}
		}
		if h.deps.Relocalizer != nil {
			dateStr := stringOr(updates.Date, docBefore.Date)
			res, err := h.deps.Relocalizer.RelocalizeFileIfNeeded(
				docBefore.NewPath, targetCategory, &targetSubcategory, &dateStr, nil,
			)
			if err != nil {
				return nil, err
			}
			if res.NewPath != docBefore.NewPath {
				if _, err := h.deps.DB.UpdateDocumentRecord(docID, database.DocumentUpdates{NewPath: &res.NewPath}); err != nil {
					return nil, err
				}
			}
		}
	}

	// Register a human decision whenever the edit actually re-classified the document, mirroring
	// PUT /api/documents/:id (the TS MCP tool omitted this; the objective requires it here).
	h.recordManualDecision(docBefore, updates, targetCategory, targetSubcategory, docID)

	if h.deps.Registry != nil {
		if err := h.deps.Registry.SyncJSONRegistry(); err != nil {
			return nil, err
		}
	}
	return textResult(fmt.Sprintf("Successfully updated metadata for document ID %d", docID)), nil
}

// recordManualDecision mirrors web-server.ts:1289-1311: only a real category/subcategory move is
// recorded, so an unrelated edit cannot pollute the teaching log. Nil Decisions skips silently.
func (h *Handler) recordManualDecision(docBefore *database.DocumentRecord, updates documentschema.UpdateDocumentInput, finalCategory, finalSubcategory string, docID int64) {
	if h.deps.Decisions == nil {
		return
	}
	catChanged := strings.ToLower(finalCategory) != strings.ToLower(docBefore.Category)
	subChanged := strings.ToLower(finalSubcategory) != strings.ToLower(docBefore.Subcategory)
	if !catChanged && !subChanged {
		return
	}
	title := docBefore.Title
	if updates.Title != nil && *updates.Title != "" {
		title = *updates.Title
	}
	h.deps.Decisions.RecordManualDecision(manualdecisions.Record{
		DocumentID:         docID,
		Checksum:           docBefore.Checksum,
		OriginalFilename:   docBefore.OriginalFilename,
		Title:              title,
		OldCategory:        docBefore.Category,
		OldSubcategory:     docBefore.Subcategory,
		NewCategory:        finalCategory,
		NewSubcategory:     finalSubcategory,
		UserFeedbackReason: "Manual user selection (Edit modal)",
		RawTextSnippet:     docBefore.RawText,
	})
}

// documentUpdatesFrom maps the validated UpdateDocumentSchema fields onto the store's update input.
func documentUpdatesFrom(u documentschema.UpdateDocumentInput) database.DocumentUpdates {
	return database.DocumentUpdates{
		Title:           u.Title,
		Titre:           u.Titre,
		Registre:        u.Registre,
		Date:            u.Date,
		Category:        u.Category,
		Categorie:       u.Categorie,
		Subcategory:     u.Subcategory,
		Subcategorie:    u.Subcategorie,
		Summary:         u.Summary,
		Tags:            u.Tags,
		MarkdownContent: u.MarkdownContent,
		TotalAmount:     u.TotalAmount,
		VatAmount:       u.VatAmount,
		Siren:           u.Siren,
		Iban:            u.Iban,
		ExpiryDate:      u.ExpiryDate,
		ContactName:     u.ContactName,
		ContactEmail:    u.ContactEmail,
		ContactPhone:    u.ContactPhone,
		ContactAddress:  u.ContactAddress,
		ContactWebsite:  u.ContactWebsite,
	}
}

// --- trigger_triage (mcp-server.ts:282-297) ---

type triagePayload struct {
	ScannedCount   int                     `json:"scannedCount"`
	ProcessedCount int                     `json:"processedCount"`
	SkippedCount   int                     `json:"skippedCount"`
	Items          []triagescan.ResultItem `json:"items"`
	OllamaDown     bool                    `json:"ollamaDown,omitempty"`
	Message        string                  `json:"message,omitempty"`
}

func (h *Handler) triggerTriage(ctx context.Context) (*mcp.CallToolResult, error) {
	if h.deps.Scanner == nil {
		return nil, errors.New("no scan runner configured")
	}
	res, err := h.deps.Scanner.RunTriageScan(ctx, nil, nil)
	if err != nil {
		var inProgress *scanlock.ScanInProgressError
		if errors.As(err, &inProgress) {
			return errorResult("Error: " + err.Error()), nil
		}
		return nil, err
	}
	items := res.Items
	if items == nil {
		items = []triagescan.ResultItem{}
	}
	return jsonResult(triagePayload{
		ScannedCount:   res.ScannedCount,
		ProcessedCount: res.ProcessedCount,
		SkippedCount:   res.SkippedCount,
		Items:          items,
		OllamaDown:     res.OllamaDown,
		Message:        res.Message,
	}), nil
}

// --- list_categories (mcp-server.ts:299-304) ---

func (h *Handler) listCategories() (*mcp.CallToolResult, error) {
	if h.deps.Categories == nil {
		return nil, errors.New("no categories store configured")
	}
	categories := h.deps.Categories.GetCategoriesConfig().Categories
	if categories == nil {
		categories = []*documentschema.CategoryItem{}
	}
	return jsonResult(categories), nil
}

// --- prepare_dossier (mcp-server.ts:306-334) ---

type prepareDossierPayload struct {
	DossierType string              `json:"dossierType"`
	Count       int                 `json:"count"`
	Documents   []prepareDossierDoc `json:"documents"`
}

type prepareDossierDoc struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Date        string `json:"date"`
	Category    string `json:"category"`
	Subcategory string `json:"subcategory"`
	FileType    string `json:"file_type"`
	Summary     string `json:"summary"`
	TotalAmount string `json:"total_amount"`
	NewPath     string `json:"new_path"`
}

func (h *Handler) prepareDossier(args map[string]any) (*mcp.CallToolResult, error) {
	dossierType := strings.ToLower(asString(args["dossierType"]))
	if h.deps.ChatSearch == nil {
		return nil, errors.New("no chat search configured")
	}
	docs, err := h.deps.ChatSearch.SearchRelevantDocuments(dossierType)
	if err != nil {
		return nil, err
	}
	formatted := make([]prepareDossierDoc, 0, len(docs))
	for _, d := range docs {
		formatted = append(formatted, prepareDossierDoc{
			ID:          d.ID,
			Title:       d.Title,
			Date:        d.Date,
			Category:    d.Category,
			Subcategory: d.Subcategory,
			FileType:    fileTypeOrDetect(d),
			Summary:     d.Summary,
			TotalAmount: d.TotalAmount,
			NewPath:     filePathOrOriginal(d),
		})
	}
	return jsonResult(prepareDossierPayload{DossierType: dossierType, Count: len(formatted), Documents: formatted}), nil
}

// --- get_document_markdown (mcp-server.ts:336-364) ---

type markdownPayload struct {
	ID              int64  `json:"id"`
	Title           string `json:"title"`
	Category        string `json:"category"`
	Subcategory     string `json:"subcategory"`
	Date            string `json:"date"`
	Summary         string `json:"summary"`
	TotalAmount     string `json:"total_amount"`
	ContactName     string `json:"contact_name"`
	ContactEmail    string `json:"contact_email"`
	ContactPhone    string `json:"contact_phone"`
	MarkdownContent string `json:"markdown_content"`
	FilePath        string `json:"file_path"`
}

func (h *Handler) getDocumentMarkdown(args map[string]any) (*mcp.CallToolResult, error) {
	docID, display := docIDArg(args)
	doc, err := h.deps.DB.GetDocumentByID(docID)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return errorResult(fmt.Sprintf("Error: Document ID %s not found", display)), nil
	}
	markdown := doc.MarkdownContent
	if markdown == "" {
		markdown = fmt.Sprintf("# %s\n\n%s", doc.Title, doc.Summary)
	}
	return jsonResult(markdownPayload{
		ID:              doc.ID,
		Title:           doc.Title,
		Category:        doc.Category,
		Subcategory:     doc.Subcategory,
		Date:            doc.Date,
		Summary:         doc.Summary,
		TotalAmount:     doc.TotalAmount,
		ContactName:     doc.ContactName,
		ContactEmail:    doc.ContactEmail,
		ContactPhone:    doc.ContactPhone,
		MarkdownContent: markdown,
		FilePath:        filePathOrOriginal(*doc),
	}), nil
}

// --- open_document_folder (mcp-server.ts:366-398) ---

func (h *Handler) openDocumentFolder(args map[string]any) (*mcp.CallToolResult, error) {
	docID, display := docIDArg(args)
	doc, err := h.deps.DB.GetDocumentByID(docID)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return errorResult(fmt.Sprintf("Error: Document ID %s not found", display)), nil
	}
	fileOnDisk := h.fileLocator().Find(doc)
	if fileOnDisk == "" || !fileExists(fileOnDisk) {
		return errorResult(fmt.Sprintf("Error: File for doc ID %d does not exist on disk", docID)), nil
	}
	_ = h.runner().Run(h.opener().RevealInFileManager(fileOnDisk))
	return textResult("Opened OS file manager at file: " + fileOnDisk), nil
}

// --- package_documents (mcp-server.ts:400-462) ---

type packagePayload struct {
	ZipPath        string  `json:"zipPath"`
	FileCount      int     `json:"fileCount"`
	IncludedDocIDs []int64 `json:"includedDocIds"`
	MissingDocIDs  []int64 `json:"missingDocIds"`
}

func (h *Handler) packageDocuments(args map[string]any) (*mcp.CallToolResult, error) {
	explicitIDs := parseDocIDs(args["docIds"])
	dossierType := asString(args["dossierType"])

	if len(explicitIDs) == 0 && dossierType == "" {
		return errorResult("Error: provide either docIds or dossierType."), nil
	}

	var docs []database.DocumentRecord
	if len(explicitIDs) > 0 {
		for _, id := range explicitIDs {
			d, err := h.deps.DB.GetDocumentByID(id)
			if err != nil {
				return nil, err
			}
			if d != nil {
				docs = append(docs, *d)
			}
		}
	} else {
		if h.deps.ChatSearch == nil {
			return nil, errors.New("no chat search configured")
		}
		found, err := h.deps.ChatSearch.SearchRelevantDocuments(dossierType)
		if err != nil {
			return nil, err
		}
		docs = found
	}

	locator := h.fileLocator()
	zipFiles := []zipbuilder.ZipFileEntry{}
	missing := []int64{}
	for i := range docs {
		doc := &docs[i]
		fileOnDisk := locator.Find(doc)
		if fileOnDisk != "" && fileExists(fileOnDisk) {
			ext := filepath.Ext(fileOnDisk)
			if ext == "" {
				ext = ".pdf"
			}
			baseTitle := firstNonEmpty(doc.Title, doc.OriginalFilename, fmt.Sprintf("doc_%d", doc.ID))
			baseTitle = zipUnsafeNameRe.ReplaceAllString(baseTitle, "_")
			fileNameInZip := baseTitle
			if !strings.HasSuffix(baseTitle, ext) {
				fileNameInZip = baseTitle + ext
			}
			zipFiles = append(zipFiles, zipbuilder.ZipFileEntry{Name: fileNameInZip, Path: fileOnDisk})
		} else {
			missing = append(missing, doc.ID)
		}
	}

	if len(zipFiles) == 0 {
		return errorResult(fmt.Sprintf(
			"Error: none of the %d resolved document(s) have a file on disk. Missing IDs: %s",
			len(docs), joinInt64OrNA(missing),
		)), nil
	}

	buf, err := zipbuilder.CreateZipArchive(zipFiles)
	if err != nil {
		return nil, err
	}

	packagesDir := filepath.Join(h.baseDir(), "__packages")
	if err := os.MkdirAll(packagesDir, 0o755); err != nil {
		return nil, err
	}
	rawName := asString(args["zipName"])
	if rawName == "" {
		if dossierType != "" {
			rawName = dossierType + "_package"
		} else {
			rawName = fmt.Sprintf("documents_package_%d", h.now().UnixMilli())
		}
	}
	safeName := zipSafeNameRe.ReplaceAllString(rawName, "_")
	zipFileName := safeName
	if !strings.HasSuffix(safeName, ".zip") {
		zipFileName = safeName + ".zip"
	}
	zipPath := filepath.Join(packagesDir, zipFileName)
	if err := os.WriteFile(zipPath, buf, 0o644); err != nil {
		return nil, err
	}

	included := []int64{}
	for _, d := range docs {
		if !containsInt64(missing, d.ID) {
			included = append(included, d.ID)
		}
	}
	return jsonResult(packagePayload{
		ZipPath:        zipPath,
		FileCount:      len(zipFiles),
		IncludedDocIDs: included,
		MissingDocIDs:  missing,
	}), nil
}

// --- shared helpers ---

func (h *Handler) baseDir() string {
	if h.deps.BaseDir != "" {
		return h.deps.BaseDir
	}
	if h.deps.Config.CategoriesFile != "" {
		return filepath.Dir(h.deps.Config.CategoriesFile)
	}
	return "."
}

func (h *Handler) now() time.Time {
	if h.deps.Now != nil {
		return h.deps.Now()
	}
	return time.Now()
}

func (h *Handler) fileLocator() FileLocator {
	if h.deps.FileLocator != nil {
		return h.deps.FileLocator
	}
	return RealFileLocator{Config: h.deps.Config}
}

func (h *Handler) opener() Opener {
	if h.deps.Opener != nil {
		return h.deps.Opener
	}
	return RealOpener{}
}

func (h *Handler) runner() Runner {
	if h.deps.Runner != nil {
		return h.deps.Runner
	}
	return osopen.ExecRunner{}
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func errorResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: true}
}

func jsonResult(v any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("Error serializing result: %v", err))
	}
	return textResult(string(b))
}

var (
	zipUnsafeNameRe = regexp.MustCompile(`[\\/?%*:|"<>]`)
	zipSafeNameRe   = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)
)

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func fileTypeOrDetect(d database.DocumentRecord) string {
	if d.FileType != "" {
		return d.FileType
	}
	return string(taxonomy.DetectFileType(d.OriginalFilename))
}

func filePathOrOriginal(d database.DocumentRecord) string {
	if d.NewPath != "" {
		return d.NewPath
	}
	return d.OriginalPath
}

func coalesceString(primary, alias *string, fallback string) string {
	if primary != nil {
		return *primary
	}
	if alias != nil {
		return *alias
	}
	return fallback
}

func stringOr(primary *string, fallback string) string {
	if primary != nil {
		return *primary
	}
	return fallback
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// docIDArg is the TS `args?.docId as number` for read-only tools: a missing/invalid value maps to
// Go's zero id, and display reproduces the `${docId}` interpolation (undefined, 1.5, 9999).
func docIDArg(args map[string]any) (int64, string) {
	raw, ok := args["docId"]
	if !ok {
		return 0, "undefined"
	}
	f, isNumber := toFloat(raw)
	if !isNumber {
		return 0, jsJSONStringify(raw)
	}
	return int64(f), jsNumberString(f)
}

// parseDocIDs is `Array.isArray(docIds) ? docIds.map(Number).filter(Number.isInteger) : []`.
func parseDocIDs(v any) []int64 {
	items := docIDSlice(v)
	out := []int64{}
	for _, item := range items {
		f, ok := numberFromAny(item)
		if !ok || math.IsNaN(f) || math.Trunc(f) != f {
			continue
		}
		out = append(out, int64(f))
	}
	return out
}

func docIDSlice(v any) []any {
	switch arr := v.(type) {
	case []any:
		return arr
	case []int64:
		out := make([]any, len(arr))
		for i, x := range arr {
			out[i] = x
		}
		return out
	case []int:
		out := make([]any, len(arr))
		for i, x := range arr {
			out[i] = x
		}
		return out
	case []float64:
		out := make([]any, len(arr))
		for i, x := range arr {
			out[i] = x
		}
		return out
	case []string:
		out := make([]any, len(arr))
		for i, x := range arr {
			out[i] = x
		}
		return out
	}
	return nil
}

// numberFromAny is JavaScript's Number(): numeric types pass through, and a string is parsed.
func numberFromAny(v any) (float64, bool) {
	if f, ok := toFloat(v); ok {
		return f, true
	}
	if s, ok := v.(string); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		return f, err == nil
	}
	return 0, false
}

func joinInt64OrNA(ids []int64) string {
	if len(ids) == 0 {
		return "n/a"
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ", ")
}

func containsInt64(ids []int64, target int64) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

// jsJSONStringify reproduces JSON.stringify for the scalar values that appear in tool errors.
func jsJSONStringify(v any) string {
	switch x := v.(type) {
	case nil:
		return "undefined"
	case string:
		return encodeJSONString(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	if f, ok := toFloat(v); ok {
		return jsNumberString(f)
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(encoded)
}

func encodeJSONString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimRight(buf.String(), "\n")
}

// jsNumberString is JSON.stringify(number) for the values docId validation can see.
func jsNumberString(f float64) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e21 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
