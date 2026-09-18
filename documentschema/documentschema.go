// Package documentschema is a Go port of pdf-triage's src/domain/document.schema.ts (151 lines,
// zero runtime dependencies beyond zod upstream). It reproduces the runtime validation semantics of
// the Zod schemas that file defines, as explicit json.Unmarshal-based Parse functions:
//
//	ParseDocumentMetadata -> DocumentMetadataSchema
//	ParseSystemSettings  -> SystemSettingsSchema
//	ParseCategoriesConfig-> CategoriesConfigSchema
//	ParseEntityDictionary-> EntityDictionarySchema
//	ParseUpdateDocument  -> UpdateDocumentSchema
//	ParseSearchQuery     -> SearchQuerySchema
//
// plus ParseCategory/ParseSubcategory/ParseEntityItem for the nested object shapes.
//
// The TypeScript source is the behavioral source of truth. The Zod semantics that matter here are
// reproduced deliberately:
//
//   - z.object() STRIPS unknown keys and does not error on them; the Go parsers read only the keys
//     they know from a map[string]json.RawMessage, so extra keys are ignored identically.
//   - `.optional()` accepts an ABSENT key but NOT an explicit JSON null. `.nullable()` accepts
//     null. The nullableOptionalString / nullableOptionalStringArray transforms collapse both
//     undefined and null to "" / [] — hence those fields are plain string / []string in Go.
//   - `.default(x)` applies only when the key is ABSENT (or undefined); an explicit null still
//     fails the inner type. That is why aliases/subcategories/description/other/tags and the
//     system-settings required strings are validated before the default is applied.
//   - `.min(1)` on a string means "non-empty"; JS `str.length` for that bound only fails on "",
//     so no UTF-16 code-unit subtlety is reachable here.
//   - z.string() accepts only a JSON string; z.number().int().positive() accepts only a JSON
//     number that is integral and > 0; z.enum() accepts only its exact members. Go's decoder is
//     checked against the JSON type first so `"50"` is rejected exactly as Zod rejects it.
//
// Optional-without-default fields are modelled with pointers, because TS distinguishes the absent
// value (undefined) from a present one: UpdateDocumentSchema's fields, SystemSettingsSchema's
// language and personal_name_denylist, and SearchQuerySchema's category/subcategory.
//
// Deviation from Zod: Zod collects every validation issue into one error with a nested path; this
// port returns on the FIRST failure, and its type-error text is a hand-written approximation of
// Zod's ("Expected string, received null"). The custom `.min()` messages are preserved verbatim,
// and no ported test asserts on any error message.
package documentschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// SubcategoryItem mirrors the TS `SubcategoryItem` type. Aliases and Subcategories are always
// non-nil: the Zod schema defaults both to [].
type SubcategoryItem struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	NameFR        *string            `json:"name_fr,omitempty"`
	NameEN        *string            `json:"name_en,omitempty"`
	Aliases       []string           `json:"aliases"`
	Subcategories []*SubcategoryItem `json:"subcategories"`
}

// CategoryItem mirrors CategorySchema. Description is always set (default ""), while the other
// nullable-looking strings are plain optional strings.
type CategoryItem struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	NameFR        *string            `json:"name_fr,omitempty"`
	NameEN        *string            `json:"name_en,omitempty"`
	Description   string             `json:"description"`
	DescriptionFR *string            `json:"description_fr,omitempty"`
	DescriptionEN *string            `json:"description_en,omitempty"`
	Aliases       []string           `json:"aliases"`
	Subcategories []*SubcategoryItem `json:"subcategories"`
}

// CategoriesConfig mirrors CategoriesConfigSchema.
type CategoriesConfig struct {
	Categories []*CategoryItem `json:"categories"`
}

// EntityItem mirrors EntityItemSchema.
type EntityItem struct {
	Slug    string   `json:"slug"`
	Name    string   `json:"name"`
	Aliases []string `json:"aliases"`
}

// EntityDictionary mirrors EntityDictionarySchema; every domain defaults to [].
type EntityDictionary struct {
	Banks     []*EntityItem `json:"banks"`
	Energy    []*EntityItem `json:"energy"`
	Telecom   []*EntityItem `json:"telecom"`
	Insurance []*EntityItem `json:"insurance"`
	Gov       []*EntityItem `json:"gov"`
	Health    []*EntityItem `json:"health"`
}

