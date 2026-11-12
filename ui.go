package main

import (
	"fmt"
	"image"
	"image/color"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gioui.org/app"
	"gioui.org/f32"
	"gioui.org/io/clipboard"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/process"
)

const (
	searchLimit = 100
	maxLogLines = 2000
	historyLen  = 60
)

var (
	terminalBg = color.NRGBA{R: 20, G: 20, B: 20, A: 255}
	colAdd     = color.NRGBA{R: 60, G: 170, B: 80, A: 255}
	colDel     = color.NRGBA{R: 220, G: 80, B: 80, A: 255}
	colNote    = color.NRGBA{R: 200, G: 150, B: 40, A: 255}
	colDanger  = color.NRGBA{R: 180, G: 50, B: 50, A: 255}
	colMuted   = color.NRGBA{R: 80, G: 80, B: 80, A: 255}
)

func levelColor(l LogLevel) color.NRGBA {
	switch l {
	case LevelOK:
		return color.NRGBA{R: 110, G: 255, B: 140, A: 255}
	case LevelWarn:
		return color.NRGBA{R: 255, G: 210, B: 90, A: 255}
	case LevelError:
		return color.NRGBA{R: 255, G: 110, B: 110, A: 255}
	case LevelCmd:
		return color.NRGBA{R: 120, G: 200, B: 255, A: 255}
	}
	return color.NRGBA{R: 190, G: 235, B: 190, A: 255}
}

// PkgItem is a package row in the Packages tab. Checkbox state lives here so it
// survives re-running a search (items are cached by name in UIContext.Items).
type PkgItem struct {
	Name    string
	Desc    string
	Checked widget.Bool
}

// SystemStats holds the rolling metrics for the dashboard.
type SystemStats struct {
	sync.Mutex
	CPUHistory []float64
	MemHistory []float64
}

type textRow struct {
	Text string
	Col  color.NRGBA
}

// UIContext holds the persistent state for the Gio immediate-mode GUI.
type UIContext struct {
	Window *app.Window
	Theme  *material.Theme

	dark        atomic.Bool // desired color scheme (set by the D-Bus watcher)
	paletteSet  bool        // UI goroutine only
	paletteDark bool        // UI goroutine only

	// Tabs
	Tabs          []string
	ActiveTab     int
	savedTab      int
	TabClickables []*widget.Clickable

	// Packages tab
	SearchBox     widget.Editor
	PkgList       widget.List // left pane: search results
	InstList      widget.List // right pane: installed with NASTY
	UpdateBtn     widget.Clickable
	Items         map[string]*PkgItem // items with state, by attribute name
	Visible       []*PkgItem          // left pane contents
	ResultsCapped bool
	refreshed     bool
	lastQuery     string
	lastIndexVer  int64

	// Dashboard tab
	Stats SystemStats

	// Processes tab
	ProcList      widget.List
	SortNameBtn   widget.Clickable
	SortCPUBtn    widget.Clickable
	SortMemBtn    widget.Clickable
	KillTermBtn   widget.Clickable
	KillForceBtn  widget.Clickable
	KillCancelBtn widget.Clickable
	procMu        sync.Mutex
	procRows      []ProcInfo
	procSort      atomic.Int32                 // procSortMode: name (default), cpu or memory
	procRank      map[string]int               // frozen group order (UI goroutine only)
	procPidRank   map[int32]int                // frozen order inside groups
	procRankAt    time.Time                    // when the frozen order was taken
	procRankMode  procSortMode                 // which sort the frozen order belongs to
	procMeta      map[int32]procMeta           // name/details per PID, collector goroutine only
	procActive    atomic.Bool                  // processes tab is showing (only then do we poll)
	procHandles   map[int32]*process.Process   // used only by collectProcesses
	procExpanded  map[string]bool              // group name -> expanded (UI goroutine only)
	procClicks    map[string]*widget.Clickable // per-row buttons, by key (UI goroutine only)
	killLabel     string                       // what the "Stop it?" bar is asking about
	killMsg       string                       // result of the last stop/kill
	killPIDs      []int32

	// Hardware tab
	ToggleBT    widget.Bool
	TogglePrint widget.Bool
	ToggleAvahi widget.Bool
	RowBT       widget.Clickable // clicking anywhere on a row toggles its switch
	RowPrint    widget.Clickable
	RowAvahi    widget.Clickable

	// History (generations) tab
	GensList          widget.List
	RefreshGensBtn    widget.Clickable
	RollbackBtn       widget.Clickable
	RollbackCancelBtn widget.Clickable
	rollbackArmed     bool

	// Bottom bar
	ReviewBtn  widget.Clickable
	CancelBtn  widget.Clickable
	CopyLogBtn widget.Clickable
	LogList    widget.List
	logMu      sync.Mutex
	logLines   []LogLine

	// Review dialog (UI goroutine only)
	ReviewApplyBtn  widget.Clickable
	ReviewCancelBtn widget.Clickable
	ReviewDiffList  widget.List
	ReviewLogList   widget.List
	ShowRawDiff     widget.Bool
	reviewDiff      StateDiff
	reviewRaw       []DiffLine

	// Operation state (shared with background goroutines)
	opMu     sync.Mutex
	op       opState
	baseline SystemState // what is currently applied
	gens     []Generation

	busy       atomic.Bool
	dashActive atomic.Bool
}

