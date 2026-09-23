# tokscope

A local proxy that sits in front of requests from AI coding tools like Claude Code and Codex,
recording **input/output token counts, prompts, and system prompts** so you can check them in
your browser. It's a single Go binary with no external dependencies.

```
claude ──HTTPS_PROXY──▶ tokscope (127.0.0.1:8899) ──▶ api.anthropic.com
                          │ Decrypts only AI API hosts to read usage
                          │ Everything else (github.com, etc.) passes through untouched
                          ▼
                 ~/.tokscope/logs/usage.jsonl ──▶ http://127.0.0.1:8899/
```

## Install

```sh
# macOS
brew install --cask cobayo/tap/tokscope

# Windows (PowerShell)
scoop bucket add tokscope https://github.com/cobayo/scoop-bucket
scoop install tokscope

# Linux / other (Go 1.22+)
go install github.com/cobayo/tokscope@latest
```

You can also unpack the tar.gz / zip from the Releases page and put it on your PATH.

## Build

Building from source requires Go 1.22+. There are no external dependencies, so `go build` is
all you need.

```sh
git clone https://github.com/cobayo/tokscope.git
cd tokscope
go build -o tokscope .
```

Put the resulting `tokscope` binary somewhere on your PATH.

## Usage

Prefix the tool with `tokscope run --` to launch it.

```sh
tokscope run -- claude
tokscope run -- codex
tokscope run -- gemini
```

While running, you can check the latest 30 requests at http://127.0.0.1:8899/. To view them in
the terminal, use `tokscope tail`.

```
TIME      CLIENT       MODEL                             INPUT  CACHE     OUTPUT  PROMPT
08:24:33  Claude Code  claude-sonnet-4-5-20250929       32,326    96%        318  ↳ Also add rollback steps for the migration
08:23:43  Claude Code  claude-sonnet-4-5-20250929       31,560    92%      2,651  Also add rollback steps for the migration
08:17:03  Gemini CLI   gemini-2.5-pro                    9,480     0%        730  Explain this repo's structure
```

`run` passes proxy environment variables (`HTTPS_PROXY`, `NODE_EXTRA_CA_CERTS`, `CODEX_CA_CERTIFICATE`, etc.)
**only to the command it launches**. It doesn't touch your OS certificate store or shell
configuration. If tokscope is already running, it logs to that instance; otherwise it starts a
temporary instance that lasts until the tool exits.

### Running it as a background service

If you want to record tools that are hard to wrap with `run` (IDE extensions, for example), run
the proxy as a background service instead.

```sh
tokscope serve                       # keep running in another terminal
eval "$(tokscope env)"               # bash / zsh
tokscope env | source                # fish
tokscope env | Invoke-Expression     # PowerShell
```

## What gets recorded

One line is appended to `~/.tokscope/logs/usage.jsonl` per request.

| Field | Description |
|---|---|
| `client` | Claude Code / Codex / Gemini CLI, etc. (detected from the User-Agent) |
| `model` | The model name returned in the response (falls back to the request or URL if absent) |
| `input_tokens` | Input that didn't come from cache |
| `cache_read_tokens` / `cache_write_tokens` | Input read from cache / input written to cache |
| `total_input_tokens` | Sum of the three above |
| `output_tokens` / `reasoning_tokens` | Output (including reasoning/thinking) / the reasoning portion of it |
| `prompt` | The most recent human-written user message in the conversation |
| `prompt_kind` | `user` (a response to a new message) / `tool_result` (a continuation after a tool result) |
| `system_prompt_id` | The system prompt's ID (see below) |
| `status` / `error` / `duration_ms` | HTTP status, any interruption/error, and how long it took |

Agentic tools send dozens of requests per instruction, resending the entire conversation each
time. Instead of the whole history, tokscope keeps only the latest human message in `prompt`, and
marks continuation requests (following a tool result) with `tool_result`. Text that tools inject
automatically, like `<system-reminder>` or `<environment_context>`, is not treated as a prompt.

Since providers differ on whether cached tokens count toward input, tokscope always normalizes so
that `total_input_tokens = input_tokens + cache_read_tokens + cache_write_tokens`.

### System prompts

Claude Code sends a system prompt that's tens of thousands of characters long on every request, so
instead of writing it into the log directly, tokscope hashes the content into an ID and stores it
once at `~/.tokscope/system-prompts/<ID>.txt`. You can follow it from the log's
`system_prompt_id`, or read it by expanding a row on the dashboard and clicking "Show full text".
Whenever the ID changes, the system prompt changed.

- Anthropic format: `system`
- OpenAI Responses (Codex): `instructions`, plus messages with the `system` / `developer` role
- Chat Completions: messages with the `system` / `developer` role
- Gemini: `systemInstruction`
- Bedrock Converse: `system`

### Aggregating with jq

```sh
# Today's totals (ts is recorded in local time)
jq -s --arg d "$(date +%F)" 'map(select(.ts | startswith($d)))
  | {requests: length, input: (map(.total_input_tokens) | add), output: (map(.output_tokens) | add)}' \
  ~/.tokscope/logs/usage.jsonl

# Output tokens by model
jq -s 'group_by(.model) | map({model: .[0].model, output: (map(.output_tokens) | add)})' ~/.tokscope/logs/usage.jsonl
```

## Coverage

tokscope only inspects traffic to the hosts below; everything else passes through undecrypted.

