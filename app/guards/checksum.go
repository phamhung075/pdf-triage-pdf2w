package guards

import "regexp"

// checksumUniqueRe is the TS test `/UNIQUE constraint failed.*checksum/i` (triage-scan.ts:382),
// matched against the SQLite insert error message.
var checksumUniqueRe = regexp.MustCompile(`(?i)UNIQUE constraint failed.*checksum`)

// ChecksumDecision is the pure outcome of the checksum-dedup guard.
type ChecksumDecision int

const (
	// ChecksumInsert means no row owns this checksum, so the scan may insert the new document.
	ChecksumInsert ChecksumDecision = iota
	// ChecksumSkipDuplicate means a row already owns this checksum, so the incoming file is moved to
	// __raws/.duplicates_files and skipped.
	ChecksumSkipDuplicate
)

// DecideChecksumDuplicate is the ONE implementation of the checksum dedup decision used by both
// triage-scan.ts dedup sites (:242 pre-check and :386 insert-time collision). It is pure: given
// whether an existing row was found for the checksum, it decides skip vs insert. The caller owns
// the lookup and the physical move.
func DecideChecksumDuplicate(existingFound bool) ChecksumDecision {
	if existingFound {
		return ChecksumSkipDuplicate
	}
	return ChecksumInsert
}

// IsChecksumUniqueViolation reports whether an insert error message is the
// `UNIQUE constraint failed: documents.checksum` collision the pre-check can miss between its
// lookup and the insert. It mirrors the TS regex exactly.
//
// The TypeScript WHY comment, preserved verbatim (triage-scan.ts:373-381):
//
// Another checksum-owning row can appear between the pre-check above (line ~146)
// and this insert — classifyPDFText's Step A/C/D round-trip takes tens of seconds,
// a wide window for a concurrent scan/repair/manual-edit to insert the same content
// first. Without this, the file is left in __raws and gets the full (expensive)
// AI classification re-run every single 10s auto-watcher tick, forever, since it's
// never blocked or moved — this is what "SQLITE_CONSTRAINT: UNIQUE constraint
// failed: documents.checksum" repeating for every subsequent file in production logs
// traced back to.
func IsChecksumUniqueViolation(errMessage string) bool {
	return checksumUniqueRe.MatchString(errMessage)
}
