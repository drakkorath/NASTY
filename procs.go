package main

import (
	"errors"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/shirou/gopsutil/v3/process"
)

// ---- data -------------------------------------------------------------------

const maxProcGroups = 80

// ProcInfo is one running process.
type ProcInfo struct {
	PID    int32
	Name   string  // cleaned up for display
	CPU    float64 // percent of ONE core, like top (100 = one core fully busy)
	Mem    float32 // percent of total RAM
	Mine   bool    // owned by the user running NASTY (the only ones we may stop)
	Detail string  // what this one is for, e.g. "renderer" (shown when a group is expanded)
}

// wrapSuffix matches the "-wrapped" that NixOS adds to program names, including
// the cut-off versions Linux produces (it only keeps 15 characters of a name).
var wrapSuffix = regexp.MustCompile(`-wr(a(p(p(e(d)?)?)?)?)?$`)

// prettyName turns ".xfce4-panel-wr" into "xfce4-panel".
func prettyName(raw string) string {
	n := strings.TrimPrefix(strings.TrimSpace(raw), ".")
	n = wrapSuffix.ReplaceAllString(n, "")
	if n == "" {
		return raw
	}
	return n
}

// procGroup is every process that shares a name (all the "firefox" ones, ...).
type procGroup struct {
	Name  string
	Procs []ProcInfo
	CPU   float64
	Mem   float32
}

// procSortMode is how the list is ordered. Name is the default because it
// never moves: a row stays where you saw it.
type procSortMode int32

const (
	sortName procSortMode = iota
	sortCPU
	sortMem
)

// procMeta is what we work out once per process (it does not change while the
// process runs), so we are not re-reading /proc for it every 2 seconds.
type procMeta struct {
	Name   string
	Detail string
}

func lessProc(a, b ProcInfo, mode procSortMode) bool {
	if mode == sortName {
		return a.PID < b.PID
	}
	if mode == sortMem {
		if a.Mem != b.Mem {
			return a.Mem > b.Mem
		}
		if a.CPU != b.CPU {
			return a.CPU > b.CPU
		}
	} else {
		if a.CPU != b.CPU {
			return a.CPU > b.CPU
		}
		if a.Mem != b.Mem {
			return a.Mem > b.Mem
		}
	}
	return a.PID < b.PID
}

// groupProcs groups by name, adds up CPU and memory, and sorts busiest first.
func groupProcs(rows []ProcInfo, mode procSortMode) []procGroup {
	idx := make(map[string]int)
	var groups []procGroup
	for _, p := range rows {
		i, ok := idx[p.Name]
		if !ok {
			i = len(groups)
			idx[p.Name] = i
			groups = append(groups, procGroup{Name: p.Name})
		}
		g := &groups[i]
		g.Procs = append(g.Procs, p)
		g.CPU += p.CPU
		g.Mem += p.Mem
	}
	for i := range groups {
		ps := groups[i].Procs
		sort.Slice(ps, func(a, b int) bool { return lessProc(ps[a], ps[b], mode) })
	}
	sort.Slice(groups, func(i, j int) bool {
		if mode == sortName {
			li, lj := strings.ToLower(groups[i].Name), strings.ToLower(groups[j].Name)
			if li != lj {
				return li < lj
			}
			return groups[i].Name < groups[j].Name
		}
		a := ProcInfo{CPU: groups[i].CPU, Mem: groups[i].Mem}
		b := ProcInfo{CPU: groups[j].CPU, Mem: groups[j].Mem}
		if a.CPU != b.CPU || a.Mem != b.Mem {
			return lessProc(a, b, mode)
		}
		return groups[i].Name < groups[j].Name
	})
	return groups
}

// applyRank puts groups and their members into a previously taken order, so
// the list holds still between re-sorts. Anything new keeps its fresh position
// after the known ones.
func applyRank(groups []procGroup, rank map[string]int, pidRank map[int32]int) {
	const unknown = 1 << 30
	sort.SliceStable(groups, func(i, j int) bool {
		ri, ok := rank[groups[i].Name]
		if !ok {
			ri = unknown
		}
		rj, ok := rank[groups[j].Name]
		if !ok {
			rj = unknown
		}
		return ri < rj
	})
	for g := range groups {
		ps := groups[g].Procs
		sort.SliceStable(ps, func(i, j int) bool {
			ri, ok := pidRank[ps[i].PID]
			if !ok {
				ri = unknown
			}
			rj, ok := pidRank[ps[j].PID]
			if !ok {
				rj = unknown
			}
			return ri < rj
		})
	}
}

