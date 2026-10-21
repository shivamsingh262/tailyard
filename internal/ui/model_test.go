package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/shivamsingh262/tailyard/internal/app"
	"github.com/shivamsingh262/tailyard/internal/config"
	"github.com/shivamsingh262/tailyard/internal/logs"
)

func testModel(t *testing.T) *Model {
	t.Helper()
	cfg, err := config.Parse([]byte(`
name: demo
sources:
  - name: api
    file: api.log
  - name: worker
    file: worker.log
proxy:
  target: http://localhost:3000
correlation:
  label: request
  keys: [request_id]
actions:
  - key: R
    run: echo hi
`), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := New(a, false)
	m.Update(tea.WindowSizeMsg{Width: 180, Height: 30})
	return m
}

func key(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestRenderAndFilter(t *testing.T) {
	m := testModel(t)
	now := time.Now()
	m.Update(linesMsg{
		{Src: "api", Time: now, Level: logs.LInfo, Plain: "started", Text: "started"},
		{Src: "api", Time: now.Add(time.Second), Level: logs.LError, Plain: "boom request_id=r1", Text: "boom", Corr: "r1", Detail: []string{"trace"}},
		{Src: "worker", Time: now.Add(2 * time.Second), Level: logs.LInfo, Plain: "job r2", Text: "job", Corr: "r2"},
		logs.HTTPLine(logs.HTTPEntry{Time: now, Method: "GET", URL: "/api/x?request_id=r1", Status: 200, Ms: 5}, &logs.Correlator{Keys: []string{"request_id"}}),
	})

	view := ansi.Strip(m.View())
	for _, want := range []string{"demo", "1 Api", "2 Worker", "3 HTTP", "boom", "▸1"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}

	m.applyPin("r1")
	m.switchTab(len(m.tabs) - 1) // All
	view = ansi.Strip(m.View())
	if strings.Contains(view, "job") || !strings.Contains(view, "boom") || !strings.Contains(view, "request: r1") {
		t.Errorf("pin didn't filter the All tab:\n%s", view)
	}

	m.handleKey(key("i"))
	if !m.inspecting || !strings.Contains(ansi.Strip(m.View()), "request=r1") {
		t.Errorf("inspector not showing the entry")
	}
}

func TestContainsToken(t *testing.T) {
	cases := map[string]bool{
		"request_id=req-1":        true,
		"request_id=req-13":       false,
		"GET /runs/req-1/steps":   true,
		"xreq-1":                  false,
		"req-13 then req-1 later": true,
	}
	for s, want := range cases {
		if got := containsToken(s, "req-1"); got != want {
			t.Errorf("containsToken(%q) = %v", s, got)
		}
	}
}

func TestHelpListsActions(t *testing.T) {
	m := testModel(t)
	m.handleKey(key("?"))
	if !strings.Contains(ansi.Strip(m.View()), "echo hi") {
		t.Error("help doesn't list the configured action")
	}
}

func TestErrorBadgeOnInactiveTab(t *testing.T) {
	m := testModel(t)
	m.Update(linesMsg{{Src: "worker", Time: time.Now().Add(time.Second), Level: logs.LError, Plain: "x", Text: "x"}})
	if m.tabs[1].newErrors != 1 {
		t.Errorf("newErrors = %d", m.tabs[1].newErrors)
	}
	m.switchTab(1)
	if m.tabs[1].newErrors != 0 {
		t.Error("switching to the tab should clear its badge")
	}
}

func TestLLMTab(t *testing.T) {
	cfg, err := config.Parse([]byte("llm:\n  providers: [{name: anthropic}]\n"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := New(a, false)
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 30})
	now := time.Now()
	call := func(stop string, status int, out int64) logs.Line {
		return logs.LLMLine(logs.LLMEntry{Time: now, ResText: "an answer", Call: logs.LLMCall{
			Provider: "anthropic", Model: "claude-opus-5-5", Served: "claude-opus-5-5", Status: status, Stop: stop,
			InputTokens: 1200, OutputTokens: out, CacheRead: 10800, CacheWrite: 0, Ms: 4100, TTFTMs: 800, Cost: 0.0123,
			Prompt: "Why did ROAS drop?",
		}}, nil)
	}
	m.Update(linesMsg{call("end_turn", 200, 845), call("max_tokens", 200, 4096)})

	view := ansi.Strip(m.View())
	for _, want := range []string{"1 LLM", "llm 2 · 29k tok · $0.025 !1", "claude-opus-5-5", "end_turn", "12k→845", "90%c", "4.1s", "$0.012", "Why did ROAS drop?", "max_tokens", "first token 800ms", "an answer"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
}
