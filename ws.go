package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// wsParser reassembles WebSocket text messages from a raw byte stream
// (either direction; masking is handled). Compressed frames are skipped.
type wsParser struct {
	buf    []byte
	msg    []byte
	msgOp  byte
	onText func([]byte)
	broken bool
}

func (w *wsParser) Write(b []byte) (int, error) {
	if w.broken {
		return len(b), nil
	}
	w.buf = append(w.buf, b...)
	off := 0
	for {
		f := w.buf[off:]
		if len(f) < 2 {
			break
		}
		fin, rsv1, op := f[0]&0x80 != 0, f[0]&0x40 != 0, f[0]&0x0f
		masked := f[1]&0x80 != 0
		plen := uint64(f[1] & 0x7f)
		hdr := 2
		switch plen {
		case 126:
			if len(f) < 4 {
				goto wait
			}
			plen, hdr = uint64(binary.BigEndian.Uint16(f[2:4])), 4
		case 127:
			if len(f) < 10 {
				goto wait
			}
			plen, hdr = binary.BigEndian.Uint64(f[2:10]), 10
		}
		if plen > maxBuffered {
			w.broken, w.buf, w.msg = true, nil, nil
			return len(b), nil
		}
		var mask []byte
		if masked {
			if len(f) < hdr+4 {
				break
			}
			mask = f[hdr : hdr+4]
			hdr += 4
		}
		if uint64(len(f)) < uint64(hdr)+plen {
			break
		}
		payload := make([]byte, plen)
		copy(payload, f[hdr:hdr+int(plen)])
		for i := range payload {
			if mask != nil {
				payload[i] ^= mask[i%4]
			}
		}
		off += hdr + int(plen)

		switch {
		case op == 0x1 || op == 0x2: // text / binary
			if rsv1 { // compressed; cannot read
				w.msg = nil
				continue
			}
			if fin {
				if op == 0x1 {
					w.onText(payload)
				}
			} else {
				w.msgOp, w.msg = op, payload
			}
		case op == 0x0: // continuation
			if w.msg == nil {
				continue
			}
			w.msg = append(w.msg, payload...)
			if len(w.msg) > maxBuffered {
				w.msg = nil
				continue
			}
			if fin {
				if w.msgOp == 0x1 {
					w.onText(w.msg)
				}
				w.msg = nil
			}
		}
		// control frames (ping/pong/close) are ignored
	}
wait:
	if off > 0 {
		w.buf = append(w.buf[:0], w.buf[off:]...)
	}
	return len(b), nil
}

// wsSession records usage for the OpenAI Responses API over WebSocket:
// the client sends "response.create" messages, the server answers with
// streamed events ending in "response.completed".
type wsSession struct {
	p    *Proxy
	base Record

	mu         sync.Mutex
	pending    *ReqInfo
	started    time.Time
	lastSystem string
	lastPrompt string
}

func newWSSession(p *Proxy, r *http.Request, provider, host string) *wsSession {
	return &wsSession{p: p, base: Record{
		Client:    detectClient(r.Header),
		Provider:  provider,
		API:       apiResponses,
		Transport: "websocket",
		Host:      host,
		Path:      r.URL.Path,
		Stream:    true,
	}}
}

func (s *wsSession) clientSink() io.Writer { return &wsParser{onText: s.onClientMessage} }
func (s *wsSession) serverSink() io.Writer { return &wsParser{onText: s.onServerMessage} }

func (s *wsSession) onClientMessage(b []byte) {
	var o map[string]any
	if json.Unmarshal(b, &o) != nil || asString(o["type"]) != "response.create" {
		return
	}
	req := o
	if inner := asMap(o["response"]); inner != nil {
		req = inner
	}
	ri := parseRequestMap(apiResponses, req)
	s.mu.Lock()
	defer s.mu.Unlock()
	// With previous_response_id only new items are sent, so carry the
	// session's system prompt and latest instruction forward.
	if ri.System == "" {
		ri.System = s.lastSystem
	} else {
		s.lastSystem = ri.System
	}
	if ri.Prompt == "" {
		ri.Prompt = s.lastPrompt
	} else {
		s.lastPrompt = ri.Prompt
	}
	s.pending = &ri
	s.started = time.Now()
}

var wsDoneMarkers = [][]byte{
	[]byte("response.completed"), []byte("response.incomplete"),
	[]byte("response.failed"), []byte("response.done"),
}

func (s *wsSession) onServerMessage(b []byte) {
	hit := false
	for _, m := range wsDoneMarkers {
		if bytes.Contains(b, m) {
			hit = true
			break
		}
	}
	if !hit {
		return
	}
	var o map[string]any
	if json.Unmarshal(b, &o) != nil {
		return
	}
	typ := asString(o["type"])
	switch typ {
	case "response.completed", "response.incomplete", "response.failed", "response.done":
	default:
		return
	}
	up := &usageParser{api: apiResponses}
	up.handleObj(o)

	s.mu.Lock()
	ri := ReqInfo{}
	if s.pending != nil {
		ri = *s.pending
	}
	started := s.started
	s.pending = nil
	s.mu.Unlock()
	if started.IsZero() {
		started = time.Now()
	}

	rec := s.base
	rec.ID = newID()
	rec.Time = started
	rec.Status = http.StatusOK
	s.p.applyRequest(&rec, ri, "", true)
	rec.DurationMs = time.Since(started).Milliseconds()
	applyUsage(&rec, up.u)
	if typ == "response.failed" {
		rec.Error = "response.failed"
		if msg := asString(asMap(asMap(o["response"])["error"])["message"]); msg != "" {
			rec.Error += ": " + msg
		}
	}
	s.p.store.Add(rec)
}
