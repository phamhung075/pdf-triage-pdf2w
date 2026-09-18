package prompt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/promptpersonalization"
)

// Byte-exact fixture tests. The expected files under testdata/ were generated once with the REAL
// TypeScript implementation (src/domain/prompt.ts) under tsx via a throwaway script, for the same
// inputs this file rebuilds. Qwen's behaviour depends on the exact prompt bytes, so any drift here
// is a behavioural change:
//
//	testdata/prompts_real_nopersonal.json       real committed prompts/ templates, no personalization
//	testdata/prompts_real_personal.json         real templates + a fake, generic personalization overlay
//	testdata/prompts_fallbacks_nopersonal.json  no templates at all, so every loadPromptPart falls back
//
// The fake overlay is generic (MYCODE_42 / ACME CONSEIL) and carries no operator data.

type promptFixtureFile struct {
	Mode                string                       `json:"mode"`
	FakePersonalization json.RawMessage              `json:"fakePersonalization"`
	Cases               map[string]promptFixtureCase `json:"cases"`
}

type promptFixtureCase struct {
	System            string `json:"system"`
	User              string `json:"user"`
	TextSnippetLength *int   `json:"textSnippetLength"`
}

var fixtureCaseNames = []string{
	"entityExtraction",
	"entityExtractionTruncated",
	"markdownPlain",
	"markdownContinuationAndRepair",
	"classificationDefaults",
	"classificationTruncated",
	"classificationErrorHintEN",
	"classificationEntityHintNoDocType",
}

func loadFixture(t *testing.T, name string) promptFixtureFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	var fix promptFixtureFile
	if err := json.Unmarshal(data, &fix); err != nil {
		t.Fatalf("parsing fixture %s: %v", name, err)
	}
	return fix
}

func fixturePersonalization(t *testing.T, raw json.RawMessage) promptpersonalization.PromptPersonalization {
	t.Helper()
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return promptpersonalization.EMPTY_PROMPT_PERSONALIZATION
	}
	p, err := promptpersonalization.Parse(raw)
	if err != nil {
		t.Fatalf("parsing fixture personalization: %v", err)
	}
	return p
}

func TestFixturesMatchTypeScript(t *testing.T) {
	now := time.Date(2026, time.August, 12, 0, 0, 0, 0, time.Local)

	t.Run("real committed templates without personalization", func(t *testing.T) {
		runFixture(t, Deps{Templates: realPromptsFS(t)}, loadFixture(t, "prompts_real_nopersonal.json"), now)
	})

	t.Run("real committed templates with a personalization overlay", func(t *testing.T) {
		fix := loadFixture(t, "prompts_real_personal.json")
		d := Deps{Templates: realPromptsFS(t), Personalization: staticPersonalization(fixturePersonalization(t, fix.FakePersonalization))}
		runFixture(t, d, fix, now)
	})

	t.Run("hardcoded fallbacks without personalization", func(t *testing.T) {
		runFixture(t, Deps{Templates: emptyTemplates()}, loadFixture(t, "prompts_fallbacks_nopersonal.json"), now)
	})
}

func runFixture(t *testing.T, d Deps, fix promptFixtureFile, now time.Time) {
	t.Helper()
	if len(fix.Cases) != len(fixtureCaseNames) {
		t.Fatalf("fixture has %d cases, want %d (%v)", len(fix.Cases), len(fixtureCaseNames), fixtureCaseNames)
	}
	for _, name := range fixtureCaseNames {
		want, ok := fix.Cases[name]
		if !ok {
			t.Fatalf("fixture is missing case %q", name)
		}
		gotSystem, gotUser, gotLen := buildFixtureCase(d, name, now)
		if gotSystem != want.System {
			t.Errorf("%s: system differs from TS fixture: %s", name, firstDiff(gotSystem, want.System))
		}
		if gotUser != want.User {
			t.Errorf("%s: user differs from TS fixture: %s", name, firstDiff(gotUser, want.User))
		}
		switch {
		case want.TextSnippetLength == nil && gotLen != nil:
			t.Errorf("%s: textSnippetLength = %d, want absent", name, *gotLen)
		case want.TextSnippetLength != nil && gotLen == nil:
			t.Errorf("%s: textSnippetLength absent, want %d", name, *want.TextSnippetLength)
		case want.TextSnippetLength != nil && gotLen != nil && *want.TextSnippetLength != *gotLen:
			t.Errorf("%s: textSnippetLength = %d, want %d", name, *gotLen, *want.TextSnippetLength)
		}
	}
}