func newUIContext(w *app.Window, theme ThemeState) *UIContext {
	ctx := &UIContext{
		Window: w,
		Theme:  material.NewTheme(),
		Tabs:   []string{"Packages", "Dashboard", "Processes", "Hardware", "History"},
		Items:  make(map[string]*PkgItem),

		procHandles:  make(map[int32]*process.Process),
		procMeta:     make(map[int32]procMeta),
		procExpanded: make(map[string]bool),
		procClicks:   make(map[string]*widget.Clickable),
	}
	ctx.dark.Store(theme.IsDark)
	applyPalette(ctx.Theme, theme.IsDark)
	ctx.paletteSet, ctx.paletteDark = true, theme.IsDark

	for range ctx.Tabs {
		ctx.TabClickables = append(ctx.TabClickables, new(widget.Clickable))
	}
	ctx.PkgList.List.Axis = layout.Vertical
	ctx.InstList.List.Axis = layout.Vertical
	ctx.GensList.List.Axis = layout.Vertical
	ctx.ProcList.List.Axis = layout.Vertical
	ctx.LogList.List.Axis = layout.Vertical
	ctx.LogList.List.ScrollToEnd = true
	ctx.ReviewLogList.List.Axis = layout.Vertical
	ctx.ReviewLogList.List.ScrollToEnd = true
	ctx.ReviewDiffList.List.Axis = layout.Vertical
	ctx.SearchBox.SingleLine = true

	if tab, err := strconv.Atoi(getSetting("tab")); err == nil && tab >= 0 && tab < len(ctx.Tabs) {
		ctx.ActiveTab = tab
	}
	ctx.savedTab = ctx.ActiveTab

	// Start from what is already configured so the first Apply can't wipe it.
	st := loadSidecarState()
	ctx.baseline = st
	ctx.ToggleBT.Value = st.Hardware.Bluetooth
	ctx.TogglePrint.Value = st.Hardware.Printing
	ctx.ToggleAvahi.Value = st.Hardware.Avahi
	for _, p := range st.Packages {
		it := &PkgItem{Name: p}
		it.Checked.Value = true
		ctx.Items[p] = it
	}
	ctx.gens, _ = listGenerations()

	ctx.appendLine(LevelInfo, "NASTY is ready.")
	if len(st.Packages) > 0 {
		ctx.appendLine(LevelInfo, fmt.Sprintf("Loaded your current configuration (%d packages).", len(st.Packages)))
	}
	for _, warn := range setupWarnings() {
		ctx.appendLine(LevelWarn, warn)
	}

	go ctx.collectTelemetry()
	go ctx.collectProcesses()
	go func() { // redraw when the background package index finishes
		for range IndexReady {
			w.Invalidate()
		}
	}()
	return ctx
}

// SetDark is called from the D-Bus watcher when the desktop theme changes.
func (ctx *UIContext) SetDark(dark bool) {
	ctx.dark.Store(dark)
	ctx.Window.Invalidate()
}

func (ctx *UIContext) applyPaletteIfNeeded() {
	dark := ctx.dark.Load()
	if !ctx.paletteSet || dark != ctx.paletteDark {
		applyPalette(ctx.Theme, dark)
		ctx.paletteSet, ctx.paletteDark = true, dark
	}
}

// ---- logging ----------------------------------------------------------------

func (ctx *UIContext) appendLine(level LogLevel, text string) {
	text = cleanLine(text)
	if text == "" {
		return
	}
	ctx.logMu.Lock()
	ctx.logLines = append(ctx.logLines, LogLine{Level: level, Text: text})
	if n := len(ctx.logLines); n > maxLogLines {
		ctx.logLines = append([]LogLine(nil), ctx.logLines[n-maxLogLines:]...)
	}
	ctx.logMu.Unlock()
}

func (ctx *UIContext) addLine(level LogLevel, text string) {
	ctx.appendLine(level, text)
	ctx.Window.Invalidate()
}

func (ctx *UIContext) logf(format string, args ...any) {
	ctx.addLine(LevelInfo, fmt.Sprintf(format, args...))
}

func (ctx *UIContext) logSnapshot() []LogLine {
	ctx.logMu.Lock()
	defer ctx.logMu.Unlock()
	return append([]LogLine(nil), ctx.logLines...)
}

func (ctx *UIContext) tailLines(n int) []LogLine {
	lines := ctx.logSnapshot()
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func (ctx *UIContext) logText() string {
	lines := ctx.logSnapshot()
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = l.Text
	}
	return strings.Join(parts, "\n")
}

func copyToClipboard(gtx layout.Context, text string) {
	gtx.Execute(clipboard.WriteCmd{
		Type: "application/text",
		Data: io.NopCloser(strings.NewReader(text)),
	})
}

