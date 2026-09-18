// Package decisionrule is a Go port of the pure functions in pdf-triage's
// src/domain/decision-rule.ts: deriveRuleKeywords and decisionsToPriorityRules, plus the
// HumanDecisionLike structural subset and the derived-rule constants.
//
// It turns a human move decision (see infrastructure/manual-decisions-store.ts) into a STEP 0
// priority rule the classifier can learn from on FUTURE runs — the missing half of Golden
// Rule #18's feedback-teaches-AI loop. Until this module existed, a relocalize reason was
// forwarded to Qwen only for the document being moved and then parked in the audit log;
// nothing ever re-read the log, so the correction taught the AI nothing beyond that one move.
//
// This is deliberately conservative: a bad auto-derived keyword misclassifies OTHER documents,
// so derivation trusts only what identifies the document's ISSUER (filename codes, scanner
// prefixes, distinctive title tokens), never the generic words that merely say what kind of
// document it is ("releve", "facture", "contrat"…). Every derived rule is visible and editable
// in the Settings → Human Decisions tab, and disabled/deleted the moment it misfires.
//
// The derived rules are injected through the SAME {{USER_PRIORITY_RULES}} STEP 0 block as the
// hand-curated .prompts.private.json rules (see prompt-personalization-store.ts), so both the
// Qwen prompt and the deterministic ruleBasedClassify fallback (matchPriorityRules) stay
// logically aligned — Golden Rule #6.
//
// The only external behavior it depends on is taxonomy.IsForbiddenSubcategory, imported from the
// sibling taxonomy package rather than reimplemented.
//
// The TypeScript source is the behavioral source of truth. Two deviations are intentional:
//
//  1. The TS PriorityRule type is inferred from a Zod schema in prompt-personalization.ts, which
//     is not ported to Go yet. This package defines an equivalent plain PriorityRule struct with
//     the same fields (keywords, category, optional subcategory, optional note, optional
//     scope defaulting to "all"); decisionsToPriorityRules always sets scope "filename", so the
//     default is never observable here.
//  2. deaccent ports JS `token.normalize('NFD').replace(/[\u0300-\u036f]/g, ”)` with the same
//     hand-written precomposed-Latin fold table used by the taxonomyconflicts package, because
//     Go's standard library has no Unicode normalization. The table covers Latin-1 Supplement and
//     Latin Extended-A, the scripts the corpus's French stopwords use.
//  3. decisionsToPriorityRules' TS `maxRules = MAX_DECISION_RULES` default parameter becomes a
//     variadic `maxRules ...int`; omitting it preserves the default. Behavior is otherwise
//     identical.
package decisionrule

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/phamhung075/pdf-triage-pdf2w/taxonomy"
)

// PriorityRule is the Go equivalent of the TS `PriorityRule` shape from
// prompt-personalization.ts. Scope is "all" or "filename"; the empty string is the Go equivalent
// of the TS default "all".
type PriorityRule struct {
	Keywords    []string `json:"keywords"`
	Category    string   `json:"category"`
	Subcategory string   `json:"subcategory,omitempty"`
	Note        string   `json:"note,omitempty"`
	Scope       string   `json:"scope,omitempty"`
}

// HumanDecisionLike is the structural subset of a manual-decisions record — keeps this domain
// module free of infra types. Enabled is a pointer so the TS `enabled?: number` (undefined)
// stays distinguishable from an explicit 0 (disabled): only `enabled === 0` skips a decision.
type HumanDecisionLike struct {
	ID                 int      `json:"id,omitempty"`
	OriginalFilename   string   `json:"original_filename,omitempty"`
	Title              string   `json:"title,omitempty"`
	NewCategory        string   `json:"new_category,omitempty"`
	NewSubcategory     string   `json:"new_subcategory,omitempty"`
	UserFeedbackReason string   `json:"user_feedback_reason,omitempty"`
	RuleKeywords       []string `json:"rule_keywords,omitempty"`
	Enabled            *int     `json:"enabled,omitempty"`
	CreatedAt          string   `json:"created_at,omitempty"`
}

const (
	// MinKeywordLength is the shortest token that can become an auto-derived keyword.
	MinKeywordLength = 3
	// MaxKeywordsPerDecision is the upper bound on keywords auto-derived from one decision.
	MaxKeywordsPerDecision = 3
	// MaxDecisionRules is the upper bound on decision-derived rules injected into one prompt
	// (newest first).
	MaxDecisionRules = 25
)

