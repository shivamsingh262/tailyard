// Package proxy is a reverse proxy that records every request and response
// passing through it, and can inject a script into HTML pages that forwards
// the browser console.
package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/shivamsingh262/tailyard/internal/config"
	"github.com/shivamsingh262/tailyard/internal/logs"
)

const (
	scriptPath  = "/__tailyard/console.js"
	consolePath = "/__tailyard/console"
)

//go:embed console.js
var consoleJS []byte

type Proxy struct {
	cfg    *config.Proxy
	corr   *logs.Correlator
	emit   func(logs.Line)
	target *url.URL
	rp     *httputil.ReverseProxy
	srv    *http.Server
}

func New(cfg *config.Proxy, corr *logs.Correlator, emit func(logs.Line)) (*Proxy, error) {
	target, err := url.Parse(cfg.Target)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("proxy target %q is not an absolute URL", cfg.Target)
	}
	p := &Proxy{cfg: cfg, corr: corr, emit: emit, target: target}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.SetXForwarded()
			// Without the client's Accept-Encoding, Go's transport asks for
			// gzip itself and decompresses transparently, so bodies arrive
			// plain for recording and script injection.
			r.Out.Header.Del("Accept-Encoding")
		},
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.errorHandler,
		ErrorLog:       log.New(io.Discard, "", 0), // the TUI owns the terminal
	}
	return p, nil
}

// Start listens and serves in the background.
func (p *Proxy) Start() error {
	ln, err := net.Listen("tcp", p.cfg.Listen)
	if err != nil {
		return err
	}
	p.srv = &http.Server{Handler: p, ErrorLog: log.New(io.Discard, "", 0), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = p.srv.Serve(ln) }()
	return nil
}

func (p *Proxy) Close() {
	if p.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = p.srv.Shutdown(ctx)
	}
}

// URL is where the proxy can be reached.
func (p *Proxy) URL() string {
	host := p.cfg.Listen
	if strings.HasPrefix(host, ":") {
		host = "localhost" + host
	}
	return "http://" + host
}

type ctxKey struct{}

// capture is one recorded exchange, filled in as it passes through.
type capture struct {
	start   time.Time
	method  string
	url     string
	reqType string
	reqBody *cappedBuffer
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.cfg.InjectConsole {
		switch r.URL.Path {
		case scriptPath:
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(consoleJS)
			return
		case consolePath:
			p.handleConsole(w, r)
			return
		}
	}
	if !p.shouldRecord(r) {
		p.rp.ServeHTTP(w, r)
		return
	}
	c := &capture{start: time.Now(), method: r.Method, url: r.URL.RequestURI(), reqType: r.Header.Get("Content-Type"), reqBody: &cappedBuffer{max: p.cfg.MaxBodyBytes}}
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = &teeBody{rc: r.Body, buf: c.reqBody}
	}
	p.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, c)))
}

// staticPrefixes and staticExts are skipped when no record prefixes are
// configured: dev-server internals and assets would drown out API calls.
var (
	staticPrefixes = []string{"/@vite/", "/@fs/", "/@id/", "/@react-refresh", "/node_modules/", "/__vite", "/_next/static/", "/_next/webpack-hmr", "/sockjs-node", "/src/"}
	staticExts     = map[string]bool{".js": true, ".mjs": true, ".cjs": true, ".ts": true, ".tsx": true, ".jsx": true, ".vue": true, ".svelte": true, ".css": true, ".scss": true, ".map": true, ".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true, ".ico": true, ".webp": true, ".avif": true, ".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".mp4": true, ".webm": true, ".mp3": true, ".wasm": true}
)

func (p *Proxy) shouldRecord(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" {
		return false
	}
	if len(p.cfg.Record) > 0 {
		for _, prefix := range p.cfg.Record {
			if strings.HasPrefix(r.URL.Path, prefix) {
				return true
			}
		}
		return false
	}
	for _, prefix := range staticPrefixes {
		if strings.HasPrefix(r.URL.Path, prefix) {
			return false
		}
	}
	return !staticExts[strings.ToLower(path.Ext(r.URL.Path))]
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return nil
	}
	if p.cfg.InjectConsole && resp.Request.Method == http.MethodGet && resp.StatusCode == http.StatusOK && isHTML(resp.Header) {
		if err := injectScript(resp); err != nil {
			return err
		}
	}
	c, _ := resp.Request.Context().Value(ctxKey{}).(*capture)
	if c == nil {
		return nil
	}
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		p.record(c, resp, nil, "event stream, not buffered")
		return nil
	}
	res := &cappedBuffer{max: p.cfg.MaxBodyBytes}
	var once sync.Once
	resp.Body = &teeBody{rc: resp.Body, buf: res, done: func() {
		once.Do(func() { p.record(c, resp, res, "") })
	}}
	return nil
}

func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusBadGateway
	if errors.Is(err, context.Canceled) {
		status = 499 // the client went away; nothing to write
	} else {
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, "tailyard proxy: %s unreachable: %v\n", p.cfg.Target, err)
	}
	if c, _ := r.Context().Value(ctxKey{}).(*capture); c != nil {
		e := p.entry(c)
		e.Status, e.Note = status, err.Error()
		p.emit(logs.HTTPLine(e, p.corr))
	}
}

