// Package mcpserver is the Go port of pdf-triage's
// src/infrastructure/mcp/mcp-server.ts (581 lines) and its test suite.
//
// It exposes the same nine MCP tools under the same names, descriptions and JSON input
// schemas, with the same call semantics, over two transports:
//
//   - stdio (a single long-lived session), and
//   - a stateless Streamable HTTP transport on POST /mcp, authenticated with a bearer token
//     persisted to BASE_DIR/.mcp-api-token.
//
// The handler logic is deliberately transport-free. Every collaborator the TypeScript module
// imported as a singleton is an injected interface (see Deps), so the tool behavior can be
// tested against a real temp SQLite store and small fakes without opening a socket or
// starting the SDK's transports. The SDK wiring lives in server.go.
//
// The SDK pinned here is github.com/modelcontextprotocol/go-sdk v1.3.1: the newest release whose
// go directive is still go 1.23.0. v1.4.0 requires go 1.24.0 and v1.4.1+ (through v1.8.0) require
// go 1.25.0, so they would force this module's go directive above go 1.23.x.
package mcpserver

import (
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerName / ServerVersion are the Implementation identity the TS createToolServer passes to
// the MCP SDK (mcp-server.ts:482).
const (
	ServerName    = "pdf-triage-agent-mcp"
	ServerVersion = "1.0.0"
)

// ToolDefinitions returns the nine tools exactly as listMcpTools() does in
// mcp-server.ts:26-136. The input schemas are raw JSON because the SDK's generated schemas
// would not be byte-for-byte identical to the TypeScript ones; AddTool accepts raw JSON and
// the golden test asserts deep equality with the captured TypeScript output.
//
// A fresh slice is returned on every call so callers cannot mutate package state.
func ToolDefinitions() []*mcp.Tool {
	return []*mcp.Tool{
		{
			Name:        "search_documents",
			Description: "Search documents by title, summary, reference registre, category, or full raw text",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Search term or keywords"},"category":{"type":"string","description":"Optional category filter"},"subcategory":{"type":"string","description":"Optional subcategory filter"},"fileType":{"type":"string","description":"Optional document type filter: PDF, IMAGE, TEXT, WORD, EXCEL"},"limit":{"type":"number","description":"Max results count (default 20)"}}}`),
		},
		{
			Name:        "get_full_document_text",
			Description: "Retrieve the complete extracted raw text of a document by document ID",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"docId":{"type":"number","description":"Document ID"}},"required":["docId"]}`),
		},
		{
			Name:        "update_document_metadata",
			Description: "Modify title, registre, date, category, subcategory, summary, or tags for a document. If category or subcategory changes, the physical file is relocalized to the new canonical folder (auto-creating the category/subcategory in categories.json first, per Golden Rule #5). subcategory cannot be general/other/divers/a bare year (Golden Rule #4).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"docId":{"type":"number","description":"Document ID"},"title":{"type":"string"},"registre":{"type":"string"},"date":{"type":"string"},"category":{"type":"string"},"subcategory":{"type":"string"},"summary":{"type":"string"},"tags":{"type":"array","items":{"type":"string"}}},"required":["docId"]}`),
		},
		{
			Name:        "trigger_triage",
			Description: "Scan the incoming PDFs input folder and process all new documents",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		{
			Name:        "list_categories",
			Description: "List all available document categories and their description/keywords",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		{
			Name:        "prepare_dossier",
			Description: "Assemble and validate a complete list of required documents for administrative dossiers (housing, tax, employment, bank, identity). Returns present documents sorted by date and explicitly lists any missing required document types.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"dossierType":{"type":"string","description":"Type of dossier: housing, tax, employment, bank, or identity"},"limit":{"type":"number","description":"Max documents per category"}},"required":["dossierType"]}`),
		},
		{
			Name:        "get_document_markdown",
			Description: "Retrieve structured Markdown representation, Executive Summary, amounts, and contact details for a specific document ID",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"docId":{"type":"number","description":"Document ID"}},"required":["docId"]}`),
		},
		{
			Name:        "open_document_folder",
			Description: "Open OS File Explorer (Windows Explorer) with the file selected at its exact location on disk",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"docId":{"type":"number","description":"Document ID"}},"required":["docId"]}`),
		},
		{
			Name:        "package_documents",
			Description: "Build a .zip package of documents for handing to a third party (e.g. a housing/tax/bank dossier). Provide either an explicit docIds list, or a dossierType free-text query (same matching as prepare_dossier) to resolve the document set automatically. Writes the zip to disk under __packages/ and returns its path plus which requested documents (if any) had no file on disk.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"docIds":{"type":"array","items":{"type":"number"},"description":"Explicit document IDs to include. Provide this or dossierType."},"dossierType":{"type":"string","description":"Free-text dossier query (e.g. \"housing\", \"3 last pay slips\") used to auto-resolve documents when docIds is not given."},"zipName":{"type":"string","description":"Optional filename for the zip (default: an auto-generated name from dossierType or timestamp)."}}}`),
		},
	}
}

// ListTools is listMcpTools() (mcp-server.ts:26): the tools/list payload. Exported so the golden
// contract test can compare it without a transport.
func ListTools() *mcp.ListToolsResult {
	return &mcp.ListToolsResult{Tools: ToolDefinitions()}
}
