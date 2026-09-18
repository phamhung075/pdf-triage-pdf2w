# pdf-triage-pdf2w

Go microservice hosting incremental ports of pdf-triage's pure, low-frequency (roughly
once-per-file) domain logic, called by the TypeScript app over plain HTTP:

- `POST /canonical-path` — `computeCanonicalPath` (`src/domain/taxonomy.ts`): the canonical
  `__archive/<category>/<subcategory>/<year>/<file>` path for a classified document.
- `POST /clean-text` — `cleanExtractedText` (`src/domain/pdf-text.ts`): normalizes raw extracted
  text (strip null bytes, normalize newlines, collapse blank runs) before it enters the pipeline.

This is part of migrating pdf-triage's backend from TypeScript to Go/Rust, one scoped slice at a
time. It does **not** implement PDF/text extraction — that stays with the existing, unmodified,
self-hosted `markdown-extract-service` (pdf2w). Hot-path/high-frequency pure functions (called
per-file-in-a-loop or per-DB-row, e.g. `isForbiddenSubcategory`, `detectFileType`) are deliberately
NOT ported here as standalone HTTP calls — see pdf-triage's
`docs/superpowers/specs/2026-09-17-pdf2w-extraction-swap-design.md` for why, and the project's
`project_go_rust_migration` memory for the target end state (this service's scope grows until a
full Go backend can replace the TypeScript one outright, rather than turning every call site into
a network hop).

## Running the pdf-triage backend

`cmd/pdf-triage` is the composition root that replaces `src/index.ts`, `src/vision-lab-main.ts` and
the npm scripts `dev`/`start`/`scan`/`mcp`/`vision:dev`. It wires the ported `app/`, `store/`,
`infra/`, `httpapi`, `mcpserver` and `visionlab` packages into one binary:

```sh
make build                 # -> dist/pdf-triage, static (CGO_ENABLED=0)
make cross                 # -> dist/pdf-triage-linux-amd64 + dist/pdf-triage-windows-amd64.exe
./dist/pdf-triage serve        # HTTP + SSE dashboard/API + 10 s auto-watcher
./dist/pdf-triage scan         # one-shot triage scan; non-zero exit when Ollama is down
./dist/pdf-triage mcp          # MCP over stdio (+ streamable HTTP per settings)
./dist/pdf-triage vision-lab   # standalone Vision Lab diagnostic server
```

`make test`, `make vet` and `make fmt` run the Go suite, `go vet` and `gofmt` across the module.

**Cutover:** the binary opens the EXISTING `pdf_triage.db`, `settings.json`, `categories.json` /
`.categories.private.json`, `manual_decisions.json`, `taxonomy_hints.json` and
`.prompts.private.json` unchanged — there is no data migration, and reverting to the TypeScript
backend touches nothing.

### Configuration (infra/settings)

`BASE_DIR` defaults to the process working directory and is overridable with
`PDF_TRIAGE_BASE_DIR`; `DATA_DIR` defaults to `BASE_DIR` and is overridable with
`PDF_TRIAGE_DATA_DIR`. Runtime settings live in `DATA_DIR/settings.json`
(`language`, `input_dir`, `output_root_dir`, `ollama_model`, `ollama_host`,
`personal_name_denylist`), edited by the dashboard. The environment variables, all read by
`infra/settings` (see `settings.go` and `.env.example` at the repository root), are:

| Variable | Purpose |
| --- | --- |
| `PDF_TRIAGE_BASE_DIR`, `PDF_TRIAGE_DATA_DIR` | asset root / writable state root |
| `PDF_INPUT_DIR`, `PDF_OUTPUT_DIR` | incoming (`__raws`) and archive (`__archive`) folders |
| `PDF_DB_PATH`, `PDF_REGISTRY_PATH` | SQLite file and JSON mirror |
| `SYSTEM_LANGUAGE` | `FR` (default) or `EN` |
| `PORT`, `PDF_TRIAGE_HOST` | dashboard port (3971) and bind host (127.0.0.1) |
| `VISION_LAB_PORT` | Vision Lab port (3179) |
| `OLLAMA_HOST`, `OLLAMA_MODEL`, `OLLAMA_EMBED_MODEL`, `OLLAMA_VISION_MODEL` | Ollama endpoints; `qwen3.5:9b` is pinned for classification |
| `PDF2W_SERVICE_URL`, `PDF2W_SERVICE_TIMEOUT_MS` | required external pdf2w extraction service |
| `MCP_HTTP_PORT`, `MCP_HTTP_HOST` | MCP streamable-HTTP transport (3972 / 0.0.0.0) |
| `PDF_TRIAGE_LOG_DIR`, `PDF_TRIAGE_LOG_MAX_BYTES`, `PDF_TRIAGE_LOG_RETAIN` | log rotation |

A `.env` in `DATA_DIR` (falling back to `BASE_DIR`) is loaded without overriding already-set
process variables.

### Dashboard

`serve` mounts `BASE_DIR/public` as static files with `Cache-Control: no-store`, exactly as
`web-server.ts` did; `/` serves `index.html`. There is no authentication, so the default bind host
is `127.0.0.1` and the port is single-instance-locked at `DATA_DIR/.server.lock` (with the same
EADDRINUSE take-over behavior as the TypeScript server). The MCP streamable HTTP transport is the
one LAN-reachable surface and is protected by the bearer token stored in `BASE_DIR/.mcp-api-token`.

## Run

```sh
go run ./cmd/server        # listens on :3985 (PORT env var to override)
curl http://127.0.0.1:3985/health
```

## Test

```sh
go test ./...
```

## API

| Endpoint | Method | Body | Response |
| --- | --- | --- | --- |
| `/health` | GET | — | `{"status":"ok","service":"pdf-triage-pdf2w"}` |
| `/canonical-path` | POST | `{"originalPath":string,"category":string,"outputRootDir":string,"subcategory":string\|null,"dateStr":string\|null,"title":string\|null}` | `{"canonicalPath":string}` |
| `/clean-text` | POST | `{"text":string}` | `{"text":string}` |
