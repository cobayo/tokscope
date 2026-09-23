package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Usage is token usage normalised across providers:
//
//	Input      input tokens NOT served from cache
//	CacheRead  input tokens served from the prompt cache
//	CacheWrite input tokens written to the prompt cache (Anthropic/Bedrock)
//	Output     all output tokens, including reasoning/thinking
//	Reasoning  the reasoning part of Output, when reported separately
//
// OpenAI and Gemini report cached tokens as a subset of the prompt; they are
// subtracted from Input so that Input+CacheRead+CacheWrite is always the
// total input.
type Usage struct {
	Model      string
	Input      int64
	CacheRead  int64
	CacheWrite int64
	Output     int64
	Reasoning  int64
	Found      bool
}

type usageSink interface {
	io.Writer
	Finish() Usage
}

const (
	modeJSON = iota
	modeSSE
	modeEventStream
)

const maxBuffered = 64 << 20

type usageParser struct {
	api    string
	mode   int
	u      Usage
	line   []byte
	data   []byte
	body   []byte
	es     []byte
	broken bool
}

func newUsageSink(api, contentType, contentEncoding string) usageSink {
	p := &usageParser{api: api, mode: modeFor(contentType)}
	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "", "identity":
		return p
	case "gzip", "x-gzip":
		return newGzipSink(p)
	default: // br, zstd...: cannot decode with the standard library
		return discardSink{}
	}
}

func modeFor(contentType string) int {
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "text/event-stream"):
		return modeSSE
	case strings.Contains(ct, "amazon.eventstream"):
		return modeEventStream
	}
	return modeJSON
}

type discardSink struct{}

func (discardSink) Write(b []byte) (int, error) { return len(b), nil }
func (discardSink) Finish() Usage               { return Usage{} }

func (p *usageParser) Write(b []byte) (int, error) {
	if p.broken {
		return len(b), nil
	}
	switch p.mode {
	case modeSSE:
		p.writeSSE(b)
	case modeEventStream:
		p.writeEventStream(b)
	default:
		if len(p.body)+len(b) > maxBuffered {
			p.broken, p.body = true, nil
		} else {
			p.body = append(p.body, b...)
		}
	}
	return len(b), nil
}

func (p *usageParser) Finish() Usage {
	switch p.mode {
	case modeSSE:
		if len(p.line) > 0 {
			p.sseLine(bytes.TrimSuffix(p.line, []byte("\r")))
			p.line = nil
		}
		p.dispatchSSE()
	case modeJSON:
		if p.broken || len(p.body) == 0 {
			break
		}
		var v any
		if err := json.Unmarshal(p.body, &v); err == nil {
			p.handleJSON(v)
		} else if t := bytes.TrimSpace(p.body); bytes.HasPrefix(t, []byte("data:")) || bytes.HasPrefix(t, []byte("event:")) {
			// SSE served with a JSON content type.
			body := p.body
			p.body, p.mode = nil, modeSSE
			p.writeSSE(body)
			return p.Finish()
		}
	}
	return p.u
}

// ---- Server-Sent Events ----

func (p *usageParser) writeSSE(b []byte) {
	p.line = append(p.line, b...)
	start := 0
	for {
		i := bytes.IndexByte(p.line[start:], '\n')
		if i < 0 {
			break
		}
		p.sseLine(bytes.TrimSuffix(p.line[start:start+i], []byte("\r")))
		start += i + 1
	}
	if start > 0 {
		p.line = append(p.line[:0], p.line[start:]...)
	}
	if len(p.line) > maxBuffered {
		p.broken, p.line = true, nil
	}
}

func (p *usageParser) sseLine(line []byte) {
	if len(line) == 0 {
		p.dispatchSSE()
		return
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return // event:, id:, retry:, comments
	}
	d := bytes.TrimPrefix(line[5:], []byte(" "))
	if len(p.data) > 0 {
		p.data = append(p.data, '\n')
	}
	p.data = append(p.data, d...)
	if len(p.data) > maxBuffered {
		p.broken, p.data = true, nil
	}
}

func (p *usageParser) dispatchSSE() {
	if len(p.data) == 0 {
		return
	}
	d := p.data
	p.data = p.data[:0]
	if bytes.Equal(bytes.TrimSpace(d), []byte("[DONE]")) {
		return
	}
	var v any
	if json.Unmarshal(d, &v) == nil {
		p.handleJSON(v)
	}
}

// ---- AWS event-stream (Bedrock streaming) ----
//
// frame = total_len(4) headers_len(4) prelude_crc(4) headers payload msg_crc(4)