// cmdDetail describes a process from its command line. Chrome-style helpers
// say "--type=renderer"; otherwise we show the arguments, store paths shortened.
func cmdDetail(args []string) string {
	if len(args) < 2 {
		return ""
	}
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "--type=") {
			return strings.TrimPrefix(a, "--type=")
		}
	}
	short := make([]string, 0, len(args)-1)
	for _, a := range args[1:] {
		short = append(short, storePathRe.ReplaceAllString(a, ""))
	}
	d := strings.Join(short, " ")
	if r := []rune(d); len(r) > 60 {
		d = string(r[:60]) + "..."
	}
	return d
}

var storePathRe = regexp.MustCompile(`/nix/store/[a-z0-9]{32}-`)

type lineKind int

const (
	lineSingle lineKind = iota // a name with only one process: a plain row
	lineGroup                  // a name with several: click to expand
	lineChild                  // one process inside an expanded group
)

// procLine is one row on screen.
type procLine struct {
	Kind     lineKind
	Name     string
	Count    int
	Expanded bool
	Proc     ProcInfo // the process itself (single/child); unused for groups
	CPU      float64
	Mem      float32
	Killable []int32 // what the Kill button would act on (empty = no button)
}

// killablePIDs picks the processes we are allowed to stop: our own, never
// PID 1 and never NASTY itself.
func killablePIDs(ps []ProcInfo, self int32) []int32 {
	var out []int32
	for _, p := range ps {
		if p.Mine && p.PID > 1 && p.PID != self {
			out = append(out, p.PID)
		}
	}
	return out
}

func flattenGroups(groups []procGroup, expanded map[string]bool, self int32) []procLine {
	var lines []procLine
	for _, g := range groups {
		if len(g.Procs) == 1 {
			p := g.Procs[0]
			lines = append(lines, procLine{Kind: lineSingle, Name: g.Name, Count: 1, Proc: p,
				CPU: p.CPU, Mem: p.Mem, Killable: killablePIDs(g.Procs, self)})
			continue
		}
		open := expanded[g.Name]
		lines = append(lines, procLine{Kind: lineGroup, Name: g.Name, Count: len(g.Procs), Expanded: open,
			CPU: g.CPU, Mem: g.Mem, Killable: killablePIDs(g.Procs, self)})
		if open {
			for _, p := range g.Procs {
				lines = append(lines, procLine{Kind: lineChild, Name: g.Name, Count: 1, Proc: p,
					CPU: p.CPU, Mem: p.Mem, Killable: killablePIDs([]ProcInfo{p}, self)})
			}
		}
	}
	return lines
}

// ---- polling ----------------------------------------------------------------

// collectProcesses polls the process list every 2 seconds, but only while the
// Processes tab is showing, so it costs nothing the rest of the time.
func (ctx *UIContext) collectProcesses() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	wasActive := false
	for range ticker.C {
		if !ctx.procActive.Load() {
			wasActive = false
			if len(ctx.procHandles) > 0 {
				ctx.procHandles = make(map[int32]*process.Process)
				ctx.procMeta = make(map[int32]procMeta)
			}
			continue
		}
		ctx.refreshProcesses()
		if !wasActive {
			// CPU needs two samples, so take a second one right away.
			wasActive = true
			time.Sleep(time.Second)
			ctx.refreshProcesses()
		}
		ctx.Window.Invalidate()
	}
}

