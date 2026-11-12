package main

import "testing"

func TestCleanLine(t *testing.T) {
	cases := map[string]string{
		"\x1b[31merror:\x1b[0m boom": "error: boom",
		"progress 10%\rprogress 90%": "progress 90%",
		"trailing spaces   ":         "trailing spaces",
		"plain":                      "plain",
	}
	for in, want := range cases {
		if got := cleanLine(in); got != want {
			t.Errorf("cleanLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		in   string
		want LogLevel
	}{
		{"==> Running: nixos-rebuild switch", LevelCmd},
		{"error: builder for '/nix/store/x.drv' failed", LevelError},
		{"warning: Git tree is dirty", LevelWarn},
		{"building '/nix/store/abc-foo.drv'...", LevelInfo},
	}
	for _, c := range cases {
		if got := classify(c.in); got != c.want {
			t.Errorf("classify(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseLevel(t *testing.T) {
	if lvl, ok := parseLevel("warn"); !ok || lvl != LevelWarn {
		t.Errorf("parseLevel(warn) = %v, %v", lvl, ok)
	}
	if _, ok := parseLevel(""); ok {
		t.Error("an empty level should not parse")
	}
}

func TestProgress(t *testing.T) {
	var p Progress
	p.Feed("these 3 derivations will be built:")
	p.Feed("building '/nix/store/abc123-foo-1.0.drv'...")
	if got, want := p.Summary(), "building 1 of 3: foo-1.0"; got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}

	var q Progress
	q.Feed("copying path '/nix/store/xyz-bar' from 'https://cache.nixos.org'...")
	q.Feed("copying path '/nix/store/xyz-baz' from 'https://cache.nixos.org'...")
	if got, want := q.Summary(), "downloaded 2 store paths"; got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}

	p.Feed("activating the configuration...")
	if got, want := p.Summary(), "activating the new configuration"; got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}
}
