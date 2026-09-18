package guards

import (
	"fmt"

	"github.com/phamhung075/pdf-triage-pdf2w/taxonomy"
)

// This file is the ONE canonical implementation of the Golden Rule 4 subcategory predicates and
// the manual-decisions generic-category predicate. Every former call-site variant (web-server.ts
// :586 and :1262, mcp-server.ts :236, triage-scan.ts :301, relocalize-document.ts :208) funnels
// through these functions; none of them keeps a local list.

// IsForbiddenSubcategory is THE canonical Golden Rule 4 predicate. It delegates to
// taxonomy.IsForbiddenSubcategory rather than hand-rolling a list, exactly as the TS comment below
// demands.
//
// The TypeScript WHY comment, preserved verbatim (triage-scan.ts:295-300):
//
// Golden Rule #4. Use the canonical predicate, not a hand-rolled three-value test: it is the
// same one every other write path uses (web-server.ts, relocalize-document.ts, mcp-server.ts,
// categories-store.ts), and resolveSubcategory deliberately returns sentinels like 'unknown',
// 'camscanner', a bare year or a file extension *verbatim* so this guard blocks them
// (classification-resolution.ts). The old inline test caught 3 of the ~20, so the rest were
// archived into junk taxonomy branches instead of being held in __raws for review.
//
// classificationresolution's own local FORBIDDEN_SUBCATEGORIES set (package comment deviation 8)
// is deliberately narrower than taxonomy.IsForbiddenSubcategory: its 22 literal names are a subset
// of taxonomy's set, and taxonomy additionally rejects bare years, pure-numeric slugs, slugs ending
// in a file extension, and scanner-name prefixes. Every sentinel ResolveSubcategory can return
// ('general', the raw forbidden slug) is therefore caught here without reaching into that
// unexported set.
func IsForbiddenSubcategory(subcategory string) bool {
	return taxonomy.IsForbiddenSubcategory(subcategory)
}

// ForbiddenSubcategoryViolation returns the canonical Golden Rule 4 violation for an explicit
// subcategory write, or nil when the value is a valid final subcategory.
//
// Message is the document/MCP/relocalize form (web-server.ts:1263, mcp-server.ts:238,
// relocalize-document.ts:209). ShortMessage is the manual-decisions form (web-server.ts:587), which
// omits the trailing guidance sentence. Both strings are the exact TS interpolations.
//
// The TypeScript WHY comment, preserved verbatim (web-server.ts:1257-1260):
//
// Golden Rule #4: reject an explicit attempt to set a forbidden subcategory
// (general/other/divers/year) before writing anything — but don't re-block an
// unrelated edit (e.g. just the title) on a document whose EXISTING subcategory
// happens to already be one of these from before this rule was enforced everywhere.
func ForbiddenSubcategoryViolation(subcategory string) *GuardViolation {
	if !IsForbiddenSubcategory(subcategory) {
		return nil
	}
	reason := fmt.Sprintf("'%s' is not a valid subcategory (general/other/divers/year strings are not allowed — Golden Rule #4).", subcategory)
	v := newViolation(CodeForbiddenSubcategory, reason+" Please choose a specific entity or document-type name.", 400)
	v.ShortMessage = reason
	v.Subcategory = subcategory
	return v
}

// StrictNoSubcategoryViolation is the triage-scan "strict no-subcategory fail guard" (Golden Rule
// 4) for a subcategory the classifier RESOLVED (rather than one a caller explicitly set). It is the
// same predicate as ForbiddenSubcategoryViolation — IsForbiddenSubcategory — and differs only in
// the scan-loop message and blocked-file reason, which this function returns verbatim.
//
// The TS source (triage-scan.ts:307, :312) blocks the file and records reason 'NO_SUBCATEGORY'.
func StrictNoSubcategoryViolation(filename, resolvedSubcategory string) *GuardViolation {
	if !IsForbiddenSubcategory(resolvedSubcategory) {
		return nil
	}
	v := newViolation(CodeNoSubcategory,
		fmt.Sprintf("❌ Blocked: Failed to assign specific subcategory to '%s'. Moved to __raws/.blocked_files.", filename),
		0)
	v.Subcategory = resolvedSubcategory
	v.BlockedFileReason = "NO_SUBCATEGORY"
	return v
}

// IsGenericCategory reports whether a target category is one of the four generic/empty values the
// manual-decisions edit rejects: ”, 'general', 'other', 'divers'. web-server.ts:589 checks the
// already-lowercased-and-trimmed value, so this predicate lowercases and trims first.
func IsGenericCategory(category string) bool {
	switch trimLower(category) {
	case "", "general", "other", "divers":
		return true
	}
	return false
}

// GenericCategoryViolation returns the manual-decisions generic-category rejection, or nil when the
// category is concrete. The message is the exact string at web-server.ts:590.
func GenericCategoryViolation(category string) *GuardViolation {
	if !IsGenericCategory(category) {
		return nil
	}
	return newViolation(CodeGenericCategory,
		"A decision needs a concrete target category — general/other/divers are not allowed.", 400)
}
