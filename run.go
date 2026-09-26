package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

func cmdRun(args []string) int {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "使い方: tokscoop run -- <command> [args...]（例: tokscoop run -- claude）")
		return 2
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tokscoop:", err)
		return 1
	}

	var caPath string
	if running(cfg.Listen) {
		ca, err := loadOrCreateCA(homeDir())
		if err != nil {
			fmt.Fprintln(os.Stderr, "tokscoop:", err)
			return 1
		}
		caPath = ca.CertPath
		fmt.Fprintf(os.Stderr, "tokscoop: 起動中の tokscoop に記録します → http://%s/\n", cfg.Listen)
	} else {
		p, err := setup(cfg, fileLogger())
		if err != nil {
			fmt.Fprintln(os.Stderr, "tokscoop:", err)
			return 1
		}
		ln, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tokscoop: %s で待ち受けできません: %v\n（ポートを変えるには TOKSCOOP_LISTEN=127.0.0.1:18899 のように指定してください）\n", cfg.Listen, err)
			return 1
		}
		srv := newServer(p)
		go func() { _ = srv.Serve(ln) }()
		defer srv.Close()
		caPath = p.ca.CertPath
		fmt.Fprintf(os.Stderr, "tokscoop: 記録しています → http://%s/ （%s が終了するまで）\n", cfg.Listen, filepath.Base(args[0]))
	}

	if cur := firstEnv("HTTPS_PROXY", "https_proxy"); cur != "" && !pointsTo(cur, cfg.Listen) &&
		cfg.UpstreamProxy == "" && os.Getenv("TOKSCOOP_UPSTREAM_PROXY") == "" {
		fmt.Fprintf(os.Stderr, "tokscoop: 既存の HTTPS_PROXY（%s）を上書きします。社内プロキシが必要な場合は config.json の upstream_proxy に設定してください。\n", cur)
	}

	vars, err := proxyEnv(cfg, caPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tokscoop:", err)
		return 1
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = mergeEnv(os.Environ(), vars)

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "tokscoop: %s を起動できません: %v\n", args[0], err)
		return 127
	}
	go func() {
		for s := range sigs {
			if s == os.Interrupt {
				continue // Ctrl+C reaches the child directly from the terminal
			}
			_ = cmd.Process.Signal(s)
		}
	}()
	err = cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code >= 0 {
			return code
		}
		return 1
	}
	if err != nil {
		return 1
	}
	return 0
}

type envVar struct{ Key, Value string }

// proxyEnv returns the variables that route a tool through tokscoop and make
// it trust tokscoop's CA. They are given to the child process only.
func proxyEnv(cfg *Config, caPath string) ([]envVar, error) {
	proxyURL := "http://" + cfg.Listen
	vars := []envVar{{"HTTPS_PROXY", proxyURL}, {"HTTP_PROXY", proxyURL}}
	if runtime.GOOS != "windows" { // env names are case-insensitive on Windows
		vars = append(vars, envVar{"https_proxy", proxyURL}, envVar{"http_proxy", proxyURL})
	}

	// Claude Code: added to Node's built-in roots.
	nodeCA, err := bundle(caPath, os.Getenv("NODE_EXTRA_CA_CERTS"), false, "node-extra-ca.pem")
	if err != nil {
		return nil, err
	}
	vars = append(vars, envVar{"NODE_EXTRA_CA_CERTS", nodeCA})

	// Codex: CODEX_CA_CERTIFICATE (falls back to SSL_CERT_FILE). Include the
	// system roots so that non-inspected hosts keep working.
	codexBase := os.Getenv("CODEX_CA_CERTIFICATE")
	if codexBase == "" {
		codexBase = os.Getenv("SSL_CERT_FILE")
	}
	codexCA, err := bundle(caPath, codexBase, true, "codex-ca.pem")
	if err != nil {
		return nil, err
	}
	vars = append(vars, envVar{"CODEX_CA_CERTIFICATE", codexCA})

	// Keep localhost direct (IDE integrations, local MCP servers) unless a
	// local LiteLLM is configured to be inspected.
	loopbacks := []string{"localhost", "127.0.0.1", "::1"}
	cur := firstEnv("NO_PROXY", "no_proxy")
	var np string
	if cfg.proxiesLocalhost() {
		np = removeNoProxy(cur, loopbacks)
	} else {
		np = addNoProxy(cur, loopbacks)
	}
	vars = append(vars, envVar{"NO_PROXY", np})
	if runtime.GOOS != "windows" {
		vars = append(vars, envVar{"no_proxy", np})
	}
	return vars, nil
}

// bundle returns a PEM file containing base (or, if includeSystem and base
// is empty, the system roots) plus tokscoop's CA.
func bundle(caPath, base string, includeSystem bool, outName string) (string, error) {
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return "", err
	}
	var prefix []byte
	switch {
	case base != "":
		b, err := os.ReadFile(base)
		if err != nil {
			return "", fmt.Errorf("%s を読めません: %w", base, err)
		}
		if bytes.Contains(b, bytes.TrimSpace(caPEM)) {
			return base, nil
		}
		prefix = b
	case includeSystem:
		if sys := systemBundlePath(); sys != "" {
			prefix, _ = os.ReadFile(sys)
		}
	}
	if len(prefix) == 0 {
		return caPath, nil
	}
	out := filepath.Join(homeDir(), outName)
	content := append(append(bytes.TrimRight(prefix, "\n"), '\n'), caPEM...)
	if err := os.WriteFile(out, content, 0o644); err != nil {
		return "", err
	}
	return out, nil
}

