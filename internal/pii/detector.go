package pii

import "regexp"

type Detector struct{}

type RedactionResult struct {
	Original    string
	Redacted    string
	Types       []string
	WasRedacted bool
}

func New() *Detector { return &Detector{} }

// Patterns are compiled once at package init — Detect runs on every request,
// recompiling per-call would dominate request latency.
var (
	emailRE = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)

	// Phone covers the four shapes called out in the spec:
	//   +1-555-555-5555 / +44 20 7946 0958  (international, separators may be - or space)
	//   (555) 555-5555                       (US parenthesized area code)
	//   555.555.5555 / 555-555-5555          (plain US dotted / dashed)
	phoneRE = regexp.MustCompile(
		`\+\d{1,3}[-\s]?\d{1,4}[-\s.]?\d{3,4}[-\s.]?\d{3,4}` +
			`|\(\d{3}\)\s?\d{3}[-\s.]\d{4}` +
			`|\d{3}[-\s.]\d{3}[-\s.]\d{4}`,
	)

	// Word boundaries keep us from mid-matching inside longer digit blobs.
	creditCardRE = regexp.MustCompile(`\b(?:\d{4}[-\s]\d{4}[-\s]\d{4}[-\s]\d{4}|\d{16})\b`)
	ssnRE        = regexp.MustCompile(`\b\d{3}[-\s]\d{2}[-\s]\d{4}\b`)
	ipv4RE       = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)

	// DOB context is case-insensitive; the date itself accepts MM/DD/YYYY,
	// DD-MM-YYYY, or "Month DD YYYY" (comma optional).
	dobRE = regexp.MustCompile(
		`(?i)(?:DOB|Date of Birth):\s*` +
			`(?:\d{1,2}[/-]\d{1,2}[/-]\d{4}` +
			`|(?:January|February|March|April|May|June|July|August|September|October|November|December)` +
			`\s+\d{1,2}[,\s]+\d{4})`,
	)

	// Name context word case-insensitive; the actual name keeps a strict
	// "Title Title" capitalization so we don't mis-redact lowercase nouns.
	nameRE = regexp.MustCompile(`((?i:patient|customer|user|name|client)):\s*[A-Z][a-z]+ [A-Z][a-z]+`)
)

// rule pairs a compiled regex with its replacement and the PII type label
// reported back in RedactionResult.Types.
type rule struct {
	re          *regexp.Regexp
	replacement string
	piiType     string
}

// Rules run in the order the spec lists them — earlier rules may consume
// substrings that later rules would otherwise match (e.g. phone before
// credit_card prevents a 16-digit grouping from being claimed as a phone).
var rules = []rule{
	{emailRE, "[REDACTED-EMAIL]", "email"},
	{phoneRE, "[REDACTED-PHONE]", "phone"},
	{creditCardRE, "[REDACTED-CARD]", "credit_card"},
	{ssnRE, "[REDACTED-SSN]", "ssn"},
	{ipv4RE, "[REDACTED-IP]", "ip_address"},
	{dobRE, "DOB: [REDACTED-DOB]", "dob"},
	{nameRE, "$1: [REDACTED-NAME]", "name"},
}

func (d *Detector) Detect(text string) RedactionResult {
	original := text
	var types []string
	seen := make(map[string]bool, len(rules))

	for _, r := range rules {
		replaced := r.re.ReplaceAllString(text, r.replacement)
		if replaced == text {
			continue
		}
		text = replaced
		if !seen[r.piiType] {
			seen[r.piiType] = true
			types = append(types, r.piiType)
		}
	}

	return RedactionResult{
		Original:    original,
		Redacted:    text,
		Types:       types,
		WasRedacted: len(types) > 0,
	}
}

// IsSafeToCache reports whether the original input was free of PII. A
// redacted result is never cacheable: Lens never serves one user's PII-laden
// response to another caller, even after redaction.
func (d *Detector) IsSafeToCache(result RedactionResult) bool {
	return !result.WasRedacted
}
