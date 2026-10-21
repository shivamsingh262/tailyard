package logs

import (
	"fmt"
	"strings"
	"time"

	"github.com/shivamsingh262/tailyard/internal/theme"
)

// LLMCall is one LLM API call recorded by the LLM proxy, normalized across
// providers. Token counts are -1 when the response didn't report them.
type LLMCall struct {
	Provider     string
	Endpoint     string // e.g. /v1/messages
	Model        string // as requested
	Served       string // as reported by the response
	Status       int
	Stream       bool
	Stop         string // stop or finish reason
	InputTokens  int64  // uncached input
	OutputTokens int64
	CacheRead    int64
	CacheWrite   int64
	MaxTokens    int64 // the request's output cap; 0 when unset
	Ms           int64
	TTFTMs       int64   // time to first token for streams; -1 when unknown
	Cost         float64 // USD; -1 without a price for the model
	Prompt       string  // the last user message, on one line
	Tools        []string
	Error        string // the API's error message, or why the call failed
	Note         string // e.g. "usage missing: set stream_options.include_usage"
}

// Truncated reports whether the model stopped because it hit the output cap.
func (c *LLMCall) Truncated() bool {
	switch c.Stop {
	case "max_tokens", "length", "max_output_tokens":
		return true
	}
	return false
}

// Refused reports whether the model declined or the output was filtered.
func (c *LLMCall) Refused() bool {
	return c.Stop == "refusal" || c.Stop == "content_filter"
}

// Rerouted reports whether a different model answered than the one asked
// for. Dated variants (gpt-4o answered by gpt-4o-2024-08-06), "-latest"
// aliases and router prefixes (openai/gpt-4o) don't count.
func (c *LLMCall) Rerouted() bool {
	if c.Model == "" || c.Served == "" {
		return false
	}
	base := func(m string) string {
		if i := strings.LastIndex(m, "/"); i >= 0 {
			m = m[i+1:]
		}
		return strings.TrimSuffix(m, "-latest")
	}
	asked, served := base(c.Model), base(c.Served)
	return !strings.HasPrefix(served, asked) && !strings.HasPrefix(asked, served)
}

// Failed reports whether the call didn't produce an answer: an HTTP error,
// no response at all, or an error sent inside a successful stream.
func (c *LLMCall) Failed() bool {
	return c.Status >= 400 || c.Status == 0 || c.Error != ""
}

// Problems lists what deserves attention, most serious first.
func (c *LLMCall) Problems() []string {
	var out []string
	switch {
	case c.Status == 429:
		out = append(out, "rate limited")
	case c.Status == 529:
		out = append(out, "overloaded")
	case c.Failed():
		out = append(out, "failed")
	}
	if c.Truncated() {
		out = append(out, fmt.Sprintf("hit the output cap (%s)", c.Stop))
	}
	if c.Refused() {
		out = append(out, "refused ("+c.Stop+")")
	}
	if c.Rerouted() {
		out = append(out, "answered by "+c.Served)
	}
	return out
}

// TotalTokens is input (cached or not) plus output, or -1 when unknown.
func (c *LLMCall) TotalTokens() int64 {
	if c.InputTokens < 0 || c.OutputTokens < 0 {
		return -1
	}
	return c.InputTokens + max(c.CacheRead, 0) + max(c.CacheWrite, 0) + c.OutputTokens
}

// CacheHitRate is the share of input read from the prompt cache, or -1.
func (c *LLMCall) CacheHitRate() float64 {
	in := max(c.InputTokens, 0) + max(c.CacheRead, 0) + max(c.CacheWrite, 0)
	if c.CacheRead < 0 || in == 0 {
		return -1
	}
	return float64(c.CacheRead) / float64(in)
}

// LLMEntry is a recorded call plus what the detail view shows.
type LLMEntry struct {
	Call    LLMCall
	Time    time.Time
	ReqBody string // redacted
	ResText string // the assembled text of the answer
	ResBody string // redacted; for non-streamed responses
	Corr    string
}

