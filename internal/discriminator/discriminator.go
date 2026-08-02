// Package discriminator decides whether two prompts may share a cached answer.
//
// ⚠ WHY THIS EXISTS. Measured on engineering traffic with text-embedding-3-small at threshold
// 0.92: the best genuine rephrasing scores 0.8488, while "How do I write a validator in Pydantic
// v1?" and "…v2?" score 0.9579. Four such pairs sit ABOVE the production threshold and pool today.
// The populations are inverted by 0.109 — no threshold serves one rephrasing without admitting a
// wrong answer first.
//
// That is not a similarity failure. Those two questions ARE about the same topic; an embedding is
// right to score them alike. What differs is an ENTITY — one digit out of fifteen tokens, carrying
// the entire semantic load, against which the two correct answers are incompatible. Cosine cannot
// weight it, because weighting it would break every genuine rephrasing that varies wording freely.
//
// So the entity check is a SEPARATE, EXACT test rather than a tuning of the similarity one. The
// embedding judges topic; this judges identity; pooling requires both.
//
// ⚠ FAIL CLOSED. Every path that cannot positively verify a match must refuse. A refusal costs one
// generation. A false accept serves a confidently wrong answer, cross-tenant, on a paid path, and
// credits a royalty for it.
package discriminator

import (
	"regexp"
	"sort"
	"strings"
)

// Canonical is the storage form: a sorted, deduplicated, typed token set rendered as one string.
//
// It is stored on the row at WRITE time and compared for exact equality at READ time, because
// prompt_embeddings keeps only prompt_hash — the prompt TEXT does not survive the write, so a
// read-time re-extraction from the stored side is impossible. Paying extraction once per stored
// answer rather than once per lookup is also the cheaper side of that trade.
type Canonical string

// Tier reports which rules produced a token. Structural rules are corpus-independent — they key on
// the SHAPE of a token and generalise to entities nobody has listed. Lexical rules recognise named
// things and are only ever as wide as their list.
type Tier int

const (
	TierStructural Tier = iota
	TierLexical
)

// Token is one extracted discriminator.
type Token struct {
	Class string
	Value string
	Tier  Tier
}

var (
	// STRUCTURAL — version numbers, error codes, exit codes, permission modes, port numbers,
	// signal numbers. One rule covers all of them because they share a shape, not a meaning.
	// This is the single highest-yield rule in the package.
	reNum = regexp.MustCompile(`\b[vV]?(\d+(?:\.\d+)*)\b`)

	// STRUCTURAL — CamelCase identifiers (ImportError, ModuleNotFoundError), dotted paths, and
	// snake_case. Exception and symbol names are entities, and confusing two is a wrong answer.
	reIdent = regexp.MustCompile(`\b(?:[A-Z][a-z0-9]+){2,}\b|\b\w+(?:_\w+)+\b|\b\w+(?:\.\w+){1,}\b`)

	// STRUCTURAL — quoted or backticked spans. A user quoting an error message is naming the
	// exact thing they hit; two different quoted errors are two different questions.
	reQuoted = regexp.MustCompile("'([^']{3,60})'|\"([^\"]{3,60})\"|`([^`]{3,60})`")

	// STRUCTURAL — alphanumeric codes: E0382, E0499, Auth0, S3, H2. A letter-digit compound is
	// almost always an identifier, and the held-out corpus showed reNum missing them because the
	// digits are not preceded by a word boundary.
	reCode = regexp.MustCompile(`\b[A-Za-z]+[0-9]+[A-Za-z0-9]*\b`)

	// STRUCTURAL — SCREAMING_CASE constants and acronyms: SIGTERM, SIGKILL, ECONNREFUSED, CPU.
	// Signal names and error constants are entities; two of them are two questions.
	reCaps = regexp.MustCompile(`\b[A-Z]{2,}(?:_[A-Z0-9]+)*\b`)

	// STRUCTURAL — a capitalised word mid-sentence is a proper noun, i.e. something's NAME.
	// ⚠ THIS IS THE RULE THAT CLOSES THE LEXICON'S HOLE. Deno, Bun, Prisma, Drizzle, Okta are
	// technologies nobody listed, and the held-out corpus showed every one of them pooling freely.
	// Keying on capitalisation rather than membership means an unlisted technology still registers.
	rePropn = regexp.MustCompile(`\b[A-Z][a-z]{2,}\b`)

	// Sentence-initial position capitalises ordinary words, so those are not proper nouns.
	reSentStart = regexp.MustCompile(`(?:^|[.?!]\s+)([A-Z][a-z]{2,})`)

	reWord = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+#]*`)
)

