package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/shivamsingh262/tailyard/internal/app"
	"github.com/shivamsingh262/tailyard/internal/config"
	"github.com/shivamsingh262/tailyard/internal/logs"
	"github.com/shivamsingh262/tailyard/internal/theme"
)

// The split view (list + detail pane) needs at least this many columns.
const splitMinWidth = 160

func (m *Model) View() string {
	if m.width == 0 {
		return "starting…"
	}
	var b strings.Builder
	b.WriteString(m.statusBar() + "\n")
	b.WriteString(m.tabBar() + "\n")
	if m.tabs[m.active].redis && !m.inspecting && !m.showHelp {
		for _, row := range m.redisPanel() {
			b.WriteString(ansi.Truncate(row, m.width, "…") + "\n")
		}
	}
	h := m.bodyHeight()
	var rows []string
	switch {
	case m.showHelp:
		rows = m.helpRows()
	case m.inspecting:
		rows = m.inspectorRows()
	case m.splitActive():
		rows = m.splitRows(h)
	default:
		rows = m.bodyRows(m.width, h)
	}
	for i := 0; i < h; i++ {
		if i < len(rows) {
			b.WriteString(rows[i])
		}
		b.WriteString("\n")
	}
	b.WriteString(m.infoLine() + "\n")
	b.WriteString(m.promptLine())
	return b.String()
}

func (m *Model) splitActive() bool {
	return m.tabs[m.active].split && m.width >= splitMinWidth && !m.inspecting && !m.showHelp
}

// ── header ────────────────────────────────────────────────────────────────

var sep = " " + theme.Sep.Render("│") + " "

// serviceLabel is a status dot plus name, followed by a short reason when
// the service isn't healthy ("api ● 502", "worker ● exited 1").
func serviceLabel(name string, s app.ServiceState) string {
	dot := theme.OK
	switch s.Health {
	case app.HealthStarting:
		dot = theme.Warn
	case app.HealthDown:
		dot = theme.Err
	}
	label := dot.Render("●") + " " + name
	if s.Reason != "" {
		label += " " + dot.Render(s.Reason)
	}
	return label
}

func (m *Model) statusBar() string {
	var services []string
	for _, src := range m.cfg.Sources {
		if st, ok := m.status.Services[src.Name]; ok {
			services = append(services, serviceLabel(src.Name, st))
		}
	}
	if m.status.Services == nil && len(m.cfg.Sources) > 0 {
		services = []string{theme.Dim.Render("checking services…")}
	}
	parts := []string{" " + theme.Brand.Render(m.cfg.Name)}
	if len(services) > 0 {
		parts = append(parts, strings.Join(services, "  "))
	}
	if badge := m.redisBadge(); badge != "" {
		parts = append(parts, badge)
	}
	if m.busy != "" {
		parts = append(parts, theme.Accent.Render("⟳ "+m.busy))
	}
	left := strings.Join(parts, sep)
	right := theme.Dim.Render(time.Now().Format("15:04:05")) + " "
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return ansi.Truncate(left, m.width, "…")
	}
	return left + strings.Repeat(" ", gap) + right
}

// redisBadge sums the backlog of every consumer group: "queues 3·0", red
// with the oldest pending age once something has waited over a minute.
func (m *Model) redisBadge() string {
	if !m.haveRedis {
		return ""
	}
	if m.redis.Err != "" {
		return theme.Err.Render("redis unreachable")
	}
	var pending, lag, alerts int64
	var oldest time.Duration
	groups := 0
	for _, s := range m.redis.Streams {
		if s.Alert {
			alerts += s.Len
		}
		if s.Group == "" {
			continue
		}
		groups++
		pending += s.Pending
		lag += s.Lag
		oldest = max(oldest, s.OldestIdle)
	}
	if groups == 0 && alerts == 0 {
		return ""
	}
	badge := fmt.Sprintf("queues %d·%d", pending, lag)
	switch {
	case oldest > time.Minute:
		badge = theme.ErrB.Render(badge + " oldest " + oldest.Round(time.Second).String())
	case pending > 0 || lag > 0:
		badge = theme.Warn.Render(badge)
	default:
		badge = theme.Dim.Render(badge)
	}
	if alerts > 0 {
		badge += " " + theme.ErrB.Render(fmt.Sprintf("DLQ %d", alerts))
	}
	return badge
}

