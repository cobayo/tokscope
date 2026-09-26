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
```

For Linux, Windows, or manual installation, unpack the tar.gz / zip from the
[Releases page](https://github.com/cobayo/tokscoop/releases) and put the `tokscope`
binary on your PATH, or [build from source](#build-from-source).

## Quickstart: start the proxy, then use Claude

Start tokscope once, then launch `claude` normally from a shell configured to use the proxy.
The following commands are for bash / zsh on macOS or Linux. If you built from source and
haven't added the binary to your PATH, use `./tokscope` instead of `tokscope`.

**1. Start the proxy in terminal 1:**

```sh
tokscope adhocrun
```

Leave this terminal running. `adhocrun` is an alias for `serve`: it runs in the foreground
and stops when you press Ctrl+C. If tokscope is already running on port 8899, use that
instance and continue with step 2.

**2. Configure terminal 2 and launch Claude:**

```sh
export https_proxy=http://127.0.0.1:8899
export NODE_EXTRA_CA_CERTS="$HOME/.tokscope/ca.pem"
claude
```

Use `http://` in the proxy URL, even for HTTPS requests. `NODE_EXTRA_CA_CERTS` lets Claude
trust tokscope's CA so that tokscope can inspect HTTPS traffic. These settings apply to
this shell and the programs you launch from it; no OS certificate registration is needed.
The example assumes the default storage directory and no existing `NODE_EXTRA_CA_CERTS`.
For a custom setup or an existing CA bundle, use the [generated settings below](#generate-shell-settings).

**3. Send a prompt, then check the dashboard:**

Open **http://127.0.0.1:8899/** to see the latest 30 requests after they finish.
You can also run `tokscope tail` in another terminal. Records are saved in
`~/.tokscope/logs/usage.jsonl`.

When finished, exit Claude and stop tokscope with Ctrl+C in terminal 1. Close terminal 2
to discard its environment settings, or restore your previous proxy and CA settings before
continuing to use it.

### Generate shell settings

With `tokscope adhocrun` running in terminal 1, you can have tokscope generate all proxy
and certificate variables in terminal 2 instead of writing the exports yourself:

```sh
# bash / zsh
eval "$(tokscope env)"
claude
```

For other shells, use the matching command before launching your tool:

```fish
# fish
tokscope env --shell fish | source
claude
```

```powershell
# PowerShell
tokscope env --shell powershell | Invoke-Expression
claude
```

`tokscope env` includes settings for Claude Code, Codex, and Gemini CLI and preserves
existing CA bundles. It prints shell commands; it does not start the proxy.
For example, after applying these settings you can launch `codex` or `gemini` directly
instead of `claude`. See [Coverage](#coverage) for verification status and limitations.

If you use a custom `TOKSCOPE_HOME`, set it in both terminals. If you start the proxy with
`tokscope adhocrun --listen 127.0.0.1:18899`, change the manual proxy URL to that port,
or set `TOKSCOPE_LISTEN=127.0.0.1:18899` before running `tokscope env`.

### Alternative: configure one command only

Use `run` if you prefer tokscope to configure and launch a single tool:

```sh
tokscope run -- claude
tokscope run -- codex
tokscope run -- gemini
```

`run` passes proxy and certificate variables **only to the command it launches**.
If tokscope is already running, it uses that instance; otherwise it starts a temporary
proxy that stops when the tool exits. It does not change your shell configuration or
OS certificate store.

## Build from source

Building from source requires Go 1.22+. There are no external dependencies.

```sh
git clone https://github.com/cobayo/tokscoop.git
cd tokscoop
go build -o tokscope .
./tokscope adhocrun
```

Put the resulting `tokscope` binary on your PATH, or keep using `./tokscope` from the
repository directory in the commands above.

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
| Codex (ChatGPT login) | chatgpt.com | Responses (SSE / WebSocket) | Mocked; see Codex verification note below |
| Codex (API key) | api.openai.com | Responses | Mocked |
| Azure OpenAI | *.openai.azure.com, etc. | Chat Completions / Responses | Mocked |
| Gemini CLI (Google login) | cloudcode-pa.googleapis.com | Code Assist | Mocked, **needs real-world verification** |
| Gemini API / Vertex Gemini | generativelanguage.googleapis.com, *-aiplatform.googleapis.com | generateContent (SSE / JSON) | Mocked |
| LiteLLM | specified via `extra_hosts` | Messages / Chat Completions / Responses | Mocked |

Verification notes:

- **Codex CLI**: The maintainer has confirmed real-world logging. The authentication mode
  and transport used for that check were not recorded, so the route-specific entries above
  retain their mock-test status. This does not establish coverage of every Codex mode.
  If records are missing, check the terminal running `adhocrun` / `serve`, or
  `~/.tokscope/logs/tokscope.log` when using `run`. tokscope supplies a bundle of system
  certificates plus its own CA through `env` / `run` (on Windows, only tokscope's CA
  is supplied because system certificates cannot be extracted as a file).
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

Download the Windows zip from the Releases page, extract it, and put `tokscope.exe`
on your PATH. Then run `tokscope adhocrun` in one PowerShell window. In another,
apply the [PowerShell environment settings](#generate-shell-settings) and launch `claude`.
Certificates are configured through environment variables, so no administrator privileges
or certificate store registration are needed. For tools used inside
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

- Verification includes mock-server tests, relay to the real api.anthropic.com
  (unauthenticated), and maintainer-confirmed Codex CLI logging. Matching each provider's
  actual billed amounts has not been checked.
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
   `Casks/tokscope.rb` appears in the tap repository.

The source repository and release assets must be public for installation without
authentication. The repository is named `tokscoop`; the distributed binary and
Homebrew cask are named `tokscope`.

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
