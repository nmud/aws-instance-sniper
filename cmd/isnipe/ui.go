package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/mattn/go-colorable"
	"golang.org/x/term"
)

// out/errW route through go-colorable so ANSI escapes render on Windows
// consoles as well as Unix terminals.
var (
	out     = colorable.NewColorableStdout()
	errW    = colorable.NewColorableStderr()
	colorOn = term.IsTerminal(int(os.Stdout.Fd()))
)

func sgr(code, s string) string {
	if !colorOn {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func bold(s string) string { return sgr("1", s) }
func dim(s string) string  { return sgr("2", s) }
func red(s string) string  { return sgr("31", s) }
func grn(s string) string  { return sgr("32", s) }
func ylw(s string) string  { return sgr("33", s) }
func cyn(s string) string  { return sgr("36", s) }

func info(format string, a ...any) {
	fmt.Fprintf(out, "%s %s\n", cyn("›"), fmt.Sprintf(format, a...))
}
func ok(format string, a ...any) { fmt.Fprintf(out, "%s %s\n", grn("✔"), fmt.Sprintf(format, a...)) }
func warn(format string, a ...any) {
	fmt.Fprintf(out, "%s %s\n", ylw("⚠"), fmt.Sprintf(format, a...))
}
func errmsg(format string, a ...any) {
	fmt.Fprintf(errW, "%s %s\n", red("✘"), fmt.Sprintf(format, a...))
}
func die(format string, a ...any) { errmsg(format, a...); os.Exit(1) }

// selectMenu shows an arrow-key picker. Returns the chosen index, and ok=false
// if the user aborted (Esc / Ctrl-C / "quit").
func selectMenu(title string, options []string) (int, bool) {
	var choice int
	opts := make([]huh.Option[int], len(options))
	for i, o := range options {
		opts[i] = huh.NewOption(o, i)
	}
	f := huh.NewSelect[int]().Title(title).Options(opts...).Value(&choice)
	if err := f.Run(); err != nil {
		return -1, false
	}
	return choice, true
}

// askInput prompts for a line of text, returning def if left blank.
func askInput(prompt, def string) string {
	val := ""
	in := huh.NewInput().Title(prompt).Value(&val)
	if def != "" {
		in = in.Placeholder(def)
	}
	if err := in.Run(); err != nil {
		return def
	}
	if strings.TrimSpace(val) == "" {
		return def
	}
	return strings.TrimSpace(val)
}

// confirm asks a yes/no question. Returns (answer, ok=false if aborted).
func confirm(title, affirm, deny string) (bool, bool) {
	var v bool
	c := huh.NewConfirm().Title(title).Affirmative(affirm).Negative(deny).Value(&v)
	if err := c.Run(); err != nil {
		return false, false
	}
	return v, true
}
