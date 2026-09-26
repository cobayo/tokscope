package main

import (
	"net"
	"net/http"
	"path"
	"strings"
)

// Hosts whose traffic is decrypted and inspected. Everything else is
// passed through as an opaque tunnel and never looked at.
var builtinHosts = []struct{ pattern, provider string }{
	{"api.anthropic.com", "anthropic"},
	{"api.openai.com", "openai"},
	{"chatgpt.com", "openai"}, // Codex signed in with ChatGPT
	{"bedrock-runtime.*.amazonaws.com", "bedrock"},
	{"bedrock-runtime-fips.*.amazonaws.com", "bedrock"},
}

type hostRule struct {
	host     string // glob for the hostname
	port     string // "" matches any port
	provider string
}

type Matcher struct{ rules []hostRule }

func newMatcher(extra []string) *Matcher {
	m := &Matcher{}
	for _, b := range builtinHosts {
		m.rules = append(m.rules, hostRule{host: b.pattern, provider: b.provider})
	}
	for _, e := range extra {
		if h, p := splitHostEntry(e); h != "" {
			m.rules = append(m.rules, hostRule{host: h, port: p, provider: "litellm"})
		}
	}
	return m
}

// Match returns the provider name for "host:port", or "" if tokscope should
// not look at this traffic.
func (m *Matcher) Match(hostport string) string {
	hostport = strings.ToLower(hostport)
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host, port = hostport, ""
	}
	host = strings.Trim(host, "[]")
	for _, r := range m.rules {
		if r.port != "" && r.port != port {
			continue
		}
		if ok, _ := path.Match(r.host, host); ok {
			return r.provider
		}
	}
	return ""
}

const (
	apiAnthropic       = "anthropic_messages"
	apiResponses       = "openai_responses"
	apiChat            = "openai_chat"
	apiGemini          = "gemini"
	apiBedrockInvoke   = "bedrock_invoke"
	apiBedrockConverse = "bedrock_converse"
)

// detectAPI decides from the URL path which wire format a request uses.
// It returns "" for endpoints that do not consume tokens (token counting,
// model lists, telemetry and so on).
func detectAPI(provider, p string) (api, model string, stream bool) {
	switch {
	case strings.HasSuffix(p, "/count_tokens"), strings.Contains(p, ":countTokens"),
		strings.HasSuffix(p, "/input_tokens"), strings.Contains(p, "/threads/"):
		return "", "", false

	case provider == "bedrock" && strings.HasPrefix(p, "/model/"):
		rest := strings.TrimPrefix(p, "/model/")
		i := strings.LastIndex(rest, "/")
		if i < 0 {
			return "", "", false
		}
		model, op := rest[:i], rest[i+1:]
		switch op {
		case "invoke":
			return apiBedrockInvoke, model, false
		case "invoke-with-response-stream":
			return apiBedrockInvoke, model, true
		case "converse":
			return apiBedrockConverse, model, false
		case "converse-stream":
			return apiBedrockConverse, model, true
		}
		return "", "", false

	case strings.Contains(p, "/publishers/anthropic/models/"): // Claude on Vertex AI
		seg := p[strings.Index(p, "/publishers/anthropic/models/")+len("/publishers/anthropic/models/"):]
		model, method, _ := strings.Cut(seg, ":")
		switch method {
		case "rawPredict":
			return apiAnthropic, model, false
		case "streamRawPredict":
			return apiAnthropic, model, true
		}
		return "", "", false

	case strings.HasSuffix(p, "/messages"):
		return apiAnthropic, "", false

	case strings.HasSuffix(p, "/responses"):
		return apiResponses, "", false

	case strings.HasSuffix(p, "/chat/completions"):
		if i := strings.Index(p, "/deployments/"); i >= 0 { // Azure OpenAI
			model, _, _ = strings.Cut(p[i+len("/deployments/"):], "/")
		}
		return apiChat, model, false

	case strings.Contains(p, ":generateContent"), strings.Contains(p, ":streamGenerateContent"):
		if i := strings.LastIndex(p, "/models/"); i >= 0 {
			model, _, _ = strings.Cut(p[i+len("/models/"):], ":")
		}
		return apiGemini, model, strings.Contains(p, ":streamGenerateContent")
	}
	return "", "", false
}

// detectClient guesses which tool sent the request.
func detectClient(h http.Header) string {
	ua := h.Get("User-Agent")
	l := strings.ToLower(ua)
	switch {
	case strings.Contains(l, "claude-cli"), strings.Contains(l, "claude-code"), h.Get("X-App") == "cli":
		return "Claude Code"
	case strings.Contains(l, "codex"), strings.Contains(strings.ToLower(h.Get("Originator")), "codex"):
		return "Codex"
	case strings.Contains(l, "geminicli"), strings.Contains(l, "gemini-cli"):
		return "Gemini CLI"
	case strings.Contains(l, "litellm"):
		return "LiteLLM"
	case ua == "":
		return "不明"
	}
	name, _, _ := strings.Cut(strings.Fields(ua)[0], "/")
	if len(name) > 24 {
		name = name[:24]
	}
	return name
}
