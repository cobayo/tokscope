package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"
)

// feed writes s in small uneven chunks to exercise buffering across reads.
func feed(t *testing.T, s usageSink, data string) Usage {
	t.Helper()
	b := []byte(data)
	for i, n := 0, 1; i < len(b); n = n%13 + 3 {
		j := min(i+n, len(b))
		_, _ = s.Write(b[i:j])
		i = j
	}
	return s.Finish()
}

func wantUsage(t *testing.T, got Usage, want Usage) {
	t.Helper()
	if got != want {
		t.Fatalf("usage mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

const anthropicSSE = "event: message_start\r\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4-5","usage":{"input_tokens":12,"cache_creation_input_tokens":300,"cache_read_input_tokens":45000,"output_tokens":1}}}` + "\r\n\r\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":321}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func TestAnthropicSSE(t *testing.T) {
	got := feed(t, newUsageSink(apiAnthropic, "text/event-stream; charset=utf-8", ""), anthropicSSE)
	wantUsage(t, got, Usage{Model: "claude-sonnet-4-5", Input: 12, CacheRead: 45000, CacheWrite: 300, Output: 321, Found: true})
}

func TestAnthropicJSON(t *testing.T) {
	body := `{"id":"msg_1","type":"message","model":"claude-haiku-4-5","usage":{"input_tokens":50,"output_tokens":7}}`
	got := feed(t, newUsageSink(apiAnthropic, "application/json", ""), body)
	wantUsage(t, got, Usage{Model: "claude-haiku-4-5", Input: 50, Output: 7, Found: true})
}

func TestResponsesSSE(t *testing.T) {
	data := `data: {"type":"response.created","response":{"model":"gpt-5-codex","usage":null}}` + "\n\n" +
		`data: {"type":"response.output_text.delta","delta":"x"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"model":"gpt-5-codex","usage":{"input_tokens":10000,"input_tokens_details":{"cached_tokens":8000},"output_tokens":900,"output_tokens_details":{"reasoning_tokens":600}}}}` + "\n\n"
	got := feed(t, newUsageSink(apiResponses, "text/event-stream", ""), data)
	wantUsage(t, got, Usage{Model: "gpt-5-codex", Input: 2000, CacheRead: 8000, Output: 900, Reasoning: 600, Found: true})
}

func TestChatSSEWithUsageChunk(t *testing.T) {
	data := `data: {"model":"gpt-4o-2024-08-06","choices":[{"delta":{"content":"a"}}],"usage":null}` + "\n\n" +
		`data: {"model":"gpt-4o-2024-08-06","choices":[],"usage":{"prompt_tokens":1200,"completion_tokens":80,"prompt_tokens_details":{"cached_tokens":1024}}}` + "\n\n" +
		"data: [DONE]\n\n"
	got := feed(t, newUsageSink(apiChat, "text/event-stream", ""), data)
	wantUsage(t, got, Usage{Model: "gpt-4o-2024-08-06", Input: 176, CacheRead: 1024, Output: 80, Found: true})
}

func TestGeminiCodeAssistSSE(t *testing.T) {
	data := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"a"}]}}],"usageMetadata":{"promptTokenCount":5000,"candidatesTokenCount":3},"modelVersion":"gemini-2.5-pro"}}` + "\r\n\r\n" +
		`data: {"response":{"candidates":[],"usageMetadata":{"promptTokenCount":5000,"cachedContentTokenCount":4000,"candidatesTokenCount":120,"thoughtsTokenCount":300},"modelVersion":"gemini-2.5-pro"}}` + "\r\n\r\n"
	got := feed(t, newUsageSink(apiGemini, "text/event-stream", ""), data)
	wantUsage(t, got, Usage{Model: "gemini-2.5-pro", Input: 1000, CacheRead: 4000, Output: 420, Reasoning: 300, Found: true})
}

func TestGeminiJSONArrayStream(t *testing.T) {
	data := `[{"candidates":[],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1}},` +
		`{"candidates":[],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":25},"modelVersion":"gemini-2.5-flash"}]`
	got := feed(t, newUsageSink(apiGemini, "application/json", ""), data)
	wantUsage(t, got, Usage{Model: "gemini-2.5-flash", Input: 10, Output: 25, Found: true})
}

