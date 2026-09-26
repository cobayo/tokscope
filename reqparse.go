package main

import (
	"encoding/json"
	"strings"
)

// ReqInfo is what tokscoop extracts from a request body.
type ReqInfo struct {
	Model  string
	Stream bool
	// System is the full system prompt (all system/developer/instructions
	// text joined together).
	System string
	// Prompt is the most recent human-written user text. Agent tools resend
	// the whole conversation each turn, so this is the instruction the
	// current turn belongs to, not the whole history.
	Prompt string
	// PromptKind is "user" when the newest message is user text and
	// "tool_result" when this turn is continuing after a tool call.
	PromptKind string
	Messages   int
}

// Text that agent tools inject into user messages and that is not something
// the person typed.
var injectedPrefixes = []string{
	"<system-reminder>",
	"<environment_context>",
	"<user_instructions>",
	"# AGENTS.md instructions",
}

func isInjected(s string) bool {
	t := strings.TrimSpace(s)
	for _, p := range injectedPrefixes {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

func parseRequest(api string, body []byte) ReqInfo {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return ReqInfo{}
	}
	return parseRequestMap(api, root)
}

func parseRequestMap(api string, root map[string]any) ReqInfo {
	var ri ReqInfo
	ri.Model = asString(root["model"])
	ri.Stream, _ = root["stream"].(bool)

	switch api {
	case apiAnthropic, apiBedrockInvoke:
		ri.System = joinTexts(root["system"])
		scanMessages(&ri, asSlice(root["messages"]), "content", nil)

	case apiBedrockConverse:
		ri.System = joinTexts(root["system"])
		scanMessages(&ri, asSlice(root["messages"]), "content", nil)

	case apiChat:
		msgs := asSlice(root["messages"])
		var sys []string
		for _, m := range msgs {
			mm := asMap(m)
			if r := asString(mm["role"]); r == "system" || r == "developer" {
				if t := joinTexts(mm["content"]); t != "" {
					sys = append(sys, t)
				}
			}
		}
		ri.System = strings.Join(sys, "\n\n")
		scanMessages(&ri, msgs, "content", func(m map[string]any) bool {
			r := asString(m["role"])
			return r == "tool" || r == "function"
		})

	case apiResponses:
		var sys []string
		if s := asString(root["instructions"]); s != "" {
			sys = append(sys, s)
		}
		if s, ok := root["input"].(string); ok {
			ri.System = strings.Join(sys, "\n\n")
			ri.Prompt, ri.PromptKind, ri.Messages = strings.TrimSpace(s), "user", 1
			return ri
		}
		items := asSlice(root["input"])
		for _, it := range items {
			m := asMap(it)
			if r := asString(m["role"]); r == "system" || r == "developer" {
				if t := joinTexts(m["content"]); t != "" {
					sys = append(sys, t)
				}
			}
		}
		ri.System = strings.Join(sys, "\n\n")
		scanMessages(&ri, items, "content", func(m map[string]any) bool {
			return strings.HasSuffix(asString(m["type"]), "_output")
		})

	case apiGemini:
		req := root
		if inner := asMap(root["request"]); inner != nil { // Code Assist wrapper
			req = inner
		}
		sys := asMap(req["systemInstruction"])
		if sys == nil {
			sys = asMap(req["system_instruction"])
		}
		ri.System = joinTexts(sys["parts"])
		scanMessages(&ri, asSlice(req["contents"]), "parts", nil)
	}
	return ri
}

// scanMessages sets Messages, PromptKind (from the newest message) and
// Prompt (the newest user message that contains human text).
func scanMessages(ri *ReqInfo, msgs []any, contentKey string, isToolMsg func(map[string]any) bool) {
	ri.Messages = len(msgs)
	for i := len(msgs) - 1; i >= 0; i-- {
		m := asMap(msgs[i])
		if m == nil {
			continue
		}
		newest := i == len(msgs)-1
		if isToolMsg != nil && isToolMsg(m) {
			if newest {
				ri.PromptKind = "tool_result"
			}
			continue
		}
		role := asString(m["role"])
		if role != "user" && !(role == "" && contentKey == "parts") {
			continue
		}
		text, hasTool := userContent(m[contentKey])
		if ri.PromptKind == "" {
			if hasTool && text == "" {
				ri.PromptKind = "tool_result"
			} else {
				ri.PromptKind = "user"
			}
		}
		if text != "" {
			ri.Prompt = text
			return
		}
	}
}

// userContent extracts human text from any provider's content shape.
func userContent(c any) (text string, hasTool bool) {
	if s, ok := c.(string); ok {
		if isInjected(s) {
			return "", false
		}
		return strings.TrimSpace(s), false
	}
	var parts []string
	for _, b := range asSlice(c) {
		m := asMap(b)
		if m == nil {
			continue
		}
		typ := asString(m["type"])
		switch {
		case typ == "tool_result", strings.HasSuffix(typ, "_output"),
			m["toolResult"] != nil, m["functionResponse"] != nil:
			hasTool = true
			continue
		case typ == "image", typ == "input_image", m["image"] != nil, m["inlineData"] != nil:
			parts = append(parts, "[画像]")
			continue
		}
		if t := asString(m["text"]); t != "" && !isInjected(t) {
			parts = append(parts, strings.TrimSpace(t))
		}
	}
	return strings.Join(parts, "\n"), hasTool
}

// joinTexts joins a string or a list of {text: ...} blocks.
func joinTexts(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var parts []string
	for _, b := range asSlice(v) {
		if s, ok := b.(string); ok {
			parts = append(parts, s)
			continue
		}
		if t := asString(asMap(b)["text"]); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func asSlice(v any) []any        { s, _ := v.([]any); return s }
func asString(v any) string      { s, _ := v.(string); return s }

func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}