func (p *usageParser) writeEventStream(b []byte) {
	p.es = append(p.es, b...)
	off := 0
	for len(p.es)-off >= 16 {
		frame := p.es[off:]
		total := int(binary.BigEndian.Uint32(frame[0:4]))
		hlen := int(binary.BigEndian.Uint32(frame[4:8]))
		if total < 16 || total > maxBuffered || hlen < 0 || 12+hlen > total-4 {
			p.broken, p.es = true, nil
			return
		}
		if len(frame) < total {
			break
		}
		h := parseEventStreamHeaders(frame[12 : 12+hlen])
		if h[":message-type"] == "event" {
			p.eventStreamPayload(frame[12+hlen : total-4])
		}
		off += total
	}
	if off > 0 {
		p.es = append(p.es[:0], p.es[off:]...)
	}
}

func parseEventStreamHeaders(b []byte) map[string]string {
	h := map[string]string{}
	for len(b) > 0 {
		nl := int(b[0])
		if len(b) < 2+nl {
			break
		}
		name := string(b[1 : 1+nl])
		typ := b[1+nl]
		b = b[2+nl:]
		size := 0
		switch typ {
		case 0, 1: // bool true / false
		case 2:
			size = 1
		case 3:
			size = 2
		case 4:
			size = 4
		case 5, 8:
			size = 8
		case 9:
			size = 16
		case 6, 7: // bytes / string
			if len(b) < 2 {
				return h
			}
			l := int(binary.BigEndian.Uint16(b))
			if len(b) < 2+l {
				return h
			}
			if typ == 7 {
				h[name] = string(b[2 : 2+l])
			}
			b = b[2+l:]
			continue
		default:
			return h
		}
		if len(b) < size {
			return h
		}
		b = b[size:]
	}
	return h
}

func (p *usageParser) eventStreamPayload(payload []byte) {
	var o map[string]any
	if json.Unmarshal(payload, &o) != nil {
		return
	}
	// InvokeModelWithResponseStream wraps each Anthropic event as base64.
	if b64 := asString(o["bytes"]); b64 != "" {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return
		}
		var inner any
		if json.Unmarshal(raw, &inner) == nil {
			p.handleJSON(inner)
		}
		return
	}
	p.handleObj(o) // ConverseStream "metadata" event carries usage directly
}

// ---- provider-specific event handling ----

func (p *usageParser) handleJSON(v any) {
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			p.handleJSON(e)
		}
	case map[string]any:
		p.handleObj(x)
	}
}

func (p *usageParser) handleObj(o map[string]any) {
	switch p.api {
	case apiAnthropic, apiBedrockInvoke:
		p.anthropic(o)
	case apiResponses:
		p.responses(o)
	case apiChat:
		p.chat(o)
	case apiGemini:
		p.gemini(o)
	case apiBedrockConverse:
		p.converse(o)
	}
}

func setPos(dst *int64, v any) {
	if n, ok := asInt(v); ok && n > 0 {
		*dst = n
	}
}

func (p *usageParser) anthropic(o map[string]any) {
	switch asString(o["type"]) {
	case "message_start":
		p.anthropicMessage(asMap(o["message"]))
	case "message_delta":
		p.anthropicUsage(asMap(o["usage"]))
	case "message":
		p.anthropicMessage(o)
	}
	if m := asMap(o["amazon-bedrock-invocationMetrics"]); m != nil && !p.u.Found {
		setPos(&p.u.Input, m["inputTokenCount"])
		setPos(&p.u.Output, m["outputTokenCount"])
		setPos(&p.u.CacheRead, m["cacheReadInputTokenCount"])
		setPos(&p.u.CacheWrite, m["cacheWriteInputTokenCount"])
		p.u.Found = true
	}
}

func (p *usageParser) anthropicMessage(m map[string]any) {
	if s := asString(m["model"]); s != "" {
		p.u.Model = s
	}
	p.anthropicUsage(asMap(m["usage"]))
}

func (p *usageParser) anthropicUsage(u map[string]any) {
	if u == nil {
		return
	}
	_, hasIn := u["input_tokens"]
	_, hasOut := u["output_tokens"]
	if !hasIn && !hasOut {
		return
	}
	setPos(&p.u.Input, u["input_tokens"])
	setPos(&p.u.CacheWrite, u["cache_creation_input_tokens"])
	setPos(&p.u.CacheRead, u["cache_read_input_tokens"])
	setPos(&p.u.Output, u["output_tokens"]) // message_delta carries the cumulative count
	p.u.Found = true
}

