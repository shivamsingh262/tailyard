package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shivamsingh262/tailyard/internal/config"
	"github.com/shivamsingh262/tailyard/internal/logs"
)

// LLMProxy forwards LLM API calls to their providers and records each one:
// model, stop reason, tokens, timing and cost. SDKs reach it by pointing
// their base URL at http://<listen>/<provider>.
//
// Streams pass through untouched and unbuffered; their events are parsed as
// they go by.
type LLMProxy struct {
	cfg    *config.LLM
	corr   *logs.Correlator
	emit   func(logs.Line)
	routes map[string]*llmRoute
	srv    *http.Server
}

type llmRoute struct {
	provider config.LLMProvider
	rp       *httputil.ReverseProxy
}

type llmCtxKey struct{}

// llmCapture is one call, filled in as it passes through.
type llmCapture struct {
	route    *llmRoute
	start    time.Time
	endpoint string
	reqBody  *cappedBuffer
	corr     string
}

func NewLLM(cfg *config.LLM, corr *logs.Correlator, emit func(logs.Line)) (*LLMProxy, error) {
	p := &LLMProxy{cfg: cfg, corr: corr, emit: emit, routes: map[string]*llmRoute{}}
	for _, prov := range cfg.Providers {
		up, err := url.Parse(prov.Upstream)
		if err != nil || up.Host == "" {
			return nil, fmt.Errorf("llm provider %s: upstream %q is not an absolute URL", prov.Name, prov.Upstream)
		}
		prefix := "/" + prov.Name
		route := &llmRoute{provider: prov}
		route.rp = &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				r.Out.URL.Scheme, r.Out.URL.Host, r.Out.Host = up.Scheme, up.Host, up.Host
				r.Out.URL.Path = up.Path + strings.TrimPrefix(r.In.URL.Path, prefix)
				r.Out.URL.RawPath = ""
				// Let Go's transport negotiate gzip and decode it, so streams
				// can be parsed as they pass.
				r.Out.Header.Del("Accept-Encoding")
			},
			FlushInterval:  -1, // forward every streamed chunk at once
			ModifyResponse: p.modifyResponse,
			ErrorHandler:   p.errorHandler,
			ErrorLog:       log.New(io.Discard, "", 0),
		}
		p.routes[prov.Name] = route
	}
	return p, nil
}

func (p *LLMProxy) Start() error {
	ln, err := net.Listen("tcp", p.cfg.Listen)
	if err != nil {
		return err
	}
	p.srv = &http.Server{Handler: p, ErrorLog: log.New(io.Discard, "", 0), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = p.srv.Serve(ln) }()
	return nil
}

func (p *LLMProxy) Close() {
	if p.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = p.srv.Shutdown(ctx)
	}
}

func (p *LLMProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	route := p.routes[name]
	if route == nil {
		var names []string
		for n := range p.routes {
			names = append(names, "/"+n)
		}
		sort.Strings(names)
		http.Error(w, fmt.Sprintf("tailyard llm proxy: no provider at /%s; set your SDK's base URL to %s plus one of %s", name, p.cfg.URL(), strings.Join(names, ", ")), http.StatusNotFound)
		return
	}
	c := &llmCapture{route: route, start: time.Now(), endpoint: "/" + rest, reqBody: &cappedBuffer{max: p.cfg.MaxBodyBytes}, corr: p.headerCorr(r.Header)}
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = &teeBody{rc: r.Body, buf: c.reqBody}
	}
	route.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), llmCtxKey{}, c)))
}

// headerCorr finds a correlation id the app passed along as a header.
func (p *LLMProxy) headerCorr(h http.Header) string {
	if p.corr == nil {
		return ""
	}
	names := []string{"X-Request-Id", "X-Correlation-Id"}
	for _, k := range p.corr.Keys {
		names = append(names, k, strings.ReplaceAll(k, "_", "-"), "X-"+strings.ReplaceAll(k, "_", "-"))
	}
	for _, n := range names {
		if v := h.Get(n); v != "" {
			return v
		}
	}
	return ""
}

func (p *LLMProxy) modifyResponse(resp *http.Response) error {
	c, _ := resp.Request.Context().Value(llmCtxKey{}).(*llmCapture)
	if c == nil || resp.StatusCode == http.StatusSwitchingProtocols {
		return nil // WebSockets (realtime APIs) are forwarded, not recorded
	}
	state := newLLMState(c.route.provider.API)
	var once sync.Once
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		sse := &sseParser{state: state, now: time.Now}
		resp.Body = &teeBody{rc: resp.Body, buf: sse, done: func() {
			once.Do(func() {
				sse.Close()
				p.record(c, resp, state, nil, true)
			})
		}}
		return nil
	}
	raw := &cappedBuffer{max: p.cfg.MaxBodyBytes}
	resp.Body = &teeBody{rc: resp.Body, buf: raw, done: func() {
		once.Do(func() { p.record(c, resp, state, raw, false) })
	}}
	return nil
}

