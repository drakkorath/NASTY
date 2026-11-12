package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestValidPkgRegex(t *testing.T) {
	good := []string{"firefox", "python3Packages.requests", "gcc_multi", "a-b", "x'"}
	bad := []string{"", "7zip", "-rf", "foo;bar", "a b", "gtk+", "foo.", "a..b", "$(id)", "a\nb"}

	for _, s := range good {
		if !validPkgRegex.MatchString(s) {
			t.Errorf("expected %q to be valid", s)
		}
	}
	for _, s := range bad {
		if validPkgRegex.MatchString(s) {
			t.Errorf("expected %q to be rejected", s)
		}
	}
}

func TestSanitizeState(t *testing.T) {
	st, dropped := sanitizeState(SystemState{Packages: []string{"htop", "bad;pkg", "git", "htop"}})
	if want := []string{"git", "htop"}; !reflect.DeepEqual(st.Packages, want) {
		t.Errorf("packages = %v, want %v", st.Packages, want)
	}
	if want := []string{"bad;pkg"}; !reflect.DeepEqual(dropped, want) {
		t.Errorf("dropped = %v, want %v", dropped, want)
	}
}

func TestSidecarRoundTrip(t *testing.T) {
	in := SystemState{
		Packages: []string{"htop", "git", "python3Packages.requests"},
		Hardware: HardwareToggles{Bluetooth: true, Avahi: true},
	}
	data, err := renderSidecar(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "lib.mkDefault true") {
		t.Errorf("rendered config missing mkDefault:\n%s", data)
	}

	got := parseSidecar(string(data))
	want := SystemState{
		Packages: []string{"git", "htop", "python3Packages.requests"},
		Hardware: HardwareToggles{Bluetooth: true, Printing: false, Avahi: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	a, _ := renderSidecar(SystemState{Packages: []string{"b", "a", "c"}})
	b, _ := renderSidecar(SystemState{Packages: []string{"c", "a", "b"}})
	if string(a) != string(b) {
		t.Error("same selection must render identical output")
	}
}

func TestDiffStates(t *testing.T) {
	oldS := SystemState{Packages: []string{"a", "b"}}
	newS := SystemState{Packages: []string{"b", "c"}, Hardware: HardwareToggles{Bluetooth: true}}
	d := diffStates(oldS, newS)

	if !reflect.DeepEqual(d.Added, []string{"c"}) || !reflect.DeepEqual(d.Removed, []string{"a"}) {
		t.Errorf("unexpected package diff: %+v", d)
	}
	if len(d.Hardware) != 1 || d.Hardware[0] != "Bluetooth: off -> on" {
		t.Errorf("unexpected hardware diff: %v", d.Hardware)
	}
	if d.Count() != 3 {
		t.Errorf("Count = %d, want 3", d.Count())
	}
	if diffStates(oldS, oldS).Count() != 0 {
		t.Error("identical states should have no diff")
	}
}

func TestLineDiff(t *testing.T) {
	got := lineDiff([]string{"a", "b", "c"}, []string{"a", "c", "d"})
	want := []DiffLine{{'-', "b"}, {'+', "d"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lineDiff = %v, want %v", got, want)
	}
	if len(lineDiff([]string{"x"}, []string{"x"})) != 0 {
		t.Error("equal inputs should produce no diff lines")
	}
}

func TestInstallSidecarRestoreAndCommit(t *testing.T) {
	dir := t.TempDir()
	old := sidecarPath
	sidecarPath = filepath.Join(dir, "nasty-config.nix")
	t.Cleanup(func() { sidecarPath = old })

	read := func() string {
		b, err := os.ReadFile(sidecarPath)
		if err != nil {
			return "<missing>"
		}
		return string(b)
	}

	// No previous file: restore removes the new one.
	restore, _, err := installSidecar([]byte("new"))
	if err != nil {
		t.Fatal(err)
	}
	if read() != "new" {
		t.Fatalf("content = %q", read())
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	if read() != "<missing>" {
		t.Errorf("restore should delete the file, got %q", read())
	}

	// Previous file: restore puts it back and removes the backup.
	os.WriteFile(sidecarPath, []byte("old"), 0o644)
	restore, _, err = installSidecar([]byte("new2"))
	if err != nil {
		t.Fatal(err)
	}
	if read() != "new2" {
		t.Fatalf("content = %q", read())
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	if read() != "old" {
		t.Errorf("restore should bring back old content, got %q", read())
	}
	if _, err := os.Stat(sidecarPath + ".bak"); err == nil {
		t.Error("backup should be removed after restore")
	}

	// Commit keeps the new file and removes the backup.
	_, commit, err := installSidecar([]byte("new3"))
	if err != nil {
		t.Fatal(err)
	}
	commit()
	if read() != "new3" {
		t.Errorf("commit should keep new content, got %q", read())
	}
	if _, err := os.Stat(sidecarPath + ".bak"); err == nil {
		t.Error("backup should be removed after commit")
	}
}

func TestListGenerations(t *testing.T) {
	dir := t.TempDir()
	for i, ver := range map[int]string{1: "24.11", 2: "25.05"} {
		target := filepath.Join(dir, "target"+string(rune('0'+i)))
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(target, "nixos-version"), []byte(ver+"\n"), 0o644)
		if err := os.Symlink(target, filepath.Join(dir, "system-"+string(rune('0'+i))+"-link")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("system-2-link", filepath.Join(dir, "system")); err != nil {
		t.Fatal(err)
	}

	gens, err := listGenerationsIn(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(gens) != 2 || gens[0].Number != 2 || gens[1].Number != 1 {
		t.Fatalf("unexpected generations: %+v", gens)
	}
	if !gens[0].Current || gens[1].Current {
		t.Errorf("wrong current flag: %+v", gens)
	}
	if gens[0].Version != "25.05" {
		t.Errorf("version = %q", gens[0].Version)
	}
	prev, ok := previousGeneration(gens)
	if !ok || prev.Number != 1 {
		t.Errorf("previousGeneration = %+v, %v", prev, ok)
	}
}
