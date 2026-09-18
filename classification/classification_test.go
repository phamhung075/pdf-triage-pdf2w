package classification

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/promptpersonalization"
)

// All cases below are ported verbatim from pdf-triage's
// src/domain/classification.test.ts (77 passing cases upstream). The TypeScript source is the
// behavioral source of truth.

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

func dictionaryWith(overrides func(*documentschema.EntityDictionary)) documentschema.EntityDictionary {
	d := emptyDictionary()
	overrides(&d)
	return d
}

func entity(slug, name string, aliases ...string) *documentschema.EntityItem {
	if aliases == nil {
		aliases = []string{}
	}
	return &documentschema.EntityItem{Slug: slug, Name: name, Aliases: aliases}
}

var defaultPersonalNameDenylist = []string{"dupond", "martin", "lefebvre", "bernard"}

func mustParsePersonalization(t *testing.T, jsonStr string) promptpersonalization.PromptPersonalization {
	t.Helper()
	p, err := promptpersonalization.Parse([]byte(jsonStr))
	if err != nil {
		t.Fatalf("Parse(%s) error = %v, want nil", jsonStr, err)
	}
	return p
}

func TestNormalizeSlug(t *testing.T) {
	t.Run("strips accents instead of replacing them with underscores — regression guard for the \"propri_taire\" bug (accented \"propriétaire\" mid-word breakage)", func(t *testing.T) {
		if got := NormalizeSlug("Propriétaire"); got != "proprietaire" {
			t.Fatalf("NormalizeSlug(Propriétaire) = %q, want %q", got, "proprietaire")
		}
	})

	t.Run("still collapses genuinely non-alphanumeric separators (spaces, punctuation) into underscores", func(t *testing.T) {
		if got := NormalizeSlug("Crédit Mutuel / Springfield"); got != "credit_mutuel_springfield" {
			t.Fatalf("NormalizeSlug(Crédit Mutuel / Springfield) = %q, want %q", got, "credit_mutuel_springfield")
		}
	})
}