// buildFixtureCase rebuilds exactly the inputs the throwaway TS generator used.
func buildFixtureCase(d Deps, name string, now time.Time) (system, user string, textSnippetLength *int) {
	switch name {
	case "entityExtraction":
		p := d.BuildEntityExtractionPrompt(fixtureFilename, fixtureRawText)
		return p.System, p.User, nil
	case "entityExtractionTruncated":
		p := d.BuildEntityExtractionPrompt(fixtureFilename, strings.Repeat("b", 2500))
		return p.System, p.User, nil
	case "markdownPlain":
		p := d.BuildMarkdownConversionPrompt("BANG cAN oor xf roAN garbled OCR noise", nil, "")
		return p.System, p.User, nil
	case "markdownContinuationAndRepair":
		p := d.BuildMarkdownConversionPrompt("100.00 | 200.00 | food", &MarkdownContinuationContext{
			Header:    "| Date | Amount | Label |",
			Separator: "| --- | --- | --- |",
		}, "the table header was repeated")
		return p.System, p.User, nil
	case "classificationDefaults":
		p := d.BuildClassificationPrompt(fixtureCategories, fixtureFilename, fixtureRawText, ClassificationOptions{Now: now})
		return p.System, p.User, intPtr(p.TextSnippetLength)
	case "classificationTruncated":
		p := d.BuildClassificationPrompt(fixtureCategories, fixtureFilename, strings.Repeat("a", 5000), ClassificationOptions{Now: now})
		return p.System, p.User, intPtr(p.TextSnippetLength)
	case "classificationErrorHintEN":
		p := d.BuildClassificationPrompt(fixtureCategories, fixtureFilename, fixtureRawText, ClassificationOptions{
			PreviousError:  "subcategory was ungrounded",
			SystemLanguage: "EN",
			EntityHint:     &EntityHint{Entity: "Crédit Mutuel", DocType: "Bank Statement"},
			Now:            now,
		})
		return p.System, p.User, intPtr(p.TextSnippetLength)
	case "classificationEntityHintNoDocType":
		p := d.BuildClassificationPrompt(fixtureCategories, fixtureFilename, fixtureRawText, ClassificationOptions{
			EntityHint: &EntityHint{Entity: "Crédit Mutuel"},
			Now:        now,
		})
		return p.System, p.User, intPtr(p.TextSnippetLength)
	default:
		return "", "", nil
	}
}

func intPtr(v int) *int { return &v }

// firstDiff returns a compact description of the first byte difference between got and want so a
// byte-exact failure does not dump two 80 KB prompts.
func firstDiff(got, want string) string {
	min := len(got)
	if len(want) < min {
		min = len(want)
	}
	for i := 0; i < min; i++ {
		if got[i] != want[i] {
			lo := i - 40
			if lo < 0 {
				lo = 0
			}
			hiGot, hiWant := i+40, i+40
			if hiGot > len(got) {
				hiGot = len(got)
			}
			if hiWant > len(want) {
				hiWant = len(want)
			}
			return "at byte " + itoa(i) + "\n  got  ..." + got[lo:hiGot] + "...\n  want ..." + want[lo:hiWant] + "..."
		}
	}
	if len(got) != len(want) {
		return "length got=" + itoa(len(got)) + " want=" + itoa(len(want))
	}
	return "identical"
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
