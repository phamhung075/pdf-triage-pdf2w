package classify

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
	"github.com/phamhung075/pdf-triage-pdf2w/prompt"
	"github.com/phamhung075/pdf-triage-pdf2w/promptpersonalization"
	"github.com/phamhung075/pdf-triage-pdf2w/store/categories"
	"github.com/phamhung075/pdf-triage-pdf2w/taxonomyconflicts"
)

// completionScript / textScript are one scripted answer for the fake Ollama client.
type completionScript struct {
	res ollama.Completion
	err error
}

type textScript struct {
	res ollama.TextCompletion
	err error
}

type callArgs struct {
	system string
	user   string
}

// fakeOllama is the injectable replacement for the TS `generate` mock. Classification and text
// scripts are separate queues because the pipeline calls the two distinct wrappers.
type fakeOllama struct {
	healthy      bool
	classScripts []completionScript
	textScripts  []textScript

	healthCalls []string
	classCalls  []callArgs
	textCalls   []callArgs
}

func (f *fakeOllama) EnsureOllamaModel(modelName string) bool {
	f.healthCalls = append(f.healthCalls, modelName)
	return f.healthy
}

func (f *fakeOllama) RequestClassificationCompletion(system, user string) (ollama.Completion, error) {
	f.classCalls = append(f.classCalls, callArgs{system: system, user: user})
	if len(f.classScripts) == 0 {
		return ollama.Completion{}, errors.New("unexpected classification completion call")
	}
	script := f.classScripts[0]
	f.classScripts = f.classScripts[1:]
	return script.res, script.err
}

func (f *fakeOllama) RequestTextChatCompletion(system, user string) (ollama.TextCompletion, error) {
	f.textCalls = append(f.textCalls, callArgs{system: system, user: user})
	if len(f.textScripts) == 0 {
		return ollama.TextCompletion{}, errors.New("unexpected text chat completion call")
	}
	script := f.textScripts[0]
	f.textScripts = f.textScripts[1:]
	return script.res, script.err
}

func (f *fakeOllama) totalCalls() int {
	return len(f.healthCalls) + len(f.classCalls) + len(f.textCalls)
}

// logRecord is one captured logger call.
type logRecord struct {
	level   string
	module  string
	message string
	meta    map[string]any
}

type fakeLogger struct {
	records []logRecord
}

func (l *fakeLogger) add(level, module, message string, meta any) {
	m, _ := meta.(map[string]any)
	l.records = append(l.records, logRecord{level: level, module: module, message: message, meta: m})
}

func (l *fakeLogger) Debug(module, message string, meta any, filename ...string) {
	l.add("DEBUG", module, message, meta)
}

func (l *fakeLogger) Info(module, message string, meta any, filename ...string) {
	l.add("INFO", module, message, meta)
}

func (l *fakeLogger) Warn(module, message string, meta any, filename ...string) {
	l.add("WARN", module, message, meta)
}

// find returns the first record at level whose message contains substr, or nil.
func (l *fakeLogger) find(level, substr string) *logRecord {
	for i := range l.records {
		r := &l.records[i]
		if r.level == level && strings.Contains(r.message, substr) {
			return r
		}
	}
	return nil
}

func (l *fakeLogger) messages(level, substr string) []string {
	out := []string{}
	for _, r := range l.records {
		if r.level == level && strings.Contains(r.message, substr) {
			out = append(out, r.message)
		}
	}
	return out
}

// fakeCategories serves the real built-in default taxonomy (publicFile "" -> absent -> defaults)
// while recording the private-overlay Save without touching disk. Golden Rule 5 is exercised for
// real in TestGoldenRule5PrivateOverlaySave.
type fakeCategories struct {
	store *categories.Store
	saves int
	saved [][]*documentschema.CategoryItem
}

func newFakeCategories() *fakeCategories {
	return &fakeCategories{store: categories.New("", "", io.Discard)}
}

