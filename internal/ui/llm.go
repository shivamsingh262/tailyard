package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/shivamsingh262/tailyard/internal/logs"
	"github.com/shivamsingh262/tailyard/internal/theme"
)

// llmTotals sums the calls seen this session for the header badge.
type llmTotals struct {
	calls    int
	problems int
	tokens   int64
	cost     float64
	priced   bool // at least one call had a known cost
}

func (t *llmTotals) add(c *logs.LLMCall) {
	t.calls++
	if len(c.Problems()) > 0 {
		t.problems++
	}
	if n := c.TotalTokens(); n > 0 {
		t.tokens += n
	}
	if c.Cost >= 0 {
		t.cost += c.Cost
		t.priced = true
	}
}

// badge reads "llm 12 · 48k tok · $0.412", with problems counted in red.
func (t *llmTotals) badge() string {
	if t.calls == 0 {
		return ""
	}
	s := fmt.Sprintf("llm %d · %s tok", t.calls, logs.Tokens(t.tokens))
	if t.priced {
		s += " · " + logs.Dollars(t.cost)
	}
	s = theme.Dim.Render(s)
	if t.problems > 0 {
		s += " " + theme.ErrB.Render(fmt.Sprintf("!%d", t.problems))
	}
	return s
}

func llmDuration(ms int64) string {
	if ms >= 1000 {
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	return fmt.Sprintf("%dms", ms)
}

// llmOutcome is the status column: the stop reason, or the HTTP status and
// error for failed calls.
func llmOutcome(c *logs.LLMCall) string {
	switch {
	case c.Status >= 400 || c.Status == 0:
		return theme.Status(c.Status).Render(fmt.Sprint(c.Status))
	case c.Truncated(), c.Refused():
		return theme.WarnB.Render(c.Stop)
	case c.Stop == "":
		return theme.Dim.Render("…")
	case c.Stop == "tool_use" || c.Stop == "tool_calls":
		return theme.Accent.Render(c.Stop)
	}
	return theme.OK.Render(c.Stop)
}

// renderLLM lays a call out as: provider  model  outcome  tokens  cache  time  cost  prompt,
// padding the model to modelWidth so the columns line up across rows.
func renderLLM(c *logs.LLMCall, modelWidth int) string {
	model := c.Model
	if model == "" {
		model = "?"
	}
	var b strings.Builder
	b.WriteString(theme.Source("llm").Render(fmt.Sprintf("%-9s", ansi.Truncate(c.Provider, 9, "…"))) + " ")
	b.WriteString(model + strings.Repeat(" ", max(modelWidth-ansi.StringWidth(model), 0)) + "  ")
	outcome := llmOutcome(c)
	b.WriteString(outcome + strings.Repeat(" ", max(14-ansi.StringWidth(outcome), 1)))
	b.WriteString(fmt.Sprintf("%13s", logs.Tokens(c.InputTokens+max(c.CacheRead, 0)+max(c.CacheWrite, 0))+"→"+logs.Tokens(c.OutputTokens)))
	if rate := c.CacheHitRate(); rate >= 0 {
		b.WriteString(theme.Dim.Render(fmt.Sprintf(" %3.0f%%c", rate*100)))
	} else {
		b.WriteString("      ")
	}
	b.WriteString(" " + theme.Duration(c.Ms).Render(fmt.Sprintf("%6s", llmDuration(c.Ms))))
	if c.Cost >= 0 {
		b.WriteString(theme.Dim.Render(fmt.Sprintf(" %8s", logs.Dollars(c.Cost))))
	}
	var tail []string
	problems := c.Problems()
	if c.Truncated() || c.Refused() {
		problems = problems[1:] // already shown as the outcome
	}
	if c.Status >= 400 {
		if c.Error != "" {
			tail = append(tail, theme.Err.Render(c.Error))
		}
		problems = nil
	}
	for _, p := range problems {
		tail = append(tail, theme.WarnB.Render(p))
	}
	if len(c.Tools) > 0 {
		tail = append(tail, theme.Accent.Render("→ "+strings.Join(c.Tools, ", ")))
	}
	if c.Prompt != "" {
		tail = append(tail, c.Prompt)
	}
	if len(tail) > 0 {
		b.WriteString("  " + strings.Join(tail, "  "))
	}
	return b.String()
}

// llmModelWidth is the display width of a call's model column.
func llmModelWidth(c *logs.LLMCall) int {
	return max(ansi.StringWidth(c.Model), 1)
}