// propnStop are words that appear capitalised mid-sentence without naming anything.
var propnStop = map[string]bool{
	"How": true, "What": true, "Why": true, "When": true, "Where": true, "Which": true,
	"Does": true, "Should": true, "Command": true, "Steps": true, "Best": true, "Getting": true,
	"Explain": true, "Convert": true, "Combine": true, "Replace": true, "Purpose": true,
	"Export": true, "Something": true, "Two": true, "Idiomatic": true, "Training": true,
	"Daily": true, "Recommended": true, "Tips": true, "Last": true, "Name": true, "Undo": true,
}

// techAliases maps every spelling of a technology to one canonical name. Aliasing is what keeps
// this rule from breaking genuine rephrasings: "in Go" and "in golang" are the same entity, and a
// gate that called them different would refuse the very traffic it exists to serve.
var techAliases = map[string]string{
	"py": "python", "python3": "python", "python2": "python",
	"js": "javascript", "node": "javascript", "nodejs": "javascript", "ecmascript": "javascript",
	"ts":     "typescript",
	"golang": "go",
	"rb":     "ruby", "rails": "rails",
	"postgresql": "postgres", "psql": "postgres", "pg": "postgres",
	"mongo": "mongodb",
	"k8s":   "kubernetes",
	"cpp":   "c++", "csharp": "c#", "dotnet": ".net",
	"nextjs": "next", "nuxtjs": "nuxt",
	"tailwindcss": "tailwind",
}

// techLexicon is the set of canonical technology names. ⚠ LEXICAL TIER: only as wide as this list.
// An unlisted technology is invisible to this rule — see Extract's contract for what that costs.
var techLexicon = map[string]bool{}

// cmdOwner maps an operation to the tool that owns it. ⚠ A BARE VERB IS NOT A DISCRIMINATOR:
// "merge two dictionaries" and "kill a process on port 3000" use command words as ordinary
// English, and treating them as entities refuses genuine rephrasings while adding no safety. The
// verb only names an operation when the prompt also names the tool whose operation it is.
var cmdOwner = map[string][]string{
	// git — the classic confusions, each with a different and irreversible blast radius
	"revert": {"git"}, "reset": {"git"}, "restore": {"git"}, "checkout": {"git"},
	"stash": {"git"}, "discard": {"git"}, "merge": {"git"}, "rebase": {"git"},
	"cherry-pick": {"git"}, "amend": {"git"}, "squash": {"git"}, "clean": {"git"},
	// sql ddl/dml — drop and truncate differ by whether the table still exists afterwards
	"drop": {"postgres", "mysql", "sqlite", "sql"}, "truncate": {"postgres", "mysql", "sqlite", "sql"},
	"alter": {"postgres", "mysql", "sqlite", "sql"}, "delete": {"postgres", "mysql", "sqlite", "sql"},
	// package managers — install and ci do different things to a lockfile
	"install": {"npm", "yarn", "pnpm", "pip", "cargo"}, "ci": {"npm", "yarn", "pnpm"},
	"uninstall": {"npm", "yarn", "pnpm", "pip"}, "prune": {"npm", "docker"},
	// container
	"exec": {"docker", "kubernetes"}, "attach": {"docker"},
}

// scopeLexicon holds words that change WHERE an operation lands. "delete a branch locally" and
// "delete a branch on the remote" are different commands with different blast radii.
var scopeLexicon = map[string]bool{}

func init() {
	for _, t := range []string{
		// languages
		"python", "javascript", "typescript", "go", "rust", "java", "kotlin", "swift", "ruby",
		"php", "c", "c++", "c#", ".net", "scala", "elixir", "haskell", "perl", "lua", "dart", "r",
		// frameworks / libraries
		"react", "vue", "angular", "svelte", "next", "nuxt", "django", "flask", "fastapi", "rails",
		"laravel", "spring", "express", "tailwind", "bootstrap", "jquery", "pytest", "jest",
		"vitest", "mocha", "pydantic", "sqlalchemy", "pandas", "numpy", "pytorch", "tensorflow",
		"axios", "webpack", "vite", "babel", "eslint", "prettier",
		// datastores
		"postgres", "mysql", "sqlite", "redis", "mongodb", "elasticsearch", "kafka", "rabbitmq",
		"cassandra", "dynamodb", "clickhouse",
		// infra / tools
		"docker", "kubernetes", "nginx", "apache", "terraform", "ansible", "aws", "gcp", "azure",
		"git", "npm", "yarn", "pnpm", "pip", "cargo", "maven", "gradle", "make", "bash", "zsh",
	} {
		techLexicon[t] = true
	}
	for _, s := range []string{
		"local", "locally", "remote", "remotely", "origin", "upstream", "global", "globally",
		"staged", "unstaged", "tracked", "untracked",
	} {
		scopeLexicon[s] = true
	}
}