// stopwordTokens ports STOPWORD_TOKENS: tokens that describe the document TYPE rather than its
// issuer — useless as match keywords. Entry 'système' is kept accented exactly as in the TS
// source (a latent no-op there, since the lookup key is deaccented).
var stopwordTokens = map[string]struct{}{
	// file mechanics / generic nouns
	"pdf": {}, "scan": {}, "scans": {}, "scanned": {}, "doc": {}, "docs": {}, "document": {}, "documents": {},
	"file": {}, "files": {}, "copy": {}, "copie": {}, "final": {}, "new": {}, "nouveau": {}, "nouvelle": {}, "num": {}, "n": {},
	// generic document-type words (FR + EN)
	"releve": {}, "releves": {}, "releve_compte": {}, "statement": {}, "statements": {}, "facture": {}, "factures": {},
	"invoice": {}, "invoices": {}, "recu": {}, "recus": {}, "receipt": {}, "receipts": {}, "lettre": {}, "courrier": {},
	"courriers": {}, "letter": {}, "letters": {}, "avis": {}, "notification": {}, "notifications": {}, "declaration": {},
	"declarations": {}, "attestation": {}, "attestations": {}, "certificate": {}, "certificates": {}, "contrat": {},
	"contrats": {}, "contract": {}, "contracts": {}, "devis": {}, "quote": {}, "quotes": {}, "justificatif": {},
	"justificatifs": {}, "extrait": {}, "extraits": {}, "compte": {}, "comptes": {}, "account": {}, "accounts": {},
	"fiche": {}, "fiches": {}, "bulletin": {}, "bulletins": {}, "salaire": {}, "salaires": {}, "pay": {}, "payroll": {},
	"paie": {}, "slip": {}, "slips": {}, "paycheck": {}, "paychecks": {}, "tax": {}, "taxes": {}, "impot": {}, "impots": {},
	"banque": {}, "banques": {}, "bank": {}, "banks": {}, "assurance": {}, "assurances": {}, "insurance": {}, "insurances": {},
	"mutuelle": {}, "mutuelles": {}, "sante": {}, "sante_": {}, "health": {}, "medical": {}, "identite": {}, "identity": {},
	"passeport": {}, "passport": {}, "domicile": {}, "logement": {}, "housing": {}, "rent": {}, "quittance": {}, "quittances": {},
	"recapitulatif": {}, "recapitulatifs": {}, "tiers": {}, "titre": {}, "titres": {}, "mail": {}, "email": {}, "courriel": {},
	"sms": {}, "detail": {}, "details": {}, "total": {}, "totaux": {}, "solde": {}, "montant": {}, "montants": {}, "numero": {},
	"number": {}, "ref": {}, "reference": {}, "date": {}, "dates": {}, "page": {}, "pages": {}, "annee": {}, "year": {}, "years": {},
	"mois": {}, "month": {}, "months": {}, "jour": {}, "jours": {}, "day": {}, "days": {}, "semaine": {}, "week": {}, "weeks": {},
	"prelevement": {}, "virement": {}, "virements": {}, "transfert": {}, "transferts": {}, "especes": {}, "cash": {},
	"cheque": {}, "cheques": {}, "check": {}, "checks": {}, "ticket": {}, "tickets": {}, "envoi": {}, "envois": {}, "reception": {},
	"reponse": {}, "demande": {}, "demandes": {}, "formulaire": {}, "formulaires": {}, "form": {}, "forms": {}, "note": {},
	"notes": {}, "memo": {}, "memos": {}, "liste": {}, "listes": {}, "list": {}, "lists": {}, "synthese": {}, "resume": {},
	"summary": {}, "rapport": {}, "rapports": {}, "report": {}, "reports": {}, "guide": {}, "guides": {}, "manuel": {},
	"manuels": {}, "manual": {}, "manuals": {}, "modele": {}, "template": {}, "templates": {}, "projet": {}, "projets": {},
	"project": {}, "projects": {}, "stage": {}, "stages": {}, "cv": {}, "motivation": {}, "alerte": {}, "alertes": {}, "alert": {},
	"alerts": {}, "rapprochement": {}, "rapprochements": {}, "historique": {}, "historiques": {}, "history": {}, "log": {},
	"logs": {}, "journal": {}, "journaux": {}, "registre": {}, "registres": {}, "proces": {}, "jugement": {}, "jugements": {},
	"ordonnance": {}, "ordonnances": {}, "decision": {}, "arrivee": {}, "depart": {}, "entree": {}, "sortie": {}, "sorties": {},
	"questionnaire": {}, "enquete": {}, "sondage": {}, "survey": {}, "preuve": {}, "proof": {}, "piece": {},
	"pieces": {}, "document_": {}, "doc_": {}, "scan_": {}, "img": {}, "img_": {}, "photo": {}, "photos": {}, "image": {}, "images": {},
	"jpg": {}, "jpeg": {}, "png": {}, "tiff": {}, "webp": {},
	// months (FR + EN)
	"janvier": {}, "fevrier": {}, "mars": {}, "avril": {}, "mai": {}, "juin": {}, "juillet": {}, "aout": {}, "septembre": {},
	"octobre": {}, "novembre": {}, "decembre": {},
	"january": {}, "february": {}, "march": {}, "april": {}, "may": {}, "june": {}, "july": {}, "august": {}, "september": {},
	"october": {}, "november": {}, "december": {},
	// common conversational noise
	"the": {}, "and": {}, "for": {}, "with": {}, "from": {}, "this": {}, "that": {}, "your": {}, "vous": {}, "votre": {}, "monsieur": {},
	"madame": {}, "bonjour": {}, "salutation": {}, "cordialement": {}, "merci": {}, "objet": {}, "re": {}, "fw": {}, "fwd": {},
	"attn": {}, "attention": {}, "auto": {}, "automatique": {}, "automatic": {}, "generated": {}, "system": {}, "système": {},
	"online": {}, "telephone": {}, "phone": {}, "portable": {}, "mobile": {}, "adresse": {}, "address": {}, "site": {}, "web": {},
	"mme": {}, "mlle": {}, "dr": {}, "prof": {}, "societe": {}, "sarl": {}, "sa": {}, "eurl": {}, "sas": {}, "gmbh": {}, "ltd": {}, "llc": {},
	"inc": {}, "corp": {}, "company": {}, "compagnie": {}, "entreprise": {}, "group": {}, "groupe": {},
	// more identity/administrative document words
	"recepisse": {}, "sejour": {}, "autorisation": {}, "autorisations": {}, "certificat": {}, "certificats": {},
	"acte": {}, "actes": {}, "duplicata": {}, "original": {}, "expiration": {}, "renouvellement": {}, "validite": {},
	"suivi": {}, "reclamation": {}, "reclamations": {}, "plainte": {}, "litige": {}, "litiges": {}, "conflit": {},
	"arbitrage": {}, "feuille": {}, "feuilles": {}, "tableau": {}, "tableaux": {}, "grille": {}, "grilles": {}, "pointage": {},
	// generic finance / money-movement words. Single-word rules derived from these hijacked
	// unrelated documents on 2026-08-31: 'paiement' (learned from "calendrier de paiement.PDF"
	// -> invoices/cdiscount) fired on a SEPA mandate and an income-tax notice, and 'échéance'
	// (learned from a Foncia quittance) fired on a property-tax notice — all three were filed
	// under the wrong category. They describe WHAT the document does, never WHO issued it, so
	// they are useless as match keywords. Derived rules are also filename-scoped (see
	// decisionsToPriorityRules), so even a survivor can only re-fire on a filename.
	"paiement": {}, "paiements": {}, "payer": {}, "paye": {}, "payee": {}, "echeance": {}, "echeances": {}, "echeancier": {},
	"calendrier": {}, "credit": {}, "credits": {}, "crediteur": {}, "crediteurs": {}, "debit": {}, "debiteur": {},
	"debiteurs": {}, "versement": {}, "versements": {}, "remboursement": {}, "remboursements": {}, "cotisation": {},
	"cotisations": {}, "abonnement": {}, "abonnements": {}, "facturation": {}, "transaction": {}, "transactions": {},
	"operation": {}, "operations": {}, "encaissement": {}, "encaissements": {}, "mandat": {}, "mandats": {}, "sepa": {},
	"rib": {}, "iban": {}, "bic": {}, "confirmation": {}, "quittancedeloyer": {},
}

