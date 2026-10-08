// Package screening keeps the UK and US sanctions lists and screens every payee and every outside payment against
// them before any money moves (B30.6).
//
// A daily job downloads each list from its official public URL — the UK Sanctions List the FCDO publishes, the UK's
// one list of designations, and OFAC's SDN list with its aliases — and replaces that list's rows in screening_entries
// in one transaction (migration 0232). A download that fails, or a file that does not read as the list, leaves the
// copy already loaded in force and records why on screening_lists, which the operator reads at GET /v1/admin/screening.
// OFSI's consolidated list, which the UK list replaced (its file was last updated on 3 June 2026), reads too: the UK
// parser takes either file.
//
// Names are compared normalised (Normalise). An exact match on a listed name or a good alias is a hit: the money does
// not move and a compliance case is opened. A close match — a Jaro-Winkler similarity at or above
// LENS_SCREENING_FUZZY_THRESHOLD, 0.9 until Nicolai sets it — or an exact match on a low-quality alias is held: a case
// is opened for an operator to release or refuse, and the money waits. partners.TestScreeningProvider screens against
// these lists; the Screener asks the provider and opens the cases.
package screening

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// The lists.
const (
	ListUK   = "UK"
	ListOFAC = "OFAC"
)

// The official public URLs each list is downloaded from.
const (
	UKListURL  = "https://sanctionslist.fcdo.gov.uk/docs/UK-Sanctions-List.csv"
	OFACSDNURL = "https://sanctionslistservice.ofac.treas.gov/api/PublicationPreview/exports/SDN.CSV"
	OFACAltURL = "https://sanctionslistservice.ofac.treas.gov/api/PublicationPreview/exports/ALT.CSV"
)

// Entry is one name on a list: a designated person's, entity's or ship's name, or one of its aliases.
type Entry struct {
	List    string `json:"list"`
	EntryID string `json:"entry_id"` // the list's own id for what is designated
	Name    string `json:"name"`     // as the list prints it
	Alias   bool   `json:"alias"`
	Weak    bool   `json:"weak"` // a low-quality alias: a match on it is held for review, never a hit
	Kind    string `json:"kind"` // individual, entity, ship, vessel or aircraft, as the list says
}

// Source is one list: where it is downloaded from and how its files read.
type Source struct {
	List  string
	URLs  []string
	Parse func(files [][]byte) (entries []Entry, published string, err error)
}

// UKSource is the UK Sanctions List at url (UKListURL).
func UKSource(url string) Source {
	return Source{List: ListUK, URLs: []string{url}, Parse: func(f [][]byte) ([]Entry, string, error) { return ParseUK(f[0]) }}
}

// OFACSource is OFAC's SDN list at sdnURL and its aliases at altURL (OFACSDNURL, OFACAltURL).
func OFACSource(sdnURL, altURL string) Source {
	return Source{List: ListOFAC, URLs: []string{sdnURL, altURL}, Parse: func(f [][]byte) ([]Entry, string, error) {
		e, err := ParseOFAC(f[0], f[1])
		return e, "", err
	}}
}

// Sources are the lists Lens screens against, at their official URLs.
func Sources() []Source {
	return []Source{UKSource(UKListURL), OFACSource(OFACSDNURL, OFACAltURL)}
}

// ErrNotTheList: a file that does not read as the list it was downloaded as.
var ErrNotTheList = errors.New("screening: the file is not the list")

func csvReader(b []byte) *csv.Reader {
	if !utf8.Valid(b) { // OFAC's files are Windows-1252
		if d, err := charmap.Windows1252.NewDecoder().Bytes(b); err == nil {
			b = d
		}
	}
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))))
	r.FieldsPerRecord, r.LazyQuotes = -1, true
	return r
}

// ParseUK reads the UK Sanctions List's CSV — or OFSI's consolidated list, the file it replaced: each row one name of
// one designation, its forenames in Name 1 to 5 and its surname or whole name in Name 6.
func ParseUK(b []byte) ([]Entry, string, error) {
	r := csvReader(b)
	var col map[string]int
	published := ""
	for col == nil {
		row, err := r.Read()
		if err != nil {
			return nil, "", fmt.Errorf("%w: no header naming Name 6", ErrNotTheList)
		}
		switch {
		case len(row) > 0 && strings.HasPrefix(row[0], "Report Date:"):
			published = strings.TrimSpace(strings.TrimPrefix(row[0], "Report Date:"))
		case len(row) > 1 && row[0] == "Last Updated" && published == "":
			published = strings.TrimSpace(row[1])
		}
		if slices.ContainsFunc(row, func(h string) bool { return strings.TrimSpace(h) == "Name 6" }) {
			col = map[string]int{}
			for i, h := range row {
				col[strings.TrimSpace(h)] = i
			}
		}
	}
	pick := func(names ...string) int {
		for _, n := range names {
			if i, ok := col[n]; ok {
				return i
			}
		}
		return -1
	}
	id, nameType, quality, kind := pick("Unique ID", "Group ID"), pick("Name type", "Alias Type"),
		pick("Alias strength", "Alias Quality"), pick("Designation Type", "Group Type")
	if id < 0 {
		return nil, "", fmt.Errorf("%w: no Unique ID or Group ID column", ErrNotTheList)
	}
	parts := []int{pick("Name 1"), pick("Name 2"), pick("Name 3"), pick("Name 4"), pick("Name 5"), pick("Name 6")}
	field := func(row []string, i int) string {
		if i < 0 || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}
	var out []Entry
	for {
		row, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("%w: %v", ErrNotTheList, err)
		}
		var words []string
		for _, i := range parts {
			if w := field(row, i); w != "" {
				words = append(words, w)
			}
		}
		if len(words) == 0 || field(row, id) == "" {
			continue
		}
		t := strings.ToLower(field(row, nameType))
		out = append(out, Entry{List: ListUK, EntryID: field(row, id), Name: strings.Join(words, " "),
			Alias: strings.Contains(t, "alias") || t == "aka" || t == "fka", Weak: strings.Contains(strings.ToLower(field(row, quality)), "low"),
			Kind: strings.ToLower(field(row, kind))})
	}
	return dedupe(out), published, nil
}