func (f *fakeCategories) GetCategoriesConfig() documentschema.CategoriesConfig {
	return f.store.GetCategoriesConfig()
}

func (f *fakeCategories) SaveCategoriesConfig(cats []*documentschema.CategoryItem) error {
	f.saves++
	f.saved = append(f.saved, append([]*documentschema.CategoryItem(nil), cats...))
	return nil
}

type fakeDictionary struct {
	dict documentschema.EntityDictionary
}

func (f *fakeDictionary) GetEntityDictionary() documentschema.EntityDictionary { return f.dict }

type fakePersonalization struct {
	calls int
}

func (f *fakePersonalization) GetPromptPersonalization() promptpersonalization.PromptPersonalization {
	f.calls++
	return promptpersonalization.EMPTY_PROMPT_PERSONALIZATION
}

type fakeHints struct {
	recorded []*taxonomyconflicts.TaxonomyHintEntry
}

func (f *fakeHints) RecordTaxonomyHint(entry *taxonomyconflicts.TaxonomyHintEntry) {
	f.recorded = append(f.recorded, entry)
}

// env bundles the fakes and a default config.
type env struct {
	ollama *fakeOllama
	log    *fakeLogger
	cats   *fakeCategories
	dict   *fakeDictionary
	pers   *fakePersonalization
	hints  *fakeHints
	cfg    Config
}

func newEnv() *env {
	return &env{
		ollama: &fakeOllama{healthy: true},
		log:    &fakeLogger{},
		cats:   newFakeCategories(),
		dict:   &fakeDictionary{},
		pers:   &fakePersonalization{},
		hints:  &fakeHints{},
		cfg: Config{
			OllamaModel: "qwen3.5:9b",
			OllamaHost:  "http://127.0.0.1:11434",
			Language:    "FR",
		},
	}
}

func (e *env) deps() Deps {
	return Deps{
		Config:                e.cfg,
		Ollama:                e.ollama,
		Categories:            e.cats,
		EntityDictionary:      e.dict,
		PromptPersonalization: e.pers,
		TaxonomyHints:         e.hints,
		Prompts:               prompt.Deps{},
		Log:                   e.log,
	}
}

// jsWhitespaceCollapse is `text.replace(/\s+/g, ' ')` for the ASCII test data.
var jsWhitespaceCollapse = regexp.MustCompile(`[` + jsWhitespaceClass + `]+`)

func collapseWhitespace(text string) string {
	return jsWhitespaceCollapse.ReplaceAllString(text, " ")
}

// assertContains fails the test when the haystack does not contain the needle.
func assertContains(t testingT, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected to contain %q, got:\n%s", needle, haystack)
	}
}

// testingT is the subset of *testing.T the small assertion helpers need.
type testingT interface {
	Helper()
	Fatalf(format string, args ...any)
	Errorf(format string, args ...any)
}

func distinctWords(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz"
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = fmt.Sprintf("libelle%c%c%c", alphabet[i%26], alphabet[(i/26)%26], alphabet[(i*5)%26])
	}
	return strings.Join(parts, " ")
}

func entity(slug, name string, aliases ...string) *documentschema.EntityItem {
	return &documentschema.EntityItem{Slug: slug, Name: name, Aliases: aliases}
}

func emptyDictionary() documentschema.EntityDictionary {
	return documentschema.EntityDictionary{
		Banks:     []*documentschema.EntityItem{},
		Energy:    []*documentschema.EntityItem{},
		Telecom:   []*documentschema.EntityItem{},
		Insurance: []*documentschema.EntityItem{},
		Gov:       []*documentschema.EntityItem{},
		Health:    []*documentschema.EntityItem{},
	}
}

func bankDictionary(items ...*documentschema.EntityItem) documentschema.EntityDictionary {
	dict := emptyDictionary()
	dict.Banks = items
	return dict
}

func govDictionary(items ...*documentschema.EntityItem) documentschema.EntityDictionary {
	dict := emptyDictionary()
	dict.Gov = items
	return dict
}
