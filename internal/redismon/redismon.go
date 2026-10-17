// Package redismon samples a Redis server for the TUI: stream and
// consumer-group backlog, memory, key prefixes, a tail of one stream, and
// MONITOR on demand.
package redismon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/shivamsingh262/tailyard/internal/config"
	"github.com/shivamsingh262/tailyard/internal/logs"
	"github.com/shivamsingh262/tailyard/internal/theme"
)

// clientName marks tailyard's own connections so MONITOR can leave out the
// commands it sends itself.
const clientName = "tailyard"

// Discovery stops after this many streams; list them in the config to see others.
const maxDiscoveredStreams = 15

type ConsumerInfo struct {
	Name    string
	Pending int64
	Idle    time.Duration
}

// StreamInfo is one stream, or one consumer group of a stream.
type StreamInfo struct {
	Name       string
	Group      string
	Alert      bool // any entry is a problem: a dead-letter queue
	Exists     bool
	Len        int64
	Pending    int64
	Lag        int64
	OldestIdle time.Duration
	Consumers  []ConsumerInfo
}

// Problem reports whether the row deserves red: a non-empty alert stream or
// a message pending for over a minute.
func (s StreamInfo) Problem() bool {
	return (s.Alert && s.Len > 0) || s.OldestIdle > time.Minute
}

type PrefixCount struct {
	Prefix string
	Count  int
}

type Snapshot struct {
	Err        string
	Streams    []StreamInfo
	Discovered bool // Streams came from discovery, not the config
	UsedHuman  string
	MaxHuman   string
	UsedBytes  int64
	MaxBytes   int64
	Keys       int64
	OpsPerSec  string
	Prefixes   []PrefixCount
}

type Monitor struct {
	cfg    *config.Redis
	rdb    *redis.Client
	tailRe *regexp.Regexp

	mu        sync.Mutex
	own       map[string]bool // addresses of our own connections
	newest    string          // newest stream matching the tail glob
	following string          // the stream TailLoop follows
	monitor   io.Closer
}

func New(cfg *config.Redis) *Monitor {
	m := &Monitor{
		cfg: cfg,
		rdb: redis.NewClient(&redis.Options{
			Addr: cfg.Addr, Username: cfg.Username, Password: cfg.Password, DB: cfg.DB,
			ClientName: clientName, DialTimeout: time.Second, ReadTimeout: 2 * time.Second, MaxRetries: -1,
		}),
		own: map[string]bool{},
	}
	if cfg.Tail != "" {
		m.tailRe = globRegexp(cfg.Tail)
	}
	return m
}

func (m *Monitor) Close() {
	m.mu.Lock()
	if m.monitor != nil {
		m.monitor.Close()
	}
	m.mu.Unlock()
	_ = m.rdb.Close()
}

func (m *Monitor) Snapshot(ctx context.Context) Snapshot {
	var snap Snapshot
	if err := m.rdb.Ping(ctx).Err(); err != nil {
		snap.Err = fmt.Sprintf("redis unreachable at %s: %v", m.cfg.Addr, err)
		return snap
	}
	refs := m.cfg.Streams
	if len(refs) == 0 {
		refs, snap.Discovered = m.discover(ctx), true
	}
	for _, ref := range refs {
		snap.Streams = append(snap.Streams, m.streamStats(ctx, ref)...)
	}

	info := infoFields(m.rdb.Info(ctx, "memory", "stats").Val())
	snap.UsedHuman, snap.MaxHuman = info["used_memory_human"], info["maxmemory_human"]
	snap.UsedBytes, _ = strconv.ParseInt(info["used_memory"], 10, 64)
	snap.MaxBytes, _ = strconv.ParseInt(info["maxmemory"], 10, 64)
	snap.OpsPerSec = info["instantaneous_ops_per_sec"]
	snap.Keys = m.rdb.DBSize(ctx).Val()

	counts := map[string]int{}
	var tailKeys []string
	iter := m.rdb.Scan(ctx, 0, "*", 1000).Iterator()
	for scanned := 0; scanned < 50000 && iter.Next(ctx); scanned++ {
		key := iter.Val()
		counts[keyPrefix(key)]++
		if m.tailRe != nil && m.tailRe.MatchString(key) {
			tailKeys = append(tailKeys, key)
		}
	}
	for prefix, n := range counts {
		snap.Prefixes = append(snap.Prefixes, PrefixCount{prefix, n})
	}
	sort.Slice(snap.Prefixes, func(i, j int) bool {
		if snap.Prefixes[i].Count != snap.Prefixes[j].Count {
			return snap.Prefixes[i].Count > snap.Prefixes[j].Count
		}
		return snap.Prefixes[i].Prefix < snap.Prefixes[j].Prefix
	})

	newest := m.newestStream(ctx, tailKeys)
	own := m.ownAddrs(ctx)
	m.mu.Lock()
	m.newest = newest
	if own != nil {
		m.own = own
	}
	m.mu.Unlock()
	return snap
}

