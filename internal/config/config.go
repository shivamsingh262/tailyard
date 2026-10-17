// Package config loads and validates tailyard.yml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileNames are the config file names searched for, in order, from the
// working directory upwards.
var FileNames = []string{"tailyard.yml", "tailyard.yaml", ".tailyard.yml"}

// Built-in sources produced by tailyard itself rather than by a configured
// process. Configured sources may not use these names.
const (
	SrcHTTP    = "http"    // requests recorded by the proxy
	SrcBrowser = "browser" // browser console, forwarded by the proxy's injected script
	SrcEvents  = "events"  // the tailed Redis stream
	SrcMonitor = "monitor" // Redis MONITOR output
	SrcSelf    = "tailyard"
)

var builtinSources = []string{SrcHTTP, SrcBrowser, SrcEvents, SrcMonitor, SrcSelf}

// Keys tailyard binds itself; actions can't use them.
var reservedKeys = "1234567890qdhjklgGieEsJKtfFaycmo?/ "

type Config struct {
	Name        string      `yaml:"name"`
	Theme       string      `yaml:"theme"`
	OpenURL     string      `yaml:"open_url"`
	Compose     *Compose    `yaml:"compose"`
	Sources     []Source    `yaml:"sources"`
	Proxy       *Proxy      `yaml:"proxy"`
	Redis       *Redis      `yaml:"redis"`
	Correlation Correlation `yaml:"correlation"`
	Tabs        []Tab       `yaml:"tabs"`
	Actions     []Action    `yaml:"actions"`

	// Dir is the folder holding the config file; relative paths in the
	// config resolve against it.
	Dir string `yaml:"-"`
}

type Compose struct {
	Dir        string   `yaml:"dir"`
	Files      []string `yaml:"files"`
	Project    string   `yaml:"project"`
	Up         *bool    `yaml:"up"`           // run `up -d` on start (default true)
	Build      bool     `yaml:"build"`        // pass --build to the initial up
	StopOnQuit *bool    `yaml:"stop_on_quit"` // `stop` on q (default true when up)
	Tail       int      `yaml:"tail"`         // history lines per service on start
}

// Source is one log stream. Exactly one of Compose, Command or File is set.
type Source struct {
	Name    string            `yaml:"name"`
	Compose string            `yaml:"compose"` // compose service to follow
	Command string            `yaml:"command"` // shell command tailyard starts and owns
	File    string            `yaml:"file"`    // file to tail
	Dir     string            `yaml:"dir"`     // working directory for Command
	Env     map[string]string `yaml:"env"`     // extra environment for Command
	Parser  string            `yaml:"parser"`  // auto, json, python, postgres, redis, plain
	Health  string            `yaml:"health"`  // URL probed for the header; 5xx or no answer is unhealthy
}

func (s Source) Kind() string {
	switch {
	case s.Compose != "":
		return "compose"
	case s.Command != "":
		return "command"
	}
	return "file"
}

type Proxy struct {
	Listen        string   `yaml:"listen"`
	Target        string   `yaml:"target"`
	MaxBody       string   `yaml:"max_body"` // per body, e.g. 512KB
	Redact        []string `yaml:"redact"`   // key substrings whose values are masked
	Record        []string `yaml:"record"`   // path prefixes to record (default: everything but static assets)
	InjectConsole bool     `yaml:"inject_console"`

	MaxBodyBytes int64 `yaml:"-"`
}

type Redis struct {
	Addr     string      `yaml:"addr"`
	Username string      `yaml:"username"`
	Password string      `yaml:"password"`
	DB       int         `yaml:"db"`
	Streams  []StreamRef `yaml:"streams"` // empty: discover streams and their groups
	Tail     string      `yaml:"tail"`    // glob of streams to tail, e.g. "events:*"
}

type StreamRef struct {
	Stream string `yaml:"stream"`
	Group  string `yaml:"group"`
	Alert  bool   `yaml:"alert"` // any entry in it is a problem (dead-letter queues)
}

// Correlation ties entries across sources to one id (a request, trace, job
// or run). Pressing f pins an id and filters every tab to it.
type Correlation struct {
	Label   string   `yaml:"label"`   // shown in the UI, e.g. "request"
	Keys    []string `yaml:"keys"`    // JSON log fields and query params holding the id
	Pattern string   `yaml:"pattern"` // regex for plain text; group 1 (or the whole match) is the id

	Regexp *regexp.Regexp `yaml:"-"`
}

