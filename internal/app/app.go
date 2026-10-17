// Package app starts and supervises everything tailyard watches: the compose
// project, owned commands, file tails, the proxy and the Redis sampler. It
// feeds log lines and status to the UI.
package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/shivamsingh262/tailyard/internal/config"
	"github.com/shivamsingh262/tailyard/internal/logs"
	"github.com/shivamsingh262/tailyard/internal/proxy"
	"github.com/shivamsingh262/tailyard/internal/redismon"
)

type App struct {
	Cfg     *config.Config
	Program *tea.Program
	Lines   chan logs.Line
	Redis   *redismon.Monitor // nil without a redis section
	Proxy   *proxy.Proxy      // nil without a proxy section
	Corr    *logs.Correlator

	closing atomic.Bool
	busy    atomic.Bool
	HungUp  atomic.Bool // the terminal closed; nothing can be printed

	mu       sync.Mutex
	running  map[*exec.Cmd]bool
	commands map[string]*command
	pinned   string
	devURL   string // a dev server's URL, read from its output
}

// command is a running (or exited) command source.
type command struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error // set once done is closed
}

// Messages sent to the UI.
type (
	BusyMsg   string
	RedisMsg  redismon.Snapshot
	StatusMsg struct {
		Services map[string]ServiceState
		URL      string // what o opens
	}
)

type Health int

const (
	HealthOK Health = iota
	HealthStarting
	HealthDown
)

type ServiceState struct {
	Health Health
	Reason string // why it isn't OK: "exited 1", "502", "missing"
}

func New(cfg *config.Config) (*App, error) {
	a := &App{
		Cfg:      cfg,
		Lines:    make(chan logs.Line, 4096),
		running:  map[*exec.Cmd]bool{},
		commands: map[string]*command{},
		Corr:     &logs.Correlator{Keys: cfg.Correlation.Keys, Pattern: cfg.Correlation.Regexp},
	}
	if cfg.Redis != nil {
		a.Redis = redismon.New(cfg.Redis)
	}
	if cfg.Proxy != nil {
		p, err := proxy.New(cfg.Proxy, a.Corr, func(l logs.Line) { a.Lines <- l })
		if err != nil {
			return nil, err
		}
		a.Proxy = p
	}
	return a, nil
}

func (a *App) emit(level logs.Level, s string) {
	a.Lines <- logs.System(level, s)
}

func (a *App) Closing() bool { return a.closing.Load() }

// Start launches everything in the background. Program must be set.
func (a *App) Start() {
	go a.statusLoop()
	if a.Redis != nil {
		go a.redisLoop()
		go a.Redis.TailLoop(a.Closing, a.Pinned, a.Lines)
	}
	if a.Proxy != nil {
		if err := a.Proxy.Start(); err != nil {
			a.emit(logs.LError, "proxy: "+err.Error())
		} else {
			a.emit(logs.LInfo, fmt.Sprintf("proxy listening on %s → %s", a.Proxy.URL(), a.Cfg.Proxy.Target))
		}
	}
	for _, src := range a.Cfg.Sources {
		switch src.Kind() {
		case "command":
			go a.runCommand(src)
		case "file":
			go a.tailFile(src)
		}
	}
	go func() {
		if a.Cfg.ComposeUp() {
			args := []string{"up", "-d"}
			if a.Cfg.Compose.Build {
				args = append(args, "--build")
			}
			a.compose("starting containers", args...)
		}
		for _, src := range a.Cfg.Sources {
			if src.Kind() == "compose" {
				go a.followCompose(src)
			}
		}
	}()
}

func (a *App) parser(src config.Source) *logs.Parser {
	return &logs.Parser{Src: src.Name, Format: src.Parser, Corr: a.Corr, Out: a.Lines, OnURL: a.setDevURL}
}

func (a *App) dockerCmd(args ...string) *exec.Cmd {
	c := a.Cfg.Compose
	full := []string{"compose"}
	for _, f := range c.Files {
		full = append(full, "-f", f)
	}
	if c.Project != "" {
		full = append(full, "-p", c.Project)
	}
	cmd := exec.Command("docker", append(full, args...)...)
	cmd.Dir = c.Dir
	return cmd
}

// composeCommandLine is the docker compose invocation, for messages.
func (a *App) composeCommandLine() string {
	parts := []string{"docker compose"}
	for _, f := range a.Cfg.Compose.Files {
		parts = append(parts, "-f", f)
	}
	if a.Cfg.Compose.Project != "" {
		parts = append(parts, "-p", a.Cfg.Compose.Project)
	}
	return strings.Join(parts, " ")
}