// discover finds the streams in the database, leaving out the ones the
// events tail covers (there is usually one per job or run).
func (m *Monitor) discover(ctx context.Context) []config.StreamRef {
	var refs []config.StreamRef
	iter := m.rdb.ScanType(ctx, 0, "*", 1000, "stream").Iterator()
	for scanned := 0; scanned < 50000 && iter.Next(ctx); scanned++ {
		key := iter.Val()
		if m.tailRe != nil && m.tailRe.MatchString(key) {
			continue
		}
		lower := strings.ToLower(key)
		refs = append(refs, config.StreamRef{Stream: key, Alert: strings.Contains(lower, "dead") || strings.Contains(lower, "dlq")})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Stream < refs[j].Stream })
	if len(refs) > maxDiscoveredStreams {
		refs = refs[:maxDiscoveredStreams]
	}
	return refs
}

// streamStats returns one row per consumer group of the stream (only the
// configured group, when one is set), or a single row when it has none.
func (m *Monitor) streamStats(ctx context.Context, ref config.StreamRef) []StreamInfo {
	base := StreamInfo{Name: ref.Stream, Group: ref.Group, Alert: ref.Alert}
	if m.rdb.Exists(ctx, ref.Stream).Val() == 0 {
		return []StreamInfo{base}
	}
	base.Exists = true
	base.Len = m.rdb.XLen(ctx, ref.Stream).Val()
	var rows []StreamInfo
	for _, g := range m.rdb.XInfoGroups(ctx, ref.Stream).Val() {
		if ref.Group != "" && g.Name != ref.Group {
			continue
		}
		row := base
		row.Group, row.Pending, row.Lag = g.Name, g.Pending, g.Lag
		for _, c := range m.rdb.XInfoConsumers(ctx, ref.Stream, g.Name).Val() {
			row.Consumers = append(row.Consumers, ConsumerInfo{Name: c.Name, Pending: c.Pending, Idle: c.Idle})
		}
		if row.Pending > 0 {
			pending := m.rdb.XPendingExt(ctx, &redis.XPendingExtArgs{Stream: ref.Stream, Group: g.Name, Start: "-", End: "+", Count: 1}).Val()
			if len(pending) > 0 {
				row.OldestIdle = pending[0].Idle
			}
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		rows = []StreamInfo{base}
	}
	return rows
}

// newestStream returns the stream whose last entry is the most recent.
func (m *Monitor) newestStream(ctx context.Context, keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	pipe := m.rdb.Pipeline()
	cmds := make([]*redis.XMessageSliceCmd, len(keys))
	for i, key := range keys {
		cmds[i] = pipe.XRevRangeN(ctx, key, "+", "-", 1)
	}
	_, _ = pipe.Exec(ctx)
	var newest, newestID string
	for i, cmd := range cmds {
		msgs := cmd.Val()
		if len(msgs) > 0 && streamIDLess(newestID, msgs[0].ID) {
			newest, newestID = keys[i], msgs[0].ID
		}
	}
	return newest
}

var reClientAddr = regexp.MustCompile(`\baddr=(\S+)`)

// ownAddrs lists the addresses of tailyard's connections, or nil when
// CLIENT LIST isn't allowed.
func (m *Monitor) ownAddrs(ctx context.Context) map[string]bool {
	list, err := m.rdb.ClientList(ctx).Result()
	if err != nil {
		return nil
	}
	own := map[string]bool{}
	for _, line := range strings.Split(list, "\n") {
		if !strings.Contains(line, " name="+clientName+" ") {
			continue
		}
		if mm := reClientAddr.FindStringSubmatch(line); mm != nil {
			own[mm[1]] = true
		}
	}
	return own
}

// pinnedStream is the stream named by putting the pinned id into the tail
// glob's *, or "" when nothing is pinned or the glob has no *.
func (m *Monitor) pinnedStream(pinned string) string {
	if pinned == "" || !strings.Contains(m.cfg.Tail, "*") {
		return ""
	}
	return strings.Replace(m.cfg.Tail, "*", pinned, 1)
}

// Following is the stream the tail currently follows.
func (m *Monitor) Following() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.following
}

