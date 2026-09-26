# tokscoop

A local proxy that sits in front of requests from AI coding tools like Claude Code and Codex,
recording **input/output token counts, prompts, and system prompts** so you can check them in
your browser. It's a single Go binary with no external dependencies.

```
claude ──HTTPS_PROXY──▶ tokscoop (127.0.0.1:8899) ──▶ api.anthropic.com
                          │ Decrypts only AI API hosts to read usage
                          │ Everything else (github.com, etc.) passes through untouched
                          ▼
                 ~/.tokscoop/logs/usage.jsonl ──▶ http://127.0.0.1:8899/
```

## Install

```sh
# macOS
brew install --cask cobayo/tap/tokscoop
```

For Linux, Windows, or manual installation, unpack the tar.gz / zip from the
[Releases page](https://github.com/cobayo/tokscoop/releases) and put the `tokscoop`
binary on your PATH, or [build from source](#build-from-source).

## Quickstart: start the proxy, then use Claude

Start tokscoop once, then launch `claude` normally from a shell configured to use the proxy.
The following commands are for bash / zsh on macOS or Linux. If you built from source and
haven't added the binary to your PATH, use `./tokscoop` instead of `tokscoop`.

**1. Start the proxy in terminal 1:**

```sh
tokscoop adhocrun
```

Leave this terminal running. `adhocrun` is an alias for `serve`: it runs in the foreground
and stops when you press Ctrl+C. If tokscoop is already running on port 8899, use that
instance and continue with step 2.

**2. Configure terminal 2 and launch Claude:**

```sh
eval "$(tokscoop env --shell sh)"
claude
```

This sets the proxy and CA certificate variables for Claude and Codex.
To use Codex, run `codex` instead of `claude` after the same `eval` command.
Claude reads `NODE_EXTRA_CA_CERTS`; Codex reads `CODEX_CA_CERTIFICATE`.
Setting only `NODE_EXTRA_CA_CERTS` does not configure Codex's certificate trust.
These settings apply to this shell and the programs you launch from it; existing
processes must be restarted. Existing CA bundles are preserved.

**3. Send a prompt, then check the dashboard:**

Open **http://127.0.0.1:8899/** to see the latest 30 requests after they finish.
You can also run `tokscoop tail` in another terminal. Records are saved in
`~/.tokscoop/logs/usage.jsonl`.

When finished, exit Claude and stop tokscoop with Ctrl+C in terminal 1. Close terminal 2
to discard its environment settings, or restore your previous proxy and CA settings before
continuing to use it.

### Generate shell settings

With `tokscoop adhocrun` running in terminal 1, apply the proxy and certificate
settings in terminal 2, then launch your tool:

```sh
# bash / zsh
eval "$(tokscoop env --shell sh)"
claude
```

The startup message uses the executable you launched. For a local build such as
`./tokscoop adhocrun`, it prints an absolute executable path so the command also
works from another directory. Paths containing spaces are quoted automatically.

For other shells:

```fish
# fish
tokscoop env --shell fish | source
```

```powershell
# PowerShell
tokscoop env --shell powershell | Invoke-Expression
```

`tokscoop env` includes settings for Claude Code and Codex and preserves
existing CA bundles. It prints shell commands; it does not start the proxy.
For example, after applying these settings you can launch `codex` directly
instead of `claude`. See [Coverage](#coverage) for verification status and limitations.

If you use a custom `TOKSCOOP_HOME`, set it in both terminals. If you start the proxy with
`tokscoop adhocrun --listen 127.0.0.1:18899`, change the manual proxy URL to that port,
or use `tokscoop env --listen 127.0.0.1:18899`. The startup command includes this
option automatically when the server is started with `--listen`.

### Alternative: configure one command only

Use `run` if you prefer tokscoop to configure and launch a single tool:

```sh
tokscoop run -- claude
tokscoop run -- codex
```

`run` passes proxy and certificate variables **only to the command it launches**.
If tokscoop is already running, it uses that instance; otherwise it starts a temporary
proxy that stops when the tool exits. It does not change your shell configuration or
OS certificate store.

## Build from source

Building from source requires Go 1.22+. There are no external dependencies.

```sh
git clone https://github.com/cobayo/tokscoop.git
cd tokscoop
go build -o tokscoop .
./tokscoop adhocrun
```

Put the resulting `tokscoop` binary on your PATH, or keep using `./tokscoop` from the
repository directory in the commands above.

## What gets recorded

One line is appended to `~/.tokscoop/logs/usage.jsonl` per request.

| Field | Description |
|---|---|
| `client` | Claude Code / Codex, etc. (detected from the User-Agent) |
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
time. Instead of the whole history, tokscoop keeps only the latest human message in `prompt`, and
marks continuation requests (following a tool result) with `tool_result`. Text that tools inject
automatically, like `<system-reminder>` or `<environment_context>`, is not treated as a prompt.

Since providers differ on whether cached tokens count toward input, tokscoop always normalizes so
that `total_input_tokens = input_tokens + cache_read_tokens + cache_write_tokens`.

### System prompts

Claude Code sends a system prompt that's tens of thousands of characters long on every request, so
instead of writing it into the log directly, tokscoop hashes the content into an ID and stores it
once at `~/.tokscoop/system-prompts/<ID>.txt`. You can follow it from the log's
`system_prompt_id`, or read it by expanding a row on the dashboard and clicking "Show full text".
Whenever the ID changes, the system prompt changed.

- Anthropic format: `system`
- OpenAI Responses (Codex): `instructions`, plus messages with the `system` / `developer` role
- Chat Completions: messages with the `system` / `developer` role
- Bedrock Converse: `system`

### Aggregating with jq

```sh
# Today's totals (ts is recorded in local time)
jq -s --arg d "$(date +%F)" 'map(select(.ts | startswith($d)))
  | {requests: length, input: (map(.total_input_tokens) | add), output: (map(.output_tokens) | add)}' \
  ~/.tokscoop/logs/usage.jsonl

# Output tokens by model
jq -s 'group_by(.model) | map({model: .[0].model, output: (map(.output_tokens) | add)})' ~/.tokscoop/logs/usage.jsonl
```

## Coverage

tokscoop only inspects traffic to the hosts below; everything else passes through undecrypted.

| Path | Host | Format | Verified |
|---|---|---|---|
| Claude Code (API key / Claude account) | api.anthropic.com | Messages (SSE / JSON) | Verified relaying to the real API; usage parsing is mocked |
| Claude Code on Bedrock | bedrock-runtime.*.amazonaws.com | InvokeModel(Stream), Converse(Stream) | Mocked (including not breaking SigV4 signing) |
| Codex (ChatGPT login) | chatgpt.com | Responses (SSE / WebSocket) | Mocked; see Codex verification note below |
| Codex (API key) | api.openai.com | Responses | Mocked |
| LiteLLM | specified via `extra_hosts` | Messages / Chat Completions / Responses | Mocked |

Verification notes:

- **Codex CLI**: The maintainer has confirmed real-world logging. The authentication mode
  and transport used for that check were not recorded, so the route-specific entries above
  retain their mock-test status. This does not establish coverage of every Codex mode.
  If records are missing, check the terminal running `adhocrun` / `serve`, or
  `~/.tokscoop/logs/tokscoop.log` when using `run`. tokscoop supplies a bundle of system
  certificates plus its own CA through `env` / `run` (on Windows, only tokscoop's CA
  is supplied because system certificates cannot be extracted as a file).
  If the proxy reports a client TLS handshake failure, exit Codex, run
  `eval "$(tokscoop env --shell sh)"` in the terminal where you will launch it, and
  start `codex` again. A manual `NODE_EXTRA_CA_CERTS` export is only for Node-based
  clients and is insufficient for Codex. Check `echo "$CODEX_CA_CERTIFICATE"`
  in that same terminal if the error persists.

## Configuration

`~/.tokscoop/config.json` (optional):

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

These can also be overridden with the environment variables `TOKSCOOP_HOME` (storage location),
`TOKSCOOP_LISTEN` (listen address), and `TOKSCOOP_UPSTREAM_PROXY`.

## About Windows

Download the Windows zip from the Releases page, extract it, and put `tokscoop.exe`
on your PATH. Then run `tokscoop adhocrun` in one PowerShell window. In another,
apply the [PowerShell environment settings](#generate-shell-settings) and launch `claude`.
Certificates are configured through environment variables, so no administrator privileges
or certificate store registration are needed. For tools used inside
WSL, install the Linux build inside WSL to record them.

Only if you need to record tools that don't read environment variables, register the CA with your
OS by following the instructions from `tokscoop ca`.

## Security

- Only listens on `127.0.0.1`. The dashboard rejects requests whose Host header isn't itself (DNS
  rebinding protection).
- `~/.tokscoop/ca-key.pem` is a key that lets you impersonate AI API hosts to any process that
  trusts this CA. Don't share it. If deleted, it's regenerated on next startup.
- Prompts and system prompts remain in the log. Files are created with owner-only read permissions
  (0600). API keys, auth headers, and URL query strings are never recorded.
- Commands run by the tool (`npm install`, `git`, etc.) also inherit the proxy environment
  variables. Traffic to anything other than the AI APIs passes through undecrypted, so this has no
  effect — but calling something like `curl https://api.anthropic.com/...` directly from a command
  will hit a certificate error, since it doesn't know tokscoop's CA.

## Limitations

- Verification includes mock-server tests, relay to the real api.anthropic.com
  (unauthenticated), and maintainer-confirmed Codex CLI logging. Matching each provider's
  actual billed amounts has not been checked.
- When streaming Chat Completions with LiteLLM, token counts won't come back
  ("no usage data") unless the client sets `stream_options.include_usage`.
- Logs are not rotated. Move or delete `usage.jsonl` once it gets large.
- Only HTTP/1.1 is used between tokscoop and the client (HTTP/2 is only used upstream).

## Development

```sh
go test -race ./...
go build -o tokscoop .
```

Pushing a tag like `v0.1.0` triggers GitHub Actions to build with GoReleaser and update the
Homebrew tap. Set these up beforehand:

1. Create the public `cobayo/homebrew-tap` repository, initialized with a README.
2. Create a fine-grained GitHub personal access token with access only to
   `homebrew-tap` and repository permission **Contents: Read and write**.
   The tap must allow this token's owner to update its default branch directly.
3. In the source repository (`cobayo/tokscoop`), add the token under
   **Settings → Secrets and variables → Actions** as the repository secret
   `HOMEBREW_TAP_GITHUB_TOKEN`. Renew the token before it expires.
   `GITHUB_TOKEN` is provided automatically by GitHub Actions.
4. Push an unused version tag, then confirm the `release` workflow succeeds and
   `Casks/tokscoop.rb` appears in the tap repository.

The source repository and release assets must be public for installation without
authentication. The repository, distributed binary, and Homebrew cask are named `tokscoop`.

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