// ---- telemetry --------------------------------------------------------------

func pushHistory(h []float64, v float64) []float64 {
	h = append(h, v)
	if len(h) > historyLen {
		h = h[len(h)-historyLen:]
	}
	return h
}

// collectTelemetry polls once a second. It only forces a redraw while the
// dashboard is visible or an operation is running (to update the elapsed time).
func (ctx *UIContext) collectTelemetry() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		cpuPercent, _ := cpu.Percent(0, false)
		vMem, _ := mem.VirtualMemory()

		ctx.Stats.Lock()
		if len(cpuPercent) > 0 {
			ctx.Stats.CPUHistory = pushHistory(ctx.Stats.CPUHistory, cpuPercent[0])
		}
		if vMem != nil {
			ctx.Stats.MemHistory = pushHistory(ctx.Stats.MemHistory, vMem.UsedPercent)
		}
		ctx.Stats.Unlock()

		if ctx.dashActive.Load() || ctx.busy.Load() {
			ctx.Window.Invalidate()
		}
	}
}

// ---- state helpers ----------------------------------------------------------

func (ctx *UIContext) selectedNames() []string {
	var names []string
	for name, it := range ctx.Items {
		if it.Checked.Value {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (ctx *UIContext) currentState() SystemState {
	return SystemState{
		Packages: ctx.selectedNames(),
		Hardware: HardwareToggles{
			Bluetooth: ctx.ToggleBT.Value,
			Printing:  ctx.TogglePrint.Value,
			Avahi:     ctx.ToggleAvahi.Value,
		},
	}
}

func (ctx *UIContext) pendingCount(snap opSnapshot) int {
	return diffStates(snap.Baseline, ctx.currentState()).Count()
}

func (ctx *UIContext) reviewRows() []textRow {
	var rows []textRow
	d := ctx.reviewDiff
	if d.Count() == 0 {
		rows = append(rows, textRow{"No changes compared with your current configuration. A rebuild will still run.", ctx.Theme.Fg})
	}
	for _, p := range d.Added {
		rows = append(rows, textRow{"+ install  " + p, colAdd})
	}
	for _, p := range d.Removed {
		rows = append(rows, textRow{"- remove   " + p, colDel})
	}
	for _, h := range d.Hardware {
		rows = append(rows, textRow{"~ " + h, colNote})
	}

	if ctx.ShowRawDiff.Value {
		rows = append(rows, textRow{" ", ctx.Theme.Fg}, textRow{"Changes to " + sidecarPath + ":", ctx.Theme.Fg})
		if len(ctx.reviewRaw) == 0 {
			rows = append(rows, textRow{"(the file would not change)", ctx.Theme.Fg})
		}
		for _, l := range ctx.reviewRaw {
			col, prefix := ctx.Theme.Fg, "  "
			switch l.Kind {
			case '+':
				col, prefix = colAdd, "+ "
			case '-':
				col, prefix = colDel, "- "
			}
			rows = append(rows, textRow{prefix + l.Text, col})
		}
	}
	return rows
}

// ---- input ------------------------------------------------------------------

func (ctx *UIContext) handleInput(gtx layout.Context, snap opSnapshot) {
	idle := snap.Phase == PhaseIdle

	if ctx.ReviewBtn.Clicked(gtx) && idle {
		ctx.beginReview()
	}
	if ctx.UpdateBtn.Clicked(gtx) && idle {
		ctx.startSimple(OpUpdateChannels, "Updating channels")
	}
	if ctx.CancelBtn.Clicked(gtx) {
		ctx.cancelOp()
	}
	if ctx.ReviewApplyBtn.Clicked(gtx) {
		ctx.startApply()
	}
	if ctx.ReviewCancelBtn.Clicked(gtx) {
		ctx.cancelOp()
	}
	if ctx.CopyLogBtn.Clicked(gtx) {
		copyToClipboard(gtx, ctx.logText())
		ctx.addLine(LevelInfo, "Log copied to the clipboard.")
	}
	if ctx.RefreshGensBtn.Clicked(gtx) {
		ctx.reloadGenerations()
	}
	if ctx.RollbackBtn.Clicked(gtx) && idle {
		if ctx.rollbackArmed {
			ctx.rollbackArmed = false
			ctx.startSimple(OpRollback, "Rolling back")
		} else {
			ctx.rollbackArmed = true
		}
	}
	if ctx.RollbackCancelBtn.Clicked(gtx) {
		ctx.rollbackArmed = false
	}
	ctx.handleProcInput(gtx)

	// Clicking anywhere on a Hardware row flips its switch.
	if ctx.RowBT.Clicked(gtx) {
		ctx.ToggleBT.Value = !ctx.ToggleBT.Value
	}
	if ctx.RowPrint.Clicked(gtx) {
		ctx.TogglePrint.Value = !ctx.TogglePrint.Value
	}
	if ctx.RowAvahi.Clicked(gtx) {
		ctx.ToggleAvahi.Value = !ctx.ToggleAvahi.Value
	}

	for i, click := range ctx.TabClickables {
		if click.Clicked(gtx) {
			ctx.ActiveTab = i
		}
	}
	if ctx.ActiveTab != ctx.savedTab {
		ctx.killLabel, ctx.killPIDs, ctx.killMsg = "", nil, "" // leaving the tab cancels a pending "stop it?"
		setSetting("tab", strconv.Itoa(ctx.ActiveTab))
		ctx.savedTab = ctx.ActiveTab
	}
}

// ---- rendering --------------------------------------------------------------

func renderMainLayout(ctx *UIContext, gtx layout.Context) {
	ctx.applyPaletteIfNeeded()
	paint.Fill(gtx.Ops, ctx.Theme.Bg)

	snap := ctx.snapshot()
	ctx.ensureBaselineItems(snap.Baseline)
	ctx.handleInput(gtx, snap)
	snap = ctx.snapshot() // input may have changed the phase
	ctx.ensureBaselineItems(snap.Baseline)
	ctx.refreshVisible(snap.Baseline)
	ctx.dashActive.Store(ctx.ActiveTab == 1)
	ctx.procActive.Store(ctx.ActiveTab == 2)

	dialogOpen := snap.Phase == PhaseChecking || snap.Phase == PhaseReview

	mainGtx := gtx
	if dialogOpen {
		mainGtx = gtx.Disabled() // the dialog is modal
	}
	renderMain(ctx, mainGtx, snap)
	if dialogOpen {
		renderReviewDialog(ctx, gtx, snap)
	}
}

func renderMain(ctx *UIContext, gtx layout.Context, snap opSnapshot) {
	layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return renderTabs(ctx, gtx)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(16)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				switch ctx.ActiveTab {
				case 0:
					return renderTabStore(ctx, gtx, snap)
				case 1:
					return renderTabDashboard(ctx, gtx)
				case 2:
					return renderTabProcesses(ctx, gtx)
				case 3:
					return renderTabHardware(ctx, gtx)
				case 4:
					return renderTabGenerations(ctx, gtx, snap)
				}
				return layout.Dimensions{}
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return renderBottomBar(ctx, gtx, snap)
		}),
	)
}

