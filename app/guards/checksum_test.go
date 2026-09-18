package guards

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// Ported from triage-scan-duplicate-collision.test.ts:110-142 and triage-scan.ts:242, :382.

func TestDecideChecksumDuplicate(t *testing.T) {
	if got := DecideChecksumDuplicate(false); got != ChecksumInsert {
		t.Fatalf("DecideChecksumDuplicate(false) = %v, want ChecksumInsert", got)
	}
	if got := DecideChecksumDuplicate(true); got != ChecksumSkipDuplicate {
		t.Fatalf("DecideChecksumDuplicate(true) = %v, want ChecksumSkipDuplicate", got)
	}
}

func TestIsChecksumUniqueViolation(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		// The exact production log line the TS WHY comment quotes.
		{"production SQLITE_CONSTRAINT", "SQLITE_CONSTRAINT: UNIQUE constraint failed: documents.checksum", true},
		// modernc.org/sqlite's spelling, which the Go port will actually see.
		{"modernc constraint failed", "constraint failed: UNIQUE constraint failed: documents.checksum (2067)", true},
		{"lowercase", "unique constraint failed: documents.checksum", true},
		{"other column", "UNIQUE constraint failed: documents.id", false},
		{"other error", "no such table: documents", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := IsChecksumUniqueViolation(tc.msg); got != tc.want {
				t.Fatalf("IsChecksumUniqueViolation(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

// TestChecksumCollisionRaceDecision pins the race path's decision.
func TestChecksumCollisionRaceDecision(t *testing.T) {
	if got := DecideChecksumDuplicate(false); got != ChecksumInsert {
		t.Fatalf("pre-check decision = %v, want ChecksumInsert", got)
	}
	if !IsChecksumUniqueViolation("constraint failed: UNIQUE constraint failed: documents.checksum (2067)") {
		t.Fatal("insert-time collision not recognized")
	}
	if got := DecideChecksumDuplicate(true); got != ChecksumSkipDuplicate {
		t.Fatalf("post-collision decision = %v, want ChecksumSkipDuplicate", got)
	}
}

// TestIsChecksumUniqueViolation_AgainstRealSQLite proves the recognition works against the actual
// driver the Go backend uses (modernc.org/sqlite) rather than only a guessed message string. It runs
// on an in-memory database — no operator file.
func TestIsChecksumUniqueViolation_AgainstRealSQLite(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE documents (id INTEGER PRIMARY KEY, checksum TEXT UNIQUE NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO documents (checksum) VALUES ('abc123')`); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO documents (checksum) VALUES ('abc123')`)
	if err == nil {
		t.Fatal("expected a UNIQUE violation")
	}
	t.Logf("modernc.org/sqlite UNIQUE error = %q", err.Error())
	if !IsChecksumUniqueViolation(err.Error()) {
		t.Fatalf("IsChecksumUniqueViolation(%q) = false, want true", err.Error())
	}
}
