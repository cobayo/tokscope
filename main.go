package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

var version = "dev"

const usageText = `tokscoop — AI コーディングツールのトークン消費を記録するローカルプロキシ

使い方:
  tokscoop run -- <command> [args...]    プロキシ経由でコマンドを起動します（例: tokscoop run -- claude）
  tokscoop serve [--listen addr]         プロキシとダッシュボードを常駐させます
  tokscoop adhocrun [--listen addr]      serve と同じ（別ターミナルでツールを起動）
  tokscoop tail [-n 10] [--json]         最新の記録をターミナルに表示します
  tokscoop env [--shell sh|fish|powershell|cmd] [--listen addr]
                                         serve と組み合わせて使う環境変数を出力します
  tokscoop ca [--path]                   CA 証明書の場所と、OS に登録する方法を表示します
  tokscoop version

ダッシュボード: http://127.0.0.1:8899/ （run または serve の実行中）
データの保存先: %s （TOKSCOOP_HOME で変更できます）
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usageText, homeDir())
		os.Exit(2)
	}
	code := 0
	switch os.Args[1] {
	case "run":
		code = cmdRun(os.Args[2:])
	case "serve", "adhocrun":
		code = cmdServe(os.Args[2:])
	case "tail":
		code = cmdTail(os.Args[2:])
	case "env":
		code = cmdEnv(os.Args[2:])
	case "ca":
		code = cmdCA(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("tokscoop", version)
	case "help", "--help", "-h":
		fmt.Printf(usageText, homeDir())
	default:
		fmt.Fprintf(os.Stderr, "不明なコマンドです: %s\n\n", os.Args[1])
		fmt.Fprintf(os.Stderr, usageText, homeDir())
		code = 2
	}
	os.Exit(code)
}

func setup(cfg *Config, logger *log.Logger) (*Proxy, error) {
	dir := homeDir()
	ca, err := loadOrCreateCA(dir)
	if err != nil {
		return nil, fmt.Errorf("CA 証明書を用意できませんでした: %w", err)
	}
	store, err := openStore(dir, cfg.Recent)
	if err != nil {
		return nil, fmt.Errorf("ログを開けませんでした: %w", err)
	}
	return newProxy(cfg, ca, store, logger)
}

// fileLogger is used by "run", so that proxy messages do not draw over the
// child's terminal UI.
func fileLogger() *log.Logger {
	dir := filepath.Join(homeDir(), "logs")
	_ = os.MkdirAll(dir, 0o700)
	f, err := os.OpenFile(filepath.Join(dir, "tokscoop.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return log.New(io.Discard, "", 0)
	}
	return log.New(f, "", log.LstdFlags)
}
