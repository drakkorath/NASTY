package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type LogLevel int

const (
	LevelInfo LogLevel = iota
	LevelCmd
	LevelOK
	LevelWarn
	LevelError
)

// LogLine is one line in the log terminal.
type LogLine struct {
	Level LogLevel
	Text  string
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// cleanLine removes ANSI escape codes and keeps only the final segment of
// carriage-return progress lines.
func cleanLine(s string) string {
	if i := strings.LastIndex(s, "\r"); i >= 0 {
		if tail := s[i+1:]; strings.TrimSpace(tail) != "" {
			s = tail
		} else {
			s = s[:i]
		}
	}
	s = ansiRe.ReplaceAllString(s, "")
	return strings.TrimRight(s, " \t\r\n")
}

func parseLevel(s string) (LogLevel, bool) {
	switch s {
	case "info":
		return LevelInfo, true
	case "cmd":
		return LevelCmd, true
	case "ok":
		return LevelOK, true
	case "warn":
		return LevelWarn, true
	case "error":
		return LevelError, true
	}
	return LevelInfo, false
}

// classify guesses a level for raw nix/nixos-rebuild output.
func classify(text string) LogLevel {
	lower := strings.ToLower(text)
	switch {
	case strings.HasPrefix(text, "==>"):
		return LevelCmd
	case strings.Contains(lower, "error:"):
		return LevelError
	case strings.Contains(lower, "warning:"):
		return LevelWarn
	}
	return LevelInfo
}

// Progress extracts a human-friendly status from nix's plain-text output.
type Progress struct {
	ToBuild int
	Built   int
	Copied  int
	Current string
	Stage   string
}

var (
	reWillBuild = regexp.MustCompile(`these (\d+) derivations? will be built`)
	reBuilding  = regexp.MustCompile(`^building '([^']+)'`)
	reCopying   = regexp.MustCompile(`^copying path '`)
)

func (p *Progress) Feed(line string) {
	line = cleanLine(line)
	if m := reWillBuild.FindStringSubmatch(line); m != nil {
		p.ToBuild, _ = strconv.Atoi(m[1])
		return
	}
	if m := reBuilding.FindStringSubmatch(line); m != nil {
		p.Built++
		p.Current = drvName(m[1])
		return
	}
	if reCopying.MatchString(line) {
		p.Copied++
		return
	}
	for _, prefix := range []string{
		"activating the configuration",
		"setting up ",
		"reloading the following units",
		"restarting the following units",
		"starting the following units",
	} {
		if strings.HasPrefix(line, prefix) {
			p.Stage = "activating the new configuration"
			return
		}
	}
}

func drvName(path string) string {
	base := filepath.Base(path)
	if i := strings.Index(base, "-"); i > 0 {
		base = base[i+1:]
	}
	return strings.TrimSuffix(base, ".drv")
}

func (p Progress) Summary() string {
	switch {
	case p.Stage != "":
		return p.Stage
	case p.ToBuild > 0 && p.Current != "":
		return fmt.Sprintf("building %d of %d: %s", p.Built, p.ToBuild, p.Current)
	case p.Current != "":
		return fmt.Sprintf("building: %s (%d so far)", p.Current, p.Built)
	case p.Copied > 0:
		return fmt.Sprintf("downloaded %d store paths", p.Copied)
	}
	return ""
}
