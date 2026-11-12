package main

import (
	"log"
	"os"
	"strconv"
	"gioui.org/app"
	"gioui.org/op"
	"gioui.org/unit"
)

func main() {
	// When started as `pkexec nasty --helper` this process is the privileged
	// half of the app: no window, no database, just the helper protocol.
	if len(os.Args) > 1 && os.Args[1] == "--helper" {
		os.Exit(runHelper(os.Stdin, os.Stdout))
	}

	if err := run(); err != nil {
		log.Fatalf("Fatal error: %v", err)
	}
}

func savedWindowSize() (w, h int) {
	w, h = 1024, 768
	if v, err := strconv.Atoi(getSetting("win_w")); err == nil && v >= 640 {
		w = v
	}
	if v, err := strconv.Atoi(getSetting("win_h")); err == nil && v >= 480 {
		h = v
	}
	return w, h
}

func saveWindowSize(w, h int) {
	if w >= 640 && h >= 480 {
		setSetting("win_w", strconv.Itoa(w))
		setSetting("win_h", strconv.Itoa(h))
	}
}

// run sets up the DB and window. app.Main() does not return on most platforms;
// the process ends via os.Exit in the window goroutine, so cleanup (closeDB)
// happens there explicitly.
func run() error {
	if err := initDB(); err != nil {
		return err
	}

	theme := detectSystemTheme()
	width, height := savedWindowSize()

	go func() {
		w := new(app.Window)
		w.Option(
			app.Title("NASTY - NixOS Visual Control Center"),
			app.Size(unit.Dp(width), unit.Dp(height)),
		)

		exitCode := 0
		if err := loop(w, theme); err != nil {
			log.Printf("Gio window loop exited: %v", err)
			exitCode = 1
		}

		closeDB()
		os.Exit(exitCode)
	}()

	app.Main()
	return nil
}

func loop(w *app.Window, theme ThemeState) error {
	var ops op.Ops
	uiCtx := newUIContext(w, theme)

	// Follow light/dark changes while the app is running.
	go watchSystemTheme(uiCtx.SetDark)

	var lastW, lastH int
	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			saveWindowSize(lastW, lastH)
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			if gtx.Metric.PxPerDp > 0 {
				lastW = int(float32(e.Size.X) / gtx.Metric.PxPerDp)
				lastH = int(float32(e.Size.Y) / gtx.Metric.PxPerDp)
			}
			renderMainLayout(uiCtx, gtx)
			e.Frame(gtx.Ops)
		}
	}
}
