package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shivamsingh262/tailyard/internal/config"
	"github.com/shivamsingh262/tailyard/internal/logs"
)

type sink struct {
	mu    sync.Mutex
	lines []logs.Line
}

func (s *sink) emit(l logs.Line) {
	s.mu.Lock()
	s.lines = append(s.lines, l)
	s.mu.Unlock()
}

func (s *sink) wait(t *testing.T, n int) []logs.Line {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		s.mu.Lock()
		got := append([]logs.Line(nil), s.lines...)
		s.mu.Unlock()
		if len(got) >= n {
			return got
		}
	}
	t.Fatalf("wanted %d lines, got %d", n, len(s.lines))
	return nil
}

func newTestProxy(t *testing.T, upstream http.HandlerFunc, mutate func(*config.Proxy)) (*httptest.Server, *sink) {
	t.Helper()
	backend := httptest.NewServer(upstream)
	t.Cleanup(backend.Close)
	cfg := &config.Proxy{Target: backend.URL, MaxBodyBytes: 1 << 20, Redact: []string{"password", "token"}}
	if mutate != nil {
		mutate(cfg)
	}
	s := &sink{}
	p, err := New(cfg, &logs.Correlator{Keys: []string{"request_id"}}, s.emit)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	return front, s
}

func TestRecordsAndRedacts(t *testing.T) {
	front, s := newTestProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept-Encoding"), "br") {
			t.Error("the client's Accept-Encoding should not reach the upstream")
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"echo":` + string(body) + `,"token":"abc"}`))
	}, nil)

	resp, err := http.Post(front.URL+"/api/login?request_id=r1&token=t", "application/json", strings.NewReader(`{"user":"a","password":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(got), `"password":"p"`) {
		t.Errorf("the client must get the unredacted response, got %s", got)
	}

	l := s.wait(t, 1)[0]
	if l.Src != "http" || l.Req.Status != 201 || l.Corr != "r1" {
		t.Errorf("line: %+v %+v", l, l.Req)
	}
	if strings.Contains(l.Req.Query, "token=t") || !strings.Contains(l.Req.Query, "redacted") {
		t.Errorf("query not redacted: %q", l.Req.Query)
	}
	detail := strings.Join(l.Detail, "\n")
	if strings.Contains(detail, `"p"`) || strings.Contains(detail, "abc") {
		t.Errorf("bodies not redacted:\n%s", detail)
	}
}

func TestSkipsStaticAssets(t *testing.T) {
	front, s := newTestProxy(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("x")) }, nil)
	for _, p := range []string{"/assets/app.js", "/@vite/client", "/logo.svg", "/api/items"} {
		resp, err := http.Get(front.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	got := s.wait(t, 1)
	time.Sleep(50 * time.Millisecond)
	if len(s.lines) != 1 || got[0].Req.Path != "/api/items" {
		t.Errorf("recorded %d lines, want only /api/items: %+v", len(s.lines), got)
	}
}

func TestRecordPrefixes(t *testing.T) {
	front, s := newTestProxy(t, func(w http.ResponseWriter, r *http.Request) {}, func(c *config.Proxy) { c.Record = []string{"/api/"} })
	for _, p := range []string{"/", "/api/x"} {
		resp, _ := http.Get(front.URL + p)
		resp.Body.Close()
	}
	got := s.wait(t, 1)
	time.Sleep(50 * time.Millisecond)
	if len(s.lines) != 1 || got[0].Req.Path != "/api/x" {
		t.Errorf("got %+v", got)
	}
}

func TestInjectsConsoleScript(t *testing.T) {
	front, s := newTestProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><html><head lang="en"><title>x</title></head></html>`))
	}, func(c *config.Proxy) { c.InjectConsole = true })

	resp, _ := http.Get(front.URL + "/")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `<head lang="en"><script src="/__tailyard/console.js"></script><title>`) {
		t.Errorf("script not injected: %s", body)
	}

	resp, _ = http.Get(front.URL + scriptPath)
	js, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(js), "__tailyard") {
		t.Error("console script not served")
	}

	resp, _ = http.Post(front.URL+consolePath, "application/json", strings.NewReader(`[{"level":"error","text":"boom","url":"/page"}]`))
	resp.Body.Close()
	var browser []logs.Line
	for _, l := range s.wait(t, 2) {
		if l.Src == "browser" {
			browser = append(browser, l)
		}
	}
	if len(browser) != 1 || browser[0].Plain != "boom" || browser[0].Level != logs.LError {
		t.Errorf("console entry: %+v", browser)
	}
}

func TestUpstreamDown(t *testing.T) {
	cfg := &config.Proxy{Target: "http://127.0.0.1:1", MaxBodyBytes: 1024}
	s := &sink{}
	p, _ := New(cfg, nil, s.emit)
	front := httptest.NewServer(p)
	defer front.Close()
	resp, err := http.Get(front.URL + "/api")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if l := s.wait(t, 1)[0]; l.Req.Status != 502 || l.Level != logs.LError {
		t.Errorf("line: %+v", l)
	}
}

func TestRedactForm(t *testing.T) {
	got := redactForm("user=a&password=hunter2&x", []string{"password"})
	if got != "user=a&password=%5Bredacted%5D&x" {
		t.Errorf("got %q", got)
	}
}
