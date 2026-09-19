package documentschema

import "testing"

// All cases below are ported verbatim from pdf-triage's src/domain/document.schema.test.ts.
// The TypeScript source is the behavioral source of truth: a case passes here only when it
// accepts/rejects exactly what the corresponding Zod schema does.

func TestDocumentMetadataSchema(t *testing.T) {
	t.Run("parses a fully-populated valid object unchanged", func(t *testing.T) {
		got, err := ParseDocumentMetadata([]byte(`{
			"titre": "Facture SFR", "registre": "REF-1", "date": "2024-05-12",
			"categorie": "invoices", "subcategorie": "sfr", "summary": "A vendor invoice",
			"tags": ["sfr", "invoice"], "markdown_content": "# Facture"
		}`))
		if err != nil {
			t.Fatalf("ParseDocumentMetadata() error = %v, want nil", err)
		}
		if got.Titre != "Facture SFR" || got.Registre != "REF-1" || got.Date != "2024-05-12" ||
			got.Categorie != "invoices" || got.Subcategorie != "sfr" || got.Summary != "A vendor invoice" ||
			got.MarkdownContent != "# Facture" {
			t.Fatalf("parsed = %+v, want the input fields unchanged", got)
		}
		if len(got.Tags) != 2 || got.Tags[0] != "sfr" || got.Tags[1] != "invoice" {
			t.Fatalf("Tags = %v, want [sfr invoice]", got.Tags)
		}
	})

	t.Run("rejects a missing titre", func(t *testing.T) {
		if _, err := ParseDocumentMetadata([]byte(`{"categorie":"invoices"}`)); err == nil {
			t.Fatal("ParseDocumentMetadata() error = nil, want an error")
		}
	})

	t.Run("rejects a missing categorie", func(t *testing.T) {
		if _, err := ParseDocumentMetadata([]byte(`{"titre":"Test"}`)); err == nil {
			t.Fatal("ParseDocumentMetadata() error = nil, want an error")
		}
	})

	t.Run("defaults optional fields when omitted", func(t *testing.T) {
		got, err := ParseDocumentMetadata([]byte(`{"titre":"Test","categorie":"administrative"}`))
		if err != nil {
			t.Fatalf("ParseDocumentMetadata() error = %v, want nil", err)
		}
		if got.Registre != "" {
			t.Fatalf("Registre = %q, want \"\"", got.Registre)
		}
		if got.Date != "" {
			t.Fatalf("Date = %q, want \"\"", got.Date)
		}
		if got.Subcategorie != "" {
			t.Fatalf("Subcategorie = %q, want \"\"", got.Subcategorie)
		}
		if got.Summary != "" {
			t.Fatalf("Summary = %q, want \"\"", got.Summary)
		}
		if len(got.Tags) != 0 {
			t.Fatalf("Tags = %v, want []", got.Tags)
		}
		if got.MarkdownContent != "" {
			t.Fatalf("MarkdownContent = %q, want \"\"", got.MarkdownContent)
		}
		if len(got.Other) != 0 {
			t.Fatalf("Other = %v, want {}", got.Other)
		}
	})

	// Regression test: Qwen frequently returns an explicit JSON `null` (not an absent key) for a
	// field that doesn't apply to this document type (e.g. expiry_date on a bank statement, iban
	// on a payslip). Before the nullableOptionalString fix, this threw "Expected string, received
	// null" out of DocumentMetadataSchema.parse(), which classify-document.ts's catch block turned
	// into a silent downgrade to the generic rule-based fallback classifier for the whole document
	// — confirmed against a real production log entry for a BNP Paribas bank statement.
	t.Run("normalizes an explicit null on an optional string field to \"\" instead of throwing", func(t *testing.T) {
		got, err := ParseDocumentMetadata([]byte(`{
			"titre": "Relevé de compte", "categorie": "bank",
			"total_amount": null, "vat_amount": null, "siren": null, "iban": null, "expiry_date": null,
			"contact_name": null, "contact_email": null, "contact_phone": null,
			"contact_address": null, "contact_website": null
		}`))
		if err != nil {
			t.Fatalf("ParseDocumentMetadata() error = %v, want nil", err)
		}
		for name, v := range map[string]string{
			"total_amount":    got.TotalAmount,
			"vat_amount":      got.VatAmount,
			"siren":           got.Siren,
			"iban":            got.Iban,
			"expiry_date":     got.ExpiryDate,
			"contact_name":    got.ContactName,
			"contact_email":   got.ContactEmail,
			"contact_phone":   got.ContactPhone,
			"contact_address": got.ContactAddress,
			"contact_website": got.ContactWebsite,
		} {
			if v != "" {
				t.Fatalf("%s = %q, want \"\"", name, v)
			}
		}
	})

	t.Run("still accepts a real string value on those same fields", func(t *testing.T) {
		got, err := ParseDocumentMetadata([]byte(`{
			"titre": "Facture", "categorie": "invoices",
			"iban": "FR7630001007941234567890185", "expiry_date": "2027-01-01"
		}`))
		if err != nil {
			t.Fatalf("ParseDocumentMetadata() error = %v, want nil", err)
		}
		if got.Iban != "FR7630001007941234567890185" {
			t.Fatalf("Iban = %q, want FR7630001007941234567890185", got.Iban)
		}
		if got.ExpiryDate != "2027-01-01" {
			t.Fatalf("ExpiryDate = %q, want 2027-01-01", got.ExpiryDate)
		}
	})
}

