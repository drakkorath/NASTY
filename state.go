package main

import (
	"os"
	"regexp"
	"strings"
)

var pkgBlockRe = regexp.MustCompile(`(?s)with pkgs;\s*\[(.*?)\]`)

// parseSidecar reads packages and toggles back out of a generated sidecar.
func parseSidecar(text string) SystemState {
	var st SystemState

	if m := pkgBlockRe.FindStringSubmatch(text); m != nil {
		for _, f := range strings.Fields(m[1]) {
			if validPkgRegex.MatchString(f) {
				st.Packages = append(st.Packages, f)
			}
		}
	}

	st.Hardware.Bluetooth = sidecarBool(text, "hardware.bluetooth.enable")
	st.Hardware.Printing = sidecarBool(text, "services.printing.enable")
	st.Hardware.Avahi = sidecarBool(text, "services.avahi.enable")
	return st
}

// loadSidecarState reads the previously generated sidecar so the UI starts from
// what is actually configured. A missing file yields an empty state.
func loadSidecarState() SystemState {
	data, err := os.ReadFile(sidecarPath)
	if err != nil {
		return SystemState{}
	}
	return parseSidecar(string(data))
}

func sidecarBool(text, option string) bool {
	re := regexp.MustCompile(regexp.QuoteMeta(option) + `\s*=\s*(?:lib\.mk\w+\s+)?(true|false)`)
	m := re.FindStringSubmatch(text)
	return m != nil && m[1] == "true"
}