func (ctx *UIContext) refreshProcesses() {
	procs, err := process.Processes()
	if err != nil {
		return
	}
	me := os.Getuid()

	alive := make(map[int32]bool, len(procs))
	rows := make([]ProcInfo, 0, len(procs))
	for _, p := range procs {
		pid := p.Pid
		alive[pid] = true

		// Keep the same handle between polls: that is what lets Percent(0)
		// measure CPU use since the previous poll.
		h, ok := ctx.procHandles[pid]
		if !ok {
			h = p
			ctx.procHandles[pid] = h
		}

		meta, ok := ctx.procMeta[pid]
		if !ok {
			// The kernel cuts program names to 15 characters, which made
			// different programs look the same. The executable's file name is
			// the full, real name, so prefer it and fall back to the short one.
			name := ""
			if exe, err := h.Exe(); err == nil && exe != "" {
				name = prettyName(filepath.Base(exe))
			}
			if name == "" {
				n, err := h.Name()
				if err != nil {
					continue // the process exited while we were looking
				}
				name = prettyName(n)
			}
			args, _ := h.CmdlineSlice()
			meta = procMeta{Name: name, Detail: cmdDetail(args)}
			ctx.procMeta[pid] = meta
		}
		cpuPct, _ := h.Percent(0)
		memPct, _ := h.MemoryPercent()
		uids, uerr := h.Uids()
		mine := uerr == nil && len(uids) > 0 && int(uids[0]) == me
		rows = append(rows, ProcInfo{PID: pid, Name: meta.Name, CPU: cpuPct, Mem: memPct, Mine: mine, Detail: meta.Detail})
	}
	for pid := range ctx.procHandles {
		if !alive[pid] {
			delete(ctx.procHandles, pid)
			delete(ctx.procMeta, pid)
		}
	}

	ctx.procMu.Lock()
	ctx.procRows = rows
	ctx.procMu.Unlock()
}

func (ctx *UIContext) currentGroups() ([]procGroup, int) {
	ctx.procMu.Lock()
	rows := append([]ProcInfo(nil), ctx.procRows...)
	ctx.procMu.Unlock()
	mode := procSortMode(ctx.procSort.Load())
	groups := groupProcs(rows, mode)

	// Name order never changes by itself. CPU and memory change constantly, so
	// instead of re-sorting every poll (rows jumping around) we keep the order
	// for 10 seconds, or until a sort button is clicked.
	if mode != sortName {
		if ctx.procRank == nil || ctx.procRankMode != mode || time.Since(ctx.procRankAt) > 10*time.Second {
			ctx.procRank = make(map[string]int, len(groups))
			ctx.procPidRank = make(map[int32]int, len(rows))
			for i, g := range groups {
				ctx.procRank[g.Name] = i
				for j, p := range g.Procs {
					ctx.procPidRank[p.PID] = j
				}
			}
			ctx.procRankMode, ctx.procRankAt = mode, time.Now()
		} else {
			applyRank(groups, ctx.procRank, ctx.procPidRank)
		}
	}
	return groups, len(rows)
}

func (ctx *UIContext) setSort(m procSortMode) {
	ctx.procSort.Store(int32(m))
	ctx.procRank = nil // take a fresh order right away
}

// ---- killing ----------------------------------------------------------------