func renderTabs(ctx *UIContext, gtx layout.Context) layout.Dimensions {
	var children []layout.FlexChild
	for i, tabName := range ctx.Tabs {
		i := i
		tabName := tabName
		children = append(children, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			btn := material.Button(ctx.Theme, ctx.TabClickables[i], tabName)
			if ctx.ActiveTab != i {
				btn.Background = color.NRGBA{R: 50, G: 50, B: 50, A: 255}
			}
			return btn.Layout(gtx)
		}))
	}
	return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, children...)
}

func renderTabDashboard(ctx *UIContext, gtx layout.Context) layout.Dimensions {
	ctx.Stats.Lock()
	cpuData := append([]float64(nil), ctx.Stats.CPUHistory...)
	memData := append([]float64(nil), ctx.Stats.MemHistory...)
	ctx.Stats.Unlock()

	cpuTitle := fmt.Sprintf("CPU: %.0f%%", lastValue(cpuData))
	memTitle := fmt.Sprintf("Memory: %.0f%%", lastValue(memData))

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(material.H6(ctx.Theme, "Real-Time Telemetry (last 60 seconds, scale 0-100%)").Layout),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return renderGraph(ctx, gtx, cpuTitle, cpuData, color.NRGBA{R: 200, G: 50, B: 50, A: 255})
				}),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return renderGraph(ctx, gtx, memTitle, memData, color.NRGBA{R: 50, G: 150, B: 200, A: 255})
				}),
			)
		}),
	)
}

func lastValue(d []float64) float64 {
	if len(d) == 0 {
		return 0
	}
	return d[len(d)-1]
}

func renderGraph(ctx *UIContext, gtx layout.Context, title string, data []float64, lineColor color.NRGBA) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(material.H6(ctx.Theme, title).Layout),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			size := gtx.Constraints.Max

			// Faint gridlines at 25%, 50% and 75% so low values are readable.
			grid := ctx.Theme.Fg
			grid.A = 45
			lineH := gtx.Dp(1)
			for _, frac := range []float32{0.25, 0.5, 0.75} {
				y := int(float32(size.Y) * frac)
				paint.FillShape(gtx.Ops, grid,
					clip.Rect{Min: image.Pt(0, y), Max: image.Pt(size.X, y+lineH)}.Op())
			}

			if len(data) < 2 {
				return layout.Dimensions{Size: size}
			}

			var path clip.Path
			path.Begin(gtx.Ops)

			width := float32(size.X)
			height := float32(size.Y)
			stepX := width / float32(historyLen-1)

			for i, val := range data {
				if val < 0 {
					val = 0
				} else if val > 100 {
					val = 100
				}
				x := float32(i) * stepX
				y := height - (float32(val) / 100.0 * height)
				if i == 0 {
					path.MoveTo(f32.Pt(x, y))
				} else {
					path.LineTo(f32.Pt(x, y))
				}
			}

			paint.FillShape(gtx.Ops, lineColor, clip.Stroke{
				Path:  path.End(),
				Width: 3,
			}.Op())

			return layout.Dimensions{Size: size}
		}),
	)
}

