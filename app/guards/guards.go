// Package guards is the ONE shared implementation of the Golden-Rule write guards that were
// duplicated across three TypeScript surfaces before this port. It is design decision 6 of the Go
// backend migration spec (docs/superpowers/specs/2026-09-18-go-backend-migration-design.md §6):
// no call site may reimplement a guard; every call site calls into this package.
//
// The TypeScript sources whose behavior this package centralizes (the behavioral source of truth):
//
//   - src/infrastructure/http/web-server.ts
//     :29-45   the resolveManagedPath path-boundary guard (WHY comment preserved verbatim below)
//     :586-591 PUT /api/manual-decisions/:id forbidden-subcategory + generic-category rejects
//     :1262    PUT /api/documents/:id forbidden-subcategory reject
//     :1275-1277 the ensureCategoryAndSubcategoryExist step BEFORE the physical relocalize
//     :1112-1138 GET /api/documents/file-by-path path-guard uses (403)
//   - src/infrastructure/mcp/mcp-server.ts
//     :236     update_document_metadata forbidden-subcategory reject
//     :261-264 ensureCategoryAndSubcategoryExist step BEFORE the physical relocalize
//   - src/application/triage-scan.ts
//     :216-240 the < 10 clean-character no-text block (Golden Rule 3)
//     :242-276 the pre-check checksum dedup
//     :295-325 the strict no-subcategory block, using the canonical predicate (Golden Rule 4)
//     :373-418 the insert-time UNIQUE-constraint checksum collision (Golden Rule 20)
//   - src/application/relocalize-document.ts
//     :142-161 findActualFileOnDisk (stale-ghost detection before a move)
//     :163-189 ensureCategoryAndSubcategoryExist (Golden Rule 5)
//     :208-210 forbidden-subcategory reject in reclassifyAndRelocalizeDocument
//   - src/domain/taxonomy.ts :3-10 isPathInsideDir, :28-52 isForbiddenSubcategory
//
// The guard-related cases from web-server.test.ts, mcp-server.test.ts,
// relocalize-document.test.ts and triage-scan*.test.ts are ported in the _test.go files here.
//
// Upstream test runs at port time (on WSL/Linux):
//
//   - src/infrastructure/http/web-server.test.ts  -> 51 passed, 2 failed
//   - src/infrastructure/mcp/mcp-server.test.ts   -> 24 passed
//   - src/application/relocalize-document.test.ts -> 24 passed
//   - src/application/triage-scan.test.ts         -> 3 passed
//   - src/application/triage-scan-duplicate-collision.test.ts -> 2 passed
//
// The two web-server failures are the known-red cases the migration design §9 names. The TOCTOU
// auto-watcher case is not a guard and is not ported. The file-by-path 404-vs-403 case IS a
// path-boundary case and is pinned here at its ACTUAL behavior (the guard REJECTS the drive-letter
// candidate -> 403), not at the test's stated 404 expectation. See ResolveManagedPath and
// path_test.go for the contradiction write-up.
//
// # TS-vs-Go semantic gaps, all resolved to MATCH the TypeScript surface
//
//  1. JS String.prototype.trim() / .length. The TS no-text guard is
//     `(raw_text || ”).trim().length < 10`, using JavaScript's WhiteSpace+LineTerminator set and
//     UTF-16 code units. Go's strings.TrimSpace (unicode.White_Space) and len/utf8.RuneCountInString
//     disagree (U+FEFF, U+0085, astral pairs), so notext.go reproduces the exact JS set and counts
//     UTF-16 code units, exactly as extractionqualitygate and classificationresolution do in their
//     unexported copies.
//  2. Node path.resolve / path.normalize on the POSIX target. taxonomy.IsPathInsideDir already
//     documented its POSIX-path.Clean deviation. ResolveManagedPath adds the one piece that matters
//     for the pinned red case: on the Linux/WSL target a Windows drive-letter candidate is a
//     RELATIVE Node path (path.isAbsolute("C:/x") === false), so path.resolve prepends the process
//     cwd and it can never be inside a drive-form managed root. The Go guard reproduces that verdict
//     lexically (no cwd I/O) and uses pathconv to translate Windows spellings only when a managed
//     root is itself a WSL mount path (`/mnt/<drive>/...`), which is what settings.ts produces on
//     WSL (settings.ts:489, NormalizePathInput). See ResolveManagedPath's comment.
//  3. Synchronous vs error-returning saves. The TS ensureCategoryAndSubcategoryExist calls
//     saveCategoriesConfig() and ignores its result; the Go store returns an error, which this
//     function propagates to the caller. The in-memory mutation and the one-way private-overlay
//     diff are unchanged.
//  4. Category/subcategory id mutation is on the store's merged config value, which is freshly
//     built by store/categories.GetCategoriesConfig on every call (exactly like the TS
//     getCategoriesConfig), so an in-place append is not observable to other callers.
//  5. Missing vs explicit-empty input. TS distinguishes `undefined` (guard skipped) from `”`
//     (forbidden). Go strings conflate them; callers therefore invoke the guard only for fields the
//     request actually supplied, and an explicit "" is treated as forbidden, matching the TS
//     predicate's `if (!subcategory) return true`.
//  6. gofmt's doc-comment formatter rewrites a pair of single quotes (the TS empty-string literal
//     ”) to a typographic ” inside doc comments; the surrounding TS words are otherwise verbatim,
//     and all user-facing message STRING LITERALS keep the exact TS text.
package guards