| Path | Host | Format | Verified |
|---|---|---|---|
| Claude Code (API key / Claude account) | api.anthropic.com | Messages (SSE / JSON) | Verified relaying to the real API; usage parsing is mocked |
| Claude Code on Bedrock | bedrock-runtime.*.amazonaws.com | InvokeModel(Stream), Converse(Stream) | Mocked (including not breaking SigV4 signing) |
| Claude Code on Vertex AI | *-aiplatform.googleapis.com | rawPredict / streamRawPredict | Mocked |
| Claude on Azure AI Foundry | *.services.ai.azure.com | Messages | Mocked |
| Codex (ChatGPT login) | chatgpt.com | Responses (SSE / WebSocket) | Mocked, **needs real-world verification** |
| Codex (API key) | api.openai.com | Responses | Mocked |
| Azure OpenAI | *.openai.azure.com, etc. | Chat Completions / Responses | Mocked |
| Gemini CLI (Google login) | cloudcode-pa.googleapis.com | Code Assist | Mocked, **needs real-world verification** |
| Gemini API / Vertex Gemini | generativelanguage.googleapis.com, *-aiplatform.googleapis.com | generateContent (SSE / JSON) | Mocked |
| LiteLLM | specified via `extra_hosts` | Messages / Chat Completions / Responses | Mocked |

Items flagged as needing real-world verification:

- **Codex**: Newer versions sometimes use WebSocket for the Responses API. tokscope reads and
  records WebSocket frames too, but it's unverified whether Codex's WebSocket connection honors
  `HTTPS_PROXY`. If Codex rows don't show up, check `~/.tokscope/logs/tokscope.log`. It's also
  unverified whether `CODEX_CA_CERTIFICATE` gets added to the default trust store or replaces it,
  so as a precaution tokscope passes a bundle of the system certificates plus tokscope's own CA
  (on Windows, system certificates can't be extracted as a file, so only tokscope's CA is passed).
- **Gemini CLI**: It's unverified whether it reads proxy environment variables. If requests aren't
  recorded, set Gemini CLI's own proxy setting to `http://127.0.0.1:8899`.

## Configuration

`~/.tokscope/config.json` (optional):

```json
{
  "listen": "127.0.0.1:8899",
  "extra_hosts": ["localhost:4000", "https://litellm.example.com"],
  "upstream_proxy": "http://proxy.example.com:8080",
  "save_prompt": true,
  "save_system_prompt": true,
  "prompt_max_chars": 20000,
  "recent": 30
}
```

| Field | Description |
|---|---|
| `extra_hosts` | Additional hosts to inspect (e.g. LiteLLM). Accepts `host`, `host:port`, or URL form |
| `upstream_proxy` | URL of a corporate proxy to chain through (`http://user:pass@host:port` also works). Not used for localhost destinations |
| `save_prompt` / `save_system_prompt` | Set to `false` to stop saving them (token counts are still recorded) |
| `prompt_max_chars` | Maximum number of characters to save for a prompt |
| `recent` | Number of records shown on the dashboard (default 30) |

These can also be overridden with the environment variables `TOKSCOPE_HOME` (storage location),
`TOKSCOPE_LISTEN` (listen address), and `TOKSCOPE_UPSTREAM_PROXY`.

## About Windows

The same binary works as-is. Install it with Scoop and use it from PowerShell, e.g.
`tokscope run -- claude`. Certificates are passed to the child process via environment variables,
so no administrator privileges or certificate store registration are needed. For tools used inside
WSL, install the Linux build inside WSL to record them.

Only if you need to record tools that don't read environment variables, register the CA with your
OS by following the instructions from `tokscope ca`.

## Security

- Only listens on `127.0.0.1`. The dashboard rejects requests whose Host header isn't itself (DNS
  rebinding protection).
- `~/.tokscope/ca-key.pem` is a key that lets you impersonate AI API hosts to any process that
  trusts this CA. Don't share it. If deleted, it's regenerated on next startup.
- Prompts and system prompts remain in the log. Files are created with owner-only read permissions
  (0600). API keys, auth headers, and URL query strings are never recorded.
- Commands run by the tool (`npm install`, `git`, etc.) also inherit the proxy environment
  variables. Traffic to anything other than the AI APIs passes through undecrypted, so this has no
  effect — but calling something like `curl https://api.anthropic.com/...` directly from a command
  will hit a certificate error, since it doesn't know tokscope's CA.

## Limitations

- Verification has only gone as far as mock-server tests and confirming relay to the real
  api.anthropic.com (unauthenticated). Matching each provider's actual billed amounts hasn't been
  checked.
- When streaming Chat Completions with Azure OpenAI or LiteLLM, token counts won't come back
  ("no usage data") unless the client sets `stream_options.include_usage`.
- Logs are not rotated. Move or delete `usage.jsonl` once it gets large.
- Only HTTP/1.1 is used between tokscope and the client (HTTP/2 is only used upstream).

## Development

```sh
go test -race ./...
go build -o tokscope .
```

Pushing a tag like `v0.1.0` triggers GitHub Actions to build with GoReleaser and update the
Homebrew tap and Scoop bucket. Set these up beforehand:

1. Create empty `homebrew-tap` and `scoop-bucket` repositories
2. Register tokens that can push to them as the `HOMEBREW_TAP_GITHUB_TOKEN` and
   `SCOOP_BUCKET_GITHUB_TOKEN` secrets

| File | Role |
|---|---|
| `proxy.go` | CONNECT handling, TLS termination for target hosts only, relaying, WebSocket |
| `match.go` | List of target hosts, API format and client detection |
| `reqparse.go` | Extracts model, system prompt, and latest prompt from requests |
| `usage.go` | Extracts token counts from SSE / JSON / AWS event-stream / gzip |
| `ws.go` | WebSocket frame parsing and Responses API recording |
| `store.go` | JSONL log, today's totals, system prompt storage |
| `ca.go` | Local CA and per-host certificates |
| `ui.go`, `web/index.html` | Dashboard |
| `run.go`, `serve.go`, `tail.go`, `main.go` | Commands |
