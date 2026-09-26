# tokscope

A local proxy that sits between AI coding tools (Claude Code and Codex) and their
APIs. It terminates TLS only for known AI API hosts, reads token usage from the traffic, and logs
it to `~/.tokscoop/logs/usage.jsonl` with a browser dashboard at `http://127.0.0.1:8899/`.
Everything else is passed through undecrypted. Single Go binary, no external dependencies.

## Build

Requires Go 1.22+.

```sh
go build -o tokscope .
go test -race ./...
```

## License

MIT — see `LICENSE`.