// ParseOFAC reads OFAC's SDN.CSV and ALT.CSV: SDN rows are ent_num, name, type ("-0-" for an entity) and more; ALT
// rows are ent_num, alt_num, alt_type, alt_name and remarks.
func ParseOFAC(sdn, alt []byte) ([]Entry, error) {
	null := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "-0-" {
			return ""
		}
		return s
	}
	read := func(b []byte, what string, each func(row []string)) error {
		r := csvReader(b)
		for {
			row, err := r.Read()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("%w: %s: %v", ErrNotTheList, what, err)
			}
			if len(row) >= 4 {
				each(row)
			}
		}
	}
	var out []Entry
	kinds := map[string]string{}
	if err := read(sdn, "SDN", func(row []string) {
		id, name, kind := null(row[0]), null(row[1]), null(row[2])
		if kind == "" {
			kind = "entity"
		}
		if id != "" && name != "" {
			kinds[id] = kind
			out = append(out, Entry{List: ListOFAC, EntryID: id, Name: name, Kind: kind})
		}
	}); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: SDN holds no names", ErrNotTheList)
	}
	if err := read(alt, "ALT", func(row []string) {
		id, name := null(row[0]), null(row[3])
		if _, ok := kinds[id]; ok && name != "" {
			out = append(out, Entry{List: ListOFAC, EntryID: id, Name: name, Alias: true, Kind: kinds[id]})
		}
	}); err != nil {
		return nil, err
	}
	return dedupe(out), nil
}

// dedupe keeps one row per entry and normalised name: the strongest — a primary name over an alias, a good alias over
// a weak one. The UK list repeats a name on a row for each address.
func dedupe(in []Entry) []Entry {
	type key struct{ id, norm string }
	at := map[key]int{}
	out := in[:0]
	for _, e := range in {
		n := Normalise(e.Name)
		if n == "" {
			continue
		}
		k := key{e.EntryID, n}
		if i, ok := at[k]; ok {
			if !e.Alias && out[i].Alias || !e.Weak && out[i].Weak && e.Alias == out[i].Alias {
				out[i] = e
			}
			continue
		}
		at[k] = len(out)
		out = append(out, e)
	}
	return out
}

// folds are letters NFKD does not take apart.
var folds = strings.NewReplacer("ß", "ss", "ẞ", "SS", "Æ", "AE", "æ", "ae", "Œ", "OE", "œ", "oe", "Ø", "O", "ø", "o",
	"Ł", "L", "ł", "l", "Đ", "D", "đ", "d", "Ð", "D", "ð", "d", "Þ", "TH", "þ", "th", "ı", "i", "'", "", "’", "", "`", "")

// Normalise is a name as it is compared: its letters folded to plain upper-case ASCII (é is E, ß is SS), apostrophes
// dropped, anything else that is not a letter or a digit a space, and its words sorted — so "HAQ, Mian Abdul" and
// "Mian Abdul Haq" are one name. A name of no letters or digits is "".
func Normalise(name string) string {
	s, _, err := transform.String(transform.Chain(norm.NFKD, runes.Remove(runes.In(unicode.Mn)), norm.NFC), folds.Replace(name))
	if err != nil {
		s = name
	}
	words := strings.FieldsFunc(strings.ToUpper(s), func(r rune) bool {
		return (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	})
	sort.Strings(words)
	return strings.Join(words, " ")
}

// similarity is the Jaro-Winkler similarity of two normalised names, in basis points: 10,000 is the same name.
func similarity(a, b string) int {
	if a == b {
		return 10_000
	}
	la, lb := len(a), len(b)
	if la == 0 || lb == 0 {
		return 0
	}
	window := max(la, lb)/2 - 1
	if window < 0 {
		window = 0
	}
	ma, mb := make([]bool, la), make([]bool, lb)
	m := 0
	for i := 0; i < la; i++ {
		for j := max(0, i-window); j < min(lb, i+window+1); j++ {
			if !mb[j] && a[i] == b[j] {
				ma[i], mb[j] = true, true
				m++
				break
			}
		}
	}
	if m == 0 {
		return 0
	}
	t, j := 0, 0
	for i := 0; i < la; i++ {
		if !ma[i] {
			continue
		}
		for !mb[j] {
			j++
		}
		if a[i] != b[j] {
			t++
		}
		j++
	}
	fm := float64(m)
	jaro := (fm/float64(la) + fm/float64(lb) + (fm-float64(t)/2)/fm) / 3
	prefix := 0
	for prefix < min(4, la, lb) && a[prefix] == b[prefix] {
		prefix++
	}
	jw := jaro + float64(prefix)*0.1*(1-jaro)
	bps := int(jw*10_000 + 0.5)
	if bps >= 10_000 { // only the same name is exact
		bps = 9_999
	}
	return bps
}

// couldReach says whether two names of these lengths could be thresholdBPS similar: Jaro-Winkler is at most
// 0.6·Jaro + 0.4, and Jaro at most (short/long + 2)/3.
func couldReach(la, lb, thresholdBPS int) bool {
	short, long := min(la, lb), max(la, lb)
	if long == 0 {
		return false
	}
	jaro := (float64(short)/float64(long) + 2) / 3
	return (0.6*jaro+0.4)*10_000 >= float64(thresholdBPS)
}