type Tab struct {
	Name    string   `yaml:"name"`
	Sources []string `yaml:"sources"`
}

// Action binds a key to a command. Exactly one of Run, Compose or Restart is set.
type Action struct {
	Key     string   `yaml:"key"`
	Label   string   `yaml:"label"`
	Run     string   `yaml:"run"`     // shell command, run in the config's folder
	Compose []string `yaml:"compose"` // docker compose arguments
	Restart string   `yaml:"restart"` // name of a command source to restart
}

// Find walks up from start looking for a config file.
func Find(start string) (string, error) {
	for dir := start; ; dir = filepath.Dir(dir) {
		for _, name := range FileNames {
			path := filepath.Join(dir, name)
			if _, err := os.Stat(path); err == nil {
				return path, nil
			}
		}
		if filepath.Dir(dir) == dir {
			return "", fmt.Errorf("no %s found in %s or any parent folder; pass -config or run `tailyard -init`", FileNames[0], start)
		}
	}
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data, filepath.Dir(abs))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes and validates a config. Environment variables in the file
// (${VAR}) are expanded first, so secrets like a Redis password can stay out of it.
func Parse(data []byte, dir string) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(os.ExpandEnv(string(data))))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	cfg.Dir = dir
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) normalize() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Name == "" {
		c.Name = filepath.Base(c.Dir)
	}
	if c.Theme == "" {
		c.Theme = "dark"
	}
	if c.Theme != "dark" && c.Theme != "light" {
		fail("theme must be dark or light, not %q", c.Theme)
	}

	if c.Compose != nil {
		c.Compose.Dir = c.resolve(c.Compose.Dir)
		if c.Compose.Up == nil {
			c.Compose.Up = ptr(true)
		}
		if c.Compose.StopOnQuit == nil {
			c.Compose.StopOnQuit = ptr(*c.Compose.Up)
		}
		if c.Compose.Tail == 0 {
			c.Compose.Tail = 300
		}
	}

	names := map[string]bool{}
	for _, b := range builtinSources {
		names[b] = true
	}
	for i := range c.Sources {
		s := &c.Sources[i]
		switch {
		case s.Name == "":
			fail("sources[%d]: name is required", i)
		case slicesContains(builtinSources, s.Name):
			fail("source %q: the name is reserved for a built-in source", s.Name)
		case names[s.Name]:
			fail("source %q is defined twice", s.Name)
		}
		names[s.Name] = true
		set := 0
		for _, v := range []string{s.Compose, s.Command, s.File} {
			if v != "" {
				set++
			}
		}
		if set != 1 {
			fail("source %q: set exactly one of compose, command or file", s.Name)
		}
		if s.Compose != "" && c.Compose == nil {
			fail("source %q follows a compose service, but there is no compose section", s.Name)
		}
		if s.Parser == "" {
			s.Parser = "auto"
		}
		if !slicesContains(Parsers, s.Parser) {
			fail("source %q: unknown parser %q (one of %s)", s.Name, s.Parser, strings.Join(Parsers, ", "))
		}
		if s.Command != "" {
			s.Dir = c.resolve(s.Dir)
		}
		if s.File != "" {
			s.File = c.resolve(s.File)
		}
	}

	if p := c.Proxy; p != nil {
		if p.Target == "" {
			fail("proxy: target is required")
		}
		if p.Listen == "" {
			p.Listen = "127.0.0.1:8899"
		}
		if p.MaxBody == "" {
			p.MaxBody = "512KB"
		}
		n, err := parseSize(p.MaxBody)
		if err != nil {
			fail("proxy: max_body: %v", err)
		}
		p.MaxBodyBytes = n
		if p.Redact == nil {
			p.Redact = []string{"password", "passwd", "secret", "token", "api_key", "apikey", "authorization", "cookie"}
		}
	}

	if r := c.Redis; r != nil && r.Addr == "" {
		r.Addr = "127.0.0.1:6379"
	}

	if c.Correlation.Label == "" {
		c.Correlation.Label = "id"
	}
	if c.Correlation.Pattern != "" {
		re, err := regexp.Compile(c.Correlation.Pattern)
		if err != nil {
			fail("correlation: pattern: %v", err)
		}
		c.Correlation.Regexp = re
	}

	if len(c.Tabs) == 0 {
		c.Tabs = c.defaultTabs()
	}
	for _, t := range c.Tabs {
		if t.Name == "" {
			fail("tabs: every tab needs a name")
		}
		for _, s := range t.Sources {
			if !names[s] {
				fail("tab %q: unknown source %q", t.Name, s)
			}
		}
	}

	keys := map[string]bool{}
	for i := range c.Actions {
		a := &c.Actions[i]
		switch {
		case len([]rune(a.Key)) != 1:
			fail("actions[%d]: key must be a single character", i)
		case strings.Contains(reservedKeys, a.Key):
			fail("action %q: key %q is used by tailyard itself", a.Label, a.Key)
		case keys[a.Key]:
			fail("action key %q is bound twice", a.Key)
		}
		keys[a.Key] = true
		set := 0
		if a.Run != "" {
			set++
		}
		if len(a.Compose) > 0 {
			set++
			if c.Compose == nil {
				fail("action %q runs docker compose, but there is no compose section", a.Key)
			}
		}
		if a.Restart != "" {
			set++
			if src, ok := c.Source(a.Restart); !ok || src.Command == "" {
				fail("action %q: restart needs the name of a command source, not %q", a.Key, a.Restart)
			}
		}
		if set != 1 {
			fail("action %q: set exactly one of run, compose or restart", a.Key)
		}
		if a.Label == "" {
			a.Label = strings.TrimSpace(a.Run + strings.Join(a.Compose, " ") + a.Restart)
			if a.Restart != "" {
				a.Label = "restart " + a.Restart
			}
		}
	}
	return errors.Join(errs...)
}

