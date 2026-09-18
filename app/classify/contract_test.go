package classify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
	"github.com/phamhung075/pdf-triage-pdf2w/store/categories"
)

// TestClassifyPDFTextPinsOllamaRequestContract is the port of the TS regression guard
// "requests think:false from Ollama" plus explicit Golden Rule 14 (only the configured model) and
// Golden Rule 20 (temperature 0.1 through the ollama package). It drives the REAL
// *infra/ollama.Client against an httptest server and inspects the wire bodies.
func TestClassifyPDFTextPinsOllamaRequestContract(t *testing.T) {
	const model = "qwen3.5:9b-classify-contract"

	var (
		mu     sync.Mutex
		bodies []map[string]any
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"name": model}}})
		case "/api/generate":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			bodies = append(bodies, body)
			n := len(bodies)
			mu.Unlock()

			response := ""
			switch n {
			case 1: // health probe
				response = "ok"
			case 2: // Step A
				response = `{"issuing_entity":"SFR","document_type":"Invoice"}`
			case 3: // Step C
				response = "# SFR\n\nFacture Total TTC 45.99"
			case 4: // Step D
				response = `{"titre":"Facture SFR","registre":"","date":"2024-05-12","categorie":"invoices","subcategorie":"sfr","summary":"s","tags":[]}`
			default:
				response = "ok"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"response": response, "done_reason": "stop"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := ollama.New(ollama.Config{BaseURL: server.URL, Model: model, Timeout: 5 * time.Second})

	e := newEnv()
	e.cfg.OllamaModel = model
	e.cfg.OllamaHost = server.URL
	deps := e.deps()
	deps.Ollama = client // the real client, not the fake

	result, err := deps.ClassifyPDFText("SFR Facture Total TTC 45.99", "facture.pdf", "", time.Now(), "")
	if err != nil {
		t.Fatalf("ClassifyPDFText: %v", err)
	}
	if result.Categorie != "invoices" || result.Subcategorie != "sfr" {
		t.Fatalf("got %s/%s, want invoices/sfr", result.Categorie, result.Subcategorie)
	}

	mu.Lock()
	got := append([]map[string]any(nil), bodies...)
	mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("generate request count = %d, want 4 (health + Step A + Step C + Step D)", len(got))
	}

	for i, body := range got {
		if body["model"] != model {
			t.Errorf("request %d model = %v, want %q (Golden Rule 14: only the configured model)", i+1, body["model"], model)
		}
	}

	// Step A (index 1) and Step C (index 2): the two calls the TS regression test names. Both must
	// request think:false.
	for _, index := range []int{1, 2} {
		if think, ok := got[index]["think"].(bool); !ok || think {
			t.Errorf("request %d think = %v, want false", index+1, got[index]["think"])
		}
	}

	// Golden Rule 20: the classification calls use temperature 0.1, format json, num_ctx 16384,
	// num_predict 4096.
	for _, index := range []int{1, 3} {
		options, ok := got[index]["options"].(map[string]any)
		if !ok {
			t.Fatalf("request %d has no options object", index+1)
		}
		if options["temperature"] != 0.1 {
			t.Errorf("request %d temperature = %v, want 0.1", index+1, options["temperature"])
		}
		if options["num_ctx"] != float64(16384) {
			t.Errorf("request %d num_ctx = %v, want 16384", index+1, options["num_ctx"])
		}
		if options["num_predict"] != float64(4096) {
			t.Errorf("request %d num_predict = %v, want 4096", index+1, options["num_predict"])
		}
		if got[index]["format"] != "json" {
			t.Errorf("request %d format = %v, want json", index+1, got[index]["format"])
		}
	}

	// Step C has no format constraint and uses the text-chat temperature.
	options, ok := got[2]["options"].(map[string]any)
	if !ok {
		t.Fatal("request 3 has no options object")
	}
	if options["temperature"] != 0.2 {
		t.Errorf("request 3 temperature = %v, want 0.2", options["temperature"])
	}
	if _, present := got[2]["format"]; present {
		t.Errorf("request 3 must not set format, got %v", got[2]["format"])
	}
}

// TestGoldenRule5AutoCreationWritesPrivateOverlayOnly pins Golden Rule 5 at the classify layer:
// the pre-move auto-creation calls the categories store's Save, which writes
// .categories.private.json, and never the committed categories.json.
func TestGoldenRule5AutoCreationWritesPrivateOverlayOnly(t *testing.T) {
	dir := t.TempDir()
	publicFile := filepath.Join(dir, "categories.json")
	privateFile := filepath.Join(dir, ".categories.private.json")

	publicJSON := `{"categories":[{"id":"invoices","name":"Factures","description":"Factures et reçus","aliases":["facture","invoice"],"subcategories":[]}]}`
	if err := os.WriteFile(publicFile, []byte(publicJSON), 0o644); err != nil {
		t.Fatalf("write public categories: %v", err)
	}
	store := categories.New(publicFile, privateFile, io.Discard)

	e := newEnv()
	e.ollama.classScripts = classifyResponses(
		`{"issuing_entity":"SFR","document_type":"Invoice"}`,
		`{"titre":"Facture SFR","registre":"","date":"2024-05-12","categorie":"invoices","subcategorie":"sfr","summary":"s","tags":[]}`,
	)
	e.ollama.textScripts = text("# SFR\n\nFacture Total TTC 45.99")

	deps := e.deps()
	deps.Categories = store

	result, err := deps.ClassifyPDFText("SFR Facture Total TTC 45.99", "facture.pdf", "", time.Now(), "")
	if err != nil {
		t.Fatalf("ClassifyPDFText: %v", err)
	}
	if result.Categorie != "invoices" || result.Subcategorie != "sfr" {
		t.Fatalf("got %s/%s, want invoices/sfr", result.Categorie, result.Subcategorie)
	}

	// The committed public file must be byte-for-byte untouched.
	after, err := os.ReadFile(publicFile)
	if err != nil {
		t.Fatalf("read public categories: %v", err)
	}
	if string(after) != publicJSON {
		t.Fatalf("categories.json was modified:\n%s", after)
	}

	// The private overlay must hold the auto-created subcategory.
	privateRaw, err := os.ReadFile(privateFile)
	if err != nil {
		t.Fatalf("read private categories: %v", err)
	}
	assertContains(t, string(privateRaw), `"sfr"`)
}