// boilerplateReasons ports BOILERPLATE_REASONS.
var boilerplateReasons = map[string]struct{}{
	"Manual user selection": {},
	"AI re-analysis":        {},
}

var (
	// tokenSeparatorRe is `[^\p{L}\p{N}]+` with the u flag: accented letters stay inside tokens.
	tokenSeparatorRe = regexp.MustCompile(`[^\p{L}\p{N}]+`)
	allDigitsRe      = regexp.MustCompile(`^[0-9]+$`)
	stripExtensionRe = regexp.MustCompile(`\.[^.]+$`)
)

// accentFold is the same precomposed-Latin fold table used by the taxonomyconflicts package; see
// the package comment.
var accentFold = map[rune]rune{
	'à': 'a', 'á': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a', 'å': 'a',
	'ç': 'c',
	'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e',
	'ì': 'i', 'í': 'i', 'î': 'i', 'ï': 'i',
	'ñ': 'n',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'õ': 'o', 'ö': 'o',
	'ù': 'u', 'ú': 'u', 'û': 'u', 'ü': 'u',
	'ý': 'y', 'ÿ': 'y',
	'ā': 'a', 'ă': 'a', 'ą': 'a',
	'ć': 'c', 'ĉ': 'c', 'ċ': 'c', 'č': 'c',
	'ď': 'd',
	'ē': 'e', 'ĕ': 'e', 'ė': 'e', 'ę': 'e', 'ě': 'e',
	'ĝ': 'g', 'ğ': 'g', 'ġ': 'g', 'ģ': 'g',
	'ĥ': 'h',
	'ĩ': 'i', 'ī': 'i', 'ĭ': 'i', 'į': 'i',
	'ĵ': 'j',
	'ķ': 'k',
	'ĺ': 'l', 'ļ': 'l', 'ľ': 'l',
	'ń': 'n', 'ņ': 'n', 'ň': 'n',
	'ō': 'o', 'ŏ': 'o', 'ő': 'o',
	'ŕ': 'r', 'ŗ': 'r', 'ř': 'r',
	'ś': 's', 'ŝ': 's', 'ş': 's', 'š': 's',
	'ţ': 't', 'ť': 't',
	'ũ': 'u', 'ū': 'u', 'ŭ': 'u', 'ů': 'u', 'ű': 'u', 'ų': 'u',
	'ŵ': 'w',
	'ŷ': 'y',
	'ź': 'z', 'ż': 'z', 'ž': 'z',
}