var errClosing = errors.New("tailyard is shutting down")

// pump runs cmd, feeding its combined output through p, and returns when the
// process exits. With dockerTS, each line's leading timestamp becomes its
// time; lines at or before skipUntil are dropped (they were shown before a
// reconnect) and *last tracks the newest one.
func (a *App) pump(cmd *exec.Cmd, p *logs.Parser, dockerTS bool, skipUntil time.Time, last *time.Time) error {
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	setProcGroup(cmd)
	// Starting and registering under the lock that Shutdown holds while it
	// sets closing means no process can start after Shutdown's kill sweep.
	a.mu.Lock()
	if a.closing.Load() {
		a.mu.Unlock()
		return errClosing
	}
	if err := cmd.Start(); err != nil {
		a.mu.Unlock()
		return err
	}
	a.running[cmd] = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.running, cmd)
		a.mu.Unlock()
	}()

	waitErr := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		pw.Close()
		waitErr <- err
	}()

	raw := make(chan logs.Raw, 256)
	parsed := make(chan struct{})
	go func() {
		p.Run(raw)
		close(parsed)
	}()

	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		text, t := sc.Text(), time.Now()
		if dockerTS {
			if ts, rest, ok := strings.Cut(text, " "); ok {
				if parsedTS, err := time.Parse(time.RFC3339Nano, ts); err == nil {
					if !parsedTS.After(skipUntil) {
						continue
					}
					t, text, *last = parsedTS.Local(), rest, parsedTS
				}
			}
		}
		raw <- logs.Raw{Time: t, Text: text}
	}
	_, _ = io.Copy(io.Discard, pr) // keep the writer unblocked if the scanner gave up
	close(raw)
	<-parsed
	return <-waitErr
}

// followCompose streams one service's logs, reconnecting when the container
// is recreated or restarted.
func (a *App) followCompose(src config.Source) {
	var last time.Time
	p := a.parser(src)
	for !a.closing.Load() {
		args := []string{"logs", "-f", "--timestamps", "--no-color", "--no-log-prefix"}
		if last.IsZero() {
			args = append(args, "--tail", strconv.Itoa(a.Cfg.Compose.Tail))
		} else {
			args = append(args, "--since", last.Format(time.RFC3339Nano))
		}
		_ = a.pump(a.dockerCmd(append(args, src.Compose)...), p, true, last, &last)
		time.Sleep(2 * time.Second)
	}
}

// compose runs one docker compose command at a time, streaming its output
// into the System tab.
func (a *App) compose(label string, args ...string) {
	a.exclusive(label, "$ docker compose "+strings.Join(args, " "), func() error {
		return a.pump(a.dockerCmd(args...), a.selfParser(), false, time.Time{}, nil)
	})
}

func (a *App) selfParser() *logs.Parser {
	return &logs.Parser{Src: config.SrcSelf, Format: "plain", Corr: a.Corr, Out: a.Lines}
}

// exclusive runs one long action at a time, showing it in the header.
func (a *App) exclusive(label, announce string, run func() error) {
	if !a.busy.CompareAndSwap(false, true) {
		a.emit(logs.LWarn, "another action is still running")
		return
	}
	defer a.busy.Store(false)
	a.Program.Send(BusyMsg(label))
	defer a.Program.Send(BusyMsg(""))

	a.emit(logs.LInfo, announce)
	started := time.Now()
	if err := run(); err != nil {
		a.emit(logs.LError, label+" failed: "+err.Error())
		return
	}
	a.emit(logs.LInfo, label+" done in "+time.Since(started).Round(100*time.Millisecond).String())
}

// RunAction performs a configured action in the background.
func (a *App) RunAction(act config.Action) {
	switch {
	case act.Run != "":
		go a.exclusive(act.Label, "$ "+act.Run, func() error {
			cmd := shellCommand(act.Run)
			cmd.Dir = a.Cfg.Dir
			return a.pump(cmd, a.selfParser(), false, time.Time{}, nil)
		})
	case len(act.Compose) > 0:
		go a.compose(act.Label, act.Compose...)
	case act.Restart != "":
		go a.RestartCommand(act.Restart)
	}
}