func systemBundlePath() string {
	for _, p := range []string{
		"/etc/ssl/cert.pem", // macOS, Alpine
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/ssl/ca-bundle.pem",
		"/opt/homebrew/etc/ca-certificates/cert.pem",
		"/usr/local/etc/ca-certificates/cert.pem",
	} {
		if st, err := os.Stat(p); err == nil && st.Size() > 0 {
			return p
		}
	}
	return ""
}

func mergeEnv(base []string, vars []envVar) []string {
	same := func(a, b string) bool {
		if runtime.GOOS == "windows" {
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	out := make([]string, 0, len(base)+len(vars))
next:
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		for _, v := range vars {
			if same(k, v.Key) {
				continue next
			}
		}
		out = append(out, kv)
	}
	for _, v := range vars {
		out = append(out, v.Key+"="+v.Value)
	}
	return out
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func addNoProxy(cur string, hosts []string) string {
	list := splitList(cur)
	for _, h := range hosts {
		found := false
		for _, e := range list {
			if strings.EqualFold(e, h) {
				found = true
				break
			}
		}
		if !found {
			list = append(list, h)
		}
	}
	return strings.Join(list, ",")
}

func removeNoProxy(cur string, hosts []string) string {
	var list []string
	for _, e := range splitList(cur) {
		drop := false
		for _, h := range hosts {
			if strings.EqualFold(e, h) {
				drop = true
				break
			}
		}
		if !drop {
			list = append(list, e)
		}
	}
	return strings.Join(list, ",")
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func pointsTo(proxyURL, listen string) bool {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return false
	}
	_, port, err := net.SplitHostPort(listen)
	return err == nil && u.Port() == port && isLoopback(u.Hostname())
}

func defaultShell() string {
	def := "sh"
	if runtime.GOOS == "windows" {
		def = "powershell"
	} else if strings.HasSuffix(os.Getenv("SHELL"), "fish") {
		def = "fish"
	}
	return def
}

func cmdEnv(args []string) int {
	fs := flag.NewFlagSet("env", flag.ContinueOnError)
	listen := fs.String("listen", "", "プロキシの待ち受けアドレス")
	shell := fs.String("shell", defaultShell(), "sh | fish | powershell | cmd")
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
	ca, err := loadOrCreateCA(homeDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "tokscoop:", err)
		return 1
	}
	vars, err := proxyEnv(cfg, ca.CertPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tokscoop:", err)
		return 1
	}
	writeEnv(os.Stdout, *shell, vars)
	if !running(cfg.Listen) {
		fmt.Fprintln(os.Stderr, "tokscoop: まだプロキシが動いていません。先に tokscoop serve を起動してください。")
	}
	return 0
}

func writeEnv(w io.Writer, shell string, vars []envVar) {
	for _, v := range vars {
		switch shell {
		case "fish":
			fmt.Fprintf(w, "set -gx %s '%s';\n", v.Key, strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v.Value))
		case "powershell", "pwsh":
			fmt.Fprintf(w, "$env:%s = '%s'\n", v.Key, strings.ReplaceAll(v.Value, "'", "''"))
		case "cmd":
			fmt.Fprintf(w, "set %s=%s\n", v.Key, v.Value)
		default:
			fmt.Fprintf(w, "export %s='%s'\n", v.Key, strings.ReplaceAll(v.Value, "'", `'\''`))
		}
	}
}

const caHelp = `CA 証明書: %s

tokscoop run で起動したツールには、環境変数（NODE_EXTRA_CA_CERTS / CODEX_CA_CERTIFICATE）で
この証明書を渡すので、OS への登録は不要です。

環境変数を読まないツールも記録したい場合だけ、次のように OS に登録してください。
登録すると、このマシン上では tokscoop が対象ホストの通信を復号できるようになります。

  macOS:   security add-trusted-cert -r trustRoot -k ~/Library/Keychains/login.keychain-db "%s"
  Windows: certutil -user -addstore Root "%s"
  Linux:   sudo cp "%s" /usr/local/share/ca-certificates/tokscoop.crt && sudo update-ca-certificates

秘密鍵（%s）は共有しないでください。消すと次回起動時に作り直されます。
`

func cmdCA(args []string) int {
	fs := flag.NewFlagSet("ca", flag.ContinueOnError)
	pathOnly := fs.Bool("path", false, "パスだけを表示します")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ca, err := loadOrCreateCA(homeDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "tokscoop:", err)
		return 1
	}
	if *pathOnly {
		fmt.Println(ca.CertPath)
		return 0
	}
	key := filepath.Join(filepath.Dir(ca.CertPath), "ca-key.pem")
	fmt.Printf(caHelp, ca.CertPath, ca.CertPath, ca.CertPath, ca.CertPath, key)
	return 0
}
