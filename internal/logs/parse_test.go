package logs

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// parseAll feeds lines through a parser and returns the entries it emits.
func parseAll(format string, corr *Correlator, lines ...string) []Line {
	out := make(chan Line, 100)
	p := &Parser{Src: "test", Format: format, Corr: corr, Out: out}
	for _, s := range lines {
		p.Handle(Raw{Time: time.Now(), Text: s})
	}
	p.flush()
	close(out)
	var got []Line
	for l := range out {
		got = append(got, l)
	}
	return got
}

func TestPythonTracebackFolds(t *testing.T) {
	got := parseAll("auto", nil,
		"ERROR:    Exception in ASGI application",
		"Traceback (most recent call last):",
		`  File "app.py", line 3, in handler`,
		"    raise ValueError('bad')",
		"ValueError: bad",
		"INFO:     next line",
	)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	if got[0].Level != LError || len(got[0].Detail) != 4 || got[0].Detail[3] != "ValueError: bad" {
		t.Errorf("traceback not folded into the error: %+v", got[0])
	}
}

func TestBareTracebackTakesExceptionAsMessage(t *testing.T) {
	got := parseAll("python", nil,
		"Traceback (most recent call last):",
		`  File "w.py", line 1`,
		"KeyError: 'x'",
	)
	if len(got) != 1 || got[0].Plain != "KeyError: 'x'" {
		t.Fatalf("got %+v", got)
	}
}

func TestJSONLogs(t *testing.T) {
	corr := &Correlator{Keys: []string{"request_id"}}
	got := parseAll("auto", corr,
		`{"level":"error","msg":"db down","request_id":"r-1","time":"2026-01-01T00:00:00Z","stack":"a\nb","attempt":3}`,
		`{"level":30,"msg":"pino info"}`,
		`{"level":50,"msg":"pino error"}`,
	)
	if len(got) != 3 {
		t.Fatalf("got %d entries", len(got))
	}
	l := got[0]
	if l.Level != LError || l.Corr != "r-1" || l.Plain != "db down attempt=3 request_id=r-1" || len(l.Detail) != 2 {
		t.Errorf("zap-style line parsed wrong: %+v", l)
	}
	if got[1].Level != LInfo || got[2].Level != LError {
		t.Errorf("pino levels wrong: %v %v", got[1].Level, got[2].Level)
	}
}

func TestAccessLines(t *testing.T) {
	corr := &Correlator{Keys: []string{"job"}, Pattern: regexp.MustCompile(`runs/(\w+)`)}
	got := parseAll("auto", corr,
		`INFO:     172.18.0.1:5555 - "GET /runs/abc?x=1 HTTP/1.1" 200 OK`,
		`127.0.0.1 - - [10/Oct/2026:13:55:36 +0000] "POST /jobs?job=j9 HTTP/1.1" 502 12 "-" "curl"`,
		`INFO:     1.2.3.4:1 - "GET /health?tailyard=1 HTTP/1.1" 200 OK`,
	)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 (the probe is dropped): %+v", len(got), got)
	}
	if r := got[0].Req; r == nil || r.Path != "/runs/abc" || r.Query != "x=1" || !got[0].Access || got[0].Corr != "abc" {
		t.Errorf("uvicorn line: %+v %+v", got[0], got[0].Req)
	}
	if got[1].Level != LError || got[1].Access || got[1].Corr != "j9" {
		t.Errorf("nginx line: %+v", got[1])
	}
}

func TestNodeStackFolds(t *testing.T) {
	got := parseAll("auto", nil,
		"  VITE v5 ready in 300 ms",
		"  ➜  Local:   http://localhost:5173/",
		"TypeError: x is undefined",
		"    at foo (/app/a.js:1:2)",
		"    at bar (/app/b.js:3:4)",
	)
	// The banner's indented lines stay separate entries; the stack folds.
	if len(got) != 3 {
		t.Fatalf("got %d entries: %+v", len(got), got)
	}
	if last := got[2]; last.Level != LError || len(last.Detail) != 2 {
		t.Errorf("stack not folded: %+v", last)
	}
}

func TestPostgresAndRedis(t *testing.T) {
	pg := parseAll("postgres", nil, `2026-01-01 10:00:00.123 UTC [42] ERROR:  relation "x" does not exist`)
	if pg[0].Level != LError || !strings.HasPrefix(pg[0].Plain, "ERROR:") {
		t.Errorf("postgres: %+v", pg[0])
	}
	rd := parseAll("redis", nil, `1:M 01 Jan 2026 10:00:00.000 # WARNING overcommit_memory is set to 0`)
	if rd[0].Level != LWarn || !strings.HasPrefix(rd[0].Plain, "WARNING") {
		t.Errorf("redis: %+v", rd[0])
	}
}

func TestHTTPLine(t *testing.T) {
	l := HTTPLine(HTTPEntry{Method: "POST", URL: "/api/login?next=%2F", Status: 401, Ms: 12,
		ReqType: "application/json", ReqBody: `{"user":"a"}`, ResType: "application/json", ResBody: `{"error":"no"}`}, nil)
	if l.Src != "http" || l.Level != LWarn || l.Req.Ms != 12 || l.Req.Query != "next=%2F" {
		t.Errorf("got %+v %+v", l, l.Req)
	}
	if len(l.Detail) < 4 {
		t.Errorf("detail too short: %q", l.Detail)
	}
}

func TestOnURL(t *testing.T) {
	var url string
	p := &Parser{Src: "web", Format: "auto", Out: make(chan Line, 4), OnURL: func(u string) { url = u }}
	p.Handle(Raw{Time: time.Now(), Text: "  ➜  Local:   http://localhost:5173/"})
	if url != "http://localhost:5173/" {
		t.Errorf("url = %q", url)
	}
}