func (a *App) runCommand(src config.Source) {
	cmd := shellCommand(src.Command)
	cmd.Dir = src.Dir
	cmd.Env = os.Environ()
	for k, v := range src.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	c := &command{cmd: cmd, done: make(chan struct{})}
	a.mu.Lock()
	a.commands[src.Name] = c
	a.mu.Unlock()

	a.emit(logs.LInfo, fmt.Sprintf("starting %s: %s", src.Name, src.Command))
	c.err = a.pump(cmd, a.parser(src), false, time.Time{}, nil)
	close(c.done)
	if !a.closing.Load() {
		msg := src.Name + " exited"
		if c.err != nil {
			msg += ": " + c.err.Error()
		}
		a.Lines <- logs.Line{Src: src.Name, Time: time.Now(), Level: logs.LError, Plain: msg, Text: logs.StyleFor(logs.LError, msg)}
	}
}

// RestartCommand stops a command source (its whole process group) and starts it again.
func (a *App) RestartCommand(name string) {
	src, ok := a.Cfg.Source(name)
	if !ok || src.Command == "" {
		return
	}
	a.mu.Lock()
	c := a.commands[name]
	a.mu.Unlock()
	if c != nil && c.cmd.Process != nil {
		signalGroup(c.cmd, false)
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
			signalGroup(c.cmd, true)
			<-c.done
		}
	}
	a.runCommand(src)
}

// tailFile follows a file like tail -F: it waits for the file to appear,
// starts near its end, and reopens it when it is truncated or replaced.
func (a *App) tailFile(src config.Source) {
	raw := make(chan logs.Raw, 256)
	go a.parser(src).Run(raw)
	defer close(raw)

	var (
		f       *os.File
		rd      *bufio.Reader
		partial string
		warned  bool
		first   = true
	)
	for !a.closing.Load() {
		if f == nil {
			var err error
			if f, err = os.Open(src.File); err != nil {
				if !warned {
					a.emit(logs.LWarn, fmt.Sprintf("%s: waiting for %s", src.Name, src.File))
					warned = true
				}
				f = nil
				time.Sleep(time.Second)
				continue
			}
			if info, err := f.Stat(); err == nil && first && info.Size() > 64<<10 {
				// Show the last 64KB as history, from the next full line.
				_, _ = f.Seek(-64<<10, io.SeekEnd)
				rd = bufio.NewReader(f)
				_, _ = rd.ReadString('\n')
			} else {
				rd = bufio.NewReader(f)
			}
			first, warned, partial = false, false, ""
		}
		line, err := rd.ReadString('\n')
		if err == nil {
			raw <- logs.Raw{Time: time.Now(), Text: strings.TrimRight(partial+line, "\n")}
			partial = ""
			continue
		}
		partial += line
		// At EOF: reopen if the file was truncated or replaced, else wait.
		pos, _ := f.Seek(0, io.SeekCurrent)
		cur, statErr := f.Stat()
		onDisk, pathErr := os.Stat(src.File)
		if statErr != nil || pathErr != nil || !os.SameFile(cur, onDisk) || onDisk.Size() < pos {
			f.Close()
			f, partial = nil, ""
			continue
		}
		time.Sleep(250 * time.Millisecond)
	}
	if f != nil {
		f.Close()
	}
}

func (a *App) setDevURL(u string) {
	a.mu.Lock()
	if a.devURL == "" {
		a.devURL = u
	}
	a.mu.Unlock()
}

// OpenURL is what o opens: the configured URL, else the proxy (the only way
// requests get recorded), else a dev server's own URL.
func (a *App) OpenURL() string {
	switch {
	case a.Cfg.OpenURL != "":
		return a.Cfg.OpenURL
	case a.Proxy != nil:
		return a.Proxy.URL()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.devURL
}

func (a *App) SetPinned(id string) {
	a.mu.Lock()
	a.pinned = id
	a.mu.Unlock()
}

func (a *App) Pinned() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pinned
}

// ToggleMonitor turns Redis MONITOR on or off and reports whether it is on.
func (a *App) ToggleMonitor() bool {
	if a.Redis == nil {
		return false
	}
	on, err := a.Redis.ToggleMonitor(a.Lines)
	if err != nil {
		go a.emit(logs.LError, "MONITOR: "+err.Error())
	}
	return on
}

func (a *App) redisLoop() {
	for !a.closing.Load() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		snap := a.Redis.Snapshot(ctx)
		cancel()
		a.Program.Send(RedisMsg(snap))
		time.Sleep(2 * time.Second)
	}
}

