package guards

// NoTextMinCleanCharacters is Golden Rule 3's floor: a document whose clean extracted text is
// shorter than this is blocked, with no DB row and no move.
const NoTextMinCleanCharacters = 10

// IsNoTextBlocked is the ONE pure implementation of Golden Rule 3's no-text guard. It reproduces
// triage-scan.ts:216-217 exactly:
//
//	const cleanText = (raw_text || '').trim();
//	if (!cleanText || cleanText.length < 10) { ... }
//
// so the comparison is over JS-trimmed text measured in UTF-16 code units (see the package comment
// deviation 1), not Go bytes or runes.
func IsNoTextBlocked(rawText string) bool {
	clean := jsTrim(rawText)
	return utf16Len(clean) < NoTextMinCleanCharacters
}

// NoTextViolation returns the Golden Rule 3 block violation with the exact message and blocked-file
// reason triage-scan.ts:222, :227 writes.
func NoTextViolation() *GuardViolation {
	v := newViolation(CodeNoTextExtracted,
		"❌ Blocked: No text extracted from PDF. Moved to __raws/blocked_files.", 0)
	v.BlockedFileReason = "NO_TEXT_EXTRACTED"
	return v
}
