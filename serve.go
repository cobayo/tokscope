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
		fmt.Fprintln(os.Stderr, "tokscope:", err)
		return 1
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if running(cfg.Listen) {
		fmt.Fprintf(os.Stderr, "tokscope はすでに %s で動いています。ダッシュボード: http://%s/\n", cfg.Listen, cfg.Listen)
		return 1
	}
	p, err := setup(cfg, log.New(os.Stderr, "tokscope: ", log.LstdFlags))
	if err != nil {
		fmt.Fprintln(os.Stderr, "tokscope:", err)
		return 1
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tokscope: %s で待ち受けできません: %v\n", cfg.Listen, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "tokscope %s\n  プロキシ        http://%s\n  ダッシュボード  http://%s/\n  CA 証明書       %s\n  ログ            %s\n\n別のターミナルで eval \"$(tokscope env)\" を実行してからツールを起動してください。\n",
		version, cfg.Listen, cfg.Listen, p.ca.CertPath, p.store.LogPath())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := newServer(p)
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "tokscope:", err)
		return 1
	}
	return 0
}

func newServer(p *Proxy) *http.Server {
	return &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second, ErrorLog: p.log}
}

// running reports whether a tokscope instance already answers on addr.
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
	return h.Name == "tokscope"
}