func (p *LLMProxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	c, _ := r.Context().Value(llmCtxKey{}).(*llmCapture)
	status := http.StatusBadGateway
	if errors.Is(err, context.Canceled) {
		status = 499
	} else {
		w.WriteHeader(status)
		upstream := ""
		if c != nil {
			upstream = c.route.provider.Upstream
		}
		_, _ = fmt.Fprintf(w, "tailyard llm proxy: %s unreachable: %v\n", upstream, err)
	}
	if c != nil {
		call := p.call(c, status, newLLMState(c.route.provider.API), false)
		call.Error = err.Error()
		p.emit(logs.LLMLine(logs.LLMEntry{Call: call, Time: c.start, ReqBody: p.display(c.reqBody), Corr: c.corr}, p.corr))
	}
}

func (p *LLMProxy) record(c *llmCapture, resp *http.Response, state *llmState, raw *cappedBuffer, streamed bool) {
	var note string
	if raw != nil {
		data, total := raw.snapshot()
		switch {
		case total > int64(len(data)):
			note = "response larger than max_body; not parsed"
		case total > 0:
			if v := decodeObject(data); v != nil {
				state.absorb(v, time.Now())
			}
		}
	}
	call := p.call(c, resp.StatusCode, state, streamed)
	switch {
	case note != "":
		call.Note = note
	case streamed && call.Stop == "" && call.Error == "" && resp.StatusCode < 400:
		call.Note = "the stream ended before the model finished (the client may have stopped reading)"
	case streamed && c.route.provider.API == "openai" && !state.gotUsage && strings.HasSuffix(c.endpoint, "/chat/completions"):
		call.Note = `no token usage: set stream_options: {"include_usage": true} to get it`
	}
	e := logs.LLMEntry{Call: call, Time: c.start, ReqBody: p.display(c.reqBody), ResText: state.text.String(), Corr: c.corr}
	if e.ResText == "" && raw != nil {
		e.ResBody = displayBody(raw, resp.Header.Get("Content-Type"), "", p.cfg.Redact)
	}
	p.emit(logs.LLMLine(e, p.corr))
}

// call builds the normalized record from the request and what the response said.
func (p *LLMProxy) call(c *llmCapture, status int, state *llmState, streamed bool) logs.LLMCall {
	var req llmRequest
	if data, total := c.reqBody.snapshot(); total == int64(len(data)) {
		req = parseLLMRequest(data)
	}
	call := logs.LLMCall{
		Provider:     c.route.provider.Name,
		Endpoint:     c.endpoint,
		Model:        req.model,
		Served:       state.served,
		Status:       status,
		Stream:       streamed,
		Stop:         state.stop,
		InputTokens:  state.in,
		OutputTokens: state.out,
		CacheRead:    state.cacheRead,
		CacheWrite:   state.cacheWrite,
		MaxTokens:    req.maxTokens,
		Ms:           time.Since(c.start).Milliseconds(),
		TTFTMs:       -1,
		Prompt:       req.prompt,
		Tools:        state.tools,
		Error:        state.errMsg,
	}
	if call.Model == "" {
		call.Model = state.served
	}
	if streamed && !state.firstToken.IsZero() {
		call.TTFTMs = state.firstToken.Sub(c.start).Milliseconds()
	}
	call.Cost = p.cost(call)
	return call
}

// cost uses the price configured for the model, or for the longest model
// prefix that has one. -1 means unknown.
func (p *LLMProxy) cost(c logs.LLMCall) float64 {
	model := c.Served
	if model == "" {
		model = c.Model
	}
	price, ok := p.cfg.Prices[model]
	if !ok {
		best := ""
		for k, v := range p.cfg.Prices {
			if strings.HasPrefix(model, k) && len(k) > len(best) {
				best, price, ok = k, v, true
			}
		}
	}
	if !ok || c.InputTokens < 0 || c.OutputTokens < 0 {
		return -1
	}
	return (float64(c.InputTokens)*price.Input +
		float64(c.OutputTokens)*price.Output +
		float64(max(c.CacheRead, 0))*price.CacheRead +
		float64(max(c.CacheWrite, 0))*price.CacheWrite) / 1e6
}

func (p *LLMProxy) display(b *cappedBuffer) string {
	return displayBody(b, "application/json", "", p.cfg.Redact)
}