func (p *usageParser) responses(o map[string]any) {
	var r map[string]any
	switch {
	case strings.HasPrefix(asString(o["type"]), "response."):
		r = asMap(o["response"])
	case asString(o["object"]) == "response":
		r = o
	}
	if r == nil {
		return
	}
	if s := asString(r["model"]); s != "" {
		p.u.Model = s
	}
	u := asMap(r["usage"])
	in, ok := asInt(u["input_tokens"])
	if !ok {
		return
	}
	cached, _ := asInt(asMap(u["input_tokens_details"])["cached_tokens"])
	out, _ := asInt(u["output_tokens"])
	reasoning, _ := asInt(asMap(u["output_tokens_details"])["reasoning_tokens"])
	p.u.Input, p.u.CacheRead = max(in-cached, 0), cached
	p.u.Output, p.u.Reasoning = out, reasoning
	p.u.Found = true
}

func (p *usageParser) chat(o map[string]any) {
	if s := asString(o["model"]); s != "" {
		p.u.Model = s
	}
	u := asMap(o["usage"])
	in, ok := asInt(u["prompt_tokens"])
	if !ok {
		return
	}
	cached, _ := asInt(asMap(u["prompt_tokens_details"])["cached_tokens"])
	if cached == 0 {
		cached, _ = asInt(u["cache_read_input_tokens"]) // LiteLLM with Anthropic models
	}
	written, _ := asInt(u["cache_creation_input_tokens"])
	out, _ := asInt(u["completion_tokens"])
	reasoning, _ := asInt(asMap(u["completion_tokens_details"])["reasoning_tokens"])
	p.u.Input, p.u.CacheRead, p.u.CacheWrite = max(in-cached-written, 0), cached, written
	p.u.Output, p.u.Reasoning = out, reasoning
	p.u.Found = true
}

func (p *usageParser) gemini(o map[string]any) {
	if r := asMap(o["response"]); r != nil { // Code Assist wrapper
		o = r
	}
	if s := asString(o["modelVersion"]); s != "" {
		p.u.Model = s
	}
	um := asMap(o["usageMetadata"])
	prompt, ok := asInt(um["promptTokenCount"])
	if !ok {
		return
	}
	cached, _ := asInt(um["cachedContentTokenCount"])
	toolPrompt, _ := asInt(um["toolUsePromptTokenCount"])
	cand, _ := asInt(um["candidatesTokenCount"])
	thoughts, _ := asInt(um["thoughtsTokenCount"])
	p.u.Input, p.u.CacheRead = max(prompt+toolPrompt-cached, 0), cached
	p.u.Output, p.u.Reasoning = cand+thoughts, thoughts
	p.u.Found = true
}

func (p *usageParser) converse(o map[string]any) {
	u := asMap(o["usage"])
	in, ok := asInt(u["inputTokens"])
	if !ok {
		return
	}
	out, _ := asInt(u["outputTokens"])
	cr, _ := asInt(u["cacheReadInputTokens"])
	cw, _ := asInt(u["cacheWriteInputTokens"])
	p.u.Input, p.u.Output, p.u.CacheRead, p.u.CacheWrite = in, out, cr, cw
	p.u.Found = true
}

// bedrockHeaderUsage is a fallback for non-streaming InvokeModel, which also
// reports token counts in response headers.
func bedrockHeaderUsage(h http.Header, u Usage) Usage {
	in, e1 := strconv.ParseInt(h.Get("X-Amzn-Bedrock-Input-Token-Count"), 10, 64)
	out, e2 := strconv.ParseInt(h.Get("X-Amzn-Bedrock-Output-Token-Count"), 10, 64)
	if e1 != nil && e2 != nil {
		return u
	}
	u.Input, u.Output, u.Found = in, out, true
	u.CacheRead, _ = strconv.ParseInt(h.Get("X-Amzn-Bedrock-Cache-Read-Input-Token-Count"), 10, 64)
	u.CacheWrite, _ = strconv.ParseInt(h.Get("X-Amzn-Bedrock-Cache-Write-Input-Token-Count"), 10, 64)
	return u
}

// ---- gzip ----

// gzipSink decompresses a copy of the stream for parsing while the original
// compressed bytes go to the client untouched.
type gzipSink struct {
	pw    *io.PipeWriter
	done  chan struct{}
	inner *usageParser
}

func newGzipSink(inner *usageParser) *gzipSink {
	pr, pw := io.Pipe()
	g := &gzipSink{pw: pw, done: make(chan struct{}), inner: inner}
	go func() {
		defer close(g.done)
		zr, err := gzip.NewReader(pr)
		if err == nil {
			_, err = io.Copy(inner, zr)
		}
		if err != nil {
			inner.broken = true
		}
		_, _ = io.Copy(io.Discard, pr) // never block the writer
	}()
	return g
}

func (g *gzipSink) Write(b []byte) (int, error) {
	_, _ = g.pw.Write(b)
	return len(b), nil
}

func (g *gzipSink) Finish() Usage {
	_ = g.pw.Close()
	<-g.done
	return g.inner.Finish()
}
