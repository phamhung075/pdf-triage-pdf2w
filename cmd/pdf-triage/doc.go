// Command pdf-triage is the single Go binary that replaces pdf-triage's TypeScript backend
// composition root: src/index.ts (subcommand dispatch), src/vision-lab-main.ts, and the npm
// scripts dev/start/scan/mcp/vision:dev. It wires the already-ported Go packages of
// github.com/phamhung075/pdf-triage-pdf2w into one process and exposes four subcommands:
//
//	pdf-triage serve        HTTP + SSE dashboard and API, plus the 10-second auto-watcher
//	pdf-triage scan         one-shot triage scan (`npm run scan`)
//	pdf-triage mcp          MCP over stdio, with the streamable HTTP transport per settings
//	pdf-triage vision-lab   standalone Vision Lab diagnostic server (`npm run vision:dev`)
//
// # Wiring
//
// Everything is constructed explicitly in newApplication; there are no package globals other than
// main itself. The dependency order mirrors the migration design's porting order
// (docs/superpowers/specs/2026-09-18-go-backend-migration-design.md, decision 10):
//
//	settings (infra/settings) -> logger (infra/logger) -> store/database -> stores
//	(categories, entitydictionary, taxonomyhints, manualdecisions, promptpersonalization)
//	-> infra adapters (ollama, pdf2w/pdfextractor, image processor, vision, orientation, crop,
//	osopen, jsonregistry, pdfscanner, zipbuilder) -> app layer (guards, classify with prompt.Deps
//	reading templates from the prompts directory, relocalize, convertimage, imagetopdf,
//	triagescan, repair, clear, scanlock, aichat, chatplanner, taskstate) -> httpapi.NewServer with
//	Deps and the DocumentReadRoutes / DocumentWriteRoutes groups plus the Watcher -> mcpserver ->
//	visionlab.
//
// # Cutover decision 5 (read this before changing anything)
//
// The binary opens the EXISTING pdf_triage.db, settings.json, taxonomy files and JSON mirrors
// unchanged. There is no data migration: database.Open runs the same idempotent schema/migrations
// as src/infrastructure/db/database.ts, settings.Store reads the same settings.json, and the
// stores read/write the same categories.json / .categories.private.json / manual_decisions.json /
// taxonomy_hints.json / .prompts.private.json files. The two former HTTP hops (computeCanonicalPath
// and cleanExtractedText) are in-process calls into canonicalpath and cleantext.
//
// # BASE_DIR
//
// Exactly like settings.ts, BASE_DIR defaults to the process working directory (the TS module URL
// resolved to the project root, which for `npm run dev`/`npm run scan` is process.cwd()) and is
// overridable with PDF_TRIAGE_BASE_DIR. The composition root passes that directory explicitly as
// settings.Options.BaseDir instead of relying on settings' executable-directory fallback, so a
// binary run from any checkout finds the checkout's prompts/, categories.json and public/.
//
// # Adapters (adapters.go)
//
// The two seams whose Go interfaces do not line up with the ported packages are adapted here,
// inside the composition root, rather than by editing those packages:
//
//   - infra/osopen builds pure osopen.Plan values; httpapi and mcpserver expect Launch / their own
//     opener and runner shapes. osLauncher and the spawner/runner adapters bridge them.
//   - mcpserver.ListTools returns the SDK result, while httpapi's GET /api/mcp/status wants
//     name/description pairs. mcpToolLister projects them.
//
// # Reported interface gaps (not worked around)
//
//  1. The composition root captures the *httpapi.Server through a RouteGroup and shares its SSE Hub
//     with the auto-watcher via httpapi.(*Server).Hub(), so the watcher now broadcasts its own raw
//     SCAN_*/FILE_* frames through the server's own hub to the same dashboard clients as a manual
//     scan (TASK_* updates already arrive through the shared taskstate.Manager broadcaster).
//     TestWatcherSSEBroadcastsScanFrames pins this.
//  2. httpapi's jsonBodyMiddleware caps every request body at 100 kB before handlers run, while
//     POST /api/images/import documents a 64 MB limit. The gap is pre-existing and reported by the
//     httpapi port itself; this composition root does not compensate for it.
package main
