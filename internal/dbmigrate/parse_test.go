package dbmigrate

import (
	"testing"
	"testing/fstest"
)

func TestParse_SortsByNumericVersionAndParsesFields(t *testing.T) {
	// Deliberately out of map order; 0010 must sort AFTER 0002 (numeric, not
	// just lexical — though zero-padding makes both work, we assert intent).
	fsys := fstest.MapFS{
		"0010_marketplace.sql": {Data: []byte("CREATE TABLE c();")},
		"0001_init.sql":        {Data: []byte("CREATE TABLE a();")},
		"0002_templates.sql":   {Data: []byte("CREATE TABLE b();")},
		"README.md":            {Data: []byte("not a migration")},
	}

	got, err := Parse(fsys)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d migrations, want 3 (non-.sql ignored)", len(got))
	}
	wantVersions := []string{"0001", "0002", "0010"}
	for i, w := range wantVersions {
		if got[i].Version != w {
			t.Fatalf("migration %d version = %q, want %q (sorted order)", i, got[i].Version, w)
		}
	}
	if got[0].Name != "0001_init.sql" {
		t.Fatalf("Name = %q, want 0001_init.sql", got[0].Name)
	}
	if got[0].SQL != "CREATE TABLE a();" {
		t.Fatalf("SQL = %q, want the file body", got[0].SQL)
	}
}

func TestParse_RejectsDuplicateVersions(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_init.sql":  {Data: []byte("CREATE TABLE a();")},
		"0001_other.sql": {Data: []byte("CREATE TABLE b();")},
	}
	if _, err := Parse(fsys); err == nil {
		t.Fatal("duplicate version 0001 must be rejected (ambiguous ordering)")
	}
}

func TestParse_RejectsMalformedName(t *testing.T) {
	fsys := fstest.MapFS{
		"init.sql": {Data: []byte("CREATE TABLE a();")}, // no NNNN_ prefix
	}
	if _, err := Parse(fsys); err == nil {
		t.Fatal("a .sql file without an NNNN_ version prefix must be rejected")
	}
}
