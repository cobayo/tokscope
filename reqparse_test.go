package main

import "testing"

func TestParseAnthropicToolTurn(t *testing.T) {
	body := `{
	  "model": "claude-sonnet-4-5",
	  "stream": true,
	  "system": [
	    {"type": "text", "text": "You are Claude Code."},
	    {"type": "text", "text": "Be concise.", "cache_control": {"type": "ephemeral"}}
	  ],
	  "messages": [
	    {"role": "user", "content": "old question"},
	    {"role": "assistant", "content": "old answer"},
	    {"role": "user", "content": [
	      {"type": "text", "text": "<system-reminder>injected</system-reminder>"},
	      {"type": "text", "text": "テストが落ちる理由を調べて"}
	    ]},
	    {"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "Bash", "input": {}}]},
	    {"role": "user", "content": [
	      {"type": "tool_result", "tool_use_id": "t1", "content": "FAIL"},
	      {"type": "text", "text": "<system-reminder>more</system-reminder>"}
	    ]}
	  ]
	}`
	ri := parseRequest(apiAnthropic, []byte(body))
	if ri.Model != "claude-sonnet-4-5" || !ri.Stream || ri.Messages != 5 {
		t.Fatalf("basic fields: %+v", ri)
	}
	if ri.System != "You are Claude Code.\n\nBe concise." {
		t.Fatalf("system: %q", ri.System)
	}
	if ri.Prompt != "テストが落ちる理由を調べて" || ri.PromptKind != "tool_result" {
		t.Fatalf("prompt: %q kind %q", ri.Prompt, ri.PromptKind)
	}
}

func TestParseResponsesCodex(t *testing.T) {
	body := `{
	  "model": "gpt-5-codex",
	  "instructions": "You are Codex.",
	  "stream": true,
	  "input": [
	    {"type": "message", "role": "developer", "content": [{"type": "input_text", "text": "Sandbox: workspace-write"}]},
	    {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "<environment_context>cwd=/x</environment_context>"}]},
	    {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "READMEを直して"}]},
	    {"type": "function_call", "name": "shell", "call_id": "c1", "arguments": "{}"},
	    {"type": "function_call_output", "call_id": "c1", "output": "done"}
	  ]
	}`
	ri := parseRequest(apiResponses, []byte(body))
	if ri.System != "You are Codex.\n\nSandbox: workspace-write" {
		t.Fatalf("system: %q", ri.System)
	}
	if ri.Prompt != "READMEを直して" || ri.PromptKind != "tool_result" {
		t.Fatalf("prompt: %q kind %q", ri.Prompt, ri.PromptKind)
	}
}

func TestParseChat(t *testing.T) {
	body := `{"model":"gpt-4o","messages":[
	  {"role":"system","content":"sys A"},
	  {"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:"}}]}
	]}`
	ri := parseRequest(apiChat, []byte(body))
	if ri.System != "sys A" || ri.Prompt != "hello" || ri.PromptKind != "user" {
		t.Fatalf("%+v", ri)
	}
}

func TestParseGeminiCodeAssist(t *testing.T) {
	body := `{"model":"gemini-2.5-pro","project":"p","request":{
	  "systemInstruction":{"role":"user","parts":[{"text":"You are Gemini CLI."}]},
	  "contents":[
	    {"role":"user","parts":[{"text":"ビルドして"}]},
	    {"role":"model","parts":[{"functionCall":{"name":"run"}}]},
	    {"role":"user","parts":[{"functionResponse":{"name":"run","response":{}}}]}
	  ]}}`
	ri := parseRequest(apiGemini, []byte(body))
	if ri.Model != "gemini-2.5-pro" || ri.System != "You are Gemini CLI." {
		t.Fatalf("%+v", ri)
	}
	if ri.Prompt != "ビルドして" || ri.PromptKind != "tool_result" || ri.Messages != 3 {
		t.Fatalf("%+v", ri)
	}
}

func TestParseConverse(t *testing.T) {
	body := `{"system":[{"text":"S1"},{"cachePoint":{"type":"default"}}],"messages":[{"role":"user","content":[{"text":"hi"}]}]}`
	ri := parseRequest(apiBedrockConverse, []byte(body))
	if ri.System != "S1" || ri.Prompt != "hi" {
		t.Fatalf("%+v", ri)
	}
}

func TestDetectAPI(t *testing.T) {
	cases := []struct {
		provider, path, api, model string
		stream                     bool
	}{
		{"anthropic", "/v1/messages", apiAnthropic, "", false},
		{"anthropic", "/v1/messages/count_tokens", "", "", false},
		{"openai", "/backend-api/codex/responses", apiResponses, "", false},
		{"azure", "/openai/deployments/my-gpt4o/chat/completions", apiChat, "my-gpt4o", false},
		{"gemini", "/v1beta/models/gemini-2.5-pro:streamGenerateContent", apiGemini, "gemini-2.5-pro", true},
		{"gemini", "/v1internal:streamGenerateContent", apiGemini, "", true},
		{"gemini", "/v1internal:countTokens", "", "", false},
		{"vertex", "/v1/projects/p/locations/us-east5/publishers/anthropic/models/claude-sonnet-4-5@20250929:streamRawPredict", apiAnthropic, "claude-sonnet-4-5@20250929", true},
		{"bedrock", "/model/us.anthropic.claude-sonnet-4-5-20250929-v1:0/invoke-with-response-stream", apiBedrockInvoke, "us.anthropic.claude-sonnet-4-5-20250929-v1:0", true},
		{"bedrock", "/model/arn:aws:bedrock:us-east-1:1:inference-profile/x/converse", apiBedrockConverse, "arn:aws:bedrock:us-east-1:1:inference-profile/x", false},
		{"openai", "/v1/models", "", "", false},
	}
	for _, c := range cases {
		api, model, stream := detectAPI(c.provider, c.path)
		if api != c.api || model != c.model || stream != c.stream {
			t.Errorf("%s: got (%q,%q,%v) want (%q,%q,%v)", c.path, api, model, stream, c.api, c.model, c.stream)
		}
	}
}

func TestMatcher(t *testing.T) {
	m := newMatcher([]string{"http://localhost:4000", "litellm.corp.example"})
	cases := map[string]string{
		"api.anthropic.com:443":                       "anthropic",
		"chatgpt.com:443":                             "openai",
		"ab.chatgpt.com:443":                          "",
		"myres.openai.azure.com:443":                  "",
		"myres.services.ai.azure.com:443":             "",
		"myres.cognitiveservices.azure.com:443":       "",
		"generativelanguage.googleapis.com:443":       "",
		"cloudcode-pa.googleapis.com:443":             "",
		"aiplatform.googleapis.com:443":               "",
		"us-central1-aiplatform.googleapis.com:443":   "",
		"bedrock-runtime.us-west-2.amazonaws.com:443": "bedrock",
		"s3.amazonaws.com:443":                        "",
		"github.com:443":                              "",
		"localhost:4000":                              "litellm",
		"localhost:5000":                              "",
		"litellm.corp.example:443":                    "litellm",
	}
	for hp, want := range cases {
		if got := m.Match(hp); got != want {
			t.Errorf("%s: got %q want %q", hp, got, want)
		}
	}
}
