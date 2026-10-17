package logs

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/shivamsingh262/tailyard/internal/theme"
)

// Raw is one line read from a process or file, with the time it was logged
// when the reader knows it (docker's --timestamps), else when it was read.
type Raw struct {
	Time time.Time
	Text string
}

var (
	// An HTTP access line in any of the usual formats (uvicorn, gunicorn,
	// nginx/Apache combined, Rails, Express "combined"): "GET /x HTTP/1.1" 200
	reAccess    = regexp.MustCompile(`"(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS) (\S+) HTTP/[\d.]+" (\d{3})\b`)
	reLevelPfx  = regexp.MustCompile(`^(DEBUG|INFO|WARNING|WARN|ERROR|CRITICAL|FATAL):\s+(.*)$`)
	rePyWarning = regexp.MustCompile(`^(\S+\.py:\d+): (\w*Warning): (.*)$`)
	rePgPrefix  = regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d(\.\d+)? \w+ \[\d+\] `)
	reRedisLog  = regexp.MustCompile(`^\d+:[A-Z] \d+ \w+ \d+ [\d:.]+ ([.*#-]) `)
	reLocalURL  = regexp.MustCompile(`Local:\s+(https?://\S+)`)
	reNodeStack = regexp.MustCompile(`^\s+at \S`)
)

// Parser turns raw lines from one source into Lines, folding continuation
// lines (indented lines, Python tracebacks, JS stack frames) into the
// previous entry's Detail.
type Parser struct {
	Src    string
	Format string // one of config.Parsers
	Corr   *Correlator
	Out    chan<- Line
	OnURL  func(string) // called with a dev server's "Local: http://…" URL

	pending   *Line
	traceback bool
}

// Run consumes raw lines until in is closed. A pending entry is flushed once
// no continuation line arrives for a short while, so the last line of a
// burst still shows up promptly.
func (p *Parser) Run(in <-chan Raw) {
	for {
		var timeout <-chan time.Time
		if p.pending != nil {
			timeout = time.After(150 * time.Millisecond)
		}
		select {
		case r, ok := <-in:
			if !ok {
				p.flush()
				return
			}
			p.Handle(r)
		case <-timeout:
			p.flush()
		}
	}
}

func (p *Parser) flush() {
	if p.pending != nil {
		p.pending.Detail = capDetail(p.pending.Detail)
		p.Out <- *p.pending
	}
	p.pending = nil
	p.traceback = false
}

func (p *Parser) Handle(r Raw) {
	s := strings.TrimRight(ansi.Strip(r.Text), "\r ")
	if strings.TrimSpace(s) == "" {
		return
	}
	indented := s[0] == ' ' || s[0] == '\t'
	if p.OnURL != nil {
		if m := reLocalURL.FindStringSubmatch(s); m != nil {
			p.OnURL(m[1])
		}
	}

	if p.pending != nil && p.traceback {
		p.pending.Detail = append(p.pending.Detail, s)
		if !indented && !isTracebackHeader(s) {
			// The exception line ends a traceback: "ValueError: bad input".
			if p.pending.Plain == "Traceback" {
				p.pending.Plain = s
				p.pending.Text = theme.ErrB.Render("✖ ") + theme.Err.Render(s)
			}
			p.flush()
		}
		return
	}
	if p.pending != nil && strings.HasPrefix(s, "Traceback (most recent call last)") && p.pending.Level >= LError {
		p.pending.Detail = append(p.pending.Detail, s)
		p.traceback = true
		return
	}
	if p.pending != nil && indented && p.continues(s) {
		p.pending.Detail = append(p.pending.Detail, s)
		return
	}
	p.flush()

	if strings.HasPrefix(s, "Traceback (most recent call last)") {
		p.pending = &Line{Src: p.Src, Time: r.Time, Level: LError, Plain: "Traceback", Text: theme.ErrB.Render("✖ Traceback"), Detail: []string{s}}
		p.traceback = true
		return
	}
	l, ok := p.parse(s)
	if !ok {
		return
	}
	l.Src, l.Time = p.Src, r.Time
	if l.Corr == "" {
		l.Corr = p.Corr.FromText(l.Plain)
	}
	p.pending = &l
}

// continues reports whether an indented line belongs to the pending entry.
// Structured loggers indent only continuations; dev servers also indent
// banners, so for them only stack frames and warnings/errors take one.
func (p *Parser) continues(s string) bool {
	switch p.Format {
	case "json", "python", "postgres":
		return true
	}
	return p.pending.Level >= LWarn || reNodeStack.MatchString(s)
}

func (p *Parser) parse(s string) (Line, bool) {
	switch p.Format {
	case "json":
		if l, ok := p.parseJSON(s); ok {
			return l, true
		}
		return plainLine(s), true
	case "postgres":
		return parsePostgres(s), true
	case "redis":
		return parseRedisLog(s), true
	case "plain":
		return plainLine(s), true
	}
	// auto and python: JSON lines, then access lines, then level prefixes.
	if strings.HasPrefix(s, "{") {
		if l, ok := p.parseJSON(s); ok {
			return l, true
		}
	}
	if m := reAccess.FindStringSubmatch(s); m != nil {
		if strings.Contains(m[2], ProbeMarker) {
			return Line{}, false
		}
		return AccessLine(m[1], m[2], m[3], p.Corr), true
	}
	if m := reLevelPfx.FindStringSubmatch(s); m != nil {
		l := Line{Level: LevelFromName(m[1]), Plain: m[2]}
		l.Text = StyleFor(l.Level, m[2])
		if strings.Contains(m[2], "Reloading") || strings.Contains(m[2], "detected changes") {
			l.Text = theme.Accent.Render("↻ " + m[2])
		}
		return l, true
	}
	if m := rePyWarning.FindStringSubmatch(s); m != nil {
		text := m[2] + ": " + m[3]
		return Line{Level: LWarn, Plain: text, Text: theme.Warn.Render(m[2]+": ") + theme.Dim.Render(m[3]), Detail: []string{"  " + m[1]}}, true
	}
	return plainLine(strings.TrimSpace(s)), true
}

func isTracebackHeader(s string) bool {
	return strings.HasPrefix(s, "Traceback") ||
		strings.HasPrefix(s, "During handling") ||
		strings.HasPrefix(s, "The above exception")
}

func plainLine(s string) Line {
	l := Line{Level: GuessLevel(s), Plain: s}
	l.Text = StyleFor(l.Level, s)
	return l
}

// AccessLine builds a request row from an access-log line.
func AccessLine(method, target, status string, corr *Correlator) Line {
	code, _ := strconv.Atoi(status)
	l := Line{Level: LInfo, Plain: method + " " + target + " " + status, Access: code < 400, Req: NewRequest(method, target, code)}
	switch {
	case code >= 500:
		l.Level = LError
	case code >= 400:
		l.Level = LWarn
	case l.Req.Path == "/health" || l.Req.Path == "/healthz" || l.Req.Path == "/ready":
		// Health checks are noise; keep them, dimmed.
		l.Level = LDebug
	}
	l.Corr = corr.FromTarget(target)
	return l
}

// Field names structured loggers commonly use (structlog, zap, zerolog,
// pino, bunyan, logrus, slog, winston).
var (
	messageKeys = []string{"event", "msg", "message"}
	levelKeys   = []string{"level", "severity", "lvl", "log.level"}
	timeKeys    = []string{"timestamp", "time", "ts", "@timestamp", "t"}
	stackKeys   = []string{"exception", "stack", "stacktrace", "stack_trace", "error.stack", "exc_info"}
)

func (p *Parser) parseJSON(s string) (Line, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		return Line{}, false
	}
	skip := map[string]bool{}
	take := func(keys []string) any {
		for _, k := range keys {
			if v, ok := fields[k]; ok {
				skip[k] = true
				return v
			}
		}
		return nil
	}
	msg, _ := take(messageKeys).(string)
	l := Line{Level: LInfo}
	switch lv := take(levelKeys).(type) {
	case string:
		l.Level = LevelFromName(lv)
	case json.Number: // pino / bunyan numeric levels
		if n, err := lv.Int64(); err == nil {
			switch {
			case n >= 50:
				l.Level = LError
			case n >= 40:
				l.Level = LWarn
			case n <= 20:
				l.Level = LDebug
			}
		}
	}
	take(timeKeys)
	if stack, ok := take(stackKeys).(string); ok && stack != "" {
		l.Detail = strings.Split(strings.TrimRight(stack, "\n"), "\n")
	}
	l.Corr = p.Corr.FromFields(fields)

	keys := make([]string, 0, len(fields))
	for k := range fields {
		if !skip[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var plain, styled strings.Builder
	plain.WriteString(msg)
	styled.WriteString(StyleFor(l.Level, theme.Event.Render(msg)))
	for _, k := range keys {
		v := CompactValue(fields[k], 160)
		plain.WriteString(" " + k + "=" + v)
		styled.WriteString("  " + theme.Key.Render(k+"=") + v)
	}
	l.Plain, l.Text = strings.TrimSpace(plain.String()), styled.String()
	return l, true
}

func parsePostgres(s string) Line {
	s = rePgPrefix.ReplaceAllString(s, "")
	l := Line{Level: LInfo, Plain: s}
	switch {
	case strings.HasPrefix(s, "ERROR:"), strings.HasPrefix(s, "FATAL:"), strings.HasPrefix(s, "PANIC:"):
		l.Level = LError
	case strings.HasPrefix(s, "WARNING:"):
		l.Level = LWarn
	case strings.HasPrefix(s, "LOG:"):
		l.Level = LDebug
	}
	l.Text = StyleFor(l.Level, s)
	return l
}

func parseRedisLog(s string) Line {
	l := Line{Level: LInfo}
	if m := reRedisLog.FindStringSubmatch(s); m != nil {
		if m[1] == "#" {
			l.Level = LWarn
		}
		s = s[len(m[0]):]
	}
	l.Plain = s
	l.Text = StyleFor(l.Level, s)
	return l
}