// TailLoop follows new entries of the pinned id's stream when it exists
// (the pinned id may be a different kind of id, like a request id), else of
// the newest matching stream, until closing returns true.
func (m *Monitor) TailLoop(closing func() bool, pinned func() string, out chan<- logs.Line) {
	if m.cfg.Tail == "" {
		return
	}
	var current, lastID string
	for !closing() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		m.mu.Lock()
		target := m.newest
		m.mu.Unlock()
		if s := m.pinnedStream(pinned()); s != "" && m.rdb.Exists(ctx, s).Val() > 0 {
			target = s
		}
		m.mu.Lock()
		m.following = target
		m.mu.Unlock()
		if target != current {
			current, lastID = target, ""
			if target != "" {
				msgs := m.rdb.XRevRangeN(ctx, target, "+", "-", 50).Val()
				text := fmt.Sprintf("── following %s (last %d entries) ──", target, len(msgs))
				out <- logs.Line{Src: config.SrcEvents, Time: time.Now(), Level: logs.LDebug, Corr: m.idOf(target), Plain: text, Text: theme.Accent.Render(text)}
				for i := len(msgs) - 1; i >= 0; i-- {
					out <- m.eventLine(target, msgs[i])
					lastID = msgs[i].ID
				}
			}
		} else if target != "" {
			start := "-"
			if lastID != "" {
				start = "(" + lastID
			}
			for _, msg := range m.rdb.XRangeN(ctx, target, start, "+", 200).Val() {
				out <- m.eventLine(target, msg)
				lastID = msg.ID
			}
		}
		cancel()
		time.Sleep(time.Second)
	}
}

// idOf is the part of a stream name the tail glob's * matched.
func (m *Monitor) idOf(stream string) string {
	prefix, suffix, ok := strings.Cut(m.cfg.Tail, "*")
	if !ok || !strings.HasPrefix(stream, prefix) || !strings.HasSuffix(stream, suffix) {
		return ""
	}
	return stream[len(prefix) : len(stream)-len(suffix)]
}

var titleFields = []string{"event_type", "type", "event", "name", "action", "status"}

