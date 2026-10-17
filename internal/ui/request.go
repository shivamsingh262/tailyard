package ui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/shivamsingh262/tailyard/internal/logs"
	"github.com/shivamsingh262/tailyard/internal/theme"
)

// shortenIDs cuts UUIDs to their first 8 characters for display; filtering
// and the inspector keep the full values.
func shortenIDs(s string) string {
	return logs.ReUUID.ReplaceAllStringFunc(s, func(id string) string { return id[:8] + "…" })
}

// keyValues renders a query string or form body as decoded key=value pairs,
// in their original order.
func keyValues(encoded string) string {
	var pairs []string
	for _, pair := range strings.Split(encoded, "&") {
		k, v, _ := strings.Cut(pair, "=")
		pairs = append(pairs, theme.Dim.Render(logs.Unescape(k)+"=")+shortenIDs(logs.CompactValue(logs.Unescape(v), 120)))
	}
	return strings.Join(pairs, " ")
}

// renderRequest lays a request out as: method  status  time  path  params  body,
// padding the path to pathWidth so params line up across rows.
func renderRequest(r *logs.Request, pathWidth int) string {
	duration := strings.Repeat(" ", 6)
	if r.Ms >= 0 {
		duration = theme.Duration(r.Ms).Render(fmt.Sprintf("%6s", strconv.FormatInt(r.Ms, 10)+"ms"))
	}
	path := shortenIDs(r.Path)
	var b strings.Builder
	b.WriteString(theme.Method(r.Method).Render(fmt.Sprintf("%-6s", r.Method)))
	b.WriteString(theme.Status(r.Status).Render(strconv.Itoa(r.Status)))
	b.WriteString(" " + duration + "  " + path)
	if r.Query != "" || r.Body != "" {
		b.WriteString(strings.Repeat(" ", max(pathWidth-ansi.StringWidth(path), 0)) + "  ")
	}
	if r.Query != "" {
		b.WriteString(keyValues(r.Query))
	}
	if r.Body != "" {
		body := r.Body
		if strings.Contains(r.BodyType, "x-www-form-urlencoded") {
			body = keyValues(body)
		} else {
			body = shortenIDs(logs.CompactValue(body, 300))
		}
		if r.Query != "" {
			b.WriteString("  ")
		}
		b.WriteString(theme.Accent.Render("body ") + body)
	}
	return b.String()
}

// requestPathWidth is the display width of a request's path column.
func requestPathWidth(r *logs.Request) int {
	return ansi.StringWidth(shortenIDs(r.Path))
}
