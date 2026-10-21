// Package ui is the Bubble Tea model: tabs of log entries, filters, the
// inspector, the split detail pane and the Redis panel.
package ui

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/shivamsingh262/tailyard/internal/app"
	"github.com/shivamsingh262/tailyard/internal/config"
	"github.com/shivamsingh262/tailyard/internal/logs"
)

const maxLines = 5000

// Inline expansion (t) shows at most this many detail lines per entry; the
// inspector (i) shows all of them.
const inlineDetailLines = 30

type tab struct {
	name       string
	srcs       map[string]bool
	multi      bool
	redis      bool // shows the Redis panel above the list
	buf        []logs.Line
	filter     *regexp.Regexp
	filterText string
	errorsOnly bool
	scroll     int // matching entries hidden below the view
	cursor     int // matching entries newer than the selected one; 0 selects the newest and follows new output
	visible    int // entries shown by the last render, for keeping the cursor in view
	newErrors  int
	split      bool   // list + detail pane on wide terminals
	paneScroll int    // detail pane scroll, reset when the selection changes
	paneKey    string // the entry the detail pane shows
}

// follow selects the newest entry and resumes following new output.
func (t *tab) follow() {
	t.scroll, t.cursor = 0, 0
}

type inputMode int

const (
	inputNone inputMode = iota
	inputFilter
	inputPin
)

type (
	linesMsg []logs.Line
	tickMsg  time.Time
)

type Model struct {
	app        *app.App
	cfg        *config.Config
	tabs       []*tab
	active     int
	width      int
	height     int
	input      textinput.Model
	mode       inputMode
	pinned     string
	recentIDs  []string
	idCycle    int
	hideOK     bool
	expand     bool
	showHelp   bool
	status     app.StatusMsg
	redis      app.RedisMsg
	haveRedis  bool
	busy       string
	monitorOn  bool
	flash      string
	flashUntil time.Time
	started    time.Time
	llm        llmTotals

	// StopOnQuit is read after the program exits: q stops the containers,
	// d detaches.
	StopOnQuit bool

	// Inspector: one entry full-screen with all of its detail, wrapped.
	inspecting    bool
	inspected     logs.Line
	inspectRows   []string
	inspectWidth  int
	inspectScroll int

	// Detail pane rows for the selected entry, cached across renders.
	paneCache struct {
		key   string
		width int
		rows  []string
	}
}

func New(a *app.App, stopOnQuit bool) *Model {
	m := &Model{app: a, cfg: a.Cfg, started: time.Now(), StopOnQuit: stopOnQuit}
	for _, def := range a.Cfg.Tabs {
		t := &tab{name: def.Name, srcs: map[string]bool{}, multi: len(def.Sources) > 1}
		for _, s := range def.Sources {
			t.srcs[s] = true
		}
		t.split = len(def.Sources) == 1 && (def.Sources[0] == config.SrcHTTP || def.Sources[0] == config.SrcLLM)
		t.redis = t.srcs[config.SrcEvents] || t.srcs[config.SrcMonitor]
		m.tabs = append(m.tabs, t)
	}
	m.input = textinput.New()
	m.input.CharLimit = 200
	return m
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(waitLines(m.app.Lines), tick())
}

