package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/shivamsingh262/tailyard/internal/logs"
)

// maxAnswerText caps the assembled answer kept for display.
const maxAnswerText = 64 << 10

// llmState accumulates what a response says about a call, from one JSON body
// or from the events of a stream.
type llmState struct {
	api        string // anthropic or openai
	served     string
	stop       string
	errMsg     string
	in, out    int64 // -1 until reported
	cacheRead  int64
	cacheWrite int64
	tools      []string
	text       strings.Builder
	firstToken time.Time
	gotUsage   bool
	refused    bool // a refusal arrived; a later "completed" status doesn't undo it
}

func newLLMState(api string) *llmState {
	return &llmState{api: api, in: -1, out: -1, cacheRead: -1, cacheWrite: -1}
}

func (s *llmState) addText(t string, at time.Time) {
	if t == "" {
		return
	}
	if s.firstToken.IsZero() {
		s.firstToken = at
	}
	if room := maxAnswerText - s.text.Len(); room > 0 {
		s.text.WriteString(logs.CutUTF8(t, room))
	}
}

func (s *llmState) addTool(name string, at time.Time) {
	if name == "" {
		return
	}
	if s.firstToken.IsZero() {
		s.firstToken = at
	}
	s.tools = append(s.tools, name)
}

// absorb takes one decoded JSON object: a whole response or one stream event.
func (s *llmState) absorb(v map[string]any, at time.Time) {
	if e, ok := v["error"].(map[string]any); ok {
		s.errMsg = str(e, "message")
		if t := str(e, "type"); t != "" && s.errMsg != "" {
			s.errMsg = t + ": " + s.errMsg
		}
		return
	}
	if s.api == "anthropic" {
		s.absorbAnthropic(v, at)
	} else {
		s.absorbOpenAI(v, at)
	}
}

func (s *llmState) absorbAnthropic(v map[string]any, at time.Time) {
	switch str(v, "type") {
	case "message": // a whole, non-streamed response
		s.served, s.stop = str(v, "model"), str(v, "stop_reason")
		s.anthropicUsage(obj(v, "usage"))
		for _, b := range list(v, "content") {
			s.anthropicBlock(asObj(b), at)
		}
	case "message_start":
		m := obj(v, "message")
		s.served = str(m, "model")
		s.anthropicUsage(obj(m, "usage"))
	case "content_block_start":
		s.anthropicBlock(obj(v, "content_block"), at)
	case "content_block_delta":
		if d := obj(v, "delta"); str(d, "type") == "text_delta" {
			s.addText(str(d, "text"), at)
		} else if s.firstToken.IsZero() && str(d, "type") != "" {
			s.firstToken = at // tool input or thinking: the model has started
		}
	case "message_delta":
		if r := str(obj(v, "delta"), "stop_reason"); r != "" {
			s.stop = r
		}
		s.anthropicUsage(obj(v, "usage"))
	}
}

func (s *llmState) anthropicBlock(b map[string]any, at time.Time) {
	switch str(b, "type") {
	case "text":
		s.addText(str(b, "text"), at)
	case "tool_use", "server_tool_use":
		s.addTool(str(b, "name"), at)
	}
}

// anthropicUsage takes a usage object. Stream events repeat or update counts,
// so only the fields present overwrite.
func (s *llmState) anthropicUsage(u map[string]any) {
	if u == nil {
		return
	}
	s.gotUsage = true
	set := func(dst *int64, key string) {
		if n, ok := num(u, key); ok {
			*dst = n
		}
	}
	set(&s.in, "input_tokens")
	set(&s.out, "output_tokens")
	set(&s.cacheRead, "cache_read_input_tokens")
	set(&s.cacheWrite, "cache_creation_input_tokens")
}

func (s *llmState) absorbOpenAI(v map[string]any, at time.Time) {
	typ := str(v, "type")
	switch {
	case typ == "response.output_text.delta":
		s.addText(str(v, "delta"), at)
		return
	case typ == "response.refusal.delta":
		s.addText(str(v, "delta"), at)
		s.refused, s.stop = true, "refusal"
		return
	case typ == "response.output_item.added":
		if item := obj(v, "item"); str(item, "type") == "function_call" {
			s.addTool(str(item, "name"), at)
		}
		return
	case typ == "response.created" || typ == "response.completed" || typ == "response.incomplete" || typ == "response.failed":
		s.openAIResponse(obj(v, "response"), at, true)
		return
	case strings.HasPrefix(typ, "response."):
		return
	}
	if str(v, "object") == "response" {
		s.openAIResponse(v, at, false)
		return
	}
	// Chat completions, whole or chunked, and embeddings.
	if m := str(v, "model"); m != "" {
		s.served = m
	}
	for _, c := range list(v, "choices") {
		choice := asObj(c)
		if r := str(choice, "finish_reason"); r != "" {
			s.stop = r
		}
		msg := obj(choice, "message")
		if msg == nil {
			msg = obj(choice, "delta")
		}
		s.addText(str(msg, "content"), at)
		if r := str(msg, "refusal"); r != "" {
			s.addText(r, at)
			s.refused = true
		}
		if s.refused {
			s.stop = "refusal"
		}
		for _, tc := range list(msg, "tool_calls") {
			s.addTool(str(obj(asObj(tc), "function"), "name"), at)
		}
	}
	if u := obj(v, "usage"); u != nil {
		s.gotUsage = true
		prompt, _ := num(u, "prompt_tokens")
		cached, _ := num(obj(u, "prompt_tokens_details"), "cached_tokens")
		s.in, s.cacheRead, s.cacheWrite = prompt-cached, cached, 0
		s.out, _ = num(u, "completion_tokens")
	}
}