// sendSignal sends sig to each pid. A process that is already gone counts as done.
func sendSignal(pids []int32, sig syscall.Signal) (done int, failed []string) {
	self := int32(os.Getpid())
	for _, pid := range pids {
		if pid <= 1 || pid == self {
			continue
		}
		err := syscall.Kill(int(pid), sig)
		if err == nil || errors.Is(err, syscall.ESRCH) {
			done++
		} else {
			failed = append(failed, fmt.Sprintf("%d (%v)", pid, err))
		}
	}
	return done, failed
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func (ctx *UIContext) doKill(sig syscall.Signal) {
	pids := ctx.killPIDs
	done, failed := sendSignal(pids, sig)
	verb := "Asked to stop"
	if sig == syscall.SIGKILL {
		verb = "Force-killed"
	}
	msg := fmt.Sprintf("%s %s (%s).", verb, plural(done, "process", "processes"), ctx.killLabel)
	if len(failed) > 0 {
		msg += " Could not stop: " + strings.Join(failed, ", ")
	}
	ctx.killMsg = msg
	ctx.killLabel, ctx.killPIDs = "", nil
}

// procClick returns the (persistent) Clickable for a key like "kill:1234".
func (ctx *UIContext) procClick(key string) *widget.Clickable {
	if len(ctx.procClicks) > 3000 { // stale PIDs pile up on a long-running session
		ctx.procClicks = make(map[string]*widget.Clickable)
	}
	c := ctx.procClicks[key]
	if c == nil {
		c = new(widget.Clickable)
		ctx.procClicks[key] = c
	}
	return c
}

// handleProcInput must run every frame (Clicked has to be polled).
func (ctx *UIContext) handleProcInput(gtx layout.Context) {
	if ctx.SortNameBtn.Clicked(gtx) {
		ctx.setSort(sortName)
	}
	if ctx.SortCPUBtn.Clicked(gtx) {
		ctx.setSort(sortCPU)
	}
	if ctx.SortMemBtn.Clicked(gtx) {
		ctx.setSort(sortMem)
	}
	if ctx.KillCancelBtn.Clicked(gtx) {
		ctx.killLabel, ctx.killPIDs = "", nil
	}
	if ctx.KillTermBtn.Clicked(gtx) && len(ctx.killPIDs) > 0 {
		ctx.doKill(syscall.SIGTERM)
	}
	if ctx.KillForceBtn.Clicked(gtx) && len(ctx.killPIDs) > 0 {
		ctx.doKill(syscall.SIGKILL)
	}

	// Snapshot the keys first: handling a click may reset the map.
	var clicked []string
	for key, c := range ctx.procClicks {
		if c.Clicked(gtx) {
			clicked = append(clicked, key)
		}
	}
	if len(clicked) == 0 {
		return
	}
	groups, _ := ctx.currentGroups()
	self := int32(os.Getpid())
	for _, key := range clicked {
		switch {
		case strings.HasPrefix(key, "tog:"):
			name := strings.TrimPrefix(key, "tog:")
			ctx.procExpanded[name] = !ctx.procExpanded[name]
		case strings.HasPrefix(key, "killg:"):
			name := strings.TrimPrefix(key, "killg:")
			for _, g := range groups {
				if g.Name == name {
					ctx.killPIDs = killablePIDs(g.Procs, self)
					ctx.killLabel = fmt.Sprintf("all %d %q", len(ctx.killPIDs), name)
					ctx.killMsg = ""
				}
			}
		case strings.HasPrefix(key, "kill:"):
			var pid int32
			fmt.Sscanf(strings.TrimPrefix(key, "kill:"), "%d", &pid)
			for _, g := range groups {
				for _, p := range g.Procs {
					if p.PID == pid {
						ctx.killPIDs = killablePIDs([]ProcInfo{p}, self)
						ctx.killLabel = fmt.Sprintf("%q, PID %d", p.Name, p.PID)
						ctx.killMsg = ""
					}
				}
			}
		}
	}
	ctx.Window.Invalidate()
}

// ---- drawing ----------------------------------------------------------------

func procCell(ctx *UIContext, txt string, col color.NRGBA) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		l := material.Body2(ctx.Theme, txt)
		l.Color = col
		l.MaxLines = 1
		return l.Layout(gtx)
	}
}

// procHeader draws the column titles.
func procHeader(ctx *UIContext, gtx layout.Context, col color.NRGBA) layout.Dimensions {
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		fixedCell(unit.Dp(32), false, procCell(ctx, "", col)),
		layout.Flexed(1, procCell(ctx, "NAME", col)),
		fixedCell(unit.Dp(70), true, procCell(ctx, "PID", col)),
		fixedCell(unit.Dp(70), true, procCell(ctx, "CPU %", col)),
		fixedCell(unit.Dp(70), true, procCell(ctx, "MEM %", col)),
		layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
		fixedCell(unit.Dp(84), false, procCell(ctx, "", col)),
	)
}

func (ctx *UIContext) killCell(ln procLine) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		if len(ln.Killable) == 0 {
			c := ctx.Theme.Fg
			c.A = 120
			word := "system"
			if ln.Proc.Mine {
				word = "protected" // NASTY itself, or PID 1
			}
			return procCell(ctx, word, c)(gtx)
		}
		key, label := "kill:"+fmt.Sprint(ln.Proc.PID), "Kill"
		if ln.Kind == lineGroup {
			key, label = "killg:"+ln.Name, "Kill all"
		}
		b := material.Button(ctx.Theme, ctx.procClick(key), label)
		b.Background = colDanger
		b.Inset = layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4), Left: unit.Dp(8), Right: unit.Dp(8)}
		b.TextSize = unit.Sp(13)
		return b.Layout(gtx)
	}
}