// labeledSwitch draws a whole-row clickable switch with a visible On/Off word.
// Clicking the switch itself or anywhere else on the row toggles it.
func labeledSwitch(ctx *UIContext, gtx layout.Context, row *widget.Clickable, b *widget.Bool, label string) layout.Dimensions {
	return row.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X // make the whole row clickable

			sw := material.Switch(ctx.Theme, b, label)
			sw.Color.Track = color.NRGBA{R: 120, G: 120, B: 120, A: 255} // visible when off

			state := "Off"
			stateCol := ctx.Theme.Fg
			stateCol.A = 170
			if b.Value {
				state = "On"
				stateCol = ctx.Theme.ContrastBg
			}
			stateLbl := material.Body1(ctx.Theme, state)
			stateLbl.Color = stateCol

			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(sw.Layout),
				layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Min.X = gtx.Dp(36) // keeps the label from jumping
					return stateLbl.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
				layout.Rigid(material.Body1(ctx.Theme, label).Layout),
			)
		})
	})
}

func renderTabHardware(ctx *UIContext, gtx layout.Context) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(material.H6(ctx.Theme, "Safe Core Services").Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: unit.Dp(8), Bottom: unit.Dp(4)}.Layout(gtx,
				material.Caption(ctx.Theme, "Click a row to switch it on or off. Nothing changes on your system until you press Review & Apply.").Layout)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return labeledSwitch(ctx, gtx, &ctx.RowBT, &ctx.ToggleBT, "Bluetooth")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return labeledSwitch(ctx, gtx, &ctx.RowPrint, &ctx.TogglePrint, "Printing (CUPS)")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return labeledSwitch(ctx, gtx, &ctx.RowAvahi, &ctx.ToggleAvahi, "Network Printer Auto-Discovery (Avahi)")
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: unit.Dp(8), Top: unit.Dp(8)}.Layout(gtx,
				material.Caption(ctx.Theme, "Settings you made by hand in configuration.nix always take priority over these switches.").Layout)
		}),
	)
}

// fixedCell lays a widget out in a column of exactly `width`, right-aligned
// (for numbers) or left-aligned.
func fixedCell(width unit.Dp, right bool, w layout.Widget) layout.FlexChild {
	return layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		px := gtx.Dp(width)
		gtx.Constraints.Min.X = px
		gtx.Constraints.Max.X = px
		var d layout.Dimensions
		if right {
			d = layout.E.Layout(gtx, w)
		} else {
			d = w(gtx)
		}
		d.Size.X = px
		return d
	})
}

func renderTabGenerations(ctx *UIContext, gtx layout.Context, snap opSnapshot) layout.Dimensions {
	idle := snap.Phase == PhaseIdle
	prev, hasPrev := previousGeneration(snap.Gens)

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, material.H6(ctx.Theme, "System generations").Layout),
				layout.Rigid(material.Button(ctx.Theme, &ctx.RefreshGensBtn, "Refresh").Layout),
			)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(8)}.Layout(gtx,
				material.Body2(ctx.Theme, "Every successful rebuild creates a new generation. If something breaks, roll back to the previous one. Older ones can be chosen from the boot menu.").Layout)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			if len(snap.Gens) == 0 {
				return material.Body1(ctx.Theme, "No generations found. (This tab needs a NixOS system.)").Layout(gtx)
			}
			return material.List(ctx.Theme, &ctx.GensList).Layout(gtx, len(snap.Gens), func(gtx layout.Context, i int) layout.Dimensions {
				g := snap.Gens[i]
				text := fmt.Sprintf("#%d    %s    %s", g.Number, g.Date.Format("2006-01-02 15:04"), g.Version)
				if g.Current {
					text += "    (current)"
				}
				l := material.Body1(ctx.Theme, text)
				if g.Current {
					l.Color = ctx.Theme.ContrastBg
				}
				return layout.UniformInset(unit.Dp(4)).Layout(gtx, l.Layout)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !idle || !hasPrev {
				gtx = gtx.Disabled()
			}
			return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				if !ctx.rollbackArmed {
					return material.Button(ctx.Theme, &ctx.RollbackBtn, "Roll back to the previous generation").Layout(gtx)
				}
				msg := fmt.Sprintf("Roll back to generation #%d?", prev.Number)
				warn := material.Body2(ctx.Theme, "Heads up: this only changes the running system. It does NOT edit nasty-config.nix, so the next Review & Apply (or nixos-rebuild) can undo the rollback. Afterwards, tick or untick packages to match what you want and apply, so the file matches again.")
				warn.Color = colNote
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(warn.Layout),
					layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
							layout.Rigid(material.Body1(ctx.Theme, msg).Layout),
							layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
							layout.Rigid(material.Button(ctx.Theme, &ctx.RollbackBtn, "Yes, roll back").Layout),
							layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
							layout.Rigid(material.Button(ctx.Theme, &ctx.RollbackCancelBtn, "Keep current").Layout),
						)
					}),
				)
			})
		}),
	)
}

