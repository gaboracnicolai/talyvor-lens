package screening

import (
	"errors"
	"os"
	"testing"
)

// B30.6 — the lists read as their publishers print them: testdata holds rows copied from the UK Sanctions List,
// OFSI's consolidated list and OFAC's SDN and ALT files of October 2026, their long free-text columns blanked.

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func find(entries []Entry, id, name string) (Entry, bool) {
	for _, e := range entries {
		if e.EntryID == id && e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

func TestParse_EachListAsItIsPublished(t *testing.T) {
	uk, published, err := ParseUK(fixture(t, "uk.csv"))
	if err != nil || published != "08-Oct-2026" || len(uk) != 14 {
		t.Fatalf("UK list: %d entries published %q, %v", len(uk), published, err)
	}
	for _, want := range []Entry{
		{List: ListUK, EntryID: "AFG0009", Name: "Muhammad Taher Anwari", Kind: "individual"},
		{List: ListUK, EntryID: "AFG0009", Name: "Mohammad Tahre Anwari", Alias: true, Kind: "individual"},
		{List: ListUK, EntryID: "AFG0009", Name: "Mudir", Alias: true, Weak: true, Kind: "individual"},
		{List: ListUK, EntryID: "AFG0001", Name: "HAJI KHAIRULLAH HAJI SATTAR MONEY EXCHANGE", Kind: "entity"},
	} {
		if got, ok := find(uk, want.EntryID, want.Name); !ok || got != want {
			t.Fatalf("UK list: %+v, want %+v", got, want)
		}
	}

	ofsi, published, err := ParseUK(fixture(t, "ofsi.csv"))
	if err != nil || published != "03/06/2026" {
		t.Fatalf("OFSI's consolidated list: published %q, %v", published, err)
	}
	if got, ok := find(ofsi, "15672", "Mian MITHOO"); !ok || got.Alias || got.Kind != "individual" {
		t.Fatalf("OFSI's consolidated list: %+v in %+v", got, ofsi)
	}

	ofac, err := ParseOFAC(fixture(t, "ofac_sdn.csv"), fixture(t, "ofac_alt.csv"))
	if err != nil || len(ofac) != 6 {
		t.Fatalf("OFAC: %d entries, %v", len(ofac), err)
	}
	if got, ok := find(ofac, "306", "BANCO NACIONAL DE CUBA"); !ok || got.Alias || got.Kind != "entity" {
		t.Fatalf("OFAC primary name: %+v", got)
	}
	if got, ok := find(ofac, "306", "NATIONAL BANK OF CUBA"); !ok || !got.Alias {
		t.Fatalf("OFAC alias: %+v", got)
	}

	for name, b := range map[string][]byte{"an error page": []byte("<html><body>Service Unavailable</body></html>"), "nothing": nil} {
		if _, _, err := ParseUK(b); !errors.Is(err, ErrNotTheList) {
			t.Fatalf("UK list from %s: %v", name, err)
		}
		if _, err := ParseOFAC(b, b); !errors.Is(err, ErrNotTheList) {
			t.Fatalf("OFAC from %s: %v", name, err)
		}
	}
}

func TestNormaliseAndSimilarity(t *testing.T) {
	for in, want := range map[string]string{
		"ÁNWARI, Muhammad  Tahér": "ANWARI MUHAMMAD TAHER",
		"Muhammad Taher Anwari":   "ANWARI MUHAMMAD TAHER",
		"O'Brien-Strauß & Co.":    "CO OBRIEN STRAUSS",
		"—":                       "",
	} {
		if got := Normalise(in); got != want {
			t.Fatalf("Normalise(%q) = %q, want %q", in, got, want)
		}
	}
	// Jaro-Winkler's published examples, in basis points.
	for _, c := range []struct {
		a, b string
		bps  int
	}{{"MARTHA", "MARHTA", 9611}, {"DWAYNE", "DUANE", 8400}, {"DIXON", "DICKSONX", 8133}, {"SAME", "SAME", 10_000}} {
		if got := similarity(c.a, c.b); got != c.bps {
			t.Fatalf("similarity(%s, %s) = %d, want %d", c.a, c.b, got, c.bps)
		}
	}
}