// SystemSettings mirrors SystemSettingsSchema. PersonalNameDenylist has no `.default()`, so nil
// means the caller did not send the field — updateConfig() relies on that distinction.
type SystemSettings struct {
	Language             *string   `json:"language,omitempty"`
	InputDir             string    `json:"input_dir"`
	OutputRootDir        string    `json:"output_root_dir"`
	OllamaModel          string    `json:"ollama_model"`
	OllamaHost           string    `json:"ollama_host"`
	PersonalNameDenylist *[]string `json:"personal_name_denylist,omitempty"`
}

// DocumentMetadata mirrors DocumentMetadataSchema. Every non-required string field is
// null-tolerant and collapses to ""; Tags collapses to [].
type DocumentMetadata struct {
	Thinking        string         `json:"thinking"`
	Titre           string         `json:"titre"`
	Registre        string         `json:"registre"`
	Date            string         `json:"date"`
	Categorie       string         `json:"categorie"`
	Subcategorie    string         `json:"subcategorie"`
	Summary         string         `json:"summary"`
	Tags            []string       `json:"tags"`
	MarkdownContent string         `json:"markdown_content"`
	TotalAmount     string         `json:"total_amount"`
	VatAmount       string         `json:"vat_amount"`
	Siren           string         `json:"siren"`
	Iban            string         `json:"iban"`
	ExpiryDate      string         `json:"expiry_date"`
	ContactName     string         `json:"contact_name"`
	ContactEmail    string         `json:"contact_email"`
	ContactPhone    string         `json:"contact_phone"`
	ContactAddress  string         `json:"contact_address"`
	ContactWebsite  string         `json:"contact_website"`
	Other           map[string]any `json:"other"`
}

// UpdateDocumentInput mirrors UpdateDocumentSchema: every field optional, none defaulted, so
// pointers preserve the undefined-vs-present distinction the update path depends on.
type UpdateDocumentInput struct {
	Title           *string   `json:"title,omitempty"`
	Titre           *string   `json:"titre,omitempty"`
	Registre        *string   `json:"registre,omitempty"`
	Date            *string   `json:"date,omitempty"`
	Category        *string   `json:"category,omitempty"`
	Categorie       *string   `json:"categorie,omitempty"`
	Subcategory     *string   `json:"subcategory,omitempty"`
	Subcategorie    *string   `json:"subcategorie,omitempty"`
	Summary         *string   `json:"summary,omitempty"`
	Tags            *[]string `json:"tags,omitempty"`
	MarkdownContent *string   `json:"markdown_content,omitempty"`
	TotalAmount     *string   `json:"total_amount,omitempty"`
	VatAmount       *string   `json:"vat_amount,omitempty"`
	Siren           *string   `json:"siren,omitempty"`
	Iban            *string   `json:"iban,omitempty"`
	ExpiryDate      *string   `json:"expiry_date,omitempty"`
	ContactName     *string   `json:"contact_name,omitempty"`
	ContactEmail    *string   `json:"contact_email,omitempty"`
	ContactPhone    *string   `json:"contact_phone,omitempty"`
	ContactAddress  *string   `json:"contact_address,omitempty"`
	ContactWebsite  *string   `json:"contact_website,omitempty"`
}

// SearchQuery mirrors SearchQuerySchema. Query/Mode/Limit are fully defaulted; Category and
// Subcategory stay optional pointers.
type SearchQuery struct {
	Query       string  `json:"query"`
	Category    *string `json:"category,omitempty"`
	Subcategory *string `json:"subcategory,omitempty"`
	Mode        string  `json:"mode"`
	Limit       int     `json:"limit"`
}

// ParseSubcategory parses a SubcategorySchema value (used recursively by ParseCategory).
func ParseSubcategory(rawJSON []byte) (SubcategoryItem, error) {
	m, err := parseObject(rawJSON)
	if err != nil {
		return SubcategoryItem{}, err
	}
	return parseSubcategoryFields(m)
}

// ParseCategory parses a CategorySchema value.
func ParseCategory(rawJSON []byte) (CategoryItem, error) {
	m, err := parseObject(rawJSON)
	if err != nil {
		return CategoryItem{}, err
	}
	return parseCategoryFields(m)
}

// ParseEntityItem parses an EntityItemSchema value.
func ParseEntityItem(rawJSON []byte) (EntityItem, error) {
	m, err := parseObject(rawJSON)
	if err != nil {
		return EntityItem{}, err
	}
	return parseEntityItemFields(m)
}