// ---- log terminal and bottom bar ---------------------------------------------

func logView(ctx *UIContext, gtx layout.Context, list *widget.List, lines []LogLine) layout.Dimensions {
	return material.List(ctx.Theme, list).Layout(gtx, len(lines), func(gtx layout.Context, i int) layout.Dimensions {
		l := material.Body2(ctx.Theme, lines[i].Text)
		l.Color = levelColor(lines[i].Level)
		l.Font.Typeface = "Go Mono"
		return l.Layout(gtx)
	})
}

// terminal draws a dark panel with the colored log inside, filling the
// constraints it is given.
func terminal(ctx *UIContext, gtx layout.Context, list *widget.List, lines []LogLine) layout.Dimensions {
	size := gtx.Constraints.Max
	paint.FillShape(gtx.Ops, terminalBg, clip.Rect{Max: size}.Op())
	layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return logView(ctx, gtx, list, lines)
	})
	return layout.Dimensions{Size: size}
}

func formatElapsed(d time.Duration) string {
	return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

func statusLine(snap opSnapshot) string {
	s := fmt.Sprintf("%s... %s", snap.Name, formatElapsed(snap.Elapsed))
	if snap.Progress != "" {
		s += "  |  " + snap.Progress
	}
	if snap.Cancelling {
		s += "  (cancelling)"
	}
	return s
}

func changesLabel(n int) string {
	if n == 1 {
		return "Review & Apply (1 change)"
	}
	return fmt.Sprintf("Review & Apply (%d changes)", n)
}

func renderBottomBar(ctx *UIContext, gtx layout.Context, snap opSnapshot) layout.Dimensions {
	lines := ctx.logSnapshot()
	busy := snap.Phase != PhaseIdle
	showCancel := snap.Phase == PhaseApplying || snap.Phase == PhaseWorking

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			h := gtx.Dp(150)
			gtx.Constraints.Min.Y = h
			gtx.Constraints.Max.Y = h
			return terminal(ctx, gtx, &ctx.LogList, lines)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !showCancel {
				return layout.Dimensions{}
			}
			return layout.Inset{Left: unit.Dp(8), Top: unit.Dp(6)}.Layout(gtx,
				material.Body2(ctx.Theme, statusLine(snap)).Layout)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if busy {
							gtx = gtx.Disabled()
						}
						label := "Review & Apply"
						if n := ctx.pendingCount(snap); n > 0 {
							label = changesLabel(n)
						}
						btn := material.Button(ctx.Theme, &ctx.ReviewBtn, label)
						btn.Background = colDanger
						return btn.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if !showCancel {
							return layout.Dimensions{}
						}
						label := "Cancel"
						if snap.Cancelling {
							label = "Cancelling..."
							gtx = gtx.Disabled()
						}
						return material.Button(ctx.Theme, &ctx.CancelBtn, label).Layout(gtx)
					}),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, 0)}
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						btn := material.Button(ctx.Theme, &ctx.CopyLogBtn, "Copy log")
						btn.Background = colMuted
						return btn.Layout(gtx)
					}),
				)
			})
		}),
	)
}

// ---- review dialog ------------------------------------------------------------

// withBackground paints a solid background behind whatever w draws.
func withBackground(gtx layout.Context, col color.NRGBA, w layout.Widget) layout.Dimensions {
	m := op.Record(gtx.Ops)
	dims := w(gtx)
	call := m.Stop()
	paint.FillShape(gtx.Ops, col, clip.Rect{Max: dims.Size}.Op())
	call.Add(gtx.Ops)
	return dims
}

func renderReviewDialog(ctx *UIContext, gtx layout.Context, snap opSnapshot) {
	paint.Fill(gtx.Ops, color.NRGBA{A: 160}) // dim the app behind the dialog

	margin := gtx.Dp(24)
	w := minInt(gtx.Constraints.Max.X-2*margin, gtx.Dp(780))
	h := minInt(gtx.Constraints.Max.Y-2*margin, gtx.Dp(620))
	if w < 280 {
		w = minInt(280, gtx.Constraints.Max.X)
	}
	if h < 240 {
		h = minInt(240, gtx.Constraints.Max.Y)
	}

	layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints = layout.Exact(image.Pt(w, h))
		return withBackground(gtx, ctx.Theme.Bg, func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(16)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return reviewContent(ctx, gtx, snap)
			})
		})
	})
}

