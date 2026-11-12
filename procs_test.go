package main

import (
	"syscall"
	"testing"
)

func TestPrettyName(t *testing.T) {
	cases := map[string]string{
		".xfce4-panel-wr":    "xfce4-panel",
		".nm-applet-wrapped": "nm-applet",
		"firefox":            "firefox",
		"my-wrench":          "my-wrench",
		".":                  ".",
		"bash":               "bash",
		".Thunar-wrapped":    "Thunar",
	}
	for in, want := range cases {
		if got := prettyName(in); got != want {
			t.Errorf("prettyName(%q) = %q, want %q", in, got, want)
		}
	}
}

func sample() []ProcInfo {
	return []ProcInfo{
		{PID: 10, Name: "firefox", CPU: 5, Mem: 3, Mine: true},
		{PID: 11, Name: "firefox", CPU: 20, Mem: 4, Mine: true},
		{PID: 12, Name: "firefox", CPU: 1, Mem: 1, Mine: true},
		{PID: 2, Name: "systemd", CPU: 0.5, Mem: 0.1, Mine: false},
		{PID: 30, Name: "bash", CPU: 0, Mem: 0.2, Mine: true},
	}
}

func TestGroupProcs(t *testing.T) {
	g := groupProcs(sample(), sortCPU)
	if len(g) != 3 {
		t.Fatalf("want 3 groups, got %d", len(g))
	}
	if g[0].Name != "firefox" || g[0].CPU != 26 || g[0].Mem != 8 {
		t.Errorf("firefox group wrong: %+v", g[0])
	}
	if g[0].Procs[0].PID != 11 {
		t.Errorf("members should be busiest first, got PID %d", g[0].Procs[0].PID)
	}
	if g[1].Name != "systemd" || g[2].Name != "bash" {
		t.Errorf("unexpected order: %s, %s", g[1].Name, g[2].Name)
	}
}

func TestFlattenGroups(t *testing.T) {
	g := groupProcs(sample(), sortCPU)

	lines := flattenGroups(g, map[string]bool{}, 999)
	if len(lines) != 3 || lines[0].Kind != lineGroup || lines[1].Kind != lineSingle {
		t.Fatalf("collapsed view wrong: %+v", lines)
	}
	if len(lines[0].Killable) != 3 {
		t.Errorf("group should be killable, got %v", lines[0].Killable)
	}
	if len(lines[1].Killable) != 0 {
		t.Errorf("system process must not be killable")
	}

	lines = flattenGroups(g, map[string]bool{"firefox": true}, 999)
	if len(lines) != 6 || lines[1].Kind != lineChild || lines[1].Proc.PID != 11 {
		t.Fatalf("expanded view wrong: %+v", lines)
	}
}

func TestKillablePIDs(t *testing.T) {
	ps := []ProcInfo{
		{PID: 1, Mine: true},
		{PID: 50, Mine: true}, // pretend this is NASTY itself
		{PID: 60, Mine: true},
		{PID: 70, Mine: false},
	}
	got := killablePIDs(ps, 50)
	if len(got) != 1 || got[0] != 60 {
		t.Errorf("killablePIDs = %v, want [60]", got)
	}
}

func TestSendSignalRefusesUnsafePIDs(t *testing.T) {
	done, failed := sendSignal([]int32{0, 1}, syscall.SIGTERM)
	if done != 0 || len(failed) != 0 {
		t.Errorf("PID 0/1 must be skipped silently, got done=%d failed=%v", done, failed)
	}
}

func TestGroupProcsByNameIsStable(t *testing.T) {
	g := groupProcs(sample(), sortName)
	if g[0].Name != "bash" || g[1].Name != "firefox" || g[2].Name != "systemd" {
		t.Fatalf("name order wrong: %s %s %s", g[0].Name, g[1].Name, g[2].Name)
	}
	if g[1].Procs[0].PID != 10 || g[1].Procs[2].PID != 12 {
		t.Errorf("members should be in PID order: %+v", g[1].Procs)
	}
}

func TestApplyRankHoldsOrder(t *testing.T) {
	g := groupProcs(sample(), sortCPU) // firefox, systemd, bash
	rank := map[string]int{"bash": 0, "firefox": 1}
	pidRank := map[int32]int{12: 0, 10: 1, 11: 2}
	applyRank(g, rank, pidRank)
	if g[0].Name != "bash" || g[1].Name != "firefox" || g[2].Name != "systemd" {
		t.Errorf("rank not applied (unknown must go last): %s %s %s", g[0].Name, g[1].Name, g[2].Name)
	}
	if g[1].Procs[0].PID != 12 || g[1].Procs[2].PID != 11 {
		t.Errorf("member rank not applied: %+v", g[1].Procs)
	}
}

func TestCmdDetail(t *testing.T) {
	if got := cmdDetail([]string{"/x/chrome", "--type=renderer", "--foo"}); got != "renderer" {
		t.Errorf("got %q", got)
	}
	if got := cmdDetail([]string{"/x/wrapper-2.0", "/nix/store/abcdefghijklmnopqrstuvwxyz012345-xfce4-panel/lib/libclock.so", "6"}); got != "xfce4-panel/lib/libclock.so 6" {
		t.Errorf("got %q", got)
	}
	if got := cmdDetail([]string{"bash"}); got != "" {
		t.Errorf("got %q", got)
	}
}