// ParseCategoriesConfig ports CategoriesConfigSchema.parse.
func ParseCategoriesConfig(rawJSON []byte) (CategoriesConfig, error) {
	var out CategoriesConfig
	m, err := parseObject(rawJSON)
	if err != nil {
		return out, err
	}
	raw, err := requiredRawArray(m, "categories")
	if err != nil {
		return out, err
	}
	out.Categories = make([]*CategoryItem, 0, len(raw))
	for _, item := range raw {
		im, err := parseObject(item)
		if err != nil {
			return out, fmt.Errorf("categories[]: %w", err)
		}
		cat, err := parseCategoryFields(im)
		if err != nil {
			return out, fmt.Errorf("categories[]: %w", err)
		}
		c := cat
		out.Categories = append(out.Categories, &c)
	}
	return out, nil
}

// ParseEntityDictionary ports EntityDictionarySchema.parse.
func ParseEntityDictionary(rawJSON []byte) (EntityDictionary, error) {
	var out EntityDictionary
	m, err := parseObject(rawJSON)
	if err != nil {
		return out, err
	}
	domains := []struct {
		name string
		dst  *[]*EntityItem
	}{
		{"banks", &out.Banks},
		{"energy", &out.Energy},
		{"telecom", &out.Telecom},
		{"insurance", &out.Insurance},
		{"gov", &out.Gov},
		{"health", &out.Health},
	}
	for _, d := range domains {
		items := []*EntityItem{}
		raw, present, err := rawArrayField(m, d.name)
		if err != nil {
			return out, err
		}
		if present {
			for _, item := range raw {
				im, err := parseObject(item)
				if err != nil {
					return out, fmt.Errorf("%s[]: %w", d.name, err)
				}
				e, err := parseEntityItemFields(im)
				if err != nil {
					return out, fmt.Errorf("%s[]: %w", d.name, err)
				}
				it := e
				items = append(items, &it)
			}
		}
		*d.dst = items
	}
	return out, nil
}

// ParseSystemSettings ports SystemSettingsSchema.parse.
func ParseSystemSettings(rawJSON []byte) (SystemSettings, error) {
	var out SystemSettings
	m, err := parseObject(rawJSON)
	if err != nil {
		return out, err
	}
	if out.InputDir, err = requiredString(m, "input_dir", "Input directory is required"); err != nil {
		return out, err
	}
	if out.OutputRootDir, err = requiredString(m, "output_root_dir", "Output directory is required"); err != nil {
		return out, err
	}
	if out.OllamaModel, err = requiredString(m, "ollama_model", "Ollama model is required"); err != nil {
		return out, err
	}
	if out.OllamaModel != "qwen3.5:9b" {
		return out, errors.New("Only 'qwen3.5:9b' is supported (Golden Rule #14) — other models, including cloud/subscription-gated ones, are rejected.")
	}
	if out.OllamaHost, err = requiredString(m, "ollama_host", "Ollama host is required"); err != nil {
		return out, err
	}
	if out.Language, err = optionalStringField(m, "language"); err != nil {
		return out, err
	}
	if out.PersonalNameDenylist, err = optionalStringArrayField(m, "personal_name_denylist"); err != nil {
		return out, err
	}
	return out, nil
}

// ParseUpdateDocument ports UpdateDocumentSchema.parse.
func ParseUpdateDocument(rawJSON []byte) (UpdateDocumentInput, error) {
	var out UpdateDocumentInput
	m, err := parseObject(rawJSON)
	if err != nil {
		return out, err
	}
	fields := []struct {
		name string
		dst  **string
	}{
		{"title", &out.Title},
		{"titre", &out.Titre},
		{"registre", &out.Registre},
		{"date", &out.Date},
		{"category", &out.Category},
		{"categorie", &out.Categorie},
		{"subcategory", &out.Subcategory},
		{"subcategorie", &out.Subcategorie},
		{"summary", &out.Summary},
		{"markdown_content", &out.MarkdownContent},
		{"total_amount", &out.TotalAmount},
		{"vat_amount", &out.VatAmount},
		{"siren", &out.Siren},
		{"iban", &out.Iban},
		{"expiry_date", &out.ExpiryDate},
		{"contact_name", &out.ContactName},
		{"contact_email", &out.ContactEmail},
		{"contact_phone", &out.ContactPhone},
		{"contact_address", &out.ContactAddress},
		{"contact_website", &out.ContactWebsite},
	}
	for _, f := range fields {
		v, err := optionalStringField(m, f.name)
		if err != nil {
			return out, err
		}
		*f.dst = v
	}
	if out.Tags, err = optionalStringArrayField(m, "tags"); err != nil {
		return out, err
	}
	return out, nil
}

