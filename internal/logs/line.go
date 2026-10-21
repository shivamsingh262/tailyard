// Package logs turns raw output into Lines: one entry each, with a level,
// styled and plain text, folded continuation lines and a correlation id.
package logs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/shivamsingh262/tailyard/internal/theme"
)

type Level int

const (
	LDebug Level = iota
	LInfo
	LWarn
	LError
)

type Line struct {
	Src    string
	Time   time.Time
	Level  Level
	Text   string // styled message
	Plain  string // unstyled message, used for filtering and copying
	Corr   string // correlation id (request, trace, run…)
	Detail []string
	Access bool     // a 2xx/3xx access-log line, hidden by the "hide OK requests" toggle
	Req    *Request // set for HTTP requests; rendered in columns instead of Text
	LLM    *LLMCall // set for LLM API calls; rendered in columns instead of Text
}

// Request holds an HTTP request's fields so rows can be laid out in columns
// at render time.
type Request struct {
	Method   string
	Path     string // without the query string
	Query    string
	Status   int
	Ms       int64 // -1 when unknown: access logs usually carry no timing
	Body     string
	BodyType string
}

func NewRequest(method, target string, status int) *Request {
	path, query, _ := strings.Cut(target, "?")
	return &Request{Method: method, Path: path, Query: query, Status: status, Ms: -1}
}

// MaxDetailLines caps the detail kept per entry.
const MaxDetailLines = 5000

// ProbeMarker is added to tailyard's own health-check URLs so their access
// lines can be dropped without hiding anyone else's calls to the same path.
const ProbeMarker = "tailyard=1"

var ReUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// System builds a line for tailyard's own output.
func System(level Level, s string) Line {
	return Line{Src: "tailyard", Time: time.Now(), Level: level, Plain: s, Text: StyleFor(level, s)}
}

func StyleFor(level Level, s string) string {
	switch level {
	case LError:
		return theme.Err.Render(s)
	case LWarn:
		return theme.Warn.Render(s)
	case LDebug:
		return theme.Dim.Render(s)
	}
	return s
}

func LevelFromName(name string) Level {
	switch strings.ToLower(name) {
	case "debug", "trace", "verbose":
		return LDebug
	case "warning", "warn":
		return LWarn
	case "error", "err", "critical", "crit", "exception", "fatal", "panic", "alert", "emergency":
		return LError
	}
	return LInfo
}

var (
	reErrorWord = regexp.MustCompile(`(?i)\b(error|exception|fatal|panic|failed)\b|\b\w+(Error|Exception):`)
	reWarnWord  = regexp.MustCompile(`(?i)\b(warn|warning|deprecated)\b`)
)

func GuessLevel(s string) Level {
	switch {
	case reErrorWord.MatchString(s):
		return LError
	case reWarnWord.MatchString(s):
		return LWarn
	}
	return LInfo
}

// CompactValue renders a decoded JSON value on one line, cut to max bytes.
func CompactValue(v any, max int) string {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case json.Number:
		s = t.String()
	case nil:
		s = "null"
	default:
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(t)
		s = strings.TrimSpace(buf.String())
	}
	s = strings.ReplaceAll(s, "\n", "⏎")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func HumanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

// PrettyBody splits a body into lines, indenting it first when it is JSON.
func PrettyBody(body string) []string {
	if body == "" {
		return []string{theme.Dim.Render("(empty)")}
	}
	var buf bytes.Buffer
	if json.Valid([]byte(body)) && json.Indent(&buf, []byte(body), "", "  ") == nil {
		body = buf.String()
	}
	return strings.Split(strings.TrimRight(body, "\n"), "\n")
}

func capDetail(detail []string) []string {
	if len(detail) > MaxDetailLines {
		return append(detail[:MaxDetailLines], theme.Dim.Render(fmt.Sprintf("… %d more lines", len(detail)-MaxDetailLines)))
	}
	return detail
}
