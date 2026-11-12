package main

import (
	"image/color"
	"log"

	"gioui.org/widget/material"
	"github.com/godbus/dbus/v5"
)

// ThemeState holds the desktop's current color scheme.
type ThemeState struct {
	IsDark bool
}

const (
	portalDest   = "org.freedesktop.portal.Desktop"
	portalPath   = "/org/freedesktop/portal/desktop"
	portalSettig = "org.freedesktop.portal.Settings"
)

// detectSystemTheme reads org.freedesktop.appearance color-scheme once.
func detectSystemTheme() ThemeState {
	state := ThemeState{IsDark: false}

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		log.Printf("Warning: Failed to connect to session bus: %v", err)
		return state
	}
	defer conn.Close()

	var variant dbus.Variant
	err = conn.Object(portalDest, portalPath).
		Call(portalSettig+".Read", 0, "org.freedesktop.appearance", "color-scheme").
		Store(&variant)
	if err != nil {
		log.Printf("Warning: Could not read color-scheme from DBus: %v", err)
		return state
	}

	// Settings.Read returns a variant wrapped in a variant; unwrap it.
	v := variant.Value()
	if inner, ok := v.(dbus.Variant); ok {
		v = inner.Value()
	}
	// 0: no preference, 1: prefer dark, 2: prefer light
	if val, ok := v.(uint32); ok {
		state.IsDark = val == 1
	}
	return state
}

// watchSystemTheme calls onChange whenever the desktop switches between light
// and dark. It blocks, so run it in a goroutine.
func watchSystemTheme(onChange func(dark bool)) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		log.Printf("Theme watcher disabled: %v", err)
		return
	}
	// The connection stays open for the life of the app.

	err = conn.AddMatchSignal(
		dbus.WithMatchObjectPath(portalPath),
		dbus.WithMatchInterface(portalSettig),
		dbus.WithMatchMember("SettingChanged"),
	)
	if err != nil {
		log.Printf("Theme watcher disabled: %v", err)
		return
	}

	signals := make(chan *dbus.Signal, 8)
	conn.Signal(signals)

	for sig := range signals {
		if sig.Name != portalSettig+".SettingChanged" || len(sig.Body) < 3 {
			continue
		}
		ns, _ := sig.Body[0].(string)
		key, _ := sig.Body[1].(string)
		if ns != "org.freedesktop.appearance" || key != "color-scheme" {
			continue
		}
		v, ok := sig.Body[2].(dbus.Variant)
		if !ok {
			continue
		}
		if val, ok := v.Value().(uint32); ok {
			onChange(val == 1)
		}
	}
}

// applyPalette switches the Material theme between light and dark colors.
func applyPalette(th *material.Theme, dark bool) {
	if dark {
		th.Bg = color.NRGBA{R: 30, G: 30, B: 30, A: 255}
		th.Fg = color.NRGBA{R: 220, G: 220, B: 220, A: 255}
		th.ContrastBg = color.NRGBA{R: 45, G: 110, B: 190, A: 255}
		th.ContrastFg = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
		return
	}
	th.Bg = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	th.Fg = color.NRGBA{R: 0, G: 0, B: 0, A: 255}
	th.ContrastBg = color.NRGBA{R: 0x3f, G: 0x51, B: 0xb5, A: 255}
	th.ContrastFg = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
}