// deaccent ports deaccent: strips diacritics so 'relevé' and 'releve' hit the same stopword entry.
func deaccent(token string) string {
	var b strings.Builder
	b.Grow(len(token))
	for _, r := range token {
		if r >= 0x0300 && r <= 0x036F {
			continue
		}
		if base, ok := accentFold[r]; ok {
			r = base
		}
		b.WriteRune(r)
	}
	return b.String()
}

// tokenize ports tokenize: `\p{L}\p{N}` with the u flag keeps accented letters INSIDE tokens
// ("relevé" stays whole so deaccent() can stopword it) — a plain [^a-z0-9] class would treat "é"
// as a separator and produce the mangled token "relev".
func tokenize(value string) []string {
	parts := tokenSeparatorRe.Split(strings.ToLower(value), -1)
	out := make([]string, 0, len(parts))
	for _, t := range parts {
		t = strings.TrimSpace(t)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// isUsefulToken ports isUsefulToken.
func isUsefulToken(token string) bool {
	if utf8.RuneCountInString(token) < MinKeywordLength {
		return false
	}
	// Pure digits are years, dates or account numbers — never distinctive of an issuer.
	if allDigitsRe.MatchString(token) {
		return false
	}
	// Accent-insensitive stopword check: 'relevé' and 'releve' must both be filtered.
	if _, ok := stopwordTokens[deaccent(token)]; ok {
		return false
	}
	return true
}

// DeriveRuleKeywords ports deriveRuleKeywords: derives conservative match keywords from the
// original filename + title of a moved document.
//
// Sources, in priority order: the filename stem (bank product codes, scanner prefixes — the
// signals taxonomy.md says are the most distinctive) then the title. A token found in BOTH is
// the strongest signal and outranks everything else. Returns at most MaxKeywordsPerDecision
// tokens, lowercased, deduplicated, stopword- and digit-filtered. Empty slice when nothing
// distinctive can be found — the decision is still registered and visible in the tab, it just
// does not become an active rule until the user edits in keywords.
func DeriveRuleKeywords(filename, title string) []string {
	fileTokens := tokenize(stripExtensionRe.ReplaceAllString(filename, ""))
	titleTokens := tokenize(title)

	type candidate struct {
		token string
		score int
	}
	candidates := make([]candidate, 0, len(fileTokens)+len(titleTokens))
	seen := make(map[string]struct{}, len(fileTokens)+len(titleTokens))
	push := func(token string, score int) {
		if !isUsefulToken(token) {
			return
		}
		if _, ok := seen[token]; ok {
			return
		}
		seen[token] = struct{}{}
		candidates = append(candidates, candidate{token: token, score: score})
	}

	titleSet := make(map[string]struct{}, len(titleTokens))
	for _, t := range titleTokens {
		titleSet[t] = struct{}{}
	}
	for _, t := range fileTokens {
		score := 1
		if _, ok := titleSet[t]; ok {
			score = 2
		}
		push(t, score)
	}
	for _, t := range titleTokens {
		push(t, 1)
	}

	// Stable sort preserves insertion order for equal scores, as V8's sort does in the TS source.
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })

	n := len(candidates)
	if n > MaxKeywordsPerDecision {
		n = MaxKeywordsPerDecision
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, candidates[i].token)
	}
	return out
}

