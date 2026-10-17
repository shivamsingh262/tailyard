package logs

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shivamsingh262/tailyard/internal/theme"
)

// HTTPEntry is one request/response pair recorded by the proxy.
type HTTPEntry struct {
	Time      time.Time
	Method    string
	URL       string // path and query
	Status    int
	Ms        int64
	ReqType   string
	ReqBody   string
	ResType   string
	ResBody   string
	Size      int64
	Truncated bool
	Note      string // e.g. "event stream, not buffered"
}

// HTTPLine renders a recorded request. The bodies go into Detail,
// pretty-printed when they are JSON.
func HTTPLine(e HTTPEntry, corr *Correlator) Line {
	l := AccessLine(e.Method, e.URL, strconv.Itoa(e.Status), corr)
	l.Src, l.Time, l.Access = "http", e.Time, false
	l.Req.Ms, l.Req.Body, l.Req.BodyType = e.Ms, e.ReqBody, e.ReqType
	if l.Corr == "" {
		l.Corr = corr.FromText(e.ReqBody)
	}
	if e.ReqBody != "" {
		l.Plain += " " + e.ReqBody
	}
	l.Plain += fmt.Sprintf(" %dms", e.Ms)

	if e.ReqBody != "" {
		l.Detail = append(l.Detail, theme.Heading.Render("▸ request")+"  "+theme.Dim.Render(e.ReqType))
		if strings.Contains(e.ReqType, "x-www-form-urlencoded") {
			for _, pair := range strings.Split(e.ReqBody, "&") {
				k, v, _ := strings.Cut(pair, "=")
				l.Detail = append(l.Detail, theme.Key.Render(Unescape(k))+" = "+Unescape(v))
			}
		} else {
			l.Detail = append(l.Detail, PrettyBody(e.ReqBody)...)
		}
	}
	meta := e.ResType + " · " + HumanBytes(e.Size)
	if e.Truncated {
		meta += " · body truncated"
	}
	l.Detail = append(l.Detail, theme.Heading.Render(fmt.Sprintf("▸ response %d", e.Status))+"  "+theme.Dim.Render(meta))
	if e.Note != "" {
		l.Detail = append(l.Detail, theme.Dim.Render("("+e.Note+")"))
	} else {
		l.Detail = append(l.Detail, PrettyBody(e.ResBody)...)
	}
	l.Detail = capDetail(l.Detail)
	return l
}

// ConsoleEntry is one browser console message sent by the injected script.
type ConsoleEntry struct {
	Level string `json:"level"`
	Text  string `json:"text"`
	Stack string `json:"stack"`
	URL   string `json:"url"`
}

func ConsoleLine(e ConsoleEntry, corr *Correlator) Line {
	l := Line{Src: "browser", Time: time.Now(), Level: LInfo}
	switch e.Level {
	case "error":
		l.Level = LError
	case "warn":
		l.Level = LWarn
	case "debug":
		l.Level = LDebug
	}
	lines := strings.Split(strings.TrimRight(e.Text, "\n"), "\n")
	l.Plain = lines[0]
	l.Text = StyleFor(l.Level, lines[0]) + "  " + theme.Dim.Render(e.URL)
	l.Detail = lines[1:]
	if e.Stack != "" && !strings.Contains(e.Text, e.Stack) {
		l.Detail = append(l.Detail, strings.Split(strings.TrimRight(e.Stack, "\n"), "\n")...)
	}
	l.Detail = capDetail(l.Detail)
	if l.Corr = corr.FromTarget(e.URL); l.Corr == "" {
		l.Corr = corr.FromText(e.Text)
	}
	return l
}

// Unescape decodes one query-string component, leaving it as-is if malformed.
func Unescape(s string) string {
	if decoded, err := url.QueryUnescape(s); err == nil {
		return decoded
	}
	return s
}