func shortCount(n int) string {
	if n < 1000 {
		return fmt.Sprint(n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

func (m *Model) tabBar() string {
	var parts []string
	for i, t := range m.tabs {
		name := theme.Tab.Render(t.name)
		if i == m.active {
			name = theme.TabOn.Render(t.name)
		}
		label := name
		if i < 9 {
			label = theme.Dim.Render(fmt.Sprint(i+1)) + " " + name
		}
		if len(t.buf) > 0 {
			label += " " + theme.Dim.Render(shortCount(len(t.buf)))
		}
		if t.newErrors > 0 {
			label += " " + theme.ErrB.Render(fmt.Sprintf("●%d", t.newErrors))
		}
		parts = append(parts, label)
	}
	return ansi.Truncate(" "+strings.Join(parts, sep), m.width, "…")
}

// ── list ──────────────────────────────────────────────────────────────────

func (m *Model) stampOf(l logs.Line) string {
	if y, mo, d := l.Time.Date(); y != m.started.Year() || mo != m.started.Month() || d != m.started.Day() {
		return l.Time.Format("Jan 02 15:04")
	}
	return l.Time.Format("15:04:05")
}

// entryText is an entry's message: request rows are laid out in columns,
// everything else uses its pre-styled Text.
func entryText(l logs.Line, pathWidth int) string {
	if l.Req != nil {
		return renderRequest(l.Req, pathWidth)
	}
	return l.Text
}

func (m *Model) entryHeight(l logs.Line) int {
	if !m.expand || len(l.Detail) == 0 {
		return 1
	}
	if len(l.Detail) > inlineDetailLines {
		return 1 + inlineDetailLines + 1
	}
	return 1 + len(l.Detail)
}

func levelBadge(level logs.Level) string {
	switch level {
	case logs.LDebug:
		return theme.Dim.Render("DBG")
	case logs.LWarn:
		return theme.WarnB.Render("WRN")
	case logs.LError:
		return theme.ErrB.Render("ERR")
	}
	return theme.Info.Render("INF")
}

// highlight paints a whole row with the selection background, re-applying it
// after every reset inside the row's own styling.
func highlight(row string, width int) string {
	bg := theme.SelectBgSeq()
	row = strings.ReplaceAll(row, "\x1b[0m", "\x1b[0m"+bg)
	return bg + row + strings.Repeat(" ", max(width-ansi.StringWidth(row), 0)) + "\x1b[0m"
}

func (m *Model) renderLine(t *tab, l logs.Line, width, pathWidth int, selected, showStamp bool) []string {
	stamp := m.stampOf(l)
	if !showStamp {
		stamp = strings.Repeat(" ", len(stamp))
	}
	gutter := " "
	if selected {
		gutter = theme.Gutter.Render("▌")
	}
	row := gutter + theme.Dim.Render(stamp) + "  "
	if t.multi {
		row += theme.Source(l.Src).Render(fmt.Sprintf("%-9s", ansi.Truncate(l.Src, 9, "…"))) + " "
	}
	text := entryText(l, pathWidth)
	if l.Req == nil {
		row += levelBadge(l.Level) + " "
	} else if l.Level == logs.LDebug {
		text = theme.Dim.Render(ansi.Strip(text))
	}
	row += text
	if len(l.Detail) > 0 && !m.expand && !m.splitActive() {
		row += theme.Dim.Render(fmt.Sprintf("  ▸%d", len(l.Detail)))
	}
	row = ansi.Truncate(row, width, "…")
	if selected {
		row = highlight(row, width)
	}
	rows := []string{row}
	if m.expand {
		indent := strings.Repeat(" ", len(stamp)+3)
		for i, d := range l.Detail {
			if i == inlineDetailLines {
				rows = append(rows, theme.Dim.Render(fmt.Sprintf("%s… %d more lines — i to inspect", indent, len(l.Detail)-i)))
				break
			}
			rows = append(rows, ansi.Truncate(theme.Dim.Render(indent+"│ ")+d, width, "…"))
		}
	}
	return rows
}

// bodyRows renders the active tab's list into at most height rows of the
// given width: newest at the bottom, `scroll` entries hidden below.
func (m *Model) bodyRows(width, height int) []string {
	t := m.tabs[m.active]
	type shown struct {
		idx      int
		selected bool
	}
	var entries []shown // newest first
	used, skipped, pathWidth := 0, 0, 0
	for i := len(t.buf) - 1; i >= 0 && used < height; i-- {
		l := t.buf[i]
		if !m.matches(t, l) {
			continue
		}
		if skipped < t.scroll {
			skipped++
			continue
		}
		entries = append(entries, shown{i, skipped+len(entries) == t.cursor})
		used += m.entryHeight(l)
		if l.Req != nil {
			pathWidth = max(pathWidth, min(requestPathWidth(l.Req), 40))
		}
	}
	t.visible = len(entries)
	if used > height && len(entries) > 1 {
		// The oldest entry is only partly on screen; don't count it as visible.
		t.visible--
	}
	if len(entries) == 0 {
		return []string{theme.Dim.Render("  (no output yet" + m.emptyHint(t) + ")")}
	}

	var rows []string
	prevStamp := ""
	for i := len(entries) - 1; i >= 0; i-- {
		l := t.buf[entries[i].idx]
		stamp := m.stampOf(l)
		rows = append(rows, m.renderLine(t, l, width, pathWidth, entries[i].selected, stamp != prevStamp || entries[i].selected)...)
		prevStamp = stamp
	}
	if len(rows) > height {
		// The oldest entry was taller than the space left; keep its tail.
		rows = rows[len(rows)-height:]
	}
	return rows
}

func (m *Model) emptyHint(t *tab) string {
	if t.filter != nil || t.errorsOnly || m.pinned != "" {
		return " matching the current filters; esc / e / F clear them"
	}
	switch {
	case t.srcs[config.SrcHTTP] && len(t.srcs) == 1 && m.app.Proxy != nil:
		return " — requests through " + m.app.Proxy.URL() + " show here"
	case t.srcs[config.SrcBrowser] && len(t.srcs) == 1 && m.app.Proxy != nil:
		return " — open the app through " + m.app.Proxy.URL() + "; its console shows here"
	}
	return ""
}

// ── detail pane and inspector ─────────────────────────────────────────────

// detailRows renders one entry in full, wrapped to width: a header, the
// message, then every detail line.
func (m *Model) detailRows(l logs.Line, width int) []string {
	header := theme.Dim.Render(l.Time.Format("2006-01-02 15:04:05.000")) + "  " +
		theme.Source(l.Src).Render(l.Src) + "  " + levelBadge(l.Level)
	if l.Corr != "" {
		header += "  " + theme.Key.Render(m.cfg.Correlation.Label+"=") + l.Corr
	}
	text := l.Text
	if l.Req != nil {
		// The full request, IDs unshortened.
		text = theme.Method(l.Req.Method).Render(l.Req.Method) + " " + theme.Status(l.Req.Status).Render(fmt.Sprint(l.Req.Status)) + " " + l.Req.Path
		if l.Req.Ms >= 0 {
			text += "  " + theme.Duration(l.Req.Ms).Render(fmt.Sprintf("%dms", l.Req.Ms))
		}
	}
	rows := []string{header}
	rows = append(rows, strings.Split(ansi.Hardwrap(text, width, true), "\n")...)
	if l.Req != nil && l.Req.Query != "" {
		rows = append(rows, theme.Heading.Render("▸ query"))
		for _, pair := range strings.Split(l.Req.Query, "&") {
			k, v, _ := strings.Cut(pair, "=")
			rows = append(rows, strings.Split(ansi.Hardwrap(theme.Key.Render(logs.Unescape(k))+" = "+logs.Unescape(v), width, true), "\n")...)
		}
	}
	rows = append(rows, theme.Sep.Render(strings.Repeat("─", width)))
	for _, d := range l.Detail {
		rows = append(rows, strings.Split(ansi.Hardwrap(d, width, false), "\n")...)
	}
	if len(l.Detail) == 0 {
		rows = append(rows, theme.Dim.Render("(no further detail)"))
	}
	return rows
}

func entryKey(l logs.Line) string {
	return l.Time.String() + "\x00" + l.Src + "\x00" + l.Plain
}

func (m *Model) inspectorRows() []string {
	if m.inspectRows == nil || m.inspectWidth != m.width {
		m.inspectRows, m.inspectWidth = m.detailRows(m.inspected, m.width), m.width
	}
	h := m.bodyHeight()
	m.inspectScroll = min(max(m.inspectScroll, 0), max(len(m.inspectRows)-h, 0))
	return m.inspectRows[m.inspectScroll:min(m.inspectScroll+h, len(m.inspectRows))]
}

// splitRows renders the list on the left and the selected entry's detail on
// the right.
func (m *Model) splitRows(height int) []string {
	t := m.tabs[m.active]
	leftW := m.width * 11 / 20
	rightW := m.width - leftW - 3
	left := m.bodyRows(leftW, height)

	var right []string
	if i, ok := m.visibleEntry(t); ok {
		l := t.buf[i]
		key := entryKey(l)
		if key != t.paneKey {
			t.paneKey, t.paneScroll = key, 0
		}
		if m.paneCache.key != key || m.paneCache.width != rightW {
			m.paneCache.key, m.paneCache.width, m.paneCache.rows = key, rightW, m.detailRows(l, rightW)
		}
		right = m.paneCache.rows
		t.paneScroll = min(max(t.paneScroll, 0), max(len(right)-height, 0))
		right = right[t.paneScroll:]
	} else {
		right = []string{theme.Dim.Render("(nothing selected)")}
	}

	rows := make([]string, height)
	for i := range rows {
		var l, r string
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		rows[i] = l + strings.Repeat(" ", max(leftW-ansi.StringWidth(l), 0)) + " " + theme.Sep.Render("│") + " " + r
	}
	return rows
}

// ── footer ────────────────────────────────────────────────────────────────

func (m *Model) infoLine() string {
	t := m.tabs[m.active]
	var parts []string
	if m.inspecting {
		total := len(m.inspectRows)
		parts = append(parts, theme.Accent.Render("inspecting")+theme.Dim.Render(fmt.Sprintf("  line %d of %d", min(m.inspectScroll+1, total), total)))
	} else {
		parts = append(parts, m.filterParts(t)...)
	}
	left := " " + strings.Join(parts, "  ")
	if m.flash != "" && time.Now().Before(m.flashUntil) {
		left += "  " + theme.Accent.Render(m.flash)
	}
	return theme.Bar.Render(ansi.Truncate(left, m.width, "…") + strings.Repeat(" ", max(0, m.width-lipgloss.Width(left))))
}

// filterParts describes the active filters and selection state of a tab.
func (m *Model) filterParts(t *tab) []string {
	var parts []string
	if t.filterText != "" {
		parts = append(parts, theme.Key.Render("filter:")+" /"+t.filterText+"/")
	}
	if m.pinned != "" {
		parts = append(parts, theme.Key.Render(m.cfg.Correlation.Label+":")+" "+m.pinned)
	}
	if t.errorsOnly {
		parts = append(parts, theme.Warn.Render("warn+err only"))
	}
	if m.hideOK {
		parts = append(parts, theme.Key.Render("hiding OK requests"))
	}
	if m.monitorOn {
		parts = append(parts, theme.Warn.Render("MONITOR on"))
	}
	if t.cursor > 0 {
		parts = append(parts, theme.Accent.Render(fmt.Sprintf("▌ entry %d of %d", t.cursor+1, m.matchCount(t)))+theme.Dim.Render(" · G follow newest"))
	} else {
		parts = append(parts, theme.Dim.Render("following"))
	}
	return parts
}

func hints(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, theme.HintKey.Render(pairs[i])+" "+theme.Dim.Render(pairs[i+1]))
	}
	return " " + strings.Join(parts, "   ")
}

// promptLine shows the keys that matter in the current mode; ? lists the rest.
func (m *Model) promptLine() string {
	var line string
	switch {
	case m.mode != inputNone:
		return m.input.View()
	case m.showHelp:
		line = hints("esc", "close help")
	case m.inspecting:
		line = hints("↑↓", "scroll", "n/p", "older/newer", "y", "copy", "esc", "close")
	default:
		pairs := []string{"↑↓", "select", "i", "inspect", "/", "filter", "e", "errors", "f", "pin " + m.cfg.Correlation.Label}
		if m.width >= splitMinWidth {
			pairs = append(pairs, "s", "split")
		}
		if m.splitActive() {
			pairs = append(pairs, "J/K", "scroll pane")
		}
		if m.tabs[m.active].redis {
			pairs = append(pairs, "m", "monitor")
		}
		pairs = append(pairs, "?", "more")
		line = hints(pairs...)
	}
	return ansi.Truncate(line, m.width, "…")
}

func (m *Model) helpRows() []string {
	section := func(title string) string { return theme.Heading.Render("  " + title) }
	key := func(k, desc string) string { return "    " + theme.HintKey.Render(fmt.Sprintf("%-12s", k)) + desc }
	label := m.cfg.Correlation.Label
	rows := []string{
		"",
		section("Navigation"),
		key("1-9 tab ←→", "switch tab"),
		key("↑↓ j k", "move the selection (▌)"),
		key("pgup pgdn", "move half a page (also ctrl+u / ctrl+d)"),
		key("g / G", "oldest entry / follow the newest"),
		"",
		section("Reading entries"),
		key("i  enter", "open the selected entry full-screen: request + response, tracebacks, payloads"),
		key("", "inside: n / p older / newer entry, y copy it, esc close"),
		key("s", fmt.Sprintf("split view: list + detail pane (terminals ≥ %d columns; on in HTTP)", splitMinWidth)),
		key("J K", "scroll the detail pane"),
		key("t", fmt.Sprintf("expand details inline (first %d lines each)", inlineDetailLines)),
		"",
		section("Filtering"),
		key("/", "regex filter for this tab (esc clears)"),
		key("e", "warnings + errors only (this tab)"),
		key("f  F", "pin a "+label+" across all tabs / clear it (tab cycles recent ones)"),
		key("a", "hide 2xx/3xx access-log lines"),
		"",
		section("Actions"),
		key("y", "copy the last error in this tab (with its traceback)"),
		key("o", "open the app in the browser"),
		key("m", "toggle Redis MONITOR (every command; slows Redis)"),
		key("c", "clear this tab"),
	}
	for _, a := range m.cfg.Actions {
		rows = append(rows, key(a.Key, a.Label))
	}
	rows = append(rows, "",
		section("Quit"),
		key("q", "quit (and docker compose stop, when tailyard started the containers)"),
		key("d", "detach: quit and leave the containers running"),
		"",
		theme.Dim.Render("  Light terminal background? Set theme: light in the config or pass -theme light."),
	)
	return rows
}

// ── Redis panel ───────────────────────────────────────────────────────────

func (m *Model) redisPanel() []string {
	if !m.haveRedis {
		return []string{theme.Dim.Render(" connecting to Redis at " + m.cfg.Redis.Addr + "…"), ""}
	}
	r := m.redis
	if r.Err != "" {
		return []string{theme.Err.Render(" " + r.Err), ""}
	}
	var rows []string
	if len(r.Streams) > 0 {
		rows = append(rows, theme.Heading.Render(fmt.Sprintf(" %-28s %-20s %7s %8s %6s %8s   %s", "STREAM", "GROUP", "LEN", "PENDING", "LAG", "OLDEST", "CONSUMERS")))
	} else if r.Discovered {
		rows = append(rows, theme.Dim.Render(" no streams yet"))
	}
	for _, s := range r.Streams {
		if !s.Exists {
			rows = append(rows, fmt.Sprintf(" %-28s %-20s %7s", ansi.Truncate(s.Name, 28, "…"), s.Group, theme.Dim.Render("—")))
			continue
		}
		oldest := "—"
		if s.OldestIdle > 0 {
			oldest = s.OldestIdle.Round(time.Second).String()
		}
		var consumers []string
		for _, c := range s.Consumers {
			consumers = append(consumers, fmt.Sprintf("%s idle %s p%d", c.Name, c.Idle.Round(time.Second), c.Pending))
		}
		group := s.Group
		if group == "" {
			group = "—"
		}
		row := fmt.Sprintf(" %-28s %-20s %7d %8d %6d %8s   %s", ansi.Truncate(s.Name, 28, "…"), ansi.Truncate(group, 20, "…"), s.Len, s.Pending, s.Lag, oldest, strings.Join(consumers, " · "))
		switch {
		case s.Problem():
			row = theme.Err.Render(row)
		case s.Pending > 0 || s.Lag > 0:
			row = theme.Warn.Render(row)
		}
		rows = append(rows, row)
	}

	memory := " " + theme.Heading.Render("MEMORY") + " " + r.UsedHuman
	if r.MaxBytes > 0 {
		pct := float64(r.UsedBytes) / float64(r.MaxBytes)
		filled := min(int(pct*20), 20)
		bar := strings.Repeat("█", filled) + strings.Repeat("░", 20-filled)
		barStyle := theme.OK
		if pct > 0.8 {
			barStyle = theme.Err
		} else if pct > 0.5 {
			barStyle = theme.Warn
		}
		memory += " / " + r.MaxHuman + " " + barStyle.Render(bar) + fmt.Sprintf(" %.0f%%", pct*100)
	}
	memory += fmt.Sprintf("   %s %d   %s %s", theme.Heading.Render("KEYS"), r.Keys, theme.Heading.Render("OPS/S"), r.OpsPerSec)
	rows = append(rows, memory)

	var prefixes []string
	for i, p := range r.Prefixes {
		if i == 8 {
			break
		}
		prefixes = append(prefixes, fmt.Sprintf("%s %d", p.Prefix, p.Count))
	}
	rows = append(rows, " "+theme.Heading.Render("PREFIXES")+" "+strings.Join(prefixes, theme.Dim.Render(" · ")))

	if m.cfg.Redis.Tail != "" {
		following := m.app.Redis.Following()
		if following == "" {
			following = theme.Dim.Render("no stream matches " + m.cfg.Redis.Tail + " yet")
		}
		rows = append(rows, " "+theme.Heading.Render("TAIL")+" "+following+theme.Dim.Render("  (f pins a "+m.cfg.Correlation.Label+" · t shows payloads · m toggles MONITOR)"))
	}
	rows = append(rows, theme.Sep.Render(strings.Repeat("─", max(m.width, 1))))
	return rows
}