// openAIResponse takes a Responses API object. In a stream the text has
// already arrived as deltas, so it isn't taken again.
func (s *llmState) openAIResponse(r map[string]any, at time.Time, streamed bool) {
	if r == nil {
		return
	}
	if m := str(r, "model"); m != "" {
		s.served = m
	}
	switch status := str(r, "status"); status {
	case "incomplete":
		s.stop = str(obj(r, "incomplete_details"), "reason")
	case "completed", "failed", "cancelled":
		s.stop = status
	}
	if e := obj(r, "error"); e != nil {
		s.errMsg = str(e, "message")
	}
	if !streamed {
		for _, item := range list(r, "output") {
			it := asObj(item)
			switch str(it, "type") {
			case "message":
				for _, part := range list(it, "content") {
					p := asObj(part)
					s.addText(str(p, "text")+str(p, "refusal"), at)
					if str(p, "type") == "refusal" {
						s.refused = true
					}
				}
			case "function_call":
				s.addTool(str(it, "name"), at)
			}
		}
	}
	if s.refused {
		s.stop = "refusal"
	}
	if u := obj(r, "usage"); u != nil {
		s.gotUsage = true
		in, _ := num(u, "input_tokens")
		cached, _ := num(obj(u, "input_tokens_details"), "cached_tokens")
		s.in, s.cacheRead, s.cacheWrite = in-cached, cached, 0
		s.out, _ = num(u, "output_tokens")
	}
}

// sseParser feeds a server-sent event stream to an llmState as it passes
// through, so nothing but the current event is buffered.
type sseParser struct {
	state   *llmState
	partial []byte
	data    []byte
	now     func() time.Time
}

func (p *sseParser) Write(b []byte) {
	p.partial = append(p.partial, b...)
	for {
		i := bytes.IndexByte(p.partial, '\n')
		if i < 0 {
			break
		}
		p.line(bytes.TrimRight(p.partial[:i], "\r"))
		p.partial = p.partial[i+1:]
	}
	if len(p.partial) > 4<<20 {
		p.partial = nil // a runaway line; drop it rather than grow without bound
	}
}

func (p *sseParser) line(line []byte) {
	switch {
	case len(line) == 0:
		p.dispatch()
	case bytes.HasPrefix(line, []byte("data:")):
		if len(p.data) > 0 {
			p.data = append(p.data, '\n')
		}
		p.data = append(p.data, bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))...)
	}
}

func (p *sseParser) dispatch() {
	data := p.data
	p.data = nil
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	if v := decodeObject(data); v != nil {
		p.state.absorb(v, p.now())
	}
}

// Close handles a final event that wasn't followed by a blank line.
func (p *sseParser) Close() {
	if len(p.partial) > 0 {
		p.line(bytes.TrimRight(p.partial, "\r"))
		p.partial = nil
	}
	p.dispatch()
}

// llmRequest is what tailyard reads from a request body.
type llmRequest struct {
	model     string
	maxTokens int64
	prompt    string
}

// toolResults stands in for the prompt when the call sends tool output back.
const toolResults = "↩ tool results"

func parseLLMRequest(body []byte) llmRequest {
	v := decodeObject(body)
	if v == nil {
		return llmRequest{}
	}
	r := llmRequest{model: str(v, "model")}
	for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if n, ok := num(v, k); ok {
			r.maxTokens = n
		}
	}
	// The last user message: chat "messages", or Responses API "input".
	msgs := list(v, "messages")
	if in, ok := v["input"].(string); ok {
		r.prompt = in
	} else if len(msgs) == 0 {
		msgs = list(v, "input")
	}
	// In a tool loop the newest turn carries tool output, not a question;
	// say so rather than repeat the question from turns ago.
	if n := len(msgs); n > 0 && r.prompt == "" && isToolResult(asObj(msgs[n-1])) {
		r.prompt = toolResults
	}
	for i := len(msgs) - 1; i >= 0 && r.prompt == ""; i-- {
		if m := asObj(msgs[i]); str(m, "role") == "user" {
			r.prompt = contentText(m["content"])
		}
	}
	r.prompt = logs.CutUTF8(strings.Join(strings.Fields(r.prompt), " "), 200)
	return r
}

// isToolResult reports whether a message returns tool output: an OpenAI
// "tool" message or function_call_output item, or an Anthropic user turn
// made only of tool_result blocks.
func isToolResult(m map[string]any) bool {
	if str(m, "role") == "tool" || str(m, "type") == "function_call_output" {
		return true
	}
	blocks := asList(m["content"])
	if str(m, "role") != "user" || len(blocks) == 0 {
		return false
	}
	for _, b := range blocks {
		if str(asObj(b), "type") != "tool_result" {
			return false
		}
	}
	return true
}

// contentText is the text of a message's content: a string, or the text
// parts of a list of blocks.
func contentText(c any) string {
	if s, ok := c.(string); ok {
		return s
	}
	var parts []string
	for _, b := range asList(c) {
		if t := str(asObj(b), "text"); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

func decodeObject(data []byte) map[string]any {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v map[string]any
	if dec.Decode(&v) != nil {
		return nil
	}
	return v
}

func obj(v map[string]any, key string) map[string]any { return asObj(v[key]) }
func list(v map[string]any, key string) []any         { return asList(v[key]) }

func asObj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func str(v map[string]any, key string) string {
	s, _ := v[key].(string)
	return s
}

func num(v map[string]any, key string) (int64, bool) {
	n, ok := v[key].(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}
