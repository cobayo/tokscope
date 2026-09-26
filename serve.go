package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "", "待ち受けアドレス（既定 127.0.0.1:8899）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tokscoop:", err)
		return 1
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if running(cfg.Listen) {
		fmt.Fprintf(os.Stderr, "tokscoop はすでに %s で動いています。ダッシュボード: http://%s/\n", cfg.Listen, cfg.Listen)
		return 1
	}
	p, err := setup(cfg, log.New(os.Stderr, "tokscoop: ", log.LstdFlags))
	if err != nil {
		fmt.Fprintln(os.Stderr, "tokscoop:", err)
		return 1
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tokscoop: %s で待ち受けできません: %v\n", cfg.Listen, err)
		return 1
	}
	defer ln.Close()
	fmt.Fprintf(os.Stderr, "tokscoop %s\n  プロキシ        http://%s\n  ダッシュボード  http://%s/\n  CA 証明書       %s\n  ログ            %s\n\n別のターミナルで以下を実行してからツールを起動してください。\n",
		version, cfg.Listen, cfg.Listen, p.ca.CertPath, p.store.LogPath())
	command := envSetupCommand(os.Args[0], defaultShell(), *listen)
	fmt.Fprintf(os.Stderr, "\nclaude:\n  %s\n  claude\n\ncodex:\n  %s\n  codex\n\n", command, command)
	fmt.Fprintln(os.Stderr, "env は両ツール共通です（Claude: NODE_EXTRA_CA_CERTS / Codex: CODEX_CA_CERTIFICATE）。設定後にツールを起動してください。")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := newServer(p)
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "tokscoop:", err)
		return 1
	}
	return 0
}

// Use an absolute path for local builds: the other terminal may be in a different directory.
func envSetupCommand(executable, shell, listen string) string {
	if strings.ContainsAny(executable, `/\`) {
		if absolute, err := filepath.Abs(executable); err == nil {
			executable = absolute
		}
	}
	quote := func(s string) string {
		if s != "" && !strings.ContainsAny(s, " \t\r\n'\"`$\\;|&()<>*?[]{}!~") {
			return s
		}
		switch shell {
		case "powershell", "pwsh":
			return "'" + strings.ReplaceAll(s, "'", "''") + "'"
		case "fish":
			return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
		default:
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	command := quote(executable) + " env"
	if listen != "" {
		command += " --listen " + quote(listen)
	}
	switch shell {
	case "fish":
		return command + " --shell fish | source"
	case "powershell", "pwsh":
		return "& " + command + " --shell powershell | Invoke-Expression"
	default:
		return "eval \"$(" + command + " --shell sh)\""
	}
}

func newServer(p *Proxy) *http.Server {
	return &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second, ErrorLog: p.log}
}

// running reports whether a tokscoop instance already answers on addr.
func running(addr string) bool {
	c := &http.Client{Timeout: 700 * time.Millisecond, Transport: &http.Transport{Proxy: nil}}
	resp, err := c.Get("http://" + addr + "/api/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var h struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&h)
	return h.Name == "tokscoop"
}
