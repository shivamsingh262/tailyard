package proxy

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shivamsingh262/tailyard/internal/config"
	"github.com/shivamsingh262/tailyard/internal/logs"
)

func newTestLLMProxy(t *testing.T, api string, upstream http.HandlerFunc) (string, *sink) {
	t.Helper()
	backend := httptest.NewServer(upstream)
	t.Cleanup(backend.Close)
	cfg := &config.LLM{
		Listen:       "127.0.0.1:0",
		MaxBodyBytes: 1 << 20,
		Redact:       []string{"token", "api_key"},
		Providers:    []config.LLMProvider{{Name: "p", API: api, Upstream: backend.URL + "/v1"}},
		Prices:       map[string]config.Price{"claude-opus": {Input: 4, Output: 20, CacheRead: 0.2, CacheWrite: 5}},
	}
	s := &sink{}
	p, err := NewLLM(cfg, &logs.Correlator{Keys: []string{"request_id"}}, s.emit)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	return front.URL + "/p", s
}

func post(t *testing.T, url, body string, header ...string) string {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	return string(got)
}

func TestLLMAnthropicMessage(t *testing.T) {
	url, s := newTestLLMProxy(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/v1/messages" || r.Header.Get("X-Api-Key") != "sk-secret" {
			t.Errorf("upstream got %s with key %q", r.URL.Path, r.Header.Get("X-Api-Key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","model":"claude-opus-5-5","stop_reason":"max_tokens",
			"content":[{"type":"text","text":"ROAS fell because"}],
			"usage":{"input_tokens":1000,"output_tokens":512,"cache_read_input_tokens":9000,"cache_creation_input_tokens":0}}`))
	})
	got := post(t, url+"/v1/messages", `{"model":"claude-opus-5-5","max_tokens":512,"messages":[{"role":"user","content":"Why did ROAS drop?"}]}`,
		"X-Api-Key", "sk-secret", "X-Request-Id", "req-7")
	if !strings.Contains(got, "ROAS fell because") {
		t.Fatalf("client got %s", got)
	}
	l := s.wait(t, 1)[0]
	c := l.LLM
	if l.Src != "llm" || c.Model != "claude-opus-5-5" || c.Stop != "max_tokens" || c.Status != 200 {
		t.Errorf("call: %+v", c)
	}
	if c.InputTokens != 1000 || c.OutputTokens != 512 || c.CacheRead != 9000 || c.MaxTokens != 512 {
		t.Errorf("tokens: %+v", c)
	}
	if c.Prompt != "Why did ROAS drop?" || l.Corr != "req-7" || l.Level != logs.LWarn || !c.Truncated() {
		t.Errorf("prompt %q corr %q level %v", c.Prompt, l.Corr, l.Level)
	}
	// (1000×4 + 512×20 + 9000×0.2) / 1e6
	if want := 0.01604; c.Cost < want-1e-9 || c.Cost > want+1e-9 {
		t.Errorf("cost = %v, want %v", c.Cost, want)
	}
	detail := strings.Join(l.Detail, "\n")
	if strings.Contains(detail, "sk-secret") || !strings.Contains(detail, `"max_tokens": 512`) {
		t.Errorf("token counts must survive redaction and keys must not appear:\n%s", detail)
	}
}

// The client must get each event as soon as the upstream sends it, while the
// proxy still sees the whole stream.
func TestLLMAnthropicStreamPassesThrough(t *testing.T) {
	release := make(chan struct{})
	url, s := newTestLLMProxy(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(event, data string) {
			_, _ = io.WriteString(w, "event: "+event+"\ndata: "+data+"\n\n")
			w.(http.Flusher).Flush()
		}
		send("message_start", `{"type":"message_start","message":{"model":"claude-opus-5-5","usage":{"input_tokens":20,"output_tokens":1,"cache_read_input_tokens":0}}}`)
		send("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		send("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`)
		<-release
		send("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"get_campaigns"}}`)
		send("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`)
		send("message_stop", `{"type":"message_stop"}`)
	})
	resp, err := http.Post(url+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-opus-5-5","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	deadline := time.After(2 * time.Second)
	sawDelta := make(chan struct{})
	go func() {
		for {
			line, err := rd.ReadString('\n')
			if strings.Contains(line, "Hello") {
				close(sawDelta)
				return
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-sawDelta:
	case <-deadline:
		t.Fatal("the first delta was held back until the stream ended")
	}
	close(release)
	_, _ = io.Copy(io.Discard, rd)

	c := s.wait(t, 1)[0].LLM
	if !c.Stream || c.Stop != "tool_use" || c.InputTokens != 20 || c.OutputTokens != 42 || c.Prompt != "hi" {
		t.Errorf("call: %+v", c)
	}
	if len(c.Tools) != 1 || c.Tools[0] != "get_campaigns" || c.TTFTMs < 0 {
		t.Errorf("tools %v ttft %d", c.Tools, c.TTFTMs)
	}
	if l := s.wait(t, 1)[0]; !strings.Contains(strings.Join(l.Detail, "\n"), "Hello") {
		t.Errorf("assembled text missing from detail")
	}
}

func TestLLMOpenAIChatStreamWithoutUsage(t *testing.T) {
	url, s := newTestLLMProxy(t, "openai", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, d := range []string{
			`{"object":"chat.completion.chunk","model":"gpt-5.1-2026-01-01","choices":[{"delta":{"content":"Hi"}}]}`,
			`{"object":"chat.completion.chunk","model":"gpt-5.1-2026-01-01","choices":[{"delta":{"content":" there"},"finish_reason":"length"}]}`,
			`[DONE]`,
		} {
			_, _ = io.WriteString(w, "data: "+d+"\n\n")
		}
	})
	post(t, url+"/chat/completions", `{"model":"gpt-5.1","stream":true,"max_completion_tokens":2,"messages":[{"role":"user","content":"hey"}]}`)
	l := s.wait(t, 1)[0]
	c := l.LLM
	if c.Endpoint != "/chat/completions" || c.Stop != "length" || !c.Truncated() || c.Rerouted() {
		t.Errorf("call: %+v", c)
	}
	if c.InputTokens != -1 || !strings.Contains(c.Note, "include_usage") {
		t.Errorf("missing usage should be noted: %+v", c)
	}
	if !strings.Contains(strings.Join(l.Detail, "\n"), "Hi there") {
		t.Errorf("detail: %v", l.Detail)
	}
}

func TestLLMOpenAIResponsesIncomplete(t *testing.T) {
	url, s := newTestLLMProxy(t, "openai", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"response","model":"gpt-5.1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},
			"output":[{"type":"function_call","name":"lookup"},{"type":"message","content":[{"type":"output_text","text":"partial"}]}],
			"usage":{"input_tokens":300,"output_tokens":100,"input_tokens_details":{"cached_tokens":200}}}`))
	})
	post(t, url+"/responses", `{"model":"gpt-5.1","input":"summarize","max_output_tokens":100}`)
	c := s.wait(t, 1)[0].LLM
	if c.Stop != "max_output_tokens" || c.InputTokens != 100 || c.CacheRead != 200 || c.OutputTokens != 100 || c.Prompt != "summarize" {
		t.Errorf("call: %+v", c)
	}
	if len(c.Tools) != 1 || c.Tools[0] != "lookup" || c.Cost != -1 {
		t.Errorf("tools %v cost %v", c.Tools, c.Cost)
	}
}

func TestLLMErrors(t *testing.T) {
	url, s := newTestLLMProxy(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(529)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	})
	post(t, url+"/v1/messages", `{"model":"claude-opus-5-5","messages":[]}`)
	l := s.wait(t, 1)[0]
	if l.LLM.Status != 529 || l.LLM.Error != "overloaded_error: Overloaded" || l.Level != logs.LWarn {
		t.Errorf("line: %+v %+v", l.LLM, l.Level)
	}

	resp, err := http.Get(strings.TrimSuffix(url, "/p") + "/nope/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 404 || !strings.Contains(string(body), "/p") {
		t.Errorf("unknown provider: %d %s", resp.StatusCode, body)
	}
}

func TestLLMUpstreamDown(t *testing.T) {
	cfg := &config.LLM{MaxBodyBytes: 1024, Providers: []config.LLMProvider{{Name: "p", API: "openai", Upstream: "http://127.0.0.1:1"}}}
	s := &sink{}
	p, _ := NewLLM(cfg, nil, s.emit)
	front := httptest.NewServer(p)
	defer front.Close()
	post(t, front.URL+"/p/chat/completions", `{"model":"m"}`)
	if l := s.wait(t, 1)[0]; l.LLM.Status != 502 || l.Level != logs.LError || l.LLM.Error == "" {
		t.Errorf("line: %+v", l.LLM)
	}
}

func TestTokenCountsAreNotSecrets(t *testing.T) {
	got := redactJSON([]byte(`{"max_tokens":5,"usage":{"input_tokens":3},"promptTokenCount":2,"token":"t","access_tokens":["a"]}`), []string{"token"})
	for _, want := range []string{`"max_tokens":5`, `"input_tokens":3`, `"promptTokenCount":2`, `"token":"[redacted]"`, `"access_tokens":"[redacted]"`} {
		if !strings.Contains(got, want) {
			t.Errorf("want %s in %s", want, got)
		}
	}
}
