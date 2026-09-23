# tokscope

Claude Code や Codex などの AI コーディングツールが送るリクエストを手元で中継し、**入力・出力トークン数、プロンプト、システムプロンプト**を記録してブラウザで確認できるようにするローカルプロキシです。Go 製の単一バイナリで、外部ライブラリには依存していません。

```
claude ──HTTPS_PROXY──▶ tokscope (127.0.0.1:8899) ──▶ api.anthropic.com
                          │ AI API のホストだけ復号して usage を読む
                          │ それ以外（github.com など）は中身を見ずに素通し
                          ▼
                 ~/.tokscope/logs/usage.jsonl ──▶ http://127.0.0.1:8899/
```

## インストール

```sh
# macOS
brew install --cask cobayo/tap/tokscope

# Windows (PowerShell)
scoop bucket add tokscope https://github.com/cobayo/scoop-bucket
scoop install tokscope

# Linux / その他（Go 1.22 以降）
go install github.com/cobayo/tokscope@latest
```

Releases ページの tar.gz / zip を展開して PATH に置いても使えます。

## ビルド

ソースからビルドする場合は Go 1.22 以降が必要です。外部ライブラリには依存していないため、`go build` だけで完結します。

```sh
git clone https://github.com/cobayo/tokscope.git
cd tokscope
go build -o tokscope .
```

できあがった `tokscope` バイナリを PATH の通った場所に置いてください。

## 使い方

ツールの前に `tokscope run --` を付けて起動します。

```sh
tokscope run -- claude
tokscope run -- codex
tokscope run -- gemini
```

起動中は http://127.0.0.1:8899/ で最新 30 件のリクエストを確認できます。ターミナルで見たいときは `tokscope tail` を使います。

```
TIME      CLIENT       MODEL                             INPUT  CACHE     OUTPUT  PROMPT
08:24:33  Claude Code  claude-sonnet-4-5-20250929       32,326    96%        318  ↳ マイグレーションのロールバック手順も追加して
08:23:43  Claude Code  claude-sonnet-4-5-20250929       31,560    92%      2,651  マイグレーションのロールバック手順も追加して
08:17:03  Gemini CLI   gemini-2.5-pro                    9,480     0%        730  このリポジトリの構成を説明して
```

`run` は、プロキシ用の環境変数（`HTTPS_PROXY`、`NODE_EXTRA_CA_CERTS`、`CODEX_CA_CERTIFICATE` など）を**起動したコマンドにだけ**渡します。OS の証明書ストアやシェルの設定は変更しません。tokscope がすでに動いていればそれに記録し、動いていなければツールの終了まで一時的に起動します。

### 常駐させる場合

IDE 拡張など、`run` を挟みにくいツールも記録したい場合はプロキシを常駐させます。

```sh
tokscope serve                       # 別のターミナルで起動したままにする
eval "$(tokscope env)"               # bash / zsh
tokscope env | source                # fish
tokscope env | Invoke-Expression     # PowerShell
```

## 記録される内容

1 リクエストごとに `~/.tokscope/logs/usage.jsonl` へ 1 行追記されます。

| 項目 | 内容 |
|---|---|
| `client` | Claude Code / Codex / Gemini CLI など（User-Agent から判定） |
| `model` | レスポンスが返したモデル名（なければリクエストや URL のモデル名） |
| `input_tokens` | キャッシュを使わなかった入力 |
| `cache_read_tokens` / `cache_write_tokens` | キャッシュから読んだ入力 / キャッシュに書き込んだ入力 |
| `total_input_tokens` | 上の 3 つの合計 |
| `output_tokens` / `reasoning_tokens` | 出力（推論・thinking を含む）/ そのうち推論分 |
| `prompt` | 会話の中で最も新しい、人が書いたユーザー発言 |
| `prompt_kind` | `user`（新しい発言への応答）/ `tool_result`（ツール実行結果を受けての続き） |
| `system_prompt_id` | システムプロンプトの ID（下記） |
| `status` / `error` / `duration_ms` | HTTP ステータス、中断などのエラー、所要時間 |