// DecisionsToPriorityRules ports decisionsToPriorityRules: maps enabled human decisions (newest
// first) to STEP 0 priority rules.
//
// Rules whose keywords had to be derived on the fly (legacy records saved before keyword
// derivation existed) get the same derivation as a fresh move. A decision with no usable
// keyword, an empty target category, or a forbidden target subcategory is skipped — a rule
// that cannot match, or that would violate Golden Rule #4, must never reach the prompt.
//
// Only the most recent `maxRules` decisions are injected, so a growing feedback log cannot
// bloat the prompt into the token budget. Omitting maxRules uses MaxDecisionRules.
func DecisionsToPriorityRules(decisions []HumanDecisionLike, maxRules ...int) []PriorityRule {
	limit := MaxDecisionRules
	if len(maxRules) > 0 {
		limit = maxRules[0]
	}

	rules := []PriorityRule{}
	for _, d := range decisions {
		if len(rules) >= limit {
			break
		}
		if d.Enabled != nil && *d.Enabled == 0 {
			continue
		}

		category := strings.ToLower(strings.TrimSpace(d.NewCategory))
		if category == "" {
			continue
		}

		subcategory := strings.ToLower(strings.TrimSpace(d.NewSubcategory))
		if subcategory != "" && taxonomy.IsForbiddenSubcategory(subcategory) {
			continue
		}

		hasStoredKeywords := false
		for _, k := range d.RuleKeywords {
			if strings.TrimSpace(k) != "" {
				hasStoredKeywords = true
				break
			}
		}

		var keywords []string
		if hasStoredKeywords {
			for _, k := range d.RuleKeywords {
				t := strings.TrimSpace(k)
				if t != "" {
					keywords = append(keywords, t)
				}
			}
		} else {
			keywords = DeriveRuleKeywords(d.OriginalFilename, d.Title)
		}
		if len(keywords) == 0 {
			continue
		}

		noteParts := []string{"Auto-learned from a human move"}
		if d.ID != 0 {
			noteParts = append(noteParts, fmt.Sprintf("(decision #%d)", d.ID))
		}
		reason := strings.TrimSpace(d.UserFeedbackReason)
		if reason != "" {
			if _, boilerplate := boilerplateReasons[reason]; !boilerplate {
				noteParts = append(noteParts, "— "+reason)
			}
		}
		note := strings.Join(noteParts, " ") + "."
		if utf8.RuneCountInString(note) > 240 {
			note = string([]rune(note)[:240]) + "…"
		}

		if len(keywords) > MaxKeywordsPerDecision {
			keywords = keywords[:MaxKeywordsPerDecision]
		}

		rules = append(rules, PriorityRule{
			Keywords:    keywords,
			Category:    category,
			Subcategory: subcategory,
			// Derived rules are FILENAME-scoped by construction: they were learned from a moved
			// file's own name, and matching them against arbitrary body text is what let a generic
			// word ('paiement', 'échéance') hijack unrelated documents. matchPriorityRules honours
			// this scope; renderPriorityRulesBlock tells the model the same thing, so the prompt
			// and the deterministic fallback stay aligned (Golden Rule #6).
			Scope: "filename",
			Note:  note,
		})
	}
	return rules
}
