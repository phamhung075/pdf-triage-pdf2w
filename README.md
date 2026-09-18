# pdf-triage-pdf2w

Go microservice powering pdf-triage's "organize-files" step: a single HTTP endpoint,
`POST /canonical-path`, wrapping a port of pdf-triage's `computeCanonicalPath`
(`src/domain/taxonomy.ts`) so the canonical `__archive/<category>/<subcategory>/<year>/<file>`
path for a classified document is computed the same way regardless of which process ends up
owning that logic long-term.

This is the first slice of migrating pdf-triage's backend from TypeScript to Go/Rust. It does
**not** implement PDF/text extraction — that stays with the existing, unmodified, self-hosted
`markdown-extract-service` (pdf2w). See pdf-triage's
`docs/superpowers/specs/2026-09-17-pdf2w-extraction-swap-design.md` for the full design.

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