func (p *Proxy) entry(c *capture) logs.HTTPEntry {
	return logs.HTTPEntry{
		Time:    c.start,
		Method:  c.method,
		URL:     redactQuery(c.url, p.cfg.Redact),
		Ms:      time.Since(c.start).Milliseconds(),
		ReqType: c.reqType,
		ReqBody: displayBody(c.reqBody, c.reqType, "", p.cfg.Redact),
	}
}

func (p *Proxy) record(c *capture, resp *http.Response, res *cappedBuffer, note string) {
	e := p.entry(c)
	e.Status, e.ResType, e.Note = resp.StatusCode, resp.Header.Get("Content-Type"), note
	if res != nil {
		_, e.Size = res.snapshot()
		e.Truncated = res.truncated()
		e.ResBody = displayBody(res, e.ResType, resp.Header.Get("Content-Encoding"), p.cfg.Redact)
	}
	p.emit(logs.HTTPLine(e, p.corr))
}

func (p *Proxy) handleConsole(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var entries []logs.ConsoleEntry
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&entries); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, e := range entries {
		p.emit(logs.ConsoleLine(e, p.corr))
	}
	w.WriteHeader(http.StatusNoContent)
}

func isHTML(h http.Header) bool {
	return strings.HasPrefix(h.Get("Content-Type"), "text/html") && h.Get("Content-Encoding") == ""
}

// injectScript adds the console script at the top of <head>, so it is in
// place before the page's own scripts run.
func injectScript(resp *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	resp.Body.Close()
	if err != nil {
		return err
	}
	tag := []byte(`<script src="` + scriptPath + `"></script>`)
	at := 0
	if i := bytes.Index(bytes.ToLower(body), []byte("<head")); i >= 0 {
		if j := bytes.IndexByte(body[i:], '>'); j >= 0 {
			at = i + j + 1
		}
	}
	out := make([]byte, 0, len(body)+len(tag))
	out = append(append(append(out, body[:at]...), tag...), body[at:]...)
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	return nil
}

// displayBody turns a captured body into text for the TUI: decoded,
// redacted, or a note when it isn't text.
func displayBody(b *cappedBuffer, contentType, encoding string, redact []string) string {
	if b == nil {
		return ""
	}
	data, total := b.snapshot()
	if total == 0 {
		return ""
	}
	switch encoding {
	case "":
	case "gzip":
		if total > int64(len(data)) {
			return fmt.Sprintf("(gzip body, %s, truncated: not decoded)", logs.HumanBytes(total))
		}
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return "(gzip body that failed to decode)"
		}
		if data, err = io.ReadAll(zr); err != nil {
			return "(gzip body that failed to decode)"
		}
	default:
		return fmt.Sprintf("(%s-encoded body, %s: not decoded)", encoding, logs.HumanBytes(total))
	}
	switch {
	case strings.HasPrefix(contentType, "multipart/"):
		return fmt.Sprintf("(multipart body, %s)", logs.HumanBytes(total))
	case !isText(contentType, data):
		return fmt.Sprintf("(binary body, %s)", logs.HumanBytes(total))
	case strings.Contains(contentType, "x-www-form-urlencoded"):
		return redactForm(string(data), redact)
	case strings.Contains(contentType, "json") || json.Valid(data):
		return redactJSON(data, redact)
	}
	return string(data)
}

func isText(contentType string, data []byte) bool {
	ct := strings.ToLower(contentType)
	for _, s := range []string{"text/", "json", "xml", "javascript", "x-www-form-urlencoded", "graphql", "yaml"} {
		if strings.Contains(ct, s) {
			return true
		}
	}
	return ct == "" && utf8.Valid(data)
}

// cappedBuffer keeps the first max bytes written to it and counts the rest.
// It is locked because a request body can still be streaming upstream when
// the response, which reads it, comes back.
type cappedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	max   int64
	total int64
}

func (c *cappedBuffer) Write(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - int64(c.buf.Len()); room > 0 {
		c.buf.Write(p[:min(int64(len(p)), room)])
	}
	c.total += int64(len(p))
}

// snapshot returns a copy of what was kept and the total written so far.
func (c *cappedBuffer) snapshot() (data []byte, total int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.buf.Bytes()), c.total
}

func (c *cappedBuffer) truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total > int64(c.buf.Len())
}

// teeBody copies what is read through it into buf and calls done once the
// body is finished: at EOF, on a read error, or when closed early.
type teeBody struct {
	rc   io.ReadCloser
	buf  interface{ Write([]byte) } // a cappedBuffer, or the LLM proxy's stream parser
	done func()
}

func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	t.buf.Write(p[:n])
	if err != nil && t.done != nil {
		t.done()
	}
	return n, err
}

func (t *teeBody) Close() error {
	err := t.rc.Close()
	if t.done != nil {
		t.done()
	}
	return err
}
