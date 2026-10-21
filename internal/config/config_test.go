package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDefaults(t *testing.T) {
	dir := filepath.FromSlash("/work/app")
	cfg, err := Parse([]byte(`
compose:
  dir: backend
sources:
  - name: api
    compose: api
  - name: web
    command: npm run dev
    dir: frontend
proxy:
  target: http://localhost:3000
redis: {}
`), dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "app" {
		t.Errorf("name = %q, want app", cfg.Name)
	}
	if cfg.Compose.Dir != filepath.Join(dir, "backend") || !*cfg.Compose.Up || !*cfg.Compose.StopOnQuit {
		t.Errorf("compose defaults wrong: %+v", cfg.Compose)
	}
	if cfg.Sources[1].Dir != filepath.Join(dir, "frontend") || cfg.Sources[0].Parser != "auto" {
		t.Errorf("source defaults wrong: %+v", cfg.Sources)
	}
	if cfg.Proxy.MaxBodyBytes != 512<<10 || cfg.Proxy.Listen == "" {
		t.Errorf("proxy defaults wrong: %+v", cfg.Proxy)
	}
	if cfg.Redis.Addr != "127.0.0.1:6379" {
		t.Errorf("redis addr = %q", cfg.Redis.Addr)
	}
	var names []string
	for _, tab := range cfg.Tabs {
		names = append(names, tab.Name)
	}
	if got := strings.Join(names, ","); got != "Api,Web,HTTP,Redis,System,All" {
		t.Errorf("default tabs = %s", got)
	}
}

func TestParseErrors(t *testing.T) {
	_, err := Parse([]byte(`
sources:
  - name: api
    compose: api
  - name: api
    file: x.log
  - name: http
    file: y.log
  - name: both
    file: a.log
    command: echo
  - name: weird
    file: z.log
    parser: xml
tabs:
  - name: T
    sources: [nope]
actions:
  - key: q
    run: echo
  - key: x
    restart: api
`), "/w")
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{
		"no compose section",
		`"api" is defined twice`,
		"reserved",
		"exactly one of compose, command or file",
		`unknown parser "xml"`,
		`unknown source "nope"`,
		"used by tailyard itself",
		"restart needs the name of a command source",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	if _, err := Parse([]byte("sorces: []\n"), "/w"); err == nil {
		t.Fatal("expected an error for a misspelled field")
	}
}

func TestEnvExpansion(t *testing.T) {
	t.Setenv("TAILYARD_TEST_PW", "hunter2")
	cfg, err := Parse([]byte("redis:\n  password: ${TAILYARD_TEST_PW}\n"), "/w")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Redis.Password != "hunter2" {
		t.Errorf("password = %q", cfg.Redis.Password)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"512KB": 512 << 10, "2mb": 2 << 20, "100": 100, "64B": 64} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := parseSize("lots"); err == nil {
		t.Error("expected an error")
	}
}

func TestLLMDefaultsAndEnv(t *testing.T) {
	cfg, err := Parse([]byte(`
sources:
  - name: api
    command: uvicorn app:app
  - name: worker
    command: python worker.py
    env:
      OPENAI_BASE_URL: http://my-gateway/v1
  - name: db
    file: db.log
llm:
  providers:
    - name: anthropic
    - name: openai
    - name: ollama
`), "/w")
	if err != nil {
		t.Fatal(err)
	}
	l := cfg.LLM
	if l.Listen != "127.0.0.1:8898" || !*l.SetEnv || l.MaxBodyBytes != 2<<20 {
		t.Errorf("llm defaults wrong: %+v", l)
	}
	if p := l.Providers[2]; p.API != "openai" || p.Upstream != "http://localhost:11434/v1" || p.Env != "" {
		t.Errorf("ollama provider: %+v", p)
	}
	api, _ := cfg.Source("api")
	if got := strings.Join(cfg.LLMEnv(api), " "); got != "ANTHROPIC_BASE_URL=http://127.0.0.1:8898/anthropic OPENAI_BASE_URL=http://127.0.0.1:8898/openai" {
		t.Errorf("api env = %s", got)
	}
	worker, _ := cfg.Source("worker")
	if got := strings.Join(cfg.LLMEnv(worker), " "); got != "ANTHROPIC_BASE_URL=http://127.0.0.1:8898/anthropic" {
		t.Errorf("a source's own env must win: %s", got)
	}
	db, _ := cfg.Source("db")
	if cfg.LLMEnv(db) != nil {
		t.Error("only command sources get the env")
	}
	var names []string
	for _, tab := range cfg.Tabs {
		names = append(names, tab.Name)
	}
	if got := strings.Join(names, ","); got != "Api,Worker,Db,LLM,System,All" {
		t.Errorf("default tabs = %s", got)
	}
}

func TestLLMErrors(t *testing.T) {
	_, err := Parse([]byte(`
proxy:
  target: http://localhost:3000
  listen: 127.0.0.1:9000
llm:
  listen: 127.0.0.1:9000
  set_env: false
  providers:
    - name: mine
    - name: anthropic
    - name: anthropic
    - name: a/b
      api: openai
      upstream: http://x
`), "/w")
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"clashes with the proxy's address", `provider "mine": api must be one of`, `"anthropic" is defined twice`, "can't contain"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
}

func TestLLMEnvAndAddressClashes(t *testing.T) {
	_, err := Parse([]byte(`
proxy:
  target: http://localhost:3000
  listen: 127.0.0.1:8899
llm:
  listen: 0.0.0.0:8899
  providers:
    - name: openai
    - name: azure
      api: openai
      upstream: https://example.openai.azure.com/openai/v1
      env: OPENAI_BASE_URL
`), "/w")
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"clashes with the proxy's address", `providers "openai" and "azure" both set OPENAI_BASE_URL`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
}