func (m *Monitor) eventLine(stream string, msg redis.XMessage) logs.Line {
	ms, _ := splitStreamID(msg.ID)
	id := m.idOf(stream)
	l := logs.Line{Src: config.SrcEvents, Time: time.UnixMilli(ms), Level: logs.LInfo, Corr: id}

	title, titleKey := msg.ID, ""
	for _, k := range titleFields {
		if v, ok := msg.Values[k].(string); ok && v != "" {
			title, titleKey = v, k
			break
		}
	}
	lower := strings.ToLower(title)
	switch {
	case strings.Contains(lower, "fail") || strings.Contains(lower, "error"):
		l.Level = logs.LError
	case strings.Contains(lower, "warn") || strings.Contains(lower, "cancel") || strings.Contains(lower, "retry") || strings.Contains(lower, "timeout"):
		l.Level = logs.LWarn
	}

	// Fields holding JSON objects are flattened one level, so their keys
	// show inline like the others.
	fields := map[string]any{}
	for k, v := range msg.Values {
		if k == titleKey {
			continue
		}
		s, _ := v.(string)
		dec := json.NewDecoder(strings.NewReader(s))
		dec.UseNumber()
		var obj map[string]any
		if strings.HasPrefix(strings.TrimSpace(s), "{") && dec.Decode(&obj) == nil {
			for ik, iv := range obj {
				fields[ik] = iv
			}
		} else {
			fields[k] = v
		}
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var plain, styled strings.Builder
	plain.WriteString(title)
	if id != "" {
		styled.WriteString(theme.Key.Render(id[:min(8, len(id))]) + " ")
	}
	styled.WriteString(logs.StyleFor(l.Level, theme.Event.Render(title)))
	for _, k := range keys {
		v := logs.CompactValue(fields[k], 100)
		plain.WriteString(" " + k + "=" + v)
		styled.WriteString("  " + theme.Key.Render(k+"=") + v)
	}
	l.Plain, l.Text = plain.String(), styled.String()
	pretty, _ := json.MarshalIndent(fields, "  ", "  ")
	l.Detail = strings.Split("  "+string(pretty), "\n")
	return l
}

var reMonitor = regexp.MustCompile(`^\+?(\d+)\.(\d+) \[(\d+) ([^\]]+)\] (.*)$`)

// quietCommands are the polling commands workers issue constantly; they are
// shown dimmed so the interesting ones stand out.
var quietCommands = map[string]bool{"PING": true, "XREADGROUP": true, "XAUTOCLAIM": true, "BRPOP": true, "BLPOP": true, "BZPOPMIN": true, "INFO": true}

// ToggleMonitor starts or stops a MONITOR session on its own connection and
// reports whether it is now on.
func (m *Monitor) ToggleMonitor(out chan<- logs.Line) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.monitor != nil {
		m.monitor.Close()
		m.monitor = nil
		return false, nil
	}
	conn, err := net.DialTimeout("tcp", m.cfg.Addr, time.Second)
	if err != nil {
		return false, err
	}
	m.monitor = conn
	var cmds []byte
	if m.cfg.Password != "" {
		args := []string{"AUTH", m.cfg.Password}
		if m.cfg.Username != "" {
			args = []string{"AUTH", m.cfg.Username, m.cfg.Password}
		}
		cmds = append(cmds, resp(args...)...)
	}
	cmds = append(cmds, resp("MONITOR")...)

	go func() {
		if _, err := conn.Write(cmds); err != nil {
			return
		}
		rd := bufio.NewReader(conn)
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "-") {
				out <- logs.System(logs.LError, "MONITOR: "+strings.TrimPrefix(line, "-"))
				continue
			}
			mm := reMonitor.FindStringSubmatch(line)
			if mm == nil {
				continue
			}
			client, command := mm[4], mm[5]
			m.mu.Lock()
			ours := m.own[client]
			m.mu.Unlock()
			if ours {
				continue
			}
			sec, _ := strconv.ParseInt(mm[1], 10, 64)
			usec, _ := strconv.ParseInt(mm[2], 10, 64)
			l := logs.Line{Src: config.SrcMonitor, Time: time.Unix(sec, usec*1000), Level: logs.LInfo, Plain: client + " " + command}
			name := strings.ToUpper(strings.Trim(strings.SplitN(command, " ", 2)[0], `"`))
			if quietCommands[name] {
				l.Level, l.Text = logs.LDebug, theme.Dim.Render(client+" "+command)
			} else {
				l.Text = theme.Key.Render(client) + " " + command
			}
			out <- l
		}
	}()
	return true, nil
}

// resp encodes a command in the Redis protocol.
func resp(args ...string) []byte {
	b := []byte(fmt.Sprintf("*%d\r\n", len(args)))
	for _, a := range args {
		b = append(b, fmt.Sprintf("$%d\r\n%s\r\n", len(a), a)...)
	}
	return b
}

func streamIDLess(a, b string) bool {
	if a == "" {
		return b != ""
	}
	am, as := splitStreamID(a)
	bm, bs := splitStreamID(b)
	return am < bm || (am == bm && as < bs)
}

func splitStreamID(id string) (int64, int64) {
	msPart, seqPart, _ := strings.Cut(id, "-")
	ms, _ := strconv.ParseInt(msPart, 10, 64)
	seq, _ := strconv.ParseInt(seqPart, 10, 64)
	return ms, seq
}

// keyPrefix groups keys by their first segment, split on : (or . when there is no :).
func keyPrefix(key string) string {
	for _, sep := range []string{":", "."} {
		if prefix, _, ok := strings.Cut(key, sep); ok && prefix != "" {
			return prefix
		}
	}
	return "(no prefix)"
}

func infoFields(info string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(info, "\r\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			fields[k] = v
		}
	}
	return fields
}

// globRegexp converts a Redis-style glob (* and ?) to an anchored regexp.
func globRegexp(glob string) *regexp.Regexp {
	q := regexp.QuoteMeta(glob)
	q = strings.ReplaceAll(q, `\*`, ".*")
	q = strings.ReplaceAll(q, `\?`, ".")
	return regexp.MustCompile("^" + q + "$")
}