// ParseSearchQuery ports SearchQuerySchema.parse.
func ParseSearchQuery(rawJSON []byte) (SearchQuery, error) {
	var out SearchQuery
	m, err := parseObject(rawJSON)
	if err != nil {
		return out, err
	}
	// query: z.string().default("")
	if raw, ok := m["query"]; ok {
		s, err := decodeString(raw)
		if err != nil {
			return out, fmt.Errorf("query: %w", err)
		}
		out.Query = s
	}
	if out.Category, err = optionalStringField(m, "category"); err != nil {
		return out, err
	}
	if out.Subcategory, err = optionalStringField(m, "subcategory"); err != nil {
		return out, err
	}
	// mode: z.enum(['hybrid','keyword','semantic']).default('hybrid')
	out.Mode = "hybrid"
	if raw, ok := m["mode"]; ok {
		s, err := decodeString(raw)
		if err != nil {
			return out, fmt.Errorf("mode: %w", err)
		}
		switch s {
		case "hybrid", "keyword", "semantic":
			out.Mode = s
		default:
			return out, fmt.Errorf("mode: Invalid enum value. Expected 'hybrid' | 'keyword' | 'semantic', received %q", s)
		}
	}
	// limit: z.number().int().positive().default(50)
	out.Limit = 50
	if raw, ok := m["limit"]; ok {
		if isJSONNull(raw) {
			return out, errors.New("limit: Expected number, received null")
		}
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil {
			return out, fmt.Errorf("limit: Expected number, received %s", jsonTypeName(raw))
		}
		if f != math.Trunc(f) {
			return out, errors.New("limit: Expected integer, received float")
		}
		if !(f > 0) {
			return out, errors.New("limit: Number must be greater than 0")
		}
		out.Limit = int(f)
	}
	return out, nil
}

// ParseDocumentMetadata ports DocumentMetadataSchema.parse.
func ParseDocumentMetadata(rawJSON []byte) (DocumentMetadata, error) {
	var out DocumentMetadata
	m, err := parseObject(rawJSON)
	if err != nil {
		return out, err
	}
	if out.Titre, err = requiredString(m, "titre", "Titre est requis"); err != nil {
		return out, err
	}
	if out.Categorie, err = requiredString(m, "categorie", "Catégorie est requise"); err != nil {
		return out, err
	}

	// Every nullableOptionalString field: absent, undefined or null all become "".
	nullable := []struct {
		name string
		dst  *string
	}{
		{"thinking", &out.Thinking},
		{"registre", &out.Registre},
		{"date", &out.Date},
		{"subcategorie", &out.Subcategorie},
		{"summary", &out.Summary},
		{"markdown_content", &out.MarkdownContent},
		{"total_amount", &out.TotalAmount},
		{"vat_amount", &out.VatAmount},
		{"siren", &out.Siren},
		{"iban", &out.Iban},
		{"expiry_date", &out.ExpiryDate},
		{"contact_name", &out.ContactName},
		{"contact_email", &out.ContactEmail},
		{"contact_phone", &out.ContactPhone},
		{"contact_address", &out.ContactAddress},
		{"contact_website", &out.ContactWebsite},
	}
	for _, f := range nullable {
		v, err := nullableStringField(m, f.name)
		if err != nil {
			return out, err
		}
		*f.dst = v
	}

	if out.Tags, err = nullableStringArrayField(m, "tags"); err != nil {
		return out, err
	}
	if out.Other, err = recordField(m, "other"); err != nil {
		return out, err
	}
	return out, nil
}