func waitLines(ch <-chan logs.Line) tea.Cmd {
	return func() tea.Msg {
		batch := linesMsg{<-ch}
		deadline := time.After(40 * time.Millisecond)
		for len(batch) < 2000 {
			select {
			case l := <-ch:
				batch = append(batch, l)
			case <-deadline:
				return batch
			}
		}
		return batch
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.Width = msg.Width - 20
	case linesMsg:
		for _, l := range msg {
			m.addLine(l)
		}
		return m, waitLines(m.app.Lines)
	case app.StatusMsg:
		m.status = msg
	case app.RedisMsg:
		m.redis, m.haveRedis = msg, true
	case app.BusyMsg:
		m.busy = string(msg)
	case tickMsg:
		return m, tick()
	case tea.KeyMsg:
		if m.mode != inputNone {
			return m, m.updateInput(msg)
		}
		if m.inspecting {
			return m, m.handleInspectKey(msg)
		}
		return m, m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) addLine(l logs.Line) {
	if l.LLM != nil {
		m.llm.add(l.LLM)
	}
	if l.Corr != "" && (len(m.recentIDs) == 0 || m.recentIDs[0] != l.Corr) {
		ids := []string{l.Corr}
		for _, r := range m.recentIDs {
			if r != l.Corr && len(ids) < 20 {
				ids = append(ids, r)
			}
		}
		m.recentIDs = ids
	}
	for i, t := range m.tabs {
		if !t.srcs[l.Src] {
			continue
		}
		// Each source's history arrives as its own burst; keep tabs that mix
		// sources in time order.
		if n := len(t.buf); n > 0 && l.Time.Before(t.buf[n-1].Time) {
			at := sort.Search(n, func(i int) bool { return t.buf[i].Time.After(l.Time) })
			t.buf = slices.Insert(t.buf, at, l)
		} else {
			t.buf = append(t.buf, l)
		}
		if len(t.buf) > maxLines+500 {
			t.buf = append([]logs.Line(nil), t.buf[len(t.buf)-maxLines:]...)
		}
		// Once something older than the newest entry is selected, keep that
		// selection and the view still while new output arrives below it.
		if t.cursor > 0 && m.matches(t, l) {
			t.cursor++
			t.scroll++
		}
		if l.Level == logs.LError && i != m.active && l.Src != config.SrcMonitor && l.Time.After(m.started) {
			t.newErrors++
		}
	}
}

func (m *Model) matches(t *tab, l logs.Line) bool {
	if t.errorsOnly && l.Level < logs.LWarn {
		return false
	}
	if m.hideOK && l.Access {
		return false
	}
	if m.pinned != "" && l.Corr != m.pinned && !containsToken(l.Plain, m.pinned) {
		return false
	}
	return t.filter == nil || t.filter.MatchString(l.Plain) || t.filter.MatchString(l.Src)
}

// containsToken reports whether id occurs in s as a whole token, so pinning
// req-1 doesn't also match req-13.
func containsToken(s, id string) bool {
	isIDChar := func(b byte) bool {
		return b == '-' || b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
	}
	for from := 0; ; {
		i := strings.Index(s[from:], id)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(id)
		if (start == 0 || !isIDChar(s[start-1])) && (end == len(s) || !isIDChar(s[end])) {
			return true
		}
		from = start + 1
	}
}

func (m *Model) setFlash(s string) {
	m.flash, m.flashUntil = s, time.Now().Add(4*time.Second)
}

func (m *Model) bodyHeight() int {
	h := m.height - 4
	if m.tabs[m.active].redis && !m.inspecting && !m.showHelp {
		h -= len(m.redisPanel())
	}
	return max(h, 1)
}

func (m *Model) matchCount(t *tab) int {
	n := 0
	for _, l := range t.buf {
		if m.matches(t, l) {
			n++
		}
	}
	return n
}

// moveCursor moves the selection by delta entries (positive is older) and
// scrolls only as far as needed to keep it on screen.
func (m *Model) moveCursor(delta int) {
	t := m.tabs[m.active]
	t.cursor = min(max(t.cursor+delta, 0), max(m.matchCount(t)-1, 0))
	visible := max(t.visible, 1)
	if t.cursor < t.scroll {
		t.scroll = t.cursor
	} else if t.cursor >= t.scroll+visible {
		t.scroll = t.cursor - visible + 1
	}
}

func (m *Model) handleKey(msg tea.KeyMsg) tea.Cmd {
	t := m.tabs[m.active]
	key := msg.String()
	switch key {
	case "ctrl+c", "q":
		return tea.Quit
	case "d":
		m.StopOnQuit = false
		return tea.Quit
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		if i := int(key[0] - '1'); i < len(m.tabs) {
			m.switchTab(i)
		}
	case "tab", "right", "l":
		m.switchTab((m.active + 1) % len(m.tabs))
	case "shift+tab", "left", "h":
		m.switchTab((m.active + len(m.tabs) - 1) % len(m.tabs))
	case "up", "k":
		m.moveCursor(1)
	case "down", "j":
		m.moveCursor(-1)
	case "pgup", "ctrl+u":
		m.moveCursor(m.bodyHeight() / 2)
	case "pgdown", "ctrl+d":
		m.moveCursor(-m.bodyHeight() / 2)
	case "g", "home":
		m.moveCursor(maxLines * 2)
	case "G", "end":
		t.follow()
	case "/":
		m.mode = inputFilter
		m.input.Prompt = "filter (regex) › "
		m.input.SetValue(t.filterText)
		m.input.CursorEnd()
		return m.input.Focus()
	case "f":
		m.mode = inputPin
		m.input.Prompt = m.cfg.Correlation.Label + " (tab cycles recent) › "
		m.idCycle = 0
		value := m.pinned
		if value == "" && len(m.recentIDs) > 0 {
			value = m.recentIDs[0]
		}
		m.input.SetValue(value)
		m.input.CursorEnd()
		return m.input.Focus()
	case "F":
		m.applyPin("")
	case "esc":
		if m.showHelp {
			m.showHelp = false
		} else {
			t.filter, t.filterText = nil, ""
			t.follow()
		}
	case "e":
		t.errorsOnly = !t.errorsOnly
		t.follow()
	case "a":
		m.hideOK = !m.hideOK
		m.resetScroll()
	case "t":
		m.expand = !m.expand
	case "s":
		if m.width < splitMinWidth {
			m.setFlash(fmt.Sprintf("split view needs a terminal at least %d columns wide (this one is %d)", splitMinWidth, m.width))
		} else {
			t.split = !t.split
		}
	case "J", "shift+down":
		t.paneScroll += 3
	case "K", "shift+up":
		t.paneScroll -= 3
	case "i", "enter":
		m.openInspector(t)
	case "c":
		t.buf = nil
		t.follow()
	case "y":
		m.copyLastError(t)
	case "o":
		if m.status.URL == "" {
			m.setFlash("nothing to open yet: set open_url, a proxy, or start a dev server that prints its URL")
		} else if err := app.OpenBrowser(m.status.URL); err != nil {
			m.setFlash("open failed: " + err.Error())
		}
	case "m":
		if m.app.Redis == nil {
			m.setFlash("no redis section in the config")
			break
		}
		m.monitorOn = m.app.ToggleMonitor()
		if m.monitorOn {
			m.setFlash("MONITOR on: every Redis command shows in the Redis tab (slows Redis; m to stop)")
		} else {
			m.setFlash("MONITOR off")
		}
	case "?":
		m.showHelp = !m.showHelp
	default:
		if act, ok := m.cfg.Action(key); ok {
			m.setFlash(act.Label + "…")
			m.app.RunAction(act)
		}
	}
	return nil
}

// visibleEntry is the selected entry's index in the buffer.
func (m *Model) visibleEntry(t *tab) (int, bool) {
	skipped := 0
	for i := len(t.buf) - 1; i >= 0; i-- {
		if !m.matches(t, t.buf[i]) {
			continue
		}
		if skipped == t.cursor {
			return i, true
		}
		skipped++
	}
	return 0, false
}

func (m *Model) openInspector(t *tab) {
	i, ok := m.visibleEntry(t)
	if !ok {
		m.setFlash("nothing to inspect in this tab")
		return
	}
	m.inspecting, m.inspected, m.inspectRows, m.inspectScroll = true, t.buf[i], nil, 0
}

func sameEntry(a, b logs.Line) bool {
	return a.Time.Equal(b.Time) && a.Src == b.Src && a.Plain == b.Plain
}

// inspectStep moves the inspector to the next older (-1) or newer (+1)
// matching entry. The buffer may have shifted, so the current entry is found
// again by identity.
func (m *Model) inspectStep(dir int) {
	t := m.tabs[m.active]
	cur := -1
	for i := len(t.buf) - 1; i >= 0; i-- {
		if sameEntry(t.buf[i], m.inspected) {
			cur = i
			break
		}
	}
	if cur < 0 {
		m.setFlash("entry has scrolled out of the buffer")
		return
	}
	for i := cur + dir; i >= 0 && i < len(t.buf); i += dir {
		if m.matches(t, t.buf[i]) {
			m.inspected, m.inspectRows, m.inspectScroll = t.buf[i], nil, 0
			return
		}
	}
	if dir < 0 {
		m.setFlash("this is the oldest matching entry")
	} else {
		m.setFlash("this is the newest matching entry")
	}
}

// selectInspected moves the list selection to the entry the inspector was
// showing, so n / p browsing carries back into the list.
func (m *Model) selectInspected() {
	t := m.tabs[m.active]
	newer := 0
	for i := len(t.buf) - 1; i >= 0; i-- {
		l := t.buf[i]
		if !m.matches(t, l) {
			continue
		}
		if sameEntry(l, m.inspected) {
			m.moveCursor(newer - t.cursor)
			return
		}
		newer++
	}
}

func (m *Model) handleInspectKey(msg tea.KeyMsg) tea.Cmd {
	page := m.bodyHeight() - 1
	switch msg.String() {
	case "ctrl+c":
		return tea.Quit
	case "esc", "q", "i", "enter":
		m.inspecting = false
		m.selectInspected()
	case "up", "k":
		m.inspectScroll--
	case "down", "j":
		m.inspectScroll++
	case "pgup", "ctrl+u", "b":
		m.inspectScroll -= page
	case "pgdown", "ctrl+d", " ", "f":
		m.inspectScroll += page
	case "g", "home":
		m.inspectScroll = 0
	case "G", "end":
		m.inspectScroll = len(m.inspectRows)
	case "n", "left", "h":
		m.inspectStep(-1)
	case "p", "N", "right", "l":
		m.inspectStep(1)
	case "y":
		m.copyEntry(m.inspected, "copied entry")
	}
	return nil
}

func (m *Model) switchTab(i int) {
	m.inspecting = false
	m.active = i
	m.tabs[i].newErrors = 0
}

func (m *Model) resetScroll() {
	for _, t := range m.tabs {
		t.follow()
	}
}

func (m *Model) applyPin(id string) {
	m.pinned = id
	m.app.SetPinned(id)
	m.resetScroll()
}

func (m *Model) updateInput(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "enter":
		value := strings.TrimSpace(m.input.Value())
		if m.mode == inputFilter {
			t := m.tabs[m.active]
			t.filterText, t.filter = value, nil
			t.follow()
			if value != "" {
				re, err := regexp.Compile("(?i)" + value)
				if err != nil {
					re = regexp.MustCompile("(?i)" + regexp.QuoteMeta(value))
				}
				t.filter = re
			}
		} else {
			m.applyPin(value)
		}
		m.mode = inputNone
		m.input.Blur()
		return nil
	case "esc":
		m.mode = inputNone
		m.input.Blur()
		return nil
	case "tab":
		if m.mode == inputPin && len(m.recentIDs) > 0 {
			m.idCycle = (m.idCycle + 1) % len(m.recentIDs)
			m.input.SetValue(m.recentIDs[m.idCycle])
			m.input.CursorEnd()
		}
		return nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return cmd
}

func (m *Model) copyEntry(l logs.Line, what string) {
	text := fmt.Sprintf("[%s] %s %s", l.Src, l.Time.Format(time.RFC3339), l.Plain)
	if len(l.Detail) > 0 {
		text += "\n" + ansi.Strip(strings.Join(l.Detail, "\n"))
	}
	if err := app.CopyToClipboard(text); err != nil {
		m.setFlash("copy failed: " + err.Error())
	} else {
		m.setFlash(fmt.Sprintf("%s (%d lines)", what, len(l.Detail)+1))
	}
}

func (m *Model) copyLastError(t *tab) {
	for i := len(t.buf) - 1; i >= 0; i-- {
		if l := t.buf[i]; l.Level == logs.LError && m.matches(t, l) {
			m.copyEntry(l, "copied last error")
			return
		}
	}
	m.setFlash("no errors in this tab")
}