// Extract returns the discriminator tokens of a prompt.
//
// ⚠ ITS CONTRACT IS ASYMMETRIC, AND DELIBERATELY SO. A token this finds is a claim that the prompt
// names a specific entity. A token it MISSES simply is not compared — an unlisted framework leaves
// no trace, so two prompts differing only in that framework will compare equal and be allowed to
// pool. Lexicon incompleteness therefore costs PRECISION, not just recall, and that is the honest
// limit of the lexical tier. The structural tier has no such hole: it keys on shape, so a version
// number nobody has ever seen still registers as a version number.
func Extract(prompt string) []Token {
	var out []Token
	seen := map[string]bool{}
	add := func(class, val string, tier Tier) {
		if val == "" {
			return
		}
		k := class + ":" + val
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, Token{Class: class, Value: val, Tier: tier})
	}

	for _, m := range reQuoted.FindAllStringSubmatch(prompt, -1) {
		for _, g := range m[1:] {
			// ⚠ ≥3 WORDS. A quoted error message is a phrase; a quoted keyword ('defer',
			// 'use strict') is what the question is ABOUT, and both sides of a rephrasing
			// rarely quote it. Counting keywords refused genuine pairs and caught nothing.
			if g != "" && len(strings.Fields(g)) >= 3 {
				add("q", strings.ToLower(strings.TrimSpace(g)), TierStructural)
			}
		}
	}
	for _, m := range reNum.FindAllStringSubmatch(prompt, -1) {
		add("num", m[1], TierStructural)
	}
	for _, m := range reCode.FindAllString(prompt, -1) {
		add("code", strings.ToLower(m), TierStructural)
	}
	for _, m := range reCaps.FindAllString(prompt, -1) {
		add("caps", strings.ToLower(m), TierStructural)
	}
	initial := map[string]bool{}
	for _, m := range reSentStart.FindAllStringSubmatch(prompt, -1) {
		initial[m[1]] = true
	}
	for _, m := range rePropn.FindAllString(prompt, -1) {
		lm := strings.ToLower(m)
		if c, ok := techAliases[lm]; ok {
			lm = c
		}
		// A listed technology is already carried by the tech class; a sentence-initial or
		// stoplisted word names nothing.
		if techLexicon[lm] || initial[m] || propnStop[m] {
			continue
		}
		add("propn", lm, TierStructural)
	}
	for _, m := range reIdent.FindAllString(prompt, -1) {
		lm := strings.ToLower(m)
		if c, ok := techAliases[lm]; ok {
			lm = c
		}
		// "JavaScript" is CamelCase AND a known technology; registering it twice is noise.
		if techLexicon[lm] {
			continue
		}
		add("id", strings.ToLower(m), TierStructural)
	}
	words := reWord.FindAllString(prompt, -1)
	present := map[string]bool{}
	for _, w := range words {
		lw := strings.ToLower(w)
		if c, ok := techAliases[lw]; ok {
			lw = c
		}
		if techLexicon[lw] {
			present[lw] = true
		}
	}
	for _, w := range words {
		lw := strings.ToLower(w)
		if c, ok := techAliases[lw]; ok {
			lw = c
		}
		switch {
		case techLexicon[lw]:
			add("tech", lw, TierLexical)
		case scopeLexicon[lw]:
			add("scope", normScope(lw), TierLexical)
		default:
			for _, owner := range cmdOwner[lw] {
				if present[owner] {
					add("cmd", lw, TierLexical)
					break
				}
			}
		}
	}
	return out
}

// normScope folds inflections so "locally" and "local" are one entity rather than two.
func normScope(s string) string {
	switch s {
	case "locally":
		return "local"
	case "remotely":
		return "remote"
	case "globally":
		return "global"
	}
	return s
}

// Canon renders a prompt's discriminators into the stored form. Sorted, so two prompts naming the
// same entities in different order produce the same string — word order is a rephrasing, not an
// entity difference, and the gate must not punish it.
func Canon(prompt string) Canonical {
	toks := Extract(prompt)
	parts := make([]string, 0, len(toks))
	for _, t := range toks {
		parts = append(parts, t.Class+":"+t.Value)
	}
	sort.Strings(parts)
	return Canonical(strings.Join(parts, "|"))
}

// Match reports whether two prompts name the same entities and may therefore share an answer.
//
// Equality is STRICT: a token present on one side and absent on the other is a mismatch, not a
// partial credit. Asymmetry is the dangerous direction in both orientations — serving a
// Pydantic-v1 answer to an unversioned "how do I write a Pydantic validator?" is as wrong as the
// reverse, because the general question deserves an answer that says which version it is about.
func Match(a, b string) bool { return Canon(a) == Canon(b) }
