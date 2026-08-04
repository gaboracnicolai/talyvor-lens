package main

import (
	"regexp"
	"sort"
	"strings"

	"github.com/talyvor/lens/internal/discriminator"
)

// PROTOTYPE CONSUMER ENTITY TIER — an EXTENSION of the live gate, never a replacement.
//
// The live gate's structural rules key on SHAPE: a version number, a CamelCase symbol, a
// SCREAMING_CASE constant, a capitalised proper noun. Consumer traffic varies none of those. It
// varies lowercase common nouns, and the ones that matter fall into CLOSED CLASSES — the person the
// answer is about, the animal, the jurisdiction, the side of a relationship, a figure glued to a
// unit, and the time frame. A closed class can be enumerated once and aliased, which is what makes
// an exact test possible at all.
//
// ⚠ WHAT IT CANNOT DO, AND THIS IS THE POINT. WHICH food, WHICH drug, WHICH cooking method, WHICH
// illness is an OPEN class. Enumerating it is enumerating the world, and any list is a lexicon with
// the same asymmetric failure the live gate already documents: an unlisted entity leaves no trace
// and the pair compares equal. The measurement reports that residue rather than hiding it.
var (
	// A figure glued to a unit. reNum requires a word boundary after the digits, so "400mg" and
	// "180C" register as nothing today — the boundary falls between two word characters.
	reUnitNum = regexp.MustCompile(`(?i)\b(\d+(?:\.\d+)?)\s*(mg|mcg|ml|g|kg|lb|oz|c|f|k|mph|kph|km|mi|%)\b`)

	reWords = regexp.MustCompile(`[A-Za-z][A-Za-z0-9'-]*`)
)

// personClass — WHO the answer is about. Aliased hard: "baby" and "infant" are one entity, and a
// gate that called them different would refuse the genuine rephrasing it exists to serve.
var personClass = map[string]string{
	"child": "minor", "children": "minor", "kid": "minor", "kids": "minor",
	"baby": "minor", "babies": "minor", "infant": "minor", "newborn": "minor",
	"toddler": "minor", "teenager": "minor", "teen": "minor", "minor": "minor",
	"adult": "adult", "adults": "adult", "grown-up": "adult",
	"pregnant": "pregnant", "pregnancy": "pregnant", "breastfeeding": "pregnant",
	"elderly": "elderly", "senior": "elderly", "pensioner": "elderly",
	"diabetic": "diabetic", "asthmatic": "asthmatic",
}

// animalClass — the other most common WHO. Species changes toxicity outright.
var animalClass = map[string]string{
	"dog": "dog", "dogs": "dog", "puppy": "dog", "puppies": "dog",
	"cat": "cat", "cats": "cat", "kitten": "cat", "kittens": "cat",
	"horse": "horse", "rabbit": "rabbit", "hamster": "hamster", "bird": "bird",
	"pet": "pet", "pets": "pet",
}

// jurisdiction — the legal system the answer belongs to. Lowercase forms only; the live gate's
// caps/propn rules already carry "UK", "US", "England".
var jurisdiction = map[string]string{
	"uk": "uk", "britain": "uk", "british": "uk", "england": "uk", "english": "uk",
	"scotland": "scotland", "scottish": "scotland", "wales": "wales", "welsh": "wales",
	"us": "us", "usa": "us", "america": "us", "american": "us",
	"ireland": "ireland", "canada": "canada", "australia": "australia", "eu": "eu",
}

// relationRole — which SIDE of a relationship is asking. The two answers are usually mirror images,
// which is the worst case: fluent, confident, and backwards.
var relationRole = map[string]string{
	"landlord": "landlord", "tenant": "tenant",
	"employer": "employer", "employee": "employee",
	"buyer": "buyer", "seller": "seller", "shop": "seller", "retailer": "seller",
	"customer": "buyer", "consumer": "buyer",
}

// topicAlias folds spellings of the same thing so an alias difference is not read as an entity
// difference. ⚠ EVERY ENTRY IS HAND-MAINTAINED. This map is the maintenance cost of the tier, and
// it grows with the traffic rather than converging.
var topicAlias = map[string]string{
	"coronavirus": "covid", "covid-19": "covid", "covid19": "covid",
	"acetaminophen": "paracetamol", "tylenol": "paracetamol",
	"advil": "ibuprofen", "nurofen": "ibuprofen",
	"faucet": "tap", "tyre": "tire",
}

// timeDeixis — an implicit year. "this year" and "last year" are different entities and the words
// carrying the difference are two of the most common in English.
var timeDeixis = map[string]string{"this": "", "last": "", "next": "", "current": "", "previous": ""}

var timeNoun = map[string]bool{"year": true, "month": true, "week": true, "season": true, "tax-year": true}

func protoCanon(prompt string) string {
	parts := []string{string(discriminator.Canon(prompt))}

	for _, m := range reUnitNum.FindAllStringSubmatch(prompt, -1) {
		parts = append(parts, "unit:"+m[1]+strings.ToLower(m[2]))
	}

	ws := reWords.FindAllString(prompt, -1)
	seen := map[string]bool{}
	add := func(class, v string) {
		k := class + ":" + v
		if v == "" || seen[k] {
			return
		}
		seen[k] = true
		parts = append(parts, k)
	}
	for i, w := range ws {
		lw := strings.ToLower(w)
		if a, ok := topicAlias[lw]; ok {
			lw = a
		}
		switch {
		case personClass[lw] != "":
			add("who", personClass[lw])
		case animalClass[lw] != "":
			add("animal", animalClass[lw])
		case jurisdiction[lw] != "":
			add("juris", jurisdiction[lw])
		case relationRole[lw] != "":
			add("role", relationRole[lw])
		}
		// time deixis only counts when it qualifies a time noun: "this year", not "this recipe".
		if _, ok := timeDeixis[lw]; ok && i+1 < len(ws) && timeNoun[strings.ToLower(ws[i+1])] {
			add("when", lw+"-"+strings.ToLower(ws[i+1]))
		}
		// covid/flu style topic aliasing surfaces as its own token so a folded alias still
		// registers as the SAME entity on both sides.
		if a, ok := topicAlias[strings.ToLower(w)]; ok {
			add("topic", a)
		} else if lw == "covid" || lw == "flu" || lw == "paracetamol" || lw == "ibuprofen" {
			add("topic", lw)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

func protoMatch(a, b string) bool { return protoCanon(a) == protoCanon(b) }
