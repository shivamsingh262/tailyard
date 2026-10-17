// Package theme holds every color and style. Colors are AdaptiveColors;
// SetLight picks the Light variants. Nothing may be rendered at package init,
// since that would resolve colors before the theme is set.
package theme

import (
	"github.com/charmbracelet/lipgloss"
)

func color(dark, light string) lipgloss.AdaptiveColor {
	return lipgloss.AdaptiveColor{Dark: dark, Light: light}
}

var (
	colDim      = color("243", "245")
	colFaint    = color("238", "252")
	colKey      = color("109", "24")
	colAccent   = color("81", "31")
	colOK       = color("71", "28")
	colWarn     = color("214", "130")
	colErr      = color("203", "160")
	colBrand    = color("205", "162")
	colBarBg    = color("236", "254")
	colBarFg    = color("252", "236")
	colTab      = color("246", "242")
	colSelectBg = color("237", "254")

	Dim     = lipgloss.NewStyle().Foreground(colDim)
	Key     = lipgloss.NewStyle().Foreground(colKey)
	Event   = lipgloss.NewStyle().Bold(true)
	Accent  = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
	OK      = lipgloss.NewStyle().Foreground(colOK)
	Warn    = lipgloss.NewStyle().Foreground(colWarn)
	WarnB   = Warn.Bold(true)
	Err     = lipgloss.NewStyle().Foreground(colErr)
	ErrB    = Err.Bold(true)
	Heading = lipgloss.NewStyle().Foreground(colKey).Bold(true)
	Brand   = lipgloss.NewStyle().Foreground(colBrand).Bold(true)
	Gutter  = lipgloss.NewStyle().Foreground(colBrand).Bold(true)
	Sep     = lipgloss.NewStyle().Foreground(colFaint)
	Bar     = lipgloss.NewStyle().Background(colBarBg).Foreground(colBarFg)
	Tab     = lipgloss.NewStyle().Foreground(colTab)
	TabOn   = lipgloss.NewStyle().Foreground(colBrand).Bold(true).Underline(true)
	HintKey = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
	Info    = lipgloss.NewStyle().Foreground(color("75", "26"))

	// Sources are colored by their position in the config, cycling through
	// this palette; built-in sources have fixed colors.
	sourcePalette = []lipgloss.AdaptiveColor{
		color("39", "25"), color("177", "91"), color("220", "136"), color("80", "30"),
		color("141", "55"), color("67", "24"), color("167", "124"), color("114", "28"),
	}
	builtinColors = map[string]lipgloss.AdaptiveColor{
		"http":     color("80", "30"),
		"browser":  color("214", "130"),
		"events":   color("43", "29"),
		"monitor":  color("244", "244"),
		"tailyard": color("250", "240"),
	}
	sourceIndex = map[string]int{}

	methodColors = map[string]lipgloss.AdaptiveColor{
		"GET":    color("75", "26"),
		"POST":   color("78", "28"),
		"PUT":    color("220", "136"),
		"PATCH":  color("220", "136"),
		"DELETE": color("203", "160"),
	}
)

func init() {
	lipgloss.SetHasDarkBackground(true)
}

func SetLight(light bool) {
	lipgloss.SetHasDarkBackground(!light)
}

// RegisterSources fixes each configured source's color by its position.
func RegisterSources(names []string) {
	for i, n := range names {
		sourceIndex[n] = i
	}
}

func Source(name string) lipgloss.Style {
	c, ok := builtinColors[name]
	if !ok {
		c = sourcePalette[sourceIndex[name]%len(sourcePalette)]
	}
	return lipgloss.NewStyle().Foreground(c)
}

func Method(method string) lipgloss.Style {
	c, ok := methodColors[method]
	if !ok {
		c = color("250", "240")
	}
	return lipgloss.NewStyle().Foreground(c).Bold(true)
}

// Status colors an HTTP status code.
func Status(code int) lipgloss.Style {
	switch {
	case code >= 500:
		return ErrB
	case code >= 400:
		return WarnB
	case code >= 300:
		return Dim
	}
	return OK
}

// Duration colors a request time: gray when fast, yellow from 100ms, red
// from a second.
func Duration(ms int64) lipgloss.Style {
	switch {
	case ms >= 1000:
		return ErrB
	case ms >= 100:
		return Warn
	}
	return Dim
}

// SelectBgSeq is the raw SGR sequence for the selection background, so it can
// be re-applied after every reset inside an already-styled row.
func SelectBgSeq() string {
	if lipgloss.HasDarkBackground() {
		return "\x1b[48;5;" + colSelectBg.Dark + "m"
	}
	return "\x1b[48;5;" + colSelectBg.Light + "m"
}