func reviewContent(ctx *UIContext, gtx layout.Context, snap opSnapshot) layout.Dimensions {
	rows := ctx.reviewRows()
	logTail := ctx.tailLines(60)
	checking := snap.Phase == PhaseChecking
	canApply := snap.Phase == PhaseReview && snap.CheckOK

	var status string
	var statusCol color.NRGBA
	switch {
	case checking:
		status = "Running a dry build to make sure this configuration works... " + formatElapsed(snap.Elapsed)
		if snap.Progress != "" {
			status += "  |  " + snap.Progress
		}
		statusCol = ctx.Theme.Fg
	case canApply:
		status = "[OK] The dry build passed. Nothing has been changed yet. Press Apply Now to switch to this configuration."
		statusCol = colAdd
	default:
		status = "[FAILED] The dry build did not pass (or was cancelled). Nothing was changed. The log below shows why."
		statusCol = colDel
	}

	gap := layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout)

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(material.H6(ctx.Theme, "Review changes").Layout),
		gap,
		layout.Flexed(2, func(gtx layout.Context) layout.Dimensions {
			return material.List(ctx.Theme, &ctx.ReviewDiffList).Layout(gtx, len(rows), func(gtx layout.Context, i int) layout.Dimensions {
				l := material.Body2(ctx.Theme, rows[i].Text)
				l.Color = rows[i].Col
				l.Font.Typeface = "Go Mono"
				return l.Layout(gtx)
			})
		}),
		layout.Rigid(material.CheckBox(ctx.Theme, &ctx.ShowRawDiff, "Show the exact changes to "+sidecarPath).Layout),
		gap,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			l := material.Body1(ctx.Theme, status)
			l.Color = statusCol
			return l.Layout(gtx)
		}),
		gap,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return terminal(ctx, gtx, &ctx.ReviewLogList, logTail)
		}),
		gap,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if !canApply {
						gtx = gtx.Disabled()
					}
					btn := material.Button(ctx.Theme, &ctx.ReviewApplyBtn, "Apply Now")
					btn.Background = colDanger
					return btn.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					label := "Close"
					if checking {
						label = "Cancel check"
					}
					if snap.Cancelling {
						label = "Cancelling..."
						gtx = gtx.Disabled()
					}
					return material.Button(ctx.Theme, &ctx.ReviewCancelBtn, label).Layout(gtx)
				}),
			)
		}),
	)
}

// ---- packages tab (two panes) -------------------------------------------------

// ensureBaselineItems makes sure every installed package has an Item, so an
// installed package can never be dropped from the desired state by accident.
func (ctx *UIContext) ensureBaselineItems(b SystemState) {
	for _, p := range b.Packages {
		if ctx.Items[p] == nil {
			it := &PkgItem{Name: p}
			it.Checked.Value = true
			ctx.Items[p] = it
		}
	}
}

// refreshVisible re-runs the search only when the query or the index changed.
// An empty search shows nothing on the left (a hint is shown instead).
func (ctx *UIContext) refreshVisible(baseline SystemState) {
	query := strings.TrimSpace(ctx.SearchBox.Text())
	ver := indexVersion.Load()
	if ctx.refreshed && query == ctx.lastQuery && ver == ctx.lastIndexVer {
		return
	}
	ctx.refreshed, ctx.lastQuery, ctx.lastIndexVer = true, query, ver

	var results []PkgResult
	if query != "" {
		results = SearchPackages(query, searchLimit)
	}
	ctx.ResultsCapped = len(results) >= searchLimit

	keep := make(map[string]bool, len(results)+len(baseline.Packages))
	for _, p := range baseline.Packages {
		keep[p] = true
	}
	visible := make([]*PkgItem, 0, len(results))
	for _, r := range results {
		it := ctx.Items[r.Name]
		if it == nil {
			it = &PkgItem{Name: r.Name}
			ctx.Items[r.Name] = it
		}
		it.Desc = r.Description
		visible = append(visible, it)
		keep[r.Name] = true
	}
	ctx.Visible = visible

	// Forget unticked packages that are neither installed nor on screen.
	for name, it := range ctx.Items {
		if !it.Checked.Value && !keep[name] {
			delete(ctx.Items, name)
		}
	}
}

// installedView is the right pane: queued installs first, then everything
// installed, both filtered by the search text.
func (ctx *UIContext) installedView(installed map[string]bool) []*PkgItem {
	q := strings.ToLower(ctx.lastQuery)
	var queued, current []*PkgItem
	for name, it := range ctx.Items {
		inst := installed[name]
		if !inst && !it.Checked.Value {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(name), q) {
			continue
		}
		if inst {
			current = append(current, it)
		} else {
			queued = append(queued, it)
		}
	}
	byName := func(s []*PkgItem) {
		sort.Slice(s, func(i, j int) bool { return s[i].Name < s[j].Name })
	}
	byName(queued)
	byName(current)
	return append(queued, current...)
}

func (ctx *UIContext) storeStatus(snap opSnapshot) string {
	d := diffStates(snap.Baseline, ctx.currentState())
	if len(d.Added)+len(d.Removed) == 0 {
		return "Search on the left to install packages. Untick an installed package on the right to remove it. Nothing changes until you press Review & Apply."
	}
	return fmt.Sprintf("Pending: install %d, remove %d. Press Review & Apply to check and apply.", len(d.Added), len(d.Removed))
}

