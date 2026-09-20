package aiprovider

import "testing"

// TestModelMatches pins the confirmation rule: an exact, case-insensitive match after trimming, a
// ":" tag, or a "-" suffix that is a build id (dated, numeric or "latest"). A "-" suffix naming a
// different model variant ("-mini", "-lite") is not a match, an empty echo is never a match, and a
// bare prefix without a separator is not either.
func TestModelMatches(t *testing.T) {
	tests := []struct {
		name      string
		requested string
		confirmed string
		want      bool
	}{
		{"exact match", "gpt-4o-mini", "gpt-4o-mini", true},
		{"case insensitive", "GPT-4O-Mini", "gpt-4o-mini", true},
		{"whitespace trimmed", "  gpt-4o-mini  ", " gpt-4o-mini ", true},
		{"mixed case and padded whitespace", "  GPT-4O  ", "  gpt-4o-2024-08-06 \t", true},
		{"dated build suffix", "gpt-4o", "gpt-4o-2024-08-06", true},
		{"numeric build suffix", "gemini-3.8-flash", "gemini-3.8-flash-001", true},
		{"eight digit build suffix", "claude-3-7-sonnet", "claude-3-7-sonnet-20250219", true},
		{"latest suffix", "gemini-3.8-flash", "gemini-3.8-flash-latest", true},
		{"colon tag suffix", "qwen3.5", "qwen3.5:9b", true},
		{"variant suffix is a different model", "gpt-4o", "gpt-4o-mini", false},
		{"lite variant suffix is a different model", "gemini-2.5-flash", "gemini-2.5-flash-lite", false},
		{"empty confirmed is never verified", "gpt-4o", "", false},
		{"empty requested", "", "gpt-4o", false},
		{"both empty", "", "", false},
		{"legacy alias mismatch", "deepseek-chat", "deepseek-flash", false},
		{"different provider model", "claude-3-5-sonnet-20241022", "claude-3-7-sonnet-20250219", false},
		{"bare prefix without separator", "gpt-4o", "gpt-4oX", false},
		{"numeric prefix without separator", "gpt-4", "gpt-4o-2024", false},
		{"too short numeric suffix", "gpt-4o", "gpt-4o-01", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ModelMatches(tc.requested, tc.confirmed); got != tc.want {
				t.Fatalf("ModelMatches(%q, %q) = %v, want %v", tc.requested, tc.confirmed, got, tc.want)
			}
		})
	}
}