// parseCategoryFields parses a decoded CategorySchema object map.
func parseCategoryFields(m map[string]json.RawMessage) (CategoryItem, error) {
	var out CategoryItem
	var err error
	if out.ID, err = requiredString(m, "id", "ID is required"); err != nil {
		return out, err
	}
	if out.Name, err = requiredString(m, "name", "Name is required"); err != nil {
		return out, err
	}
	if out.NameFR, err = optionalStringField(m, "name_fr"); err != nil {
		return out, err
	}
	if out.NameEN, err = optionalStringField(m, "name_en"); err != nil {
		return out, err
	}
	// description: z.string().optional().default("")
	if raw, ok := m["description"]; ok {
		s, err := decodeString(raw)
		if err != nil {
			return out, fmt.Errorf("description: %w", err)
		}
		out.Description = s
	}
	if out.DescriptionFR, err = optionalStringField(m, "description_fr"); err != nil {
		return out, err
	}
	if out.DescriptionEN, err = optionalStringField(m, "description_en"); err != nil {
		return out, err
	}
	// aliases: z.array(z.string()).default([]) — note: no .optional(), so null still fails.
	if raw, ok := m["aliases"]; ok {
		a, err := decodeStringArray(raw, "aliases")
		if err != nil {
			return out, err
		}
		out.Aliases = a
	} else {
		out.Aliases = []string{}
	}
	// subcategories: z.array(SubcategorySchema).optional().default([])
	out.Subcategories = []*SubcategoryItem{}
	if raw, present, err := rawArrayField(m, "subcategories"); err != nil {
		return out, err
	} else if present {
		for _, item := range raw {
			im, err := parseObject(item)
			if err != nil {
				return out, fmt.Errorf("subcategories[]: %w", err)
			}
			sub, err := parseSubcategoryFields(im)
			if err != nil {
				return out, fmt.Errorf("subcategories[]: %w", err)
			}
			s := sub
			out.Subcategories = append(out.Subcategories, &s)
		}
	}
	return out, nil
}

// parseSubcategoryFields parses a decoded SubcategorySchema object map (recursive).
func parseSubcategoryFields(m map[string]json.RawMessage) (SubcategoryItem, error) {
	var out SubcategoryItem
	var err error
	if out.ID, err = requiredString(m, "id", "Subcategory ID is required"); err != nil {
		return out, err
	}
	if out.Name, err = requiredString(m, "name", "Subcategory Name is required"); err != nil {
		return out, err
	}
	if out.NameFR, err = optionalStringField(m, "name_fr"); err != nil {
		return out, err
	}
	if out.NameEN, err = optionalStringField(m, "name_en"); err != nil {
		return out, err
	}
	// aliases: z.array(z.string()).optional().default([])
	out.Aliases = []string{}
	if raw, ok := m["aliases"]; ok {
		a, err := decodeStringArray(raw, "aliases")
		if err != nil {
			return out, err
		}
		out.Aliases = a
	}
	// subcategories: z.array(SubcategorySchema).optional().default([])
	out.Subcategories = []*SubcategoryItem{}
	if raw, present, err := rawArrayField(m, "subcategories"); err != nil {
		return out, err
	} else if present {
		for _, item := range raw {
			im, err := parseObject(item)
			if err != nil {
				return out, fmt.Errorf("subcategories[]: %w", err)
			}
			sub, err := parseSubcategoryFields(im)
			if err != nil {
				return out, fmt.Errorf("subcategories[]: %w", err)
			}
			s := sub
			out.Subcategories = append(out.Subcategories, &s)
		}
	}
	return out, nil
}

// parseEntityItemFields parses a decoded EntityItemSchema object map.
func parseEntityItemFields(m map[string]json.RawMessage) (EntityItem, error) {
	var out EntityItem
	var err error
	if out.Slug, err = requiredString(m, "slug", "Entity slug is required"); err != nil {
		return out, err
	}
	if out.Name, err = requiredString(m, "name", "Entity name is required"); err != nil {
		return out, err
	}
	out.Aliases = []string{}
	if raw, ok := m["aliases"]; ok {
		a, err := decodeStringArray(raw, "aliases")
		if err != nil {
			return out, err
		}
		out.Aliases = a
	}
	return out, nil
}

// ---- Zod-primitive helpers ----

// decodeString ports z.string() on an already-present raw value: null and every non-string JSON
// value fail.
func decodeString(raw json.RawMessage) (string, error) {
	if isJSONNull(raw) {
		return "", errors.New("Expected string, received null")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("Expected string, received %s", jsonTypeName(raw))
	}
	return s, nil
}

