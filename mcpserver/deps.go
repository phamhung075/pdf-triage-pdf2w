package mcpserver

import (
	"context"
	"runtime"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/app/aichat"
	"github.com/phamhung075/pdf-triage-pdf2w/app/guards"
	"github.com/phamhung075/pdf-triage-pdf2w/app/relocalize"
	"github.com/phamhung075/pdf-triage-pdf2w/app/triagescan"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/osopen"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/settings"
	"github.com/phamhung075/pdf-triage-pdf2w/store/categories"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
	"github.com/phamhung075/pdf-triage-pdf2w/store/manualdecisions"
)

// DocumentStore is the slice of *store/database.Store the MCP tools use. The TypeScript module
// imported getAllDocuments / getDocumentById / updateDocumentRecord directly; every write still
// goes through a named store method, so this package issues no raw SQL.
type DocumentStore interface {
	GetAllDocuments() ([]database.DocumentRecord, error)
	GetDocumentByID(id int64) (*database.DocumentRecord, error)
	UpdateDocumentRecord(id any, updates database.DocumentUpdates) (bool, error)
}

// Categories is the categories.json reader/writer. It is exactly guards.CategoriesStore, so
// guards.EnsureCategoryAndSubcategoryExist (Golden Rule 5) accepts it directly.
type Categories interface {
	GetCategoriesConfig() documentschema.CategoriesConfig
	SaveCategoriesConfig(categories []*documentschema.CategoryItem) error
}

// Relocalizer is the subset of app/relocalize.Deps the update tool calls. The TS passes four
// arguments; the Go port adds an optional title pointer, passed nil here to match the TS call.
type Relocalizer interface {
	RelocalizeFileIfNeeded(filePath, category string, subcategory, dateStr, title *string) (relocalize.RelocalizeResult, error)
}

// FileLocator is the injected guards.FindActualFileOnDisk seam (relocalize-document.ts:142). It is
// a guard because a move must never be planned from a row whose file is gone.
type FileLocator interface {
	Find(doc *database.DocumentRecord) string
}

// Registry is the JSON-registry mirror writer (syncJSONRegistry). Nil skips the sync.
type Registry interface {
	SyncJSONRegistry() error
}

// ScanRunner is runTriageScan. *triagescan.Scanner satisfies it; a scan context is supplied by the
// caller and the two callback seams may be nil, exactly as the HTTP/CLI callers pass them.
type ScanRunner interface {
	RunTriageScan(ctx context.Context, onProgress func(triagescan.Event), shouldAbort func() bool) (triagescan.Result, error)
}

// ChatSearch is the searchRelevantDocuments seam used by prepare_dossier and the dossierType form
// of package_documents.
type ChatSearch interface {
	SearchRelevantDocuments(userMessage string) ([]database.DocumentRecord, error)
}

// Opener builds the spawn-ready plan that reveals a file in the OS file manager.
type Opener interface {
	RevealInFileManager(filePath string) osopen.Plan
}

// Runner executes an Opener plan. It is injectable so tests never launch a real process.
type Runner interface {
	Run(plan osopen.Plan) error
}

// ManualDecisions is the human-decision writer (recordManualDecision). *store/manualdecisions.Store
// satisfies it. Nil skips recording.
type ManualDecisions interface {
	RecordManualDecision(record manualdecisions.Record)
}

// RegistrySource is the read-only store surface the JSON mirror needs.
type RegistrySource interface {
	GetAllDocuments() ([]database.DocumentRecord, error)
}

// RealFileLocator is the production FileLocator over guards.FindActualFileOnDisk.
type RealFileLocator struct {
	Config settings.Config
}

// Find projects the stored record onto the guard's three-field location input.
func (l RealFileLocator) Find(doc *database.DocumentRecord) string {
	if doc == nil {
		return ""
	}
	return guards.FindActualFileOnDisk(guards.DocumentLocation{
		NewPath:          doc.NewPath,
		OriginalPath:     doc.OriginalPath,
		OriginalFilename: doc.OriginalFilename,
	}, l.Config.InputDir, l.Config.OutputRootDir)
}

// RealOpener is the production Opener over infra/osopen. An empty Platform means the host platform.
type RealOpener struct {
	Platform string
	Deps     osopen.Deps
}

// RevealInFileManager mirrors the TS revealInFileManager(fileOnDisk) call, deriving the platform
// and the WSL path conversion from the host when they are not supplied.
func (o RealOpener) RevealInFileManager(filePath string) osopen.Plan {
	platform := o.Platform
	if platform == "" {
		platform = osopen.PlatformFromGOOS(runtime.GOOS)
	}
	deps := o.Deps
	if deps.PathConv == nil {
		deps = osopen.DefaultDeps()
	}
	return osopen.RevealInFileManager(filePath, platform, deps)
}

// ChatSearcher adapts app/aichat to the ChatSearch seam.
type ChatSearcher struct {
	Deps aichat.Deps
}

// SearchRelevantDocuments forwards to aichat.SearchRelevantDocuments.
func (c ChatSearcher) SearchRelevantDocuments(userMessage string) ([]database.DocumentRecord, error) {
	return aichat.SearchRelevantDocuments(c.Deps, userMessage)
}

// JSONRegistrySyncer adapts app/relocalize's JSONRegistrySync to the zero-argument Registry seam.
type JSONRegistrySyncer struct {
	DB   RegistrySource
	Path string
}

// SyncJSONRegistry writes the registry mirror from the injected store.
func (s JSONRegistrySyncer) SyncJSONRegistry() error {
	return relocalize.JSONRegistrySync{DB: s.DB, Path: s.Path}.SyncJSONRegistry()
}

// Deps is the explicit injected surface. Construct it in the composition root; tests build it with
// fakes and a temp SQLite store.
type Deps struct {
	// Config is the effective settings snapshot (TS CONFIG), including MCP_HTTP_PORT/HOST,
	// INPUT_DIR/OUTPUT_ROOT_DIR and DB_PATH.
	Config settings.Config
	// BaseDir is BASE_DIR, the root for __packages/ and the token file. When empty it is derived
	// from Config.CategoriesFile's directory, then the process working directory.
	BaseDir string

	DB          DocumentStore
	Categories  Categories
	Relocalizer Relocalizer
	FileLocator FileLocator
	Registry    Registry
	Scanner     ScanRunner
	ChatSearch  ChatSearch
	Opener      Opener
	Runner      Runner
	Decisions   ManualDecisions

	// Now defaults to time.Now. It only drives the auto-generated ZIP name (Date.now()).
	Now func() time.Time
}

// Compile-time proof that the real collaborators satisfy the injected seams.
var (
	_ DocumentStore   = (*database.Store)(nil)
	_ Categories      = (*categories.Store)(nil)
	_ Relocalizer     = relocalize.Deps{}
	_ Registry        = relocalize.JSONRegistrySync{}
	_ ScanRunner      = (*triagescan.Scanner)(nil)
	_ ChatSearch      = ChatSearcher{}
	_ Opener          = RealOpener{}
	_ Runner          = osopen.ExecRunner{}
	_ ManualDecisions = (*manualdecisions.Store)(nil)
	_ FileLocator     = RealFileLocator{}
	_ RegistrySource  = (*database.Store)(nil)
)