func TestSystemSettingsSchema(t *testing.T) {
	t.Run("accepts qwen3.5:9b as ollama_model", func(t *testing.T) {
		got, err := ParseSystemSettings([]byte(`{
			"input_dir": "/in", "output_root_dir": "/out",
			"ollama_model": "qwen3.5:9b", "ollama_host": "http://127.0.0.1:11434"
		}`))
		if err != nil {
			t.Fatalf("ParseSystemSettings() error = %v, want nil", err)
		}
		if got.InputDir != "/in" || got.OutputRootDir != "/out" ||
			got.OllamaModel != "qwen3.5:9b" || got.OllamaHost != "http://127.0.0.1:11434" {
			t.Fatalf("parsed = %+v, want the input fields unchanged", got)
		}
	})

	t.Run("rejects any ollama_model other than qwen3.5:9b (Golden Rule #14)", func(t *testing.T) {
		if _, err := ParseSystemSettings([]byte(`{
			"input_dir": "/in", "output_root_dir": "/out",
			"ollama_model": "llama3", "ollama_host": "http://127.0.0.1:11434"
		}`)); err == nil {
			t.Fatal("ParseSystemSettings() error = nil, want an error")
		}
	})

	t.Run("accepts cloud AI settings with google provider and keys", func(t *testing.T) {
		got, err := ParseSystemSettings([]byte(`{
			"input_dir": "/in", "output_root_dir": "/out",
			"ai_provider": "cloud", "cloud_provider": "google",
			"google_api_key": "ai-key", "google_model": "gemini-2.5-flash"
		}`))
		if err != nil {
			t.Fatalf("ParseSystemSettings() error = %v, want nil", err)
		}
		if got.AIProvider == nil || *got.AIProvider != "cloud" {
			t.Errorf("AIProvider = %v, want cloud", got.AIProvider)
		}
		if got.CloudProvider == nil || *got.CloudProvider != "google" {
			t.Errorf("CloudProvider = %v, want google", got.CloudProvider)
		}
		if got.GoogleAPIKey == nil || *got.GoogleAPIKey != "ai-key" {
			t.Errorf("GoogleAPIKey = %v, want ai-key", got.GoogleAPIKey)
		}
	})
}

func TestCategoriesConfigSchema(t *testing.T) {
	t.Run("parses nested subcategories recursively", func(t *testing.T) {
		got, err := ParseCategoriesConfig([]byte(`{
			"categories": [
				{
					"id": "invoices", "name": "Factures", "aliases": ["facture"],
					"subcategories": [
						{ "id": "sfr", "name": "SFR", "aliases": [], "subcategories": [{ "id": "sfr_mobile", "name": "SFR Mobile" }] }
					]
				}
			]
		}`))
		if err != nil {
			t.Fatalf("ParseCategoriesConfig() error = %v, want nil", err)
		}
		if len(got.Categories) != 1 || len(got.Categories[0].Subcategories) != 1 ||
			len(got.Categories[0].Subcategories[0].Subcategories) != 1 {
			t.Fatalf("parsed = %+v, want a nested subcategory", got)
		}
		if id := got.Categories[0].Subcategories[0].Subcategories[0].ID; id != "sfr_mobile" {
			t.Fatalf("nested ID = %q, want sfr_mobile", id)
		}
	})

	t.Run("rejects a category with no id", func(t *testing.T) {
		if _, err := ParseCategoriesConfig([]byte(`{"categories":[{"name":"Factures"}]}`)); err == nil {
			t.Fatal("ParseCategoriesConfig() error = nil, want an error")
		}
	})
}