func (ctx *UIContext) procLineRow(gtx layout.Context, ln procLine) layout.Dimensions {
	fg := ctx.Theme.Fg

	toggle := ""
	switch {
	case ln.Kind == lineGroup && ln.Expanded:
		toggle = "-"
	case ln.Kind == lineGroup:
		toggle = "+"
	}
	name := ln.Name
	pid := fmt.Sprintf("%d", ln.Proc.PID)
	switch ln.Kind {
	case lineGroup:
		name = fmt.Sprintf("%s  (%d)", ln.Name, ln.Count)
		pid = ""
	case lineChild:
		name = ln.Proc.Detail // children show what they are for; the group name is right above
	}

	content := func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				fixedCell(unit.Dp(32), false, func(gtx layout.Context) layout.Dimensions {
					l := material.H6(ctx.Theme, toggle)
					l.Color = fg
					return l.Layout(gtx)
				}),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					left := unit.Dp(0)
					if ln.Kind == lineChild {
						left = unit.Dp(18)
					}
					return layout.Inset{Left: left}.Layout(gtx, procCell(ctx, name, fg))
				}),
				fixedCell(unit.Dp(70), true, procCell(ctx, pid, fg)),
				fixedCell(unit.Dp(70), true, procCell(ctx, fmt.Sprintf("%.1f", ln.CPU), fg)),
				fixedCell(unit.Dp(70), true, procCell(ctx, fmt.Sprintf("%.1f", ln.Mem), fg)),
				layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
				fixedCell(unit.Dp(84), false, ctx.killCell(ln)),
			)
		})
	}

	if ln.Kind != lineGroup {
		return content(gtx)
	}
	// A group row is clickable all over: click anywhere to expand or collapse.
	shade := fg
	shade.A = 22
	return withBackground(gtx, shade, func(gtx layout.Context) layout.Dimensions {
		return ctx.procClick("tog:"+ln.Name).Layout(gtx, content)
	})
}

func (ctx *UIContext) killBar(gtx layout.Context) layout.Dimensions {
	if ctx.killLabel == "" {
		if ctx.killMsg == "" {
			return layout.Dimensions{}
		}
		return layout.Inset{Bottom: unit.Dp(6)}.Layout(gtx, material.Body2(ctx.Theme, ctx.killMsg).Layout)
	}
	bar := colNote
	bar.A = 70
	return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return withBackground(gtx, bar, func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
							layout.Rigid(material.Body1(ctx.Theme, "Stop "+ctx.killLabel+"?").Layout),
							layout.Rigid(material.Caption(ctx.Theme,
								"Stop asks the program to close, so it can save its work. Force kill ends it instantly and unsaved work is lost.").Layout),
						)
					}),
					layout.Rigid(material.Button(ctx.Theme, &ctx.KillTermBtn, "Stop").Layout),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						b := material.Button(ctx.Theme, &ctx.KillForceBtn, "Force kill")
						b.Background = colDanger
						return b.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						b := material.Button(ctx.Theme, &ctx.KillCancelBtn, "Cancel")
						b.Background = colMuted
						return b.Layout(gtx)
					}),
				)
			})
		})
	})
}

func renderTabProcesses(ctx *UIContext, gtx layout.Context) layout.Dimensions {
	groups, total := ctx.currentGroups()
	nGroups := len(groups)
	if len(groups) > maxProcGroups {
		groups = groups[:maxProcGroups]
	}
	lines := flattenGroups(groups, ctx.procExpanded, int32(os.Getpid()))
	mode := procSortMode(ctx.procSort.Load())

	muted := ctx.Theme.Fg
	muted.A = 170

	sortBtn := func(click *widget.Clickable, label string, active bool) layout.Widget {
		return func(gtx layout.Context) layout.Dimensions {
			b := material.Button(ctx.Theme, click, label)
			if !active {
				b.Background = colMuted
			}
			return b.Layout(gtx)
		}
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, material.H6(ctx.Theme, "Running processes").Layout),
				layout.Rigid(sortBtn(&ctx.SortNameBtn, "Sort by Name", mode == sortName)),
				layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
				layout.Rigid(sortBtn(&ctx.SortCPUBtn, "Sort by CPU", mode == sortCPU)),
				layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
				layout.Rigid(sortBtn(&ctx.SortMemBtn, "Sort by Memory", mode == sortMem)),
			)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			status := "Reading processes..."
			if total > 0 {
				status = fmt.Sprintf("%d processes in %d programs. Click a row with + to see its processes. Numbers update every 2 seconds; CPU and Memory sorting re-orders every 10 seconds so rows hold still. CPU %% is per core (100%% = one full core). You can only stop your own processes; system ones show \"system\".", total, nGroups)
			}
			return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(8)}.Layout(gtx,
				material.Caption(ctx.Theme, status).Layout)
		}),
		layout.Rigid(ctx.killBar),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return procHeader(ctx, gtx, muted)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return material.List(ctx.Theme, &ctx.ProcList).Layout(gtx, len(lines), func(gtx layout.Context, i int) layout.Dimensions {
				return ctx.procLineRow(gtx, lines[i])
			})
		}),
	)
}