// defaultTabs is one tab per configured source, then the built-in ones that
// are enabled, then everything.
func (c *Config) defaultTabs() []Tab {
	var tabs []Tab
	all := []string{}
	for _, s := range c.Sources {
		tabs = append(tabs, Tab{Name: titleCase(s.Name), Sources: []string{s.Name}})
		all = append(all, s.Name)
	}
	if c.Proxy != nil {
		tabs = append(tabs, Tab{Name: "HTTP", Sources: []string{SrcHTTP}})
		all = append(all, SrcHTTP)
		if c.Proxy.InjectConsole {
			tabs = append(tabs, Tab{Name: "Browser", Sources: []string{SrcBrowser}})
			all = append(all, SrcBrowser)
		}
	}
	if c.Redis != nil {
		tabs = append(tabs, Tab{Name: "Redis", Sources: []string{SrcEvents, SrcMonitor}})
		all = append(all, SrcEvents)
	}
	tabs = append(tabs, Tab{Name: "System", Sources: []string{SrcSelf}})
	all = append(all, SrcSelf)
	return append(tabs, Tab{Name: "All", Sources: all})
}

func (c *Config) Source(name string) (Source, bool) {
	for _, s := range c.Sources {
		if s.Name == name {
			return s, true
		}
	}
	return Source{}, false
}

// Action returns the action bound to key, if any.
func (c *Config) Action(key string) (Action, bool) {
	for _, a := range c.Actions {
		if a.Key == key {
			return a, true
		}
	}
	return Action{}, false
}

// ComposeUp reports whether tailyard starts the compose project itself.
func (c *Config) ComposeUp() bool {
	return c.Compose != nil && *c.Compose.Up
}

func (c *Config) resolve(path string) string {
	if path == "" {
		return c.Dir
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(c.Dir, path)
}

// Parsers are the log formats a source can declare.
var Parsers = []string{"auto", "json", "python", "postgres", "redis", "plain"}

func parseSize(s string) (int64, error) {
	upper := strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, unit := range []struct {
		suffix string
		mult   int64
	}{{"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(upper, unit.suffix) {
			upper, mult = strings.TrimSuffix(upper, unit.suffix), unit.mult
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(upper), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid size %q (examples: 512KB, 2MB)", s)
	}
	return n * mult, nil
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func slicesContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func ptr[T any](v T) *T { return &v }
