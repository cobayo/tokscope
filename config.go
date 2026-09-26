package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Config is read from $TOKSCOPE_HOME/config.json (optional).
type Config struct {
	// Listen is the proxy + dashboard address. Default 127.0.0.1:8899.
	Listen string `json:"listen"`
	// ExtraHosts are additional hosts to inspect, e.g. a LiteLLM gateway:
	// "litellm.example.com", "localhost:4000", "https://llm.corp.example".
	ExtraHosts []string `json:"extra_hosts"`
	// UpstreamProxy is a corporate proxy to chain to, e.g. "http://proxy:8080".
	UpstreamProxy string `json:"upstream_proxy"`
	// SavePrompt / SaveSystemPrompt default to true.
	SavePrompt       *bool `json:"save_prompt"`
	SaveSystemPrompt *bool `json:"save_system_prompt"`
	// PromptMaxChars truncates the stored user prompt. Default 20000.
	PromptMaxChars int `json:"prompt_max_chars"`
	// Recent is how many records the dashboard shows. Default 30.
	Recent int `json:"recent"`
}

func homeDir() string {
	if d := os.Getenv("TOKSCOPE_HOME"); d != "" {
		return d
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ".tokscoop"
	}
	return filepath.Join(h, ".tokscoop")
}

func loadConfig() (*Config, error) {
	cfg := &Config{}
	b, err := os.ReadFile(filepath.Join(homeDir(), "config.json"))
	switch {
	case err == nil:
		if err := json.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("config.json の形式が正しくありません: %w", err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	if v := os.Getenv("TOKSCOPE_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8899"
	}
	if cfg.PromptMaxChars <= 0 {
		cfg.PromptMaxChars = 20000
	}
	if cfg.Recent <= 0 {
		cfg.Recent = 30
	}
	return cfg, nil
}

func (c *Config) savePrompt() bool       { return c.SavePrompt == nil || *c.SavePrompt }
func (c *Config) saveSystemPrompt() bool { return c.SaveSystemPrompt == nil || *c.SaveSystemPrompt }

// proxiesLocalhost reports whether an extra host points at this machine
// (e.g. a LiteLLM running on localhost:4000).
func (c *Config) proxiesLocalhost() bool {
	for _, h := range c.ExtraHosts {
		host, _ := splitHostEntry(h)
		if isLoopback(host) {
			return true
		}
	}
	return false
}

func isLoopback(host string) bool {
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// splitHostEntry accepts "host", "host:port" or "http(s)://host:port/...".
func splitHostEntry(e string) (host, port string) {
	e = strings.ToLower(strings.TrimSpace(e))
	if e == "" {
		return "", ""
	}
	if strings.Contains(e, "://") {
		u, err := url.Parse(e)
		if err != nil {
			return "", ""
		}
		return u.Hostname(), u.Port()
	}
	if h, p, err := net.SplitHostPort(e); err == nil {
		return strings.Trim(h, "[]"), p
	}
	return strings.Trim(e, "[]"), ""
}
