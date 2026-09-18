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
