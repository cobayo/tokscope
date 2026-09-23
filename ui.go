package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

func newUI(p *Proxy) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"name": "tokscope", "version": version, "ca": p.ca.CertPath})
	})
	mux.HandleFunc("GET /api/recent", func(w http.ResponseWriter, r *http.Request) {
		recs, today := p.store.Snapshot()
		writeJSON(w, map[string]any{"records": recs, "today": today})
	})
	mux.HandleFunc("GET /api/stream", p.serveStream)
	mux.HandleFunc("GET /api/system-prompts/{id}", func(w http.ResponseWriter, r *http.Request) {
		b, err := p.store.SystemPrompt(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /ca.pem", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-pem-file")
		http.ServeFile(w, r, p.ca.CertPath)
	})
	return p.localOnly(mux)
}

// localOnly rejects requests whose Host is not this machine, so that a web
// page cannot read the dashboard through DNS rebinding.
func (p *Proxy) localOnly(h http.Handler) http.Handler {
	_, port, _ := net.SplitHostPort(p.cfg.Listen)
	allowed := map[string]bool{
		strings.ToLower(p.cfg.Listen): true,
		"localhost:" + port:           true,
		"127.0.0.1:" + port:           true,
		"[::1]:" + port:               true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[strings.ToLower(r.Host)] {
			http.Error(w, "tokscope: forbidden host", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func (p *Proxy) serveStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	ch, cancel := p.store.Subscribe()
	defer cancel()
	_, _ = fmt.Fprint(w, "retry: 2000\n\n")
	fl.Flush()
	t := time.NewTicker(25 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			_, _ = fmt.Fprint(w, "event: update\ndata: {}\n\n")
		case <-t.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
		}
		fl.Flush()
	}
}