// esFrame builds one AWS event-stream frame (CRCs are not checked).
func esFrame(headers map[string]string, payload []byte) []byte {
	var hb bytes.Buffer
	for k, v := range headers {
		hb.WriteByte(byte(len(k)))
		hb.WriteString(k)
		hb.WriteByte(7)
		_ = binary.Write(&hb, binary.BigEndian, uint16(len(v)))
		hb.WriteString(v)
	}
	total := 12 + hb.Len() + len(payload) + 4
	var f bytes.Buffer
	_ = binary.Write(&f, binary.BigEndian, uint32(total))
	_ = binary.Write(&f, binary.BigEndian, uint32(hb.Len()))
	f.Write([]byte{0, 0, 0, 0})
	f.Write(hb.Bytes())
	f.Write(payload)
	f.Write([]byte{0, 0, 0, 0})
	return f.Bytes()
}

func bedrockChunk(event string) []byte {
	p, _ := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString([]byte(event))})
	return esFrame(map[string]string{":message-type": "event", ":event-type": "chunk", ":content-type": "application/json"}, p)
}

func bedrockInvokeStream() []byte {
	var b bytes.Buffer
	b.Write(bedrockChunk(`{"type":"message_start","message":{"model":"claude-sonnet-4-5-20250929","usage":{"input_tokens":20,"cache_read_input_tokens":7000,"cache_creation_input_tokens":0,"output_tokens":1}}}`))
	b.Write(bedrockChunk(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"ok"}}`))
	b.Write(bedrockChunk(`{"type":"message_delta","usage":{"output_tokens":55}}`))
	b.Write(bedrockChunk(`{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":20,"outputTokenCount":55}}`))
	return b.Bytes()
}

func TestBedrockInvokeEventStream(t *testing.T) {
	got := feed(t, newUsageSink(apiBedrockInvoke, "application/vnd.amazon.eventstream", ""), string(bedrockInvokeStream()))
	wantUsage(t, got, Usage{Model: "claude-sonnet-4-5-20250929", Input: 20, CacheRead: 7000, Output: 55, Found: true})
}

func TestBedrockConverseStream(t *testing.T) {
	var b bytes.Buffer
	b.Write(esFrame(map[string]string{":message-type": "event", ":event-type": "messageStart"}, []byte(`{"role":"assistant"}`)))
	b.Write(esFrame(map[string]string{":message-type": "event", ":event-type": "metadata"},
		[]byte(`{"usage":{"inputTokens":300,"outputTokens":40,"cacheReadInputTokens":1000,"cacheWriteInputTokens":20,"totalTokens":1360},"metrics":{"latencyMs":900}}`)))
	got := feed(t, newUsageSink(apiBedrockConverse, "application/vnd.amazon.eventstream", ""), b.String())
	wantUsage(t, got, Usage{Input: 300, CacheRead: 1000, CacheWrite: 20, Output: 40, Found: true})
}

func TestGzipSink(t *testing.T) {
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	_, _ = zw.Write([]byte(anthropicSSE))
	_ = zw.Close()
	got := feed(t, newUsageSink(apiAnthropic, "text/event-stream", "gzip"), z.String())
	if !got.Found || got.Output != 321 || got.CacheRead != 45000 {
		t.Fatalf("gzip usage: %+v", got)
	}
}

func TestSSEWithJSONContentType(t *testing.T) {
	got := feed(t, newUsageSink(apiAnthropic, "application/json", ""), anthropicSSE)
	if !got.Found || got.Output != 321 {
		t.Fatalf("fallback usage: %+v", got)
	}
}

func TestWSParserFragmentsAndMask(t *testing.T) {
	var got []string
	p := &wsParser{onText: func(b []byte) { got = append(got, string(b)) }}
	msg := []byte(`{"type":"response.create","input":"hello"}`)
	mask := []byte{1, 2, 3, 4}
	frame := func(fin bool, op byte, payload []byte) []byte {
		b0 := op
		if fin {
			b0 |= 0x80
		}
		out := []byte{b0, 0x80 | byte(len(payload))}
		out = append(out, mask...)
		for i, c := range payload {
			out = append(out, c^mask[i%4])
		}
		return out
	}
	var stream []byte
	stream = append(stream, frame(false, 0x1, msg[:10])...)
	stream = append(stream, frame(true, 0x9, []byte("ping"))...) // control frame in between
	stream = append(stream, frame(true, 0x0, msg[10:])...)
	for i := range stream { // byte by byte
		_, _ = p.Write(stream[i : i+1])
	}
	if len(got) != 1 || got[0] != string(msg) {
		t.Fatalf("got %q", got)
	}
}