エージェント系のツールは 1 つの指示に対して何十回もリクエストを送り、そのたびに会話全体を送り直します。tokscope は履歴全体ではなく最新の人の発言だけを `prompt` に残し、ツール実行の続きのリクエストには `tool_result` の印を付けます。`<system-reminder>` や `<environment_context>` のようにツールが自動で差し込む文は、プロンプトとして扱いません。

プロバイダーによって「キャッシュ分が入力に含まれるか」の数え方が違うため、tokscope は常に `total_input_tokens = input_tokens + cache_read_tokens + cache_write_tokens` になるようにそろえています。

### システムプロンプト

Claude Code は毎回数万文字のシステムプロンプトを送るため、ログには直接書かず、内容のハッシュを ID にして `~/.tokscope/system-prompts/<ID>.txt` に 1 回だけ保存します。ログの `system_prompt_id` からたどれるほか、ダッシュボードで行を開いて「全文を表示」を押すと読めます。ID が変わったところが、システムプロンプトが変わったところです。

- Anthropic 形式: `system`
- OpenAI Responses（Codex）: `instructions` と、`system` / `developer` ロールのメッセージ
- Chat Completions: `system` / `developer` ロールのメッセージ
- Gemini: `systemInstruction`
- Bedrock Converse: `system`

### jq での集計例

```sh
# 今日の合計（ts はローカル時刻で記録されます）
jq -s --arg d "$(date +%F)" 'map(select(.ts | startswith($d)))
  | {requests: length, input: (map(.total_input_tokens) | add), output: (map(.output_tokens) | add)}' \
  ~/.tokscope/logs/usage.jsonl

# モデル別の出力トークン
jq -s 'group_by(.model) | map({model: .[0].model, output: (map(.output_tokens) | add)})' ~/.tokscope/logs/usage.jsonl
```

## 対応状況

中身を見るのは下のホストへの通信だけで、それ以外はすべて復号せずに素通しします。

| 経路 | ホスト | 形式 | 検証 |
|---|---|---|---|
| Claude Code（API キー / Claude アカウント） | api.anthropic.com | Messages（SSE / JSON） | 実 API への中継を確認、usage 解析はモック |
| Claude Code on Bedrock | bedrock-runtime.*.amazonaws.com | InvokeModel(Stream)、Converse(Stream) | モック（SigV4 署名を壊さないことを含む） |
| Claude Code on Vertex AI | *-aiplatform.googleapis.com | rawPredict / streamRawPredict | モック |
| Claude on Azure AI Foundry | *.services.ai.azure.com | Messages | モック |
| Codex（ChatGPT ログイン） | chatgpt.com | Responses（SSE / WebSocket） | モック、**要実機確認** |
| Codex（API キー） | api.openai.com | Responses | モック |
| Azure OpenAI | *.openai.azure.com ほか | Chat Completions / Responses | モック |
| Gemini CLI（Google ログイン） | cloudcode-pa.googleapis.com | Code Assist | モック、**要実機確認** |
| Gemini API / Vertex Gemini | generativelanguage.googleapis.com、*-aiplatform.googleapis.com | generateContent（SSE / JSON） | モック |
| LiteLLM | `extra_hosts` で指定 | Messages / Chat Completions / Responses | モック |

要実機確認としている点は次のとおりです。

- **Codex**: 新しい版は Responses API に WebSocket を使うことがあります。tokscope は WebSocket のフレームも読んで記録しますが、Codex の WebSocket 接続が `HTTPS_PROXY` に従うかは未確認です。Codex の行が出ない場合は `~/.tokscope/logs/tokscope.log` を確認してください。また `CODEX_CA_CERTIFICATE` が既定の信頼ストアに追加されるのか置き換えるのかが未確認のため、tokscope は念のためシステムの証明書と tokscope の CA をまとめたファイルを渡しています（Windows ではシステムの証明書をファイルとして取り出せないため、tokscope の CA だけを渡します）。
- **Gemini CLI**: プロキシの環境変数を読むかどうかが未確認です。記録されない場合は Gemini CLI 側のプロキシ設定を `http://127.0.0.1:8899` にしてください。