func TestEntityDictionarySchema(t *testing.T) {
	t.Run("defaults missing domains to empty arrays", func(t *testing.T) {
		got, err := ParseEntityDictionary([]byte(`{"banks":[{"slug":"ca","name":"Crédit Agricole"}]}`))
		if err != nil {
			t.Fatalf("ParseEntityDictionary() error = %v, want nil", err)
		}
		if len(got.Banks) != 1 {
			t.Fatalf("len(Banks) = %d, want 1", len(got.Banks))
		}
		for name, arr := range map[string][]*EntityItem{
			"energy": got.Energy, "telecom": got.Telecom, "insurance": got.Insurance,
			"gov": got.Gov, "health": got.Health,
		} {
			if len(arr) != 0 {
				t.Fatalf("%s = %v, want []", name, arr)
			}
		}
	})

	t.Run("defaults an entity item aliases to an empty array when omitted", func(t *testing.T) {
		got, err := ParseEntityDictionary([]byte(`{"banks":[{"slug":"ca","name":"Crédit Agricole"}]}`))
		if err != nil {
			t.Fatalf("ParseEntityDictionary() error = %v, want nil", err)
		}
		if len(got.Banks[0].Aliases) != 0 {
			t.Fatalf("Banks[0].Aliases = %v, want []", got.Banks[0].Aliases)
		}
	})
}

func TestUpdateDocumentSchema(t *testing.T) {
	t.Run("accepts a partial update with only some fields set", func(t *testing.T) {
		got, err := ParseUpdateDocument([]byte(`{"title":"New Title","tags":["a","b"]}`))
		if err != nil {
			t.Fatalf("ParseUpdateDocument() error = %v, want nil", err)
		}
		if got.Title == nil || *got.Title != "New Title" {
			t.Fatalf("Title = %v, want New Title", got.Title)
		}
		if got.Tags == nil || len(*got.Tags) != 2 || (*got.Tags)[0] != "a" || (*got.Tags)[1] != "b" {
			t.Fatalf("Tags = %v, want [a b]", got.Tags)
		}
		if got.Category != nil {
			t.Fatalf("Category = %v, want nil (undefined)", *got.Category)
		}
	})

	t.Run("accepts an empty object (every field optional)", func(t *testing.T) {
		if _, err := ParseUpdateDocument([]byte(`{}`)); err != nil {
			t.Fatalf("ParseUpdateDocument() error = %v, want nil", err)
		}
	})
}

func TestSearchQuerySchema(t *testing.T) {
	t.Run("defaults query to \"\", mode to \"hybrid\", limit to 50 when omitted", func(t *testing.T) {
		got, err := ParseSearchQuery([]byte(`{}`))
		if err != nil {
			t.Fatalf("ParseSearchQuery() error = %v, want nil", err)
		}
		if got.Query != "" {
			t.Fatalf("Query = %q, want \"\"", got.Query)
		}
		if got.Mode != "hybrid" {
			t.Fatalf("Mode = %q, want hybrid", got.Mode)
		}
		if got.Limit != 50 {
			t.Fatalf("Limit = %d, want 50", got.Limit)
		}
	})

	t.Run("rejects an invalid mode value", func(t *testing.T) {
		if _, err := ParseSearchQuery([]byte(`{"mode":"fuzzy"}`)); err == nil {
			t.Fatal("ParseSearchQuery() error = nil, want an error")
		}
	})

	t.Run("rejects a non-positive limit", func(t *testing.T) {
		if _, err := ParseSearchQuery([]byte(`{"limit":0}`)); err == nil {
			t.Fatal("ParseSearchQuery(limit=0) error = nil, want an error")
		}
		if _, err := ParseSearchQuery([]byte(`{"limit":-5}`)); err == nil {
			t.Fatal("ParseSearchQuery(limit=-5) error = nil, want an error")
		}
	})
}