// GuardCode identifies which Golden Rule a GuardViolation enforces. It is stable and
// machine-readable so callers (HTTP, MCP, the scan loop) can branch on it instead of matching
// message text.
type GuardCode string

const (
	// CodeForbiddenSubcategory is Golden Rule 4: an explicit or resolved subcategory is one of
	// general/other/divers, a scanner name, a file extension, a bare year, or otherwise unusable.
	CodeForbiddenSubcategory GuardCode = "FORBIDDEN_SUBCATEGORY"
	// CodeGenericCategory is the manual-decisions guard: a decision needs a concrete target
	// category, not general/other/divers/empty.
	CodeGenericCategory GuardCode = "GENERIC_CATEGORY"
	// CodeNoSubcategory is the triage-scan strict fail guard: classification resolved to a
	// forbidden subcategory, so the file is blocked in __raws for manual review.
	CodeNoSubcategory GuardCode = "NO_SUBCATEGORY"
	// CodeNoTextExtracted is Golden Rule 3: fewer than 10 clean characters were extracted.
	CodeNoTextExtracted GuardCode = "NO_TEXT_EXTRACTED"
	// CodePathOutsideManaged is the resolveManagedPath guard: a caller-supplied path is not inside
	// INPUT_DIR (__raws) or OUTPUT_ROOT_DIR (__archive).
	CodePathOutsideManaged GuardCode = "PATH_OUTSIDE_MANAGED"
)

// GuardViolation is the typed error every guard returns. It carries the exact user-facing message
// the corresponding TypeScript surface produces today plus its HTTP-status hint, so the HTTP and
// MCP ports can return byte-identical responses without re-deriving either.
type GuardViolation struct {
	// Code is the stable machine-readable guard id.
	Code GuardCode
	// Message is the canonical user-facing message (the document/MCP/relocalize form where the
	// surfaces differ).
	Message string
	// ShortMessage is the manual-decisions form of Message where the TS surface omits the trailing
	// guidance sentence (web-server.ts:587 vs :1263). Empty when there is no variant.
	ShortMessage string
	// HTTPStatus is the HTTP status the corresponding route returns (0 when the guard is not an
	// HTTP route, e.g. the scan loop).
	HTTPStatus int
	// Subcategory is set for CodeForbiddenSubcategory / CodeNoSubcategory.
	Subcategory string
	// BlockedFileReason is the blocked_files.reason the scan loop writes (triage-scan.ts:312, :227);
	// empty when the guard is not a scan-block guard.
	BlockedFileReason string
}

// Error implements error so a GuardViolation can be returned directly from application code.
func (v *GuardViolation) Error() string {
	if v == nil {
		return ""
	}
	return v.Message
}

var _ error = (*GuardViolation)(nil)

// newViolation is the only place a GuardViolation is constructed, keeping the typed error
// exhaustive and the message fields consistent.
func newViolation(code GuardCode, message string, status int) *GuardViolation {
	return &GuardViolation{Code: code, Message: message, HTTPStatus: status}
}