// checkRow draws a package checkbox and asks for another frame when it changes,
// so the other pane catches up immediately.
func (ctx *UIContext) checkRow(gtx layout.Context, it *PkgItem) layout.Dimensions {
	before := it.Checked.Value
	d := material.CheckBox(ctx.Theme, &it.Checked, it.Name).Layout(gtx)
	if it.Checked.Value != before {
		ctx.Window.Invalidate()
	}
	return d
}

func renderTabStore(ctx *UIContext, gtx layout.Context, snap opSnapshot) layout.Dimensions {
	idle := snap.Phase == PhaseIdle
	installed := make(map[string]bool, len(snap.Baseline.Packages))
	for _, p := range snap.Baseline.Packages {
		installed[p] = true
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					ed := material.Editor(ctx.Theme, &ctx.SearchBox, "Search packages...")
					return layout.UniformInset(unit.Dp(8)).Layout(gtx, ed.Layout)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if !idle {
						gtx = gtx.Disabled()
					}
					return material.Button(ctx.Theme, &ctx.UpdateBtn, "Update Repos").Layout(gtx)
				}),
			)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: unit.Dp(8), Bottom: unit.Dp(8)}.Layout(gtx,
				material.Body2(ctx.Theme, ctx.storeStatus(snap)).Layout)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return ctx.availablePane(gtx, installed)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(16)}.Layout),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return ctx.installedPane(gtx, installed)
				}),
			)
		}),
	)
}

// availablePane (left): search results. Ticking installs; packages that are
// already installed show a label instead of a checkbox, so unticking here can
// never remove anything.
func (ctx *UIContext) availablePane(gtx layout.Context, installed map[string]bool) layout.Dimensions {
	hint := "Tick a package to install it."
	switch {
	case ctx.lastQuery == "":
		hint = "Type in the search box to find packages."
	case len(ctx.Visible) == 0:
		hint = fmt.Sprintf("No packages match %q. (On first launch the package list may still be building.)", ctx.lastQuery)
	case ctx.ResultsCapped:
		hint = fmt.Sprintf("Tick a package to install it. Showing the first %d matches, type more to narrow down.", searchLimit)
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(material.H6(ctx.Theme, "Available packages").Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(6)}.Layout(gtx, material.Caption(ctx.Theme, hint).Layout)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return material.List(ctx.Theme, &ctx.PkgList).Layout(gtx, len(ctx.Visible), func(gtx layout.Context, i int) layout.Dimensions {
				pkg := ctx.Visible[i]
				return layout.Inset{Top: unit.Dp(3), Bottom: unit.Dp(3)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							if installed[pkg.Name] {
								return layout.Inset{Left: unit.Dp(32)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									l := material.Body1(ctx.Theme, pkg.Name+"   (installed)")
									c := ctx.Theme.Fg
									c.A = 150
									l.Color = c
									return l.Layout(gtx)
								})
							}
							return ctx.checkRow(gtx, pkg)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							if pkg.Desc == "" {
								return layout.Dimensions{}
							}
							lbl := material.Caption(ctx.Theme, pkg.Desc)
							c := ctx.Theme.Fg
							c.A = 150
							lbl.Color = c
							lbl.MaxLines = 1
							return layout.Inset{Left: unit.Dp(32)}.Layout(gtx, lbl.Layout)
						}),
					)
				})
			})
		}),
	)
}

// installedPane (right): what NASTY has installed, plus anything queued.
// Unticking an installed package removes it on the next apply.
func (ctx *UIContext) installedPane(gtx layout.Context, installed map[string]bool) layout.Dimensions {
	items := ctx.installedView(installed) // built after the left pane drew, so it is up to date

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(material.H6(ctx.Theme, fmt.Sprintf("Installed with NASTY (%d)", len(installed))).Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(6)}.Layout(gtx, material.Caption(ctx.Theme,
				"Untick a package to remove it when you apply. Only packages managed by NASTY are listed here, not everything on your system.").Layout)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			if len(items) == 0 {
				msg := "Nothing installed through NASTY yet. Tick packages on the left to add them."
				if len(installed) > 0 {
					msg = fmt.Sprintf("No installed packages match %q.", ctx.lastQuery)
				}
				return material.Body2(ctx.Theme, msg).Layout(gtx)
			}
			return material.List(ctx.Theme, &ctx.InstList).Layout(gtx, len(items), func(gtx layout.Context, i int) layout.Dimensions {
				it := items[i]
				inst := installed[it.Name]
				return layout.Inset{Top: unit.Dp(3), Bottom: unit.Dp(3)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return ctx.checkRow(gtx, it)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							var txt string
							var col color.NRGBA
							switch {
							case inst && !it.Checked.Value:
								txt, col = "will be removed when you apply", colDel
							case !inst && it.Checked.Value:
								txt, col = "will be installed when you apply", colAdd
							default:
								return layout.Dimensions{}
							}
							l := material.Caption(ctx.Theme, txt)
							l.Color = col
							return layout.Inset{Left: unit.Dp(32)}.Layout(gtx, l.Layout)
						}),
					)
				})
			})
		}),
	)
}