func TestCleanAndParseJSON(t *testing.T) {
	t.Run("strips ```json fences and trailing commas", func(t *testing.T) {
		got, err := CleanAndParseJSON("```json\n{\"titre\": \"Test\", \"categorie\": \"invoices\",}\n```")
		if err != nil {
			t.Fatalf("CleanAndParseJSON error = %v, want nil", err)
		}
		want := map[string]any{"titre": "Test", "categorie": "invoices"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("CleanAndParseJSON = %#v, want %#v", got, want)
		}
	})

	t.Run("throws when the response has no JSON object at all", func(t *testing.T) {
		_, err := CleanAndParseJSON("I cannot help with that request.")
		if err == nil {
			t.Fatal("CleanAndParseJSON error = nil, want an error")
		}
		if !strings.Contains(err.Error(), "No JSON object found in AI response") {
			t.Fatalf("CleanAndParseJSON error = %q, want it to contain %q", err.Error(), "No JSON object found in AI response")
		}
	})

	t.Run("repairs a truncated response (unterminated string, missing closing brace)", func(t *testing.T) {
		got, err := CleanAndParseJSON("{\"titre\": \"Test Doc\", \"markdown_content\": \"some unterminated text")
		if err != nil {
			t.Fatalf("CleanAndParseJSON error = %v, want nil", err)
		}
		want := map[string]any{"titre": "Test Doc", "markdown_content": "some unterminated text"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("CleanAndParseJSON = %#v, want %#v", got, want)
		}
	})

	t.Run("repairs truncation inside a nested array", func(t *testing.T) {
		got, err := CleanAndParseJSON("{\"titre\": \"Test\", \"tags\": [\"a\", \"b\"")
		if err != nil {
			t.Fatalf("CleanAndParseJSON error = %v, want nil", err)
		}
		want := map[string]any{"titre": "Test", "tags": []any{"a", "b"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("CleanAndParseJSON = %#v, want %#v", got, want)
		}
	})

	t.Run("ignores text before the first { and after the last }", func(t *testing.T) {
		got, err := CleanAndParseJSON("Here is the JSON: {\"titre\": \"Test\"} — hope that helps!")
		if err != nil {
			t.Fatalf("CleanAndParseJSON error = %v, want nil", err)
		}
		want := map[string]any{"titre": "Test"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("CleanAndParseJSON = %#v, want %#v", got, want)
		}
	})
}

func TestPreprocessRawText(t *testing.T) {
	t.Run("de-concatenates fused OCR text with camelCase, number boundaries, and currency symbols", func(t *testing.T) {
		raw := "Invoice Customersname:DUPONDJ Totalpayable€12.98 Invoicedate/ Deliverydate02.05.2024 Order#406-5109483"
		processed := PreprocessRawText(raw)
		for _, want := range []string{
			"Invoice Customers name: DUPONDJ",
			"Total payable € 12.98",
			"Invoice date/ Delivery date 02.05.2024",
			"Order # 406-5109483",
		} {
			if !strings.Contains(processed, want) {
				t.Fatalf("PreprocessRawText(...) = %q, want it to contain %q", processed, want)
			}
		}
	})
}

func TestMatchEntityDictionary(t *testing.T) {
	t.Run("matches an entity by its exact name, case-insensitively", func(t *testing.T) {
		dict := dictionaryWith(func(d *documentschema.EntityDictionary) {
			d.Banks = []*documentschema.EntityItem{entity("credit_agricole", "Crédit Agricole", "ca")}
		})
		got := MatchEntityDictionary("extrait de compte crédit agricole paris", []string{"banks"}, dict)
		assertEntityMatch(t, got, "bank", "credit_agricole")
	})

	t.Run("matches an entity by alias", func(t *testing.T) {
		dict := dictionaryWith(func(d *documentschema.EntityDictionary) {
			d.Insurance = []*documentschema.EntityItem{entity("maif", "MAIF", "mutuelle assurance instituteurs")}
		})
		got := MatchEntityDictionary("contrat mutuelle assurance instituteurs 2024", []string{"insurance"}, dict)
		assertEntityMatch(t, got, "insurance", "maif")
	})

	t.Run("matches accented entity names against accented text (Unicode word boundary)", func(t *testing.T) {
		dict := dictionaryWith(func(d *documentschema.EntityDictionary) {
			d.Banks = []*documentschema.EntityItem{entity("societe_generale", "Société Générale")}
		})
		got := MatchEntityDictionary("extrait de compte société générale paris", []string{"banks"}, dict)
		assertEntityMatch(t, got, "bank", "societe_generale")
	})

	t.Run("does NOT match an accented entity name against unaccented search text — this is why every accented entity in entity_dictionary.json must also ship an unaccented alias", func(t *testing.T) {
		dict := dictionaryWith(func(d *documentschema.EntityDictionary) {
			d.Banks = []*documentschema.EntityItem{entity("credit_agricole", "Crédit Agricole")}
		})
		if got := MatchEntityDictionary("extrait de compte credit agricole paris", []string{"banks"}, dict); got != nil {
			t.Fatalf("MatchEntityDictionary = %#v, want nil", got)
		}
	})

	t.Run("does not match a name as a substring of a longer word (word-boundary correctness)", func(t *testing.T) {
		dict := dictionaryWith(func(d *documentschema.EntityDictionary) {
			d.Insurance = []*documentschema.EntityItem{entity("axa", "AXA")}
		})
		// "taxaphone" contains "axa" as a substring but is not a match
		if got := MatchEntityDictionary("société taxaphone service client", []string{"insurance"}, dict); got != nil {
			t.Fatalf("MatchEntityDictionary = %#v, want nil", got)
		}
	})

	t.Run("returns null when nothing matches", func(t *testing.T) {
		dict := dictionaryWith(func(d *documentschema.EntityDictionary) {
			d.Banks = []*documentschema.EntityItem{entity("credit_agricole", "Crédit Agricole")}
		})
		if got := MatchEntityDictionary("nothing recognizable here", []string{"banks"}, dict); got != nil {
			t.Fatalf("MatchEntityDictionary = %#v, want nil", got)
		}
	})
}

func TestBuildEntityHintLine(t *testing.T) {
	t.Run("formats matching entities as \"slug (Name), slug (Name).\"", func(t *testing.T) {
		dict := dictionaryWith(func(d *documentschema.EntityDictionary) {
			d.Banks = []*documentschema.EntityItem{
				entity("credit_agricole", "Crédit Agricole"),
				entity("fortuneo", "Fortuneo"),
			}
		})
		want := " Known real-world entities: credit_agricole (Crédit Agricole), fortuneo (Fortuneo)."
		if got := BuildEntityHintLine("bank", dict); got != want {
			t.Fatalf("BuildEntityHintLine = %q, want %q", got, want)
		}
	})

	t.Run("returns an empty string when no domain maps to the category", func(t *testing.T) {
		dict := dictionaryWith(func(d *documentschema.EntityDictionary) {
			d.Banks = []*documentschema.EntityItem{entity("credit_agricole", "Crédit Agricole")}
		})
		if got := BuildEntityHintLine("totally_made_up_category_xyz", dict); got != "" {
			t.Fatalf("BuildEntityHintLine = %q, want %q", got, "")
		}
	})
}

func TestIsGroundedSubcategorySlug(t *testing.T) {
	t.Run("rejects a slug shorter than 3 characters", func(t *testing.T) {
		if got := IsGroundedSubcategorySlug("ab", "ab ab ab", "file.pdf", defaultPersonalNameDenylist); got {
			t.Fatalf("IsGroundedSubcategorySlug = %v, want false", got)
		}
	})

	t.Run("rejects a generic/structural word even if it appears in the text", func(t *testing.T) {
		if got := IsGroundedSubcategorySlug("page", "page 1 of page 2", "file.pdf", defaultPersonalNameDenylist); got {
			t.Fatalf("IsGroundedSubcategorySlug = %v, want false", got)
		}
	})

	t.Run("rejects a slug built from a personal/household name token", func(t *testing.T) {
		if got := IsGroundedSubcategorySlug("jean_dupond", "jean dupond jean dupond", "file.pdf", defaultPersonalNameDenylist); got {
			t.Fatalf("IsGroundedSubcategorySlug = %v, want false", got)
		}
	})

	t.Run("rejects a slug with zero occurrences in the document text", func(t *testing.T) {
		if got := IsGroundedSubcategorySlug("veolia", "nothing here", "random.pdf", defaultPersonalNameDenylist); got {
			t.Fatalf("IsGroundedSubcategorySlug = %v, want false", got)
		}
	})

	t.Run("rejects a filename-echoed slug that appears only once in the text", func(t *testing.T) {
		if got := IsGroundedSubcategorySlug("veolia", "Veolia mentioned once", "veolia_invoice.pdf", defaultPersonalNameDenylist); got {
			t.Fatalf("IsGroundedSubcategorySlug = %v, want false", got)
		}
	})

	t.Run("accepts a filename-echoed slug that appears at least twice in the text", func(t *testing.T) {
		if got := IsGroundedSubcategorySlug("veolia", "Veolia here and Veolia there", "veolia_invoice.pdf", defaultPersonalNameDenylist); !got {
			t.Fatalf("IsGroundedSubcategorySlug = %v, want true", got)
		}
	})

	t.Run("accepts a non-filename-echoed slug that appears once in the text", func(t *testing.T) {
		if got := IsGroundedSubcategorySlug("france_travail", "Contact France Travail for details", "doc123.pdf", defaultPersonalNameDenylist); !got {
			t.Fatalf("IsGroundedSubcategorySlug = %v, want true", got)
		}
	})
}

func TestPreprocessRawTextFalseSplits(t *testing.T) {
	t.Run("leaves ordinary words that merely end in a field keyword intact", func(t *testing.T) {
		// This text is what Step A extracts the issuer from and Step D writes titre/summary from, so
		// a false split corrupts a value that is then fed back to Step D as "GROUND TRUTH".
		if got := PreprocessRawText("private corporate surname username rate item code"); got != "private corporate surname username rate item code" {
			t.Fatalf("PreprocessRawText = %q, want unchanged", got)
		}
		if got := PreprocessRawText("the candidate will consolidate and validate the mandate"); got != "the candidate will consolidate and validate the mandate" {
			t.Fatalf("PreprocessRawText = %q, want unchanged", got)
		}
	})

	t.Run("still splits the genuine OCR fusions it exists for", func(t *testing.T) {
		if got := PreprocessRawText("Customersname"); got != "Customers name" {
			t.Fatalf("PreprocessRawText(Customersname) = %q, want %q", got, "Customers name")
		}
		if got := PreprocessRawText("Invoicedate"); got != "Invoice date" {
			t.Fatalf("PreprocessRawText(Invoicedate) = %q, want %q", got, "Invoice date")
		}
	})
}

func TestRuleBasedClassifyBranchShadowingRegressions(t *testing.T) {
	// Each case below was a live misclassification found by audit and reproduced before the fix.
	c := func(text, filename string) string {
		r := RuleBasedClassify(text, filename, emptyDictionary(), defaultPersonalNameDenylist)
		return r.Categorie + "/" + r.Subcategorie
	}

	t.Run("keeps a bank statement as bank when a fines row appears inside it (Golden Rule #6)", func(t *testing.T) {
		if got := c("RELEVE DE COMPTE CREDIT MUTUEL solde crediteur\n12/03 PRLV ANTAI amende -45,00", "releve.pdf"); got != "bank/credit_mutuel" {
			t.Fatalf("got %q, want %q", got, "bank/credit_mutuel")
		}
	})

	t.Run("still classifies a genuine fine when the document is not a statement", func(t *testing.T) {
		if got := c("Avis de contravention ANTAI amende forfaitaire 45 euros", "avis.pdf"); got != "administrative/amende" {
			t.Fatalf("got %q, want %q", got, "administrative/amende")
		}
	})

	t.Run("does not read \"CNIL\" boilerplate as an identity document", func(t *testing.T) {
		if got := c("Conformement a la loi informatique et libertes, la CNIL peut etre saisie. Facture n 123 Total TTC 45", "lettre.pdf"); got != "invoices/facture" {
			t.Fatalf("got %q, want %q", got, "invoices/facture")
		}
	})

	t.Run("does not read a Visa card payment as a residence permit", func(t *testing.T) {
		if got := c("Paiement par carte visa le 12/03 Facture n 998 Total TTC 22,50", "recu.pdf"); got != "invoices/facture" {
			t.Fatalf("got %q, want %q", got, "invoices/facture")
		}
	})

	t.Run("still classifies a real long-stay visa as identity", func(t *testing.T) {
		if got := c("Visa de long sejour titre de sejour prefecture", "visa.pdf"); got != "identity/titre_sejour" {
			t.Fatalf("got %q, want %q", got, "identity/titre_sejour")
		}
	})

	t.Run("does not turn \"Business Center\" in an address into a transport pass", func(t *testing.T) {
		if got := c("Justificatif de domicile Business Center 12 rue des Lilas quittance", "dom.pdf"); got != "housing/justificatif_domicile" {
			t.Fatalf("got %q, want %q", got, "housing/justificatif_domicile")
		}
	})

	t.Run("still classifies an actual transport document", func(t *testing.T) {
		if got := c("Abonnement Navigo annuel ile-de-france", "navigo.pdf"); got != "administrative/navigo" {
			t.Fatalf("got %q, want %q", got, "administrative/navigo")
		}
	})

	t.Run("does not file a CDI as an internship because it mentions a past stage", func(t *testing.T) {
		if got := c("Contrat de travail CDI. Le salarie a effectue un stage en 2019.", "contrat.pdf"); got != "contracts/cdi_cdd" {
			t.Fatalf("got %q, want %q", got, "contracts/cdi_cdd")
		}
	})

	t.Run("still classifies a real internship attestation", func(t *testing.T) {
		if got := c("Attestation de stage effectue du 01/01 au 30/06", "stage.pdf"); got != "education/attestation_stage" {
			t.Fatalf("got %q, want %q", got, "education/attestation_stage")
		}
	})

	t.Run("does not mint administrative/dossier_administratif from the word \"dossier\" in a letter", func(t *testing.T) {
		if got := c("Madame, Monsieur, votre dossier a bien ete recu. Cordialement.", "courrier.pdf"); got != "correspondence/general" {
			t.Fatalf("got %q, want %q", got, "correspondence/general")
		}
	})

	t.Run("does not mint an invoices/free subcategory from the English word \"free\"", func(t *testing.T) {
		// This one was grounded, so it was genuinely auto-created into the private taxonomy.
		if got := c("Invoice 42 Free delivery included Total due 99.00 USD", "inv.pdf"); got != "invoices/facture" {
			t.Fatalf("got %q, want %q", got, "invoices/facture")
		}
	})

	t.Run("still recognises the Free telecom operator", func(t *testing.T) {
		if got := c("Facture Free Mobile forfait Total TTC 19,99", "facture.pdf"); got != "invoices/free" {
			t.Fatalf("got %q, want %q", got, "invoices/free")
		}
	})
}

func TestRuleBasedClassify(t *testing.T) {
	t.Run("classifies a pay slip under bulletin_salaire (never invoices), extracting employer + DD/MM/YYYY date", func(t *testing.T) {
		dict := dictionaryWith(func(d *documentschema.EntityDictionary) {
			d.Gov = []*documentschema.EntityItem{entity("acme_corp", "Acme Corp", "acmecorp")}
		})
		result := RuleBasedClassify(
			"Bulletin de salaire AcmeCorp Salaire brut 3000 Net a payer 2400 01/03/2023",
			"bulletin_mars.pdf",
			dict,
			defaultPersonalNameDenylist,
		)
		if result.Categorie != "bulletin_salaire" {
			t.Fatalf("Categorie = %q, want %q", result.Categorie, "bulletin_salaire")
		}
		if result.Subcategorie != "acme_corp" {
			t.Fatalf("Subcategorie = %q, want %q", result.Subcategorie, "acme_corp")
		}
		if result.Title != "bulletin mars" {
			t.Fatalf("Title = %q, want %q", result.Title, "bulletin mars")
		}
		if result.Date != "2023-03-01" {
			t.Fatalf("Date = %q, want %q", result.Date, "2023-03-01")
		}
	})

	t.Run("classifies a passport under identity/passeport", func(t *testing.T) {
		result := RuleBasedClassify("Republique Francaise Passeport N 12AB34567", "doc.pdf", emptyDictionary(), defaultPersonalNameDenylist)
		if result.Categorie != "identity" {
			t.Fatalf("Categorie = %q, want %q", result.Categorie, "identity")
		}
		if result.Subcategorie != "passeport" {
			t.Fatalf("Subcategorie = %q, want %q", result.Subcategorie, "passeport")
		}
	})

	t.Run("classifies titre-An-Ngo.pdf and titre-Dung-Ngo.pdf under identity/titre_sejour", func(t *testing.T) {
		res1 := RuleBasedClassify("Carte de sejour residence permit", "titre-An-Ngo.pdf", emptyDictionary(), defaultPersonalNameDenylist)
		if res1.Categorie != "identity" || res1.Subcategorie != "titre_sejour" {
			t.Fatalf("res1 = %q/%q, want identity/titre_sejour", res1.Categorie, res1.Subcategorie)
		}

		res2 := RuleBasedClassify("Residence permit France", "titre-Dung-Ngo.pdf", emptyDictionary(), defaultPersonalNameDenylist)
		if res2.Categorie != "identity" || res2.Subcategorie != "titre_sejour" {
			t.Fatalf("res2 = %q/%q, want identity/titre_sejour", res2.Categorie, res2.Subcategorie)
		}
	})

	t.Run("classifies a plain tax notice under administrative/impot", func(t *testing.T) {
		result := RuleBasedClassify(
			"Direction Generale des Finances Publiques DGFIP Avis d'impot sur le revenu 2023",
			"impot2023.pdf",
			emptyDictionary(),
			defaultPersonalNameDenylist,
		)
		if result.Categorie != "administrative" {
			t.Fatalf("Categorie = %q, want %q", result.Categorie, "administrative")
		}
		if result.Subcategorie != "impot" {
			t.Fatalf("Subcategorie = %q, want %q", result.Subcategorie, "impot")
		}
	})

	t.Run("does NOT misfile a bank statement as impot just because a transaction row mentions impots (Golden Rule #6 guard)", func(t *testing.T) {
		result := RuleBasedClassify(
			"RELEVE DE COMPTE Credit Mutuel Springfield PRLV IMPOTS DGFIP SOLDE CREDITEUR 1234.56",
			"releve.pdf",
			emptyDictionary(),
			defaultPersonalNameDenylist,
		)
		if result.Categorie != "bank" {
			t.Fatalf("Categorie = %q, want %q", result.Categorie, "bank")
		}
		if result.Subcategorie != "credit_mutuel" {
			t.Fatalf("Subcategorie = %q, want %q", result.Subcategorie, "credit_mutuel")
		}
	})

	t.Run("classifies a vendor invoice via the hardcoded regex branch, with compact YYYYMMDD date", func(t *testing.T) {
		result := RuleBasedClassify("Facture SFR n 123456 Total TTC 45.99 EUR 20240512", "facture.pdf", emptyDictionary(), defaultPersonalNameDenylist)
		if result.Categorie != "invoices" {
			t.Fatalf("Categorie = %q, want %q", result.Categorie, "invoices")
		}
		if result.Subcategorie != "sfr" {
			t.Fatalf("Subcategorie = %q, want %q", result.Subcategorie, "sfr")
		}
		if result.InvoiceType != "SUPPLIER" {
			t.Fatalf("InvoiceType = %q, want %q", result.InvoiceType, "SUPPLIER")
		}
		if result.Date != "2024-05-12" {
			t.Fatalf("Date = %q, want %q", result.Date, "2024-05-12")
		}
	})

	t.Run("classifies a client sales invoice under factures_clients and detects PAID / UNPAID status", func(t *testing.T) {
		resClientPaid := RuleBasedClassify(
			"Facture client N 2026-001 Destinataire Acme Corp Acme Corp Montant 1500 EUR PAYÉ PAR VIREMENT",
			"facture_client_acme.pdf",
			emptyDictionary(),
			defaultPersonalNameDenylist,
		)
		if resClientPaid.Categorie != "factures_clients" {
			t.Fatalf("Categorie = %q, want %q", resClientPaid.Categorie, "factures_clients")
		}
		if resClientPaid.Subcategorie != "acme" {
			t.Fatalf("Subcategorie = %q, want %q", resClientPaid.Subcategorie, "acme")
		}
		if resClientPaid.InvoiceType != "CLIENT" {
			t.Fatalf("InvoiceType = %q, want %q", resClientPaid.InvoiceType, "CLIENT")
		}
		if resClientPaid.PaymentStatus != "PAID" {
			t.Fatalf("PaymentStatus = %q, want %q", resClientPaid.PaymentStatus, "PAID")
		}

		resClientUnpaid := RuleBasedClassify(
			"Facture de vente N 2026-002 Client Beta Solde à régler avant le 15/09/2026 EN ATTENTE",
			"facture_client_beta.pdf",
			emptyDictionary(),
			defaultPersonalNameDenylist,
		)
		if resClientUnpaid.Categorie != "factures_clients" {
			t.Fatalf("Categorie = %q, want %q", resClientUnpaid.Categorie, "factures_clients")
		}
		if resClientUnpaid.InvoiceType != "CLIENT" {
			t.Fatalf("InvoiceType = %q, want %q", resClientUnpaid.InvoiceType, "CLIENT")
		}
		if resClientUnpaid.PaymentStatus != "UNPAID" {
			t.Fatalf("PaymentStatus = %q, want %q", resClientUnpaid.PaymentStatus, "UNPAID")
		}
	})

	t.Run("classifies a vendor invoice via the entity-dictionary fallback when no hardcoded regex matches", func(t *testing.T) {
		dict := dictionaryWith(func(d *documentschema.EntityDictionary) {
			d.Energy = []*documentschema.EntityItem{entity("ekwateur", "Ekwateur")}
		})
		result := RuleBasedClassify("Facture Ekwateur Total TTC 45 EUR", "facture2.pdf", dict, defaultPersonalNameDenylist)
		if result.Categorie != "invoices" {
			t.Fatalf("Categorie = %q, want %q", result.Categorie, "invoices")
		}
		if result.Subcategorie != "ekwateur" {
			t.Fatalf("Subcategorie = %q, want %q", result.Subcategorie, "ekwateur")
		}
	})

	t.Run("leaves subcategorie as \"general\" when no signal matches and the filename word is not grounded in the text", func(t *testing.T) {
		result := RuleBasedClassify(
			"Hello world this is a test document with nothing recognizable.",
			"randomfile.pdf",
			emptyDictionary(),
			defaultPersonalNameDenylist,
		)
		if result.Categorie != "administrative" {
			t.Fatalf("Categorie = %q, want %q", result.Categorie, "administrative")
		}
		if result.Subcategorie != "general" {
			t.Fatalf("Subcategorie = %q, want %q", result.Subcategorie, "general")
		}
		if !isISODateShape(result.Date) {
			t.Fatalf("Date = %q, want it to match ^\\d{4}-\\d{2}-\\d{2}$", result.Date)
		}
	})

	t.Run("dynamically accepts a new subcategory slug from the filename when it is genuinely grounded in the text", func(t *testing.T) {
		result := RuleBasedClassify(
			"Contrat Veolia Eau - consommation trimestrielle, montant total 32.10 EUR. Merci de votre confiance, Veolia.",
			"veolia_invoice.pdf",
			emptyDictionary(),
			defaultPersonalNameDenylist,
		)
		if result.Categorie != "administrative" {
			t.Fatalf("Categorie = %q, want %q", result.Categorie, "administrative")
		}
		if result.Subcategorie != "veolia" {
			t.Fatalf("Subcategorie = %q, want %q", result.Subcategorie, "veolia")
		}
	})

	t.Run("classifies a savings summary naming its bank as bank/<bank_slug> off the generic French phrasing alone", func(t *testing.T) {
		res := RuleBasedClassify("SYNTHESE EPARGNE CRÉDIT MUTUEL COMPTE N 00000000000000000000", "savings_summary_20190401.pdf", emptyDictionary(), defaultPersonalNameDenylist)
		if res.Categorie != "bank" {
			t.Fatalf("Categorie = %q, want %q", res.Categorie, "bank")
		}
		if res.Subcategorie != "credit_mutuel" {
			t.Fatalf("Subcategorie = %q, want %q", res.Subcategorie, "credit_mutuel")
		}
	})

	t.Run("classifies an unbranded check statement as a generic bank statement, naming no bank it cannot see", func(t *testing.T) {
		res := RuleBasedClassify("RELEVE DE CHEQUES COMPTE 00000000000000000000", "STMT_CHK_101_20100607.pdf", emptyDictionary(), defaultPersonalNameDenylist)
		if res.Categorie != "bank" {
			t.Fatalf("Categorie = %q, want %q", res.Categorie, "bank")
		}
		if res.Subcategorie != "releve_bancaire" {
			t.Fatalf("Subcategorie = %q, want %q", res.Subcategorie, "releve_bancaire")
		}
	})

	t.Run("resolves that same unbranded statement to the configured bank once the private overlay supplies the code", func(t *testing.T) {
		overlay := mustParsePersonalization(t, `{"priority_rules":[{"keywords":["STMT_CHK_"],"category":"bank","subcategory":"my_bank"}]}`)
		res := RuleBasedClassify("RELEVE DE CHEQUES COMPTE 00000000000000000000", "STMT_CHK_101_20100607.pdf", emptyDictionary(), defaultPersonalNameDenylist, overlay)
		if res.Categorie != "bank" {
			t.Fatalf("Categorie = %q, want %q", res.Categorie, "bank")
		}
		if res.Subcategorie != "my_bank" {
			t.Fatalf("Subcategorie = %q, want %q", res.Subcategorie, "my_bank")
		}
	})

	t.Run("recognises a statement whose ONLY signal is an overlay filename code, with no generic statement phrasing", func(t *testing.T) {
		overlay := mustParsePersonalization(t, `{"priority_rules":[{"keywords":["STMT_CHK_"],"category":"bank","subcategory":"my_bank"}]}`)
		res := RuleBasedClassify("COMPTE 00000000000000000000 solde 1234", "STMT_CHK_101_20100607.pdf", emptyDictionary(), defaultPersonalNameDenylist, overlay)
		if res.Categorie != "bank" {
			t.Fatalf("Categorie = %q, want %q", res.Categorie, "bank")
		}
		if res.Subcategorie != "my_bank" {
			t.Fatalf("Subcategorie = %q, want %q", res.Subcategorie, "my_bank")
		}
	})

	t.Run("never lets a non-bank overlay rule outrank a bank statement (Golden Rule #6 archetypal trap)", func(t *testing.T) {
		overlay := mustParsePersonalization(t, `{"priority_rules":[{"keywords":["Northwind Realty"],"category":"housing","subcategory":"northwind"}]}`)
		res := RuleBasedClassify(
			"RELEVE DE COMPTE CREDIT MUTUEL solde crediteur\n12/03 PRLV Northwind Realty loyer -750,00",
			"statement_202403.pdf",
			emptyDictionary(),
			defaultPersonalNameDenylist,
			overlay,
		)
		if res.Categorie != "bank" {
			t.Fatalf("Categorie = %q, want %q", res.Categorie, "bank")
		}
		if res.Subcategorie != "credit_mutuel" {
			t.Fatalf("Subcategorie = %q, want %q", res.Subcategorie, "credit_mutuel")
		}
	})

	t.Run("applies that same non-bank overlay rule normally when the document is NOT a bank statement", func(t *testing.T) {
		overlay := mustParsePersonalization(t, `{"priority_rules":[{"keywords":["Northwind Realty"],"category":"housing","subcategory":"northwind"}]}`)
		res := RuleBasedClassify("Quittance de loyer Northwind Realty mars 2024", "quittance.pdf", emptyDictionary(), defaultPersonalNameDenylist, overlay)
		if res.Categorie != "housing" {
			t.Fatalf("Categorie = %q, want %q", res.Categorie, "housing")
		}
		if res.Subcategorie != "northwind" {
			t.Fatalf("Subcategorie = %q, want %q", res.Subcategorie, "northwind")
		}
	})

	t.Run("correctly classifies academic transcripts (relevés de notes)", func(t *testing.T) {
		res := RuleBasedClassify("Relevé de notes semestriels et trimestriels Université", "relevés de notes semestriels ou trimestriels.pdf", emptyDictionary(), defaultPersonalNameDenylist)
		if res.Categorie != "education" {
			t.Fatalf("Categorie = %q, want %q", res.Categorie, "education")
		}
		if res.Subcategorie != "releve_notes" {
			t.Fatalf("Subcategorie = %q, want %q", res.Subcategorie, "releve_notes")
		}
	})

	t.Run("correctly classifies récépissés and identity papers", func(t *testing.T) {
		resRec := RuleBasedClassify("Récépissé de demande de titre de séjour Préfecture", "Recipisse20240424.pdf", emptyDictionary(), defaultPersonalNameDenylist)
		if resRec.Categorie != "identity" {
			t.Fatalf("resRec Categorie = %q, want %q", resRec.Categorie, "identity")
		}
		if resRec.Subcategorie != "recipisse_sejour" {
			t.Fatalf("resRec Subcategorie = %q, want %q", resRec.Subcategorie, "recipisse_sejour")
		}

		resIdentite := RuleBasedClassify("Carte d'identité nationale République Française", "piece_identiteB20200313.pdf", emptyDictionary(), defaultPersonalNameDenylist)
		if resIdentite.Categorie != "identity" {
			t.Fatalf("resIdentite Categorie = %q, want %q", resIdentite.Categorie, "identity")
		}
		if resIdentite.Subcategorie != "carte_identite" {
			t.Fatalf("resIdentite Subcategorie = %q, want %q", resIdentite.Subcategorie, "carte_identite")
		}
	})

	t.Run("correctly classifies Kbis and Work Stoppages (Arrêt de travail)", func(t *testing.T) {
		resKbis := RuleBasedClassify("Extrait Kbis Greffe du Tribunal de Commerce", "Kbis_Laviedessouvenirs20190414_13001827.pdf", emptyDictionary(), defaultPersonalNameDenylist)
		if resKbis.Categorie != "administrative" {
			t.Fatalf("resKbis Categorie = %q, want %q", resKbis.Categorie, "administrative")
		}
		if resKbis.Subcategorie != "kbis" {
			t.Fatalf("resKbis Subcategorie = %q, want %q", resKbis.Subcategorie, "kbis")
		}

		resArret := RuleBasedClassify("Avis d'arrêt de travail Sécurité Sociale AMELI", "Arrêt de travail.pdf", emptyDictionary(), defaultPersonalNameDenylist)
		if resArret.Categorie != "health" {
			t.Fatalf("resArret Categorie = %q, want %q", resArret.Categorie, "health")
		}
		if resArret.Subcategorie != "arret_travail" {
			t.Fatalf("resArret Subcategorie = %q, want %q", resArret.Subcategorie, "arret_travail")
		}
	})

	t.Run("correctly classifies theft claims (déclaration de vol)", func(t *testing.T) {
		resVol := RuleBasedClassify("Procès verbal de dépôt de plainte pour déclaration de vol", "declaration-de-vol.pdf", emptyDictionary(), defaultPersonalNameDenylist)
		if resVol.Categorie != "insurance" {
			t.Fatalf("Categorie = %q, want %q", resVol.Categorie, "insurance")
		}
		if resVol.Subcategorie != "declaration_vol" {
			t.Fatalf("Subcategorie = %q, want %q", resVol.Subcategorie, "declaration_vol")
		}
	})

	t.Run("correctly classifies English documents (Bank Statements, Payslips, Invoices, Contracts, Identity, Transcripts)", func(t *testing.T) {
		resBank := RuleBasedClassify("Checking Account Statement Opening Balance $5,000.00 Closing Balance $4,200.00", "bank_statement_2026.pdf", emptyDictionary(), defaultPersonalNameDenylist)
		if resBank.Categorie != "bank" {
			t.Fatalf("resBank Categorie = %q, want %q", resBank.Categorie, "bank")
		}
		if resBank.Subcategorie != "releve_bancaire" {
			t.Fatalf("resBank Subcategorie = %q, want %q", resBank.Subcategorie, "releve_bancaire")
		}

		resPayslip := RuleBasedClassify("Employee Pay Stub Gross Pay $3,500 Net Pay $2,800 Employer ACME Corp", "payslip_august.pdf", emptyDictionary(), defaultPersonalNameDenylist)
		if resPayslip.Categorie != "bulletin_salaire" {
			t.Fatalf("resPayslip Categorie = %q, want %q", resPayslip.Categorie, "bulletin_salaire")
		}

		resContract := RuleBasedClassify("Employment Agreement Terms and Conditions Non-Disclosure Agreement", "employment_contract.pdf", emptyDictionary(), defaultPersonalNameDenylist)
		if resContract.Categorie != "contracts" {
			t.Fatalf("resContract Categorie = %q, want %q", resContract.Categorie, "contracts")
		}

		resTranscript := RuleBasedClassify("Official Academic Transcript Grade Report Bachelor of Science Degree", "transcript.pdf", emptyDictionary(), defaultPersonalNameDenylist)
		if resTranscript.Categorie != "education" {
			t.Fatalf("resTranscript Categorie = %q, want %q", resTranscript.Categorie, "education")
		}
		if resTranscript.Subcategorie != "releve_notes" {
			t.Fatalf("resTranscript Subcategorie = %q, want %q", resTranscript.Subcategorie, "releve_notes")
		}
	})
}

func TestReconcileDocumentDate(t *testing.T) {
	NOW := time.Date(2026, 8, 12, 0, 0, 0, 0, time.Local)

	t.Run("corrects a future DD/MM/YYYY date to the titre year when it is an OCR two-digit-year misread (the doc #2472 bug)", func(t *testing.T) {
		result := ReconcileDocumentDate("30/11/2026", "Bulletin de salaire - Novembre 2025", NOW)
		if !result.Corrected {
			t.Fatalf("Corrected = %v, want true", result.Corrected)
		}
		if result.Date != "2025-11-30" {
			t.Fatalf("Date = %q, want %q", result.Date, "2025-11-30")
		}
		if !strings.Contains(result.Reason, "later than today") {
			t.Fatalf("Reason = %q, want it to contain %q", result.Reason, "later than today")
		}
	})

	t.Run("leaves a past date untouched, even if the titre mentions a different year", func(t *testing.T) {
		result := ReconcileDocumentDate("2026-06-30", "Bulletin de salaire - Juin 2026", NOW)
		if result.Corrected {
			t.Fatalf("Corrected = %v, want false", result.Corrected)
		}
		if result.Date != "2026-06-30" {
			t.Fatalf("Date = %q, want %q", result.Date, "2026-06-30")
		}
	})

	t.Run("leaves a future date untouched when the titre has no extractable year", func(t *testing.T) {
		result := ReconcileDocumentDate("30/11/2026", "Bulletin de salaire", NOW)
		if result.Corrected {
			t.Fatalf("Corrected = %v, want false", result.Corrected)
		}
		if result.Date != "30/11/2026" {
			t.Fatalf("Date = %q, want %q", result.Date, "30/11/2026")
		}
	})

	t.Run("leaves a future date untouched when the titre year would still be in the future", func(t *testing.T) {
		result := ReconcileDocumentDate("30/11/2099", "Facture 2098", NOW)
		if result.Corrected {
			t.Fatalf("Corrected = %v, want false", result.Corrected)
		}
		if result.Date != "30/11/2099" {
			t.Fatalf("Date = %q, want %q", result.Date, "30/11/2099")
		}
	})

	t.Run("leaves an unparseable date string untouched", func(t *testing.T) {
		result := ReconcileDocumentDate("N/A", "Bulletin de salaire - Novembre 2025", NOW)
		if result.Corrected {
			t.Fatalf("Corrected = %v, want false", result.Corrected)
		}
		if result.Date != "N/A" {
			t.Fatalf("Date = %q, want %q", result.Date, "N/A")
		}
	})
}

func TestBuildEntityHintLineDocumentFilter(t *testing.T) {
	DICT := dictionaryWith(func(d *documentschema.EntityDictionary) {
		d.Banks = []*documentschema.EntityItem{
			entity("credit_mutuel", "Credit Mutuel", "ccm"),
			entity("bnp_paribas", "BNP Paribas", "bnp"),
		}
	})

	t.Run("lists only the entities the document actually mentions", func(t *testing.T) {
		hint := BuildEntityHintLine("bank", DICT, "RELEVE DE COMPTE CREDIT MUTUEL solde crediteur")
		if !strings.Contains(hint, "credit_mutuel") {
			t.Fatalf("hint = %q, want it to contain %q", hint, "credit_mutuel")
		}
		if strings.Contains(hint, "bnp_paribas") {
			t.Fatalf("hint = %q, want it NOT to contain %q", hint, "bnp_paribas")
		}
	})

	t.Run("matches on an alias as well as the full name", func(t *testing.T) {
		if hint := BuildEntityHintLine("bank", DICT, "operation ccm du 12/03"); !strings.Contains(hint, "credit_mutuel") {
			t.Fatalf("hint = %q, want it to contain %q", hint, "credit_mutuel")
		}
	})

	t.Run("emits nothing when the document names no known entity", func(t *testing.T) {
		if got := BuildEntityHintLine("bank", DICT, "Attestation de residence Paris"); got != "" {
			t.Fatalf("BuildEntityHintLine = %q, want %q", got, "")
		}
	})

	t.Run("still lists everything when no document text is supplied (unchanged callers)", func(t *testing.T) {
		hint := BuildEntityHintLine("bank", DICT)
		if !strings.Contains(hint, "credit_mutuel") || !strings.Contains(hint, "bnp_paribas") {
			t.Fatalf("hint = %q, want both credit_mutuel and bnp_paribas", hint)
		}
	})
}

func TestMatchEntityDictionaryPreFilterAndMemoization(t *testing.T) {
	t.Run("still matches when the document text is uppercase and the entity name is not", func(t *testing.T) {
		dict := dictionaryWith(func(d *documentschema.EntityDictionary) {
			d.Banks = []*documentschema.EntityItem{entity("bnp_paribas", "BNP Paribas")}
		})
		assertEntityMatch(t, MatchEntityDictionary("RELEVE DE COMPTE BNP PARIBAS AGENCE", []string{"banks"}, dict), "bank", "bnp_paribas")
	})

	t.Run("returns the same answer on repeated calls with the same dictionary object", func(t *testing.T) {
		dict := dictionaryWith(func(d *documentschema.EntityDictionary) {
			d.Banks = []*documentschema.EntityItem{entity("lcl", "LCL")}
		})
		first := MatchEntityDictionary("virement lcl agence", []string{"banks"}, dict)
		second := MatchEntityDictionary("virement lcl agence", []string{"banks"}, dict)
		miss := MatchEntityDictionary("aucune banque ici", []string{"banks"}, dict)
		assertEntityMatch(t, first, "bank", "lcl")
		if !reflect.DeepEqual(second, first) {
			t.Fatalf("second = %#v, want %#v", second, first)
		}
		if miss != nil {
			t.Fatalf("miss = %#v, want nil", miss)
		}
	})

	t.Run("does not serve one dictionary's entities to another dictionary", func(t *testing.T) {
		dictA := dictionaryWith(func(d *documentschema.EntityDictionary) {
			d.Banks = []*documentschema.EntityItem{entity("lcl", "LCL")}
		})
		dictB := dictionaryWith(func(d *documentschema.EntityDictionary) {
			d.Banks = []*documentschema.EntityItem{entity("cic", "CIC")}
		})
		assertEntityMatch(t, MatchEntityDictionary("virement lcl agence", []string{"banks"}, dictA), "bank", "lcl")
		if got := MatchEntityDictionary("virement lcl agence", []string{"banks"}, dictB); got != nil {
			t.Fatalf("dictB lcl = %#v, want nil", got)
		}
		assertEntityMatch(t, MatchEntityDictionary("virement cic agence", []string{"banks"}, dictB), "bank", "cic")
	})

	t.Run("keeps word-boundary correctness for a candidate that survives the substring pre-filter", func(t *testing.T) {
		dict := dictionaryWith(func(d *documentschema.EntityDictionary) {
			d.Insurance = []*documentschema.EntityItem{entity("axa", "AXA")}
		})
		if got := MatchEntityDictionary("societe taxaphone service", []string{"insurance"}, dict); got != nil {
			t.Fatalf("taxaphone = %#v, want nil", got)
		}
		assertEntityMatch(t, MatchEntityDictionary("contrat axa 2024", []string{"insurance"}, dict), "insurance", "axa")
	})
}

func TestRuleBasedClassifyStep0OverlayVsSemanticAnchors(t *testing.T) {
	// The exact poison rules that misfiled the last three triaged documents: auto-learned from a
	// manual move ("calendrier de paiement.PDF" -> invoices/cdiscount and a Foncia quittance ->
	// invoices/foncia), they matched GENERIC words in the body text of unrelated documents.
	poisonOverlay := mustParsePersonalization(t, `{"priority_rules":[
		{"keywords":["paiement"],"category":"invoices","subcategory":"cdiscount"},
		{"keywords":["echeance"],"category":"invoices","subcategory":"foncia"}
	]}`)

	t.Run("does not let a generic-keyword overlay pull an income-tax notice out of administrative/impot", func(t *testing.T) {
		text := "AVIS_IR_RG\nCENTRE DES FINANCES PUBLIQUES\nSIP MARSEILLE REPUBLIQUE\nImpôt sur les revenus de 2025\nDate de paiement: votre paiement sera prélevé le 25 septembre"
		r := RuleBasedClassify(text, "Avis_d_impot_2026_sur_les_revenus_2025.pdf", emptyDictionary(), defaultPersonalNameDenylist, poisonOverlay)
		if r.Categorie != "administrative" {
			t.Fatalf("Categorie = %q, want %q", r.Categorie, "administrative")
		}
		if r.Subcategorie != "impot" {
			t.Fatalf("Subcategorie = %q, want %q", r.Subcategorie, "impot")
		}
	})

	t.Run("does not let a generic-keyword overlay pull a property-tax notice into housing/foncia", func(t *testing.T) {
		text := "AVIS_TF_RG\nCENTRE DES FINANCES PUBLIQUES\nSIP MARSEILLE REPUBLIQUE\nSomme à payer 2 396,00 €\nDate limite de paiement : 15/10/2026\nprélèvement à l'échéance avant le 01/10/2026\nLes taxes foncières sont affectées aux collectivités"
		r := RuleBasedClassify(text, "Avis_de_taxes_foncieres_2026.pdf", emptyDictionary(), defaultPersonalNameDenylist, poisonOverlay)
		if r.Categorie != "administrative" {
			t.Fatalf("Categorie = %q, want %q", r.Categorie, "administrative")
		}
		if r.Subcategorie != "impot" {
			t.Fatalf("Subcategorie = %q, want %q", r.Subcategorie, "impot")
		}
	})

	t.Run("still lets a Foncia quittance stay housing/foncia even though its charges list \"taxe foncière\"", func(t *testing.T) {
		merged := mustParsePersonalization(t, `{"priority_rules":[
			{"keywords":["paiement"],"category":"invoices","subcategory":"cdiscount"},
			{"keywords":["echeance"],"category":"invoices","subcategory":"foncia"},
			{"keywords":["Foncia"],"category":"housing","subcategory":"foncia"}
		]}`)
		text := "FONCIA VIEUX PORT\nQuittance de loyer\nLoyer : 1 200,00 €\nCharges récupérables dont taxe foncière : 40,00 €\nTotal à payer"
		r := RuleBasedClassify(text, "QuittanceDeLoyer-20190101.pdf", emptyDictionary(), defaultPersonalNameDenylist, merged)
		if r.Categorie != "housing" {
			t.Fatalf("Categorie = %q, want %q", r.Categorie, "housing")
		}
		if r.Subcategorie != "foncia" {
			t.Fatalf("Subcategorie = %q, want %q", r.Subcategorie, "foncia")
		}
	})

	t.Run("does not let a generic-keyword overlay pull a pay slip into invoices", func(t *testing.T) {
		text := "BULLETIN DE SALAIRE\nSalaire brut : 2 500,00 €\nNet à payer : 1 900,00 €\nMode de paiement : virement"
		r := RuleBasedClassify(text, "bulletinDeSalaire20181031.pdf", emptyDictionary(), defaultPersonalNameDenylist, poisonOverlay)
		if r.Categorie != "bulletin_salaire" {
			t.Fatalf("Categorie = %q, want %q", r.Categorie, "bulletin_salaire")
		}
	})

	t.Run("honours a filename-scoped learned rule: body mention alone never fires it", func(t *testing.T) {
		filenameScoped := mustParsePersonalization(t, `{"priority_rules":[
			{"keywords":["paiement"],"category":"invoices","subcategory":"cdiscount","scope":"filename"}
		]}`)
		// 'paiement' appears only in the body, not in the filename -> rule must NOT fire.
		miss := RuleBasedClassify(
			"Mandat de prélèvement SEPA\nType de paiement : Récurrent\nIBAN FR76 3000 3020 2600 0509 7464 283",
			"Mandat_SEPA_XX502170932SEPA.pdf", emptyDictionary(), defaultPersonalNameDenylist, filenameScoped)
		if miss.Categorie == "invoices" {
			t.Fatalf("miss.Categorie = %q, want anything but %q", miss.Categorie, "invoices")
		}
		// Same rule DOES fire when the keyword is in the filename (its derivation source).
		hit := RuleBasedClassify(
			"MON CALENDRIER DE PAIEMENT\npour le contrat N° CO00047332",
			"calendrier de paiement.PDF", emptyDictionary(), defaultPersonalNameDenylist, filenameScoped)
		if hit.Categorie != "invoices" {
			t.Fatalf("hit.Categorie = %q, want %q", hit.Categorie, "invoices")
		}
		if hit.Subcategorie != "cdiscount" {
			t.Fatalf("hit.Subcategorie = %q, want %q", hit.Subcategorie, "cdiscount")
		}
	})

	t.Run("files a SEPA direct-debit mandate under contracts/mandat_sepa, not invoices", func(t *testing.T) {
		text := "Objet : Mandat de prélèvement\nMANDAT DE PRELEVEMENT SEPA\nVous autorisez le créancier à envoyer des instructions à votre banque pour débiter votre compte\nIBAN: FR76\nCode BIC"
		r := RuleBasedClassify(text, "Mandat_SEPA_XX502170932SEPA.pdf", emptyDictionary(), defaultPersonalNameDenylist)
		if r.Categorie != "contracts" {
			t.Fatalf("Categorie = %q, want %q", r.Categorie, "contracts")
		}
		if r.Subcategorie != "mandat_sepa" {
			t.Fatalf("Subcategorie = %q, want %q", r.Subcategorie, "mandat_sepa")
		}
	})
}

// Go-only additions covering the exported functions the upstream suite does not exercise, so the
// whole surface is validated rather than just the functions the TS test happened to pin.
func TestGoOnlyExportedSurface(t *testing.T) {
	t.Run("FormatLocalDate uses local calendar fields", func(t *testing.T) {
		d := time.Date(2026, 8, 12, 23, 30, 0, 0, time.Local)
		if got := FormatLocalDate(d); got != "2026-08-12" {
			t.Fatalf("FormatLocalDate = %q, want %q", got, "2026-08-12")
		}
	})

	t.Run("ExtractRuleBasedContact returns all-empty contact fields", func(t *testing.T) {
		got := ExtractRuleBasedContact("Call 01 23 45 67 89 or mail a@b.fr")
		if got.ContactName != "" || got.ContactEmail != "" || got.ContactPhone != "" || got.ContactAddress != "" || got.ContactWebsite != "" {
			t.Fatalf("ExtractRuleBasedContact = %#v, want all empty", got)
		}
	})

	t.Run("BuildCategoriesDescriptionStr renders each category with subcategory ids and an entity hint", func(t *testing.T) {
		cfg := documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{
			{
				ID:            "bank",
				Name:          "Banque",
				Description:   "Comptes et relevés",
				Subcategories: []*documentschema.SubcategoryItem{{ID: "credit_mutuel"}, {ID: "bnp_paribas"}},
			},
			{ID: "empty", Name: "Vide", Description: "Sans sous-catégorie"},
		}}
		dict := dictionaryWith(func(d *documentschema.EntityDictionary) {
			d.Banks = []*documentschema.EntityItem{entity("credit_mutuel", "Credit Mutuel")}
		})
		got := BuildCategoriesDescriptionStr(cfg, dict, "RELEVE DE COMPTE CREDIT MUTUEL")
		if !strings.Contains(got, "- Category 'bank' (Banque): Comptes et relevés. Existing subcategories: [credit_mutuel, bnp_paribas].") {
			t.Fatalf("output = %q, missing bank line", got)
		}
		if !strings.Contains(got, "credit_mutuel (Credit Mutuel)") {
			t.Fatalf("output = %q, missing entity hint", got)
		}
		if !strings.Contains(got, "- Category 'empty' (Vide): Sans sous-catégorie. Existing subcategories: [none].") {
			t.Fatalf("output = %q, missing empty line", got)
		}
	})
}

func assertEntityMatch(t *testing.T, got *EntityMatch, categorie, subcategorie string) {
	t.Helper()
	if got == nil {
		t.Fatalf("MatchEntityDictionary = nil, want %s/%s", categorie, subcategorie)
	}
	if got.Categorie != categorie || got.Subcategorie != subcategorie {
		t.Fatalf("MatchEntityDictionary = %s/%s, want %s/%s", got.Categorie, got.Subcategorie, categorie, subcategorie)
	}
}

// isISODateShape is the Go equivalent of the TS assertion `/^\d{4}-\d{2}-\d{2}$/`.
func isISODateShape(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i, r := range s {
		if i == 4 || i == 7 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