## 設定

`~/.tokscope/config.json`（任意）:

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

| 項目 | 説明 |
|---|---|
| `extra_hosts` | 追加で中身を見るホスト（LiteLLM など）。`host`、`host:port`、URL の形で書けます |
| `upstream_proxy` | 社内プロキシを経由させる場合の URL（`http://user:pass@host:port` も可）。localhost 宛てには使いません |
| `save_prompt` / `save_system_prompt` | `false` にすると保存しません（トークン数は記録されます） |
| `prompt_max_chars` | 保存するプロンプトの最大文字数 |
| `recent` | ダッシュボードに表示する件数（既定 30） |

環境変数 `TOKSCOPE_HOME`（保存先）、`TOKSCOPE_LISTEN`（待ち受けアドレス）、`TOKSCOPE_UPSTREAM_PROXY` でも上書きできます。

## Windows について

同じバイナリがそのまま動きます。Scoop で入れて、PowerShell で `tokscope run -- claude` のように使ってください。証明書は環境変数で子プロセスに渡すため、管理者権限も証明書ストアへの登録も不要です。WSL の中で使うツールは、WSL に Linux 版を入れて記録してください。

環境変数を読まないツールまで記録したい場合だけ、`tokscope ca` の案内に従って CA を OS に登録します。

## セキュリティ

- 待ち受けは `127.0.0.1` だけです。ダッシュボードは Host ヘッダーが自分自身でないリクエストを拒否します（DNS リバインディング対策）。
- `~/.tokscope/ca-key.pem` は、この CA を信頼しているプロセスに対して AI API のホストになりすませる鍵です。共有しないでください。消すと次回起動時に作り直されます。
- ログにはプロンプトとシステムプロンプトが残ります。ファイルは所有者だけが読める権限（0600）で作られます。API キー、認証ヘッダー、URL のクエリ文字列は記録しません。
- ツールが実行するコマンド（`npm install` や `git` など）にもプロキシの環境変数が引き継がれます。AI API 以外への通信は中身を見ずに素通しするので影響はありませんが、コマンドから直接 `curl https://api.anthropic.com/...` のように呼ぶと、tokscope の CA を知らないため証明書エラーになります。

## 制限

- 検証はモックのサーバーを使ったテストと、実際の api.anthropic.com への中継確認（認証なし）までです。各社の請求額と一致するかは確かめていません。
- Azure OpenAI や LiteLLM の Chat Completions をストリーミングで使う場合、クライアントが `stream_options.include_usage` を付けないとトークン数が返らず「使用量なし」になります。
- ログのローテーションはしません。大きくなったら `usage.jsonl` を移動・削除してください。
- クライアントとの間は HTTP/1.1 のみです（HTTP/2 は上流側だけで使います）。

## 開発

```sh
go test -race ./...
go build -o tokscope .
```

`v0.1.0` のようなタグを push すると、GitHub Actions が GoReleaser でビルドし、Homebrew tap と Scoop bucket を更新します。事前に次を用意してください。

1. 空のリポジトリ `homebrew-tap` と `scoop-bucket` を作る
2. それらに push できるトークンを、Secrets の `HOMEBREW_TAP_GITHUB_TOKEN` と `SCOOP_BUCKET_GITHUB_TOKEN` に登録する

| ファイル | 役割 |
|---|---|
| `proxy.go` | CONNECT の処理、対象ホストだけの TLS 終端、中継、WebSocket |
| `match.go` | 対象ホストの一覧、API 形式とクライアントの判定 |
| `reqparse.go` | リクエストからモデル、システムプロンプト、最新のプロンプトを取り出す |
| `usage.go` | SSE / JSON / AWS event-stream / gzip からトークン数を取り出す |
| `ws.go` | WebSocket フレームの解析と Responses API の記録 |
| `store.go` | JSONL ログ、今日の合計、システムプロンプトの保存 |
| `ca.go` | ローカル CA とホストごとの証明書 |
| `ui.go`、`web/index.html` | ダッシュボード |
| `run.go`、`serve.go`、`tail.go`、`main.go` | コマンド |
