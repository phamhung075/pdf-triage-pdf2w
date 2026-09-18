package aichat

import "testing"

// TestParseDocDateAndRequestedCountParity pins the two pure helpers whose JavaScript semantics are
// easy to get subtly wrong. Every expected value was produced by running the real TypeScript
// functions under node:
//
//   - parseDocDate: JS Date validates month 01-12 and day 01-31 and normalizes an over-long day
//     into the next month ("2024-02-30" -> 2024-03), while month 00/13 and day 00/32 are invalid
//     and collapse to 0; "2024-1-5" falls through the ISO/FR patterns to the year-only branch.
//   - extractRequestedCount: the trailing \b after the accented "relevé(s)" means "mon relevé"
//     yields no count, the second pattern's first capture group is a word so parseInt yields NaN
//     and that alternative never produces a number, and the 1..20 bound rejects "25 documents".
func TestParseDocDateAndRequestedCountParity(t *testing.T) {
	dates := map[string]int64{
		"2026-05-31":           1780185600000,
		"31/05/2026":           1780185600000,
		"2026":                 1767225600000,
		"2024-02-30":           1709251200000,
		"2024-13-01":           0,
		"2024-00-10":           0,
		"2024-01-00":           0,
		"2024-1-5":             1704067200000,
		"20240531":             1717113600000,
		"15.01.2024":           1705276800000,
		"2024_01_15":           1705276800000,
		"2024-01-15T10:00:00Z": 1704067200000,
		"2026-05-31 12:34":     1780185600000,
		"no date":              0,
		"20-01-2024":           1705708800000,
		"31-12-2025":           1767139200000,
	}
	for in, want := range dates {
		if got := parseDocDate(in); got != want {
			t.Errorf("parseDocDate(%q) = %d, want %d", in, got, want)
		}
	}

	counts := map[string]int{
		"j'ai besoin 2 derniers fiche de paie": 2,
		"3 fiche de paie":                      3,
		"les 3 derniers bulletins de salaire":  3,
		"mon dernier bulletin de salaire":      1,
		"last 2 statements":                    2,
		"top 5 docs":                           5,
		"single invoice":                       1,
		"deux factures":                        2,
		"trois bulletins":                      3,
		"dernière facture":                     1,
		"2 bulletins":                          2,
	}
	for in, want := range counts {
		got := extractRequestedCount(in)
		if got == nil || *got != want {
			t.Errorf("extractRequestedCount(%q) = %v, want %d", in, got, want)
		}
	}
	for _, in := range []string{"j'ai besoin d'un RIB", "mon relevé", "mes relevés", "25 documents", "0 factures"} {
		if got := extractRequestedCount(in); got != nil {
			t.Errorf("extractRequestedCount(%q) = %d, want nil", in, *got)
		}
	}
}
