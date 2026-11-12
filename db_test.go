package main

import (
	"path/filepath"
	"strings"
	"testing"

	"go.etcd.io/bbolt"
)

func TestParseIndexNixEnv(t *testing.T) {
	const js = `{
		"htop": {"name": "htop-3.3", "pname": "htop", "meta": {"description": "An interactive process viewer"}},
		"python3Packages.requests": {"meta": {"description": "HTTP for humans"}},
		"7zip": {"meta": {}},
		"weird": {"meta": {"description": 42}}
	}`
	entries, err := parseIndex(strings.NewReader(js))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 { // "7zip" is not a valid attribute name and is skipped
		t.Fatalf("got %d entries: %+v", len(entries), entries)
	}
	if entries[0].Attr != "htop" || entries[0].Desc != "An interactive process viewer" {
		t.Errorf("unexpected entry: %+v", entries[0])
	}
	if entries[2].Desc != "" {
		t.Errorf("non-string description should become empty, got %q", entries[2].Desc)
	}
}

func TestParseIndexNixSearch(t *testing.T) {
	const js = `{"legacyPackages.x86_64-linux.git": {"pname": "git", "version": "2.45", "description": "Distributed version control"}}`
	entries, err := parseIndex(strings.NewReader(js))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Attr != "git" || entries[0].Desc != "Distributed version control" {
		t.Errorf("unexpected entries: %+v", entries)
	}
}

func TestParseIndexRejectsGarbage(t *testing.T) {
	if _, err := parseIndex(strings.NewReader(`[1,2,3]`)); err == nil {
		t.Error("expected an error for a non-object")
	}
}

func TestScoreMatchOrdering(t *testing.T) {
	exact := scoreMatch("htop", "htop")
	base := scoreMatch("requests", "python3packages.requests")
	prefix := scoreMatch("req", "requests")
	boundary := scoreMatch("requests", "oauthlib-requests") // match right after a '-'
	contains := scoreMatch("ests", "requests")
	fuzzy := scoreMatch("rqst", "requests")
	none := scoreMatch("zzz", "requests")

	if !(exact > base && base > prefix && prefix > boundary && boundary > contains && contains > fuzzy && fuzzy > 0) {
		t.Errorf("unexpected ordering: exact=%d base=%d prefix=%d boundary=%d contains=%d fuzzy=%d",
			exact, base, prefix, boundary, contains, fuzzy)
	}
	if none != 0 {
		t.Errorf("non-match should score 0, got %d", none)
	}
}

func TestSearchPackages(t *testing.T) {
	d, err := bbolt.Open(filepath.Join(t.TempDir(), "test.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	db = d
	t.Cleanup(func() { d.Close(); db = nil })

	err = d.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucket(pkgBucket)
		if err != nil {
			return err
		}
		for k, v := range map[string]string{
			"htop":                     "process viewer",
			"python3Packages.requests": "HTTP for humans",
			"requests-oauthlib":        "OAuth for requests",
			"SDL2":                     "multimedia library",
		} {
			if err := b.Put([]byte(k), []byte(v)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if res := SearchPackages("htop", 10); len(res) == 0 || res[0].Name != "htop" {
		t.Errorf("htop search = %+v", res)
	}
	if res := SearchPackages("requests", 10); len(res) < 2 || res[0].Name != "python3Packages.requests" {
		t.Errorf("requests search should rank the base-name match first: %+v", res)
	}
	if res := SearchPackages("sdl", 10); len(res) != 1 || res[0].Name != "SDL2" {
		t.Errorf("search must be case-insensitive: %+v", res)
	}
	if res := SearchPackages("humans", 10); len(res) != 1 {
		t.Errorf("description search failed: %+v", res)
	}
	if res := SearchPackages("", 2); len(res) != 2 {
		t.Errorf("empty query should respect the limit: %+v", res)
	}
}