func (a *App) statusLoop() {
	client := &http.Client{Timeout: time.Second}
	// probe describes a failed health check briefly: a status code or
	// "no response". Empty means the check passed.
	probe := func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "bad health URL"
		}
		q := u.Query()
		q.Set(strings.Split(logs.ProbeMarker, "=")[0], "1")
		u.RawQuery = q.Encode()
		resp, err := client.Get(u.String())
		if err != nil {
			return "no response"
		}
		resp.Body.Close()
		if resp.StatusCode >= 500 {
			return strconv.Itoa(resp.StatusCode)
		}
		return ""
	}
	for !a.closing.Load() {
		var ps map[string]psRow
		if a.Cfg.Compose != nil {
			ps = a.composeStatus()
		}
		services := map[string]ServiceState{}
		for _, src := range a.Cfg.Sources {
			st := a.sourceState(src, ps)
			if st.Health == HealthOK && src.Health != "" {
				if problem := probe(src.Health); problem != "" {
					st = ServiceState{HealthDown, problem}
				}
			}
			services[src.Name] = st
		}
		a.Program.Send(StatusMsg{Services: services, URL: a.OpenURL()})
		time.Sleep(3 * time.Second)
	}
}

func (a *App) sourceState(src config.Source, ps map[string]psRow) ServiceState {
	switch src.Kind() {
	case "compose":
		row, ok := ps[src.Compose]
		switch {
		case ps == nil:
			return ServiceState{HealthStarting, "checking"}
		case !ok:
			return ServiceState{HealthDown, "missing"}
		case row.State != "running":
			return ServiceState{HealthDown, row.State}
		case row.Health != "" && row.Health != "healthy":
			return ServiceState{HealthStarting, row.Health}
		}
	case "command":
		a.mu.Lock()
		c := a.commands[src.Name]
		a.mu.Unlock()
		if c == nil {
			return ServiceState{HealthStarting, "starting"}
		}
		select {
		case <-c.done:
			if c.err != nil {
				var exit *exec.ExitError
				if errors.As(c.err, &exit) {
					return ServiceState{HealthDown, "exited " + strconv.Itoa(exit.ExitCode())}
				}
				return ServiceState{HealthDown, "failed"}
			}
			return ServiceState{HealthDown, "exited"}
		default:
		}
	case "file":
		if _, err := os.Stat(src.File); err != nil {
			return ServiceState{HealthDown, "no file"}
		}
	}
	return ServiceState{}
}

type psRow struct {
	Service string
	State   string
	Health  string
}

func (a *App) composeStatus() map[string]psRow {
	out, err := a.dockerCmd("ps", "-a", "--format", "json").Output()
	if err != nil {
		return nil
	}
	var rows []psRow
	trimmed := strings.TrimSpace(string(out))
	if strings.HasPrefix(trimmed, "[") { // Compose before 2.21 printed one array
		_ = json.Unmarshal([]byte(trimmed), &rows)
	} else {
		for _, line := range strings.Split(trimmed, "\n") {
			var row psRow
			if json.Unmarshal([]byte(line), &row) == nil {
				rows = append(rows, row)
			}
		}
	}
	states := map[string]psRow{}
	for _, row := range rows {
		states[row.Service] = row
	}
	return states
}

// Shutdown stops the processes tailyard started (commands, log followers,
// an action in flight), escalating to a kill for any still running after a
// few seconds. Containers keep running unless stopContainers is set.
func (a *App) Shutdown(stopContainers bool) {
	// The UI no longer reads lines; drain them so parsers don't block the
	// processes they read from on their way out.
	go func() {
		for range a.Lines {
		}
	}()

	a.mu.Lock()
	a.closing.Store(true)
	var cmds []*exec.Cmd
	for cmd := range a.running {
		cmds = append(cmds, cmd)
		signalGroup(cmd, false)
	}
	a.mu.Unlock()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		a.mu.Lock()
		remaining := len(a.running)
		a.mu.Unlock()
		if remaining == 0 {
			break
		}
	}
	for _, cmd := range cmds {
		signalGroup(cmd, true) // no-op for groups that already exited
	}
	if a.Proxy != nil {
		a.Proxy.Close()
	}
	if a.Redis != nil {
		a.Redis.Close()
	}

	if a.Cfg.Compose == nil {
		return
	}
	if stopContainers {
		cmd := a.dockerCmd("stop")
		if !a.HungUp.Load() {
			os.Stdout.WriteString("tailyard: stopping containers (ctrl+c aborts)…\n")
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		}
		_ = cmd.Run()
		return
	}
	if a.Cfg.ComposeUp() && !a.HungUp.Load() {
		os.Stdout.WriteString("tailyard: containers are still running. Stop them with:\n  cd " + a.Cfg.Compose.Dir + " && " + a.composeCommandLine() + " stop\n")
	}
}
