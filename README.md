# tailyard

[![ci](https://github.com/shivamsingh262/tailyard/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/shivamsingh262/tailyard/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/shivamsingh262/tailyard)](https://github.com/shivamsingh262/tailyard/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/shivamsingh262/tailyard.svg)](https://pkg.go.dev/github.com/shivamsingh262/tailyard)
[![license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

One terminal for your whole local dev stack: every log stream, every HTTP request with its bodies, the browser console, and your Redis queues, side by side and filterable by request id.

```
 myapp │ ● api  ● worker  ● web  ● db │ queues 2·1 oldest 1m4s DLQ 1                        14:02:11
 1 Api 812 │ 2 Worker 95 ●1 │ 3 Web 40 │ 4 Db 12 │ 5 HTTP 64 │ 6 Browser 3 │ 7 Redis 18 │ 8 All
 14:01:58  GET   200   12ms  /api/items      page=2                       │ 2026-10-17 14:02:03.114  http  WRN
           POST  201   48ms  /api/items      body {"name":"lamp"}         │ POST 422 /api/orders  31ms
▌14:02:03  POST  422   31ms  /api/orders     body {"qty":-1}              │ ▸ request  application/json
                                                                          │ { "qty": -1 }
                                                                          │ ▸ response 422  application/json · 88B
                                                                          │ { "detail": "qty must be positive" }
```

- **Runs the stack.** `docker compose up` on start and `stop` on quit; dev servers and other commands run as child processes tailyard owns; log files are tailed.
- **Reads the logs properly.** JSON logs (zap, zerolog, pino, structlog, slog…), access-log lines laid out as request rows, Python tracebacks and JS stack frames folded into the entry they belong to, Postgres and Redis server logs.
- **Records HTTP.** A reverse proxy records each request and response with query, bodies (gzip decoded, JSON pretty-printed) and timing, and masks passwords and tokens.
- **Captures the browser console.** The proxy can inject a small script into your HTML pages that forwards `console.*`, uncaught errors and unhandled rejections.
- **Records LLM calls.** A second proxy sits between your code and the Anthropic or OpenAI API (or any OpenAI-compatible server). Each call shows the model, stop reason, tokens, cache hits, time to first token and cost, and calls that hit the output cap, were refused or were answered by a different model stand out.
- **Watches Redis.** Streams and consumer groups are discovered automatically and shown with length, pending, lag, oldest-pending age and consumers. It also shows memory, key prefixes, a live tail of a stream, and `MONITOR` on a key press.
- **Correlates.** Pin a request, trace or job id with `f`, and every tab shows only what mentions it.
- **Binds your common commands to keys.** Restart a worker, rebuild images or run migrations without leaving the screen.

## Install

Homebrew (macOS and Linux):

```sh
brew install --cask shivamsingh262/tap/tailyard
```

Or download a binary for your platform from [Releases](https://github.com/shivamsingh262/tailyard/releases), or install with Go 1.24+:

```sh
go install github.com/shivamsingh262/tailyard@latest
```

## Quick start

```sh
cd your-project
tailyard -init      # writes an annotated tailyard.yml
$EDITOR tailyard.yml
tailyard
```

tailyard looks for `tailyard.yml` in the current folder and its parents. `tailyard -check` validates the config without starting anything.

A minimal config, a compose backend plus a Vite frontend:

```yaml
compose:
  files: [docker-compose.yml]
sources:
  - name: api
    compose: api
    health: http://localhost:8000/health
  - name: web
    command: npm run dev
    dir: ./frontend
proxy:
  target: http://localhost:5173   # open http://127.0.0.1:8899 instead
  inject_console: true
redis:
  addr: 127.0.0.1:6379
correlation:
  keys: [request_id]
```

See [`examples/tailyard.yml`](examples/tailyard.yml) for every option.

## Configuration

| Section | What it does |
|---|---|
| `compose` | A docker compose project: `dir`, `files`, `project`. `up: false` only follows containers that are already running; `build: true` rebuilds on start. |
| `sources` | Log streams. Each sets one of `compose` (a service), `command` (a shell command tailyard starts, with optional `dir` and `env`) or `file` (tailed like `tail -F`). Optional: `parser` (`auto`, `json`, `python`, `postgres`, `redis`, `plain`) and `health` (a URL; 5xx or no answer shows in the header). |
| `proxy` | `target` to forward to, `listen` address (default `127.0.0.1:8899`), `record` path prefixes (default: everything except static assets and dev-server internals), `max_body`, `redact` key substrings, `inject_console`. |
| `llm` | `listen` address (default `127.0.0.1:8898`), `providers` (default `anthropic` and `openai`; also `ollama`, `openrouter`, `groq`, `together`, or any server with `api: openai` and an `upstream`), `set_env`, `max_body`, `redact`, and `prices` in USD per million tokens by model or model prefix. |
| `redis` | `addr`, `username`, `password`, `db`. `streams` lists stream/group pairs (default: discover them; `alert: true` marks a dead-letter queue). `tail` is a glob such as `events:*`: tailyard follows the newest matching stream, or the pinned id's stream when one exists. |
| `correlation` | `keys` are JSON log fields and query parameters holding the id; `pattern` is a regex for plain text (group 1 is the id); `label` names it in the UI. |
| `tabs` | Optional custom tabs, each a `name` and a list of `sources`. Built-in source names: `http`, `browser`, `llm`, `events`, `monitor`, `tailyard`. |
| `actions` | Keys bound to a `run` shell command, `compose` arguments, or `restart` of a command source. |

`${VAR}` anywhere in the file is replaced from the environment, so secrets can stay out of it.

## Keys

Press `?` in the app for the full list, including your actions.

| Key | Action |
|---|---|
| `1`–`9`, tab, ←→ | Switch tab |
| ↑↓ / `j` `k` | Move the selection; `g` / `G` jump to the oldest entry / follow the newest |
| `i`, enter | Open the entry full-screen; inside, `n` / `p` step older / newer, `y` copies it |
| `s` | Split view: list plus detail pane (160+ columns; on by default in HTTP). `J` / `K` scroll the pane |
| `/` | Regex filter for this tab; esc clears it |
| `e` | Warnings and errors only |
| `f` / `F` | Pin a correlation id across all tabs (tab cycles recent ids) / clear it |
| `a` | Hide 2xx/3xx access-log lines |
| `t` | Expand details inline |
| `y` | Copy the last error in this tab, with its traceback |
| `o` | Open the app in your browser |
| `m` | Toggle Redis `MONITOR` |
| `q`, ctrl+c | Quit, and `docker compose stop` if tailyard started the containers |
| `d` | Detach: quit and leave the containers running |

Flags: `-config`, `-init`, `-check`, `-build`, `-no-up`, `-keep-running`, `-theme light`, `-version`.

## How it works

- **The proxy** is a standard reverse proxy. Requests are recorded only if they go through it, so open the app at the proxy's address. If your frontend dev server proxies `/api` to a backend, put tailyard in front of the dev server: page loads, API calls and HMR all pass through, and the console script can be injected into the pages. WebSockets are forwarded but not recorded. Server-sent event streams are noted but not buffered.
- **The LLM proxy** forwards `http://127.0.0.1:8898/<provider>/…` to the provider, so an SDK whose base URL is `http://127.0.0.1:8898/anthropic` works unchanged; your API key passes through and is never recorded. tailyard sets `ANTHROPIC_BASE_URL` and `OPENAI_BASE_URL` for the command sources it starts (not if a source or your shell already sets them). For compose services, listen on `0.0.0.0` and set the variable in the service, pointing at `host.docker.internal`. Streams are forwarded chunk by chunk and parsed as they pass. With OpenAI chat streams, set `stream_options: {"include_usage": true}` to get token counts. Calls show in the LLM tab, and the header totals calls, tokens and cost for the session. Each attempt is its own row, so SDK retries after a 429 or 529 show separately.
- **Console capture** adds `<script src="/__tailyard/console.js">` to the top of `<head>` in HTML responses. It wraps `console.*` and forwards messages in batches to the proxy. Browsers will show the wrapper as the source of console messages; add `/__tailyard/` to DevTools' ignore list to see your own code instead.
- **Redaction** masks values under keys containing `password`, `secret`, `token`, `api_key`, `authorization` or `cookie` (configurable), in JSON bodies at any depth, form bodies and query strings. Token counts such as `max_tokens` are left visible. Only the recorded copy is masked; your app sees the real traffic.
- **Redis** is sampled every 2 seconds. tailyard names its connections `tailyard` so its own commands are left out of `MONITOR`.
- **Process control:** commands run under `sh -c` (`cmd /C` on Windows) in their own process group, and quitting stops the whole group. Closing the terminal window counts as `q`.

## Compared to

| Tool | Covers |
|---|---|
| lazydocker, k9s | containers and their logs, no HTTP bodies or Redis |
| RedisInsight | Redis in depth, in a separate GUI |
| mitmproxy, Proxyman | HTTP in depth, without your logs next to it |
| Overmind, foreman | running processes, raw interleaved output |

tailyard is for the local dev loop: the logs, the request that caused them, and the queue they landed in, all on one screen.

## Limitations

- Only HTTP that goes through the proxy is recorded.
- LLM calls are recorded only if the SDK honours a base URL. AWS Bedrock signs requests for its own host, so it can't go through the LLM proxy. Realtime (WebSocket) APIs are forwarded but not recorded.
- Brotli/zstd responses aren't decoded (the proxy asks upstreams for gzip or nothing, so this is rare).
- `MONITOR` slows Redis down; it is off until you press `m`.
- Windows builds, but process control and the UI are tested mostly on macOS and Linux.

## Development

```sh
go vet ./... && go test -race ./...
```

Releases: push a `v*` tag and [the release workflow](.github/workflows/release.yml) runs GoReleaser, which publishes binaries to GitHub Releases and updates the Homebrew tap. `goreleaser release --snapshot --clean` builds the same archives locally without publishing.

| Package | Contents |
|---|---|
| `main.go` | Flags, config discovery, wiring |
| `internal/config` | `tailyard.yml` schema, defaults and validation |
| `internal/app` | Compose, commands, file tails, health checks, actions, shutdown |
| `internal/logs` | The `Line` model and parsers (JSON, access logs, tracebacks, Postgres, Redis), HTTP and console entries |
| `internal/proxy` | Recording reverse proxy, redaction, console script injection, and the LLM proxy with its stream parsing |
| `internal/redismon` | Stream/group stats, stream tail, `MONITOR` |
| `internal/ui` | Bubble Tea model and rendering |
| `internal/theme` | Colors for dark and light terminals |

## License

[MIT](LICENSE)
