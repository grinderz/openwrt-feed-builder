package feedbuilder

// Terminal output speaks the visual language a package manager taught
// everyone's eyes (yay/pacman):
//
//	::  bold cyan — a phase or a section header
//	==> bold green — one source inside a batch
//	WARNING: / ERROR: — yellow and red, with the colon, at the start
//	dim — hints and side notes; plain indented text for details
//
// Colour is decided once per process (flags, then the terminal).

import (
	"fmt"
	"os"
	"strings"
)

// ANSI codes used for the little colour this tool needs.
const (
	codeBold   = "1"
	codeDim    = "2"
	codeRed    = "31"
	codeGreen  = "32"
	codeYellow = "33"
	codeCyan   = "36"
)

var colorEnabled bool //nolint:gochecknoglobals // one terminal per process

// SetColor fixes whether output is coloured.
func SetColor(on bool) { colorEnabled = on }

// AutoColor is the default: colour when a terminal is attached and nobody
// opted out through the usual environment variables.
func AutoColor() bool {
	if os.Getenv("NO_COLOR") != "" || strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}

	fi, err := os.Stdout.Stat()

	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func paint(code, text string) string {
	if !colorEnabled || text == "" {
		return text
	}

	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

// Bold, Dim, Red, Green, Yellow and Cyan wrap text in one ANSI style.
func Bold(text string) string   { return paint(codeBold, text) }
func Dim(text string) string    { return paint(codeDim, text) }
func Red(text string) string    { return paint(codeRed, text) }
func Green(text string) string  { return paint(codeGreen, text) }
func Yellow(text string) string { return paint(codeYellow, text) }
func Cyan(text string) string   { return paint(codeCyan, text) }

// Fail is the error label.
func Fail() string { return Red("ERROR:") }

func warnLabel() string { return Yellow("WARNING:") }

// phasef prints a section header: ":: title".
func phasef(format string, args ...any) {
	fmt.Println(Bold(Cyan("::")) + " " + Bold(fmt.Sprintf(format, args...)))
}

// sourceLinef prints one source's status: "==> name: message".
func sourceLinef(name, format string, args ...any) {
	fmt.Println(Bold(Green("==>")) + " " + Bold(name) + ": " + fmt.Sprintf(format, args...))
}

// warnf prints a warning to stderr.
func warnf(format string, args ...any) {
	fmt.Fprintln(os.Stderr, warnLabel()+" "+fmt.Sprintf(format, args...))
}

// problemf prints an indented per-item problem to stderr ("  ! ...").
func problemf(format string, args ...any) {
	fmt.Fprintln(os.Stderr, "  "+Yellow("!")+" "+fmt.Sprintf(format, args...))
}