// parseObject decodes a JSON object into its raw fields. A JSON null, array, string or number
// all fail exactly as z.object() fails them; unknown keys are simply never read (strip).
func parseObject(rawJSON []byte) (map[string]json.RawMessage, error) {
	if isJSONNull(rawJSON) {
		return nil, errors.New("Expected object, received null")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rawJSON, &m); err != nil {
		return nil, fmt.Errorf("Expected object, received %s", jsonTypeName(rawJSON))
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m, nil
}

// requiredString ports a mandatory z.string().min(1, msg) field.
func requiredString(m map[string]json.RawMessage, name, message string) (string, error) {
	raw, ok := m[name]
	if !ok {
		return "", fmt.Errorf("%s: Required", name)
	}
	if isJSONNull(raw) {
		return "", fmt.Errorf("%s: Expected string, received null", name)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%s: Expected string, received %s", name, jsonTypeName(raw))
	}
	if s == "" {
		return "", fmt.Errorf("%s: %s", name, message)
	}
	return s, nil
}

// optionalStringField ports z.string().optional(): absent -> nil, null -> error, string -> value.
func optionalStringField(m map[string]json.RawMessage, name string) (*string, error) {
	raw, ok := m[name]
	if !ok {
		return nil, nil
	}
	if isJSONNull(raw) {
		return nil, fmt.Errorf("%s: Expected string, received null", name)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%s: Expected string, received %s", name, jsonTypeName(raw))
	}
	return &s, nil
}

// nullableStringField ports nullableOptionalString: absent/null -> "", string -> value.
func nullableStringField(m map[string]json.RawMessage, name string) (string, error) {
	raw, ok := m[name]
	if !ok {
		return "", nil
	}
	if isJSONNull(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%s: Expected string, received %s", name, jsonTypeName(raw))
	}
	return s, nil
}

// decodeStringArray ports z.array(z.string()): null -> error, array of strings -> slice.
func decodeStringArray(raw json.RawMessage, name string) ([]string, error) {
	if isJSONNull(raw) {
		return nil, fmt.Errorf("%s: Expected array, received null", name)
	}
	var a []string
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("%s: Expected array of strings, received %s", name, jsonTypeName(raw))
	}
	if a == nil {
		a = []string{}
	}
	return a, nil
}

// optionalStringArrayField ports z.array(z.string()).optional(): absent -> nil, null -> error.
func optionalStringArrayField(m map[string]json.RawMessage, name string) (*[]string, error) {
	raw, ok := m[name]
	if !ok {
		return nil, nil
	}
	a, err := decodeStringArray(raw, name)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// nullableStringArrayField ports nullableOptionalStringArray: absent/null -> [], array -> values.
func nullableStringArrayField(m map[string]json.RawMessage, name string) ([]string, error) {
	raw, ok := m[name]
	if !ok || isJSONNull(raw) {
		return []string{}, nil
	}
	return decodeStringArray(raw, name)
}

// recordField ports z.record(z.string(), z.any()).optional().default({}). z.any() accepts every
// JSON value, so the record values decode as map[string]any; null and non-objects fail.
func recordField(m map[string]json.RawMessage, name string) (map[string]any, error) {
	raw, ok := m[name]
	if !ok {
		return map[string]any{}, nil
	}
	if isJSONNull(raw) {
		return nil, fmt.Errorf("%s: Expected object, received null", name)
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("%s: Expected object, received %s", name, jsonTypeName(raw))
	}
	if rec == nil {
		rec = map[string]any{}
	}
	return rec, nil
}

// requiredRawArray decodes a mandatory JSON array, keeping elements raw for recursive parsing.
func requiredRawArray(m map[string]json.RawMessage, name string) ([]json.RawMessage, error) {
	raw, ok := m[name]
	if !ok {
		return nil, fmt.Errorf("%s: Required", name)
	}
	if isJSONNull(raw) {
		return nil, fmt.Errorf("%s: Expected array, received null", name)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("%s: Expected array, received %s", name, jsonTypeName(raw))
	}
	return arr, nil
}

// rawArrayField decodes an optional JSON array field. present is false when the key is absent;
// an explicit null fails (z.array is not nullable).
func rawArrayField(m map[string]json.RawMessage, name string) (arr []json.RawMessage, present bool, err error) {
	raw, ok := m[name]
	if !ok {
		return nil, false, nil
	}
	if isJSONNull(raw) {
		return nil, false, fmt.Errorf("%s: Expected array, received null", name)
	}
	if uerr := json.Unmarshal(raw, &arr); uerr != nil {
		return nil, false, fmt.Errorf("%s: Expected array, received %s", name, jsonTypeName(raw))
	}
	return arr, true, nil
}

// isJSONNull reports whether the raw JSON token is exactly null.
func isJSONNull(raw []byte) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// jsonTypeName names the JSON value type the way Zod's error messages do ("string", "number",
// "boolean", "array", "object", "null").
func jsonTypeName(raw []byte) string {
	s := bytes.TrimSpace(raw)
	if len(s) == 0 {
		return "undefined"
	}
	switch s[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	default:
		return "number"
	}
}