func LLMLine(e LLMEntry, corr *Correlator) Line {
	c := e.Call
	l := Line{Src: "llm", Time: e.Time, Level: LInfo, LLM: &c, Corr: e.Corr}
	problems := c.Problems()
	switch {
	case c.Failed() && c.Status != 429 && c.Status != 529:
		l.Level = LError
	case len(problems) > 0:
		l.Level = LWarn
	}
	if l.Corr == "" {
		l.Corr = corr.FromText(e.ReqBody)
	}
	l.Plain = strings.Join(nonEmpty(c.Provider, c.Model, c.Served, c.Stop, c.Error, strings.Join(problems, " "), c.Prompt, strings.Join(c.Tools, " ")), " ")
	l.Plain += fmt.Sprintf(" %d %dms", c.Status, c.Ms)

	kv := func(k, v string) string { return theme.Key.Render(fmt.Sprintf("%-12s", k)) + v }
	for _, p := range problems {
		l.Detail = append(l.Detail, theme.WarnB.Render("! "+p))
	}
	if c.Error != "" {
		l.Detail = append(l.Detail, theme.Err.Render(c.Error))
	}
	l.Detail = append(l.Detail, kv("endpoint", c.Provider+" "+c.Endpoint))
	model := c.Model
	if c.Served != "" && c.Served != c.Model {
		model += theme.Dim.Render(" → ") + c.Served
	}
	l.Detail = append(l.Detail, kv("model", model))
	if c.Stop != "" {
		l.Detail = append(l.Detail, kv("stop", c.Stop))
	}
	tokens := fmt.Sprintf("%s in · %s out", Tokens(c.InputTokens), Tokens(c.OutputTokens))
	if c.CacheRead > 0 || c.CacheWrite > 0 {
		tokens += fmt.Sprintf(" · cache %s read, %s written", Tokens(c.CacheRead), Tokens(c.CacheWrite))
	}
	if c.MaxTokens > 0 {
		tokens += theme.Dim.Render(" · cap " + Tokens(c.MaxTokens))
	}
	l.Detail = append(l.Detail, kv("tokens", tokens))
	timing := fmt.Sprintf("%dms", c.Ms)
	if c.TTFTMs >= 0 {
		timing += fmt.Sprintf(" · first token %dms", c.TTFTMs)
	}
	l.Detail = append(l.Detail, kv("time", timing))
	if c.Cost >= 0 {
		l.Detail = append(l.Detail, kv("cost", Dollars(c.Cost)))
	}
	if len(c.Tools) > 0 {
		l.Detail = append(l.Detail, kv("tool calls", strings.Join(c.Tools, ", ")))
	}
	if c.Note != "" {
		l.Detail = append(l.Detail, theme.Dim.Render("("+c.Note+")"))
	}
	if e.ReqBody != "" {
		l.Detail = append(l.Detail, theme.Heading.Render("▸ request"))
		l.Detail = append(l.Detail, PrettyBody(e.ReqBody)...)
	}
	switch {
	case e.ResText != "":
		l.Detail = append(l.Detail, theme.Heading.Render(fmt.Sprintf("▸ response %d", c.Status))+"  "+theme.Dim.Render("text"))
		l.Detail = append(l.Detail, strings.Split(strings.TrimRight(e.ResText, "\n"), "\n")...)
	case e.ResBody != "":
		l.Detail = append(l.Detail, theme.Heading.Render(fmt.Sprintf("▸ response %d", c.Status)))
		l.Detail = append(l.Detail, PrettyBody(e.ResBody)...)
	}
	l.Detail = capDetail(l.Detail)
	return l
}

// Tokens renders a token count compactly: 845, 12.3k, 1.2M; "?" when unknown.
func Tokens(n int64) string {
	switch {
	case n < 0:
		return "?"
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

// Dollars renders a cost with enough precision for one cheap call.
func Dollars(usd float64) string {
	switch {
	case usd >= 10:
		return fmt.Sprintf("$%.2f", usd)
	case usd >= 0.01:
		return fmt.Sprintf("$%.3f", usd)
	}
	return fmt.Sprintf("$%.4f", usd)
}

func nonEmpty(values ...string) []string {
	var out []string
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
