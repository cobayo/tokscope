package main

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

type testEnv struct {
	p      *Proxy
	client *http.Client
	pool   *x509.CertPool
	addr   string
}

// newTestProxy routes every upstream dial to the given test server, so that
// requests for api.anthropic.com etc. reach the mock.
func newTestProxy(t *testing.T, upstream *httptest.Server) *testEnv {
	t.Helper()
	t.Setenv("TOKSCOOP_HOME", t.TempDir())
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	p, err := setup(cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	upAddr := upstream.Listener.Addr().String()
	p.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", upAddr)
	}
	p.upstreamTLS.InsecureSkipVerify = true // the mock upstream uses a self-signed cert

	ps := httptest.NewServer(p)
	t.Cleanup(ps.Close)

	pool := x509.NewCertPool()
	caPEM, err := os.ReadFile(p.ca.CertPath)
	if err != nil {
		t.Fatal(err)
	}
	pool.AppendCertsFromPEM(caPEM)
	pool.AddCert(upstream.Certificate())

	pu, _ := url.Parse(ps.URL)
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(pu), TLSClientConfig: &tls.Config{RootCAs: pool}},
	}
	return &testEnv{p: p, client: client, pool: pool, addr: pu.Host}
}

func waitRecords(t *testing.T, s *Store, n int) []Record {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		recs, _ := s.Snapshot()
		if len(recs) >= n || time.Now().After(deadline) {
			return recs
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const claudeBody = `{"model":"claude-sonnet-4-5","stream":true,
 "system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI."}],
 "messages":[{"role":"user","content":[{"type":"text","text":"このバグを直して"}]}]}`

func TestProxyAnthropicMITM(t *testing.T) {
	var gotHost, gotAE, gotQuery, gotKey string
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotAE, gotQuery, gotKey = r.Host, r.Header.Get("Accept-Encoding"), r.URL.RawQuery, r.Header.Get("X-Api-Key")
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicSSE)
	}))
	defer up.Close()
	env := newTestProxy(t, up)

	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(claudeBody))
		req.Header.Set("User-Agent", "claude-cli/2.0.14 (external, cli)")
		req.Header.Set("X-Api-Key", "sk-ant-secret")
		req.Header.Set("Accept-Encoding", "br")
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), `"type":"message_stop"`) {
			t.Fatalf("client got %d %q", resp.StatusCode, b)
		}
	}
	if gotHost != "api.anthropic.com" || gotQuery != "beta=true" || gotKey != "sk-ant-secret" {
		t.Fatalf("upstream saw host=%q query=%q key=%q", gotHost, gotQuery, gotKey)
	}
	if gotAE == "br" {
		t.Fatalf("Accept-Encoding should have been renegotiated, got %q", gotAE)
	}

	recs := waitRecords(t, env.p.store, 2)
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d", len(recs))
	}
	r := recs[0]
	if r.Client != "Claude Code" || r.Model != "claude-sonnet-4-5" || r.API != apiAnthropic || r.Status != 200 || !r.Stream {
		t.Fatalf("record: %+v", r)
	}
	if r.InputTokens != 12 || r.CacheReadTokens != 45000 || r.CacheWriteTokens != 300 || r.OutputTokens != 321 || r.TotalInputTokens != 45312 {
		t.Fatalf("usage: %+v", r)
	}
	if r.Prompt != "このバグを直して" || r.PromptKind != "user" {
		t.Fatalf("prompt: %q %q", r.Prompt, r.PromptKind)
	}
	sp, err := env.p.store.SystemPrompt(r.SystemPromptID)
	if err != nil || string(sp) != "You are Claude Code, Anthropic's official CLI." {
		t.Fatalf("system prompt: %q %v", sp, err)
	}
	if recs[1].SystemPromptID != r.SystemPromptID {
		t.Fatalf("identical system prompts should share an id")
	}
	files, _ := os.ReadDir(systemPromptDir(homeDir()))
	if len(files) != 1 {
		t.Fatalf("want 1 stored system prompt, got %d", len(files))
	}
	log, _ := os.ReadFile(logPath(homeDir()))
	if strings.Contains(string(log), "sk-ant-secret") || strings.Contains(string(log), "beta=true") {
		t.Fatalf("secrets or query strings leaked into the log")
	}
	if strings.Count(string(log), "\n") != 2 {
		t.Fatalf("want 2 log lines: %s", log)
	}
}

func TestProxyTunnelsOtherHosts(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello from example.com")
	}))
	defer up.Close()
	env := newTestProxy(t, up)

	// example.com is not an AI API: the client must talk TLS to the real
	// server (here: the mock, whose cert covers example.com), not to tokscoop.
	resp, err := env.client.Post("https://example.com/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hello from example.com" {
		t.Fatalf("got %q", b)
	}
	if resp.TLS == nil || resp.TLS.PeerCertificates[0].Issuer.CommonName == env.p.ca.cert.Subject.CommonName {
		t.Fatalf("non-target host was intercepted")
	}
	if recs, _ := env.p.store.Snapshot(); len(recs) != 0 {
		t.Fatalf("non-target host was recorded: %+v", recs)
	}
}

func TestProxyBedrockKeepsSignedRequestIntact(t *testing.T) {
	var gotURI, gotAE, gotAuth string
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI, gotAE, gotAuth = r.RequestURI, r.Header.Get("Accept-Encoding"), r.Header.Get("Authorization")
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(bedrockInvokeStream())
	}))
	defer up.Close()
	env := newTestProxy(t, up)

	u := "https://bedrock-runtime.us-east-1.amazonaws.com/model/us.anthropic.claude-sonnet-4-5-20250929-v1%3A0/invoke-with-response-stream"
	req, _ := http.NewRequest("POST", u, strings.NewReader(`{"anthropic_version":"bedrock-2023-05-31","messages":[{"role":"user","content":"hi"}],"system":"S"}`))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIA/20260923/us-east-1/bedrock/aws4_request, SignedHeaders=accept-encoding;host;x-amz-date, Signature=abc")
	req.Header.Set("X-Amz-Date", "20260923T000000Z")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(gotURI, "v1%3A0") {
		t.Fatalf("path encoding changed (breaks SigV4): %q", gotURI)
	}
	if gotAE != "identity" || !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256") {
		t.Fatalf("signed headers changed: ae=%q auth=%q", gotAE, gotAuth)
	}
	recs := waitRecords(t, env.p.store, 1)
	if len(recs) != 1 {
		t.Fatalf("no record")
	}
	r := recs[0]
	if r.Provider != "bedrock" || r.Model != "claude-sonnet-4-5-20250929" || r.CacheReadTokens != 7000 || r.OutputTokens != 55 || r.Prompt != "hi" {
		t.Fatalf("record: %+v", r)
	}
}

// ---- WebSocket (Responses API over WebSocket, as used by Codex) ----

func wsWrite(w io.Writer, payload []byte, masked bool) error {
	var mbit byte
	if masked {
		mbit = 0x80
	}
	hdr := []byte{0x81}
	switch n := len(payload); {
	case n < 126:
		hdr = append(hdr, mbit|byte(n))
	case n < 65536:
		hdr = append(hdr, mbit|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, mbit|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	body := append([]byte(nil), payload...)
	if masked {
		key := []byte{0x37, 0xfa, 0x21, 0x3d}
		hdr = append(hdr, key...)
		for i := range body {
			body[i] ^= key[i%4]
		}
	}
	_, err := w.Write(append(hdr, body...))
	return err
}

func wsRead(r *bufio.Reader) ([]byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	var key []byte
	if h[1]&0x80 != 0 {
		key = make([]byte, 4)
		if _, err := io.ReadFull(r, key); err != nil {
			return nil, err
		}
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return nil, err
	}
	for i := range p {
		if key != nil {
			p[i] ^= key[i%4]
		}
	}
	return p, nil
}

func TestProxyResponsesWebSocket(t *testing.T) {
	extSeen := make(chan string, 1)
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		extSeen <- r.Header.Get("Sec-WebSocket-Extensions")
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
			base64.StdEncoding.EncodeToString(sum[:]))
		_ = brw.Flush()
		if _, err := wsRead(brw.Reader); err != nil { // response.create
			return
		}
		_ = wsWrite(conn, []byte(`{"type":"response.created","response":{"model":"gpt-5-codex"}}`), false)
		_ = wsWrite(conn, []byte(`{"type":"response.output_text.delta","delta":"`+strings.Repeat("x", 300)+`"}`), false)
		_ = wsWrite(conn, []byte(`{"type":"response.completed","response":{"model":"gpt-5-codex","usage":{"input_tokens":10000,"input_tokens_details":{"cached_tokens":8000},"output_tokens":900,"output_tokens_details":{"reasoning_tokens":600}}}}`), false)
		_, _ = wsRead(brw.Reader) // wait for the client to go away
	}))
	defer up.Close()
	env := newTestProxy(t, up)

	conn, err := net.Dial("tcp", env.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT chatgpt.com:443 HTTP/1.1\r\nHost: chatgpt.com:443\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	tc := tls.Client(&bufferedConn{Conn: conn, r: br}, &tls.Config{RootCAs: env.pool, ServerName: "chatgpt.com", NextProtos: []string{"http/1.1"}})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("TLS to tokscoop: %v", err)
	}
	fmt.Fprintf(tc, "GET /backend-api/codex/responses HTTP/1.1\r\nHost: chatgpt.com\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Extensions: permessage-deflate\r\n"+
		"User-Agent: codex_cli_rs/0.150.0\r\n\r\n")
	tbr := bufio.NewReader(tc)
	resp, err = http.ReadResponse(tbr, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	create := `{"type":"response.create","model":"gpt-5-codex","instructions":"You are Codex.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"テストを追加して"}]}]}`
	if err := wsWrite(tc, []byte(create), true); err != nil {
		t.Fatal(err)
	}
	var last []byte
	for i := 0; i < 3; i++ {
		if last, err = wsRead(tbr); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	if !strings.Contains(string(last), "response.completed") {
		t.Fatalf("client did not receive completion: %s", last)
	}
	if ext := <-extSeen; ext != "" {
		t.Fatalf("compression extension should be stripped, upstream saw %q", ext)
	}

	recs := waitRecords(t, env.p.store, 1)
	if len(recs) != 1 {
		t.Fatalf("no record")
	}
	r := recs[0]
	if r.Transport != "websocket" || r.Client != "Codex" || r.InputTokens != 2000 || r.CacheReadTokens != 8000 ||
		r.OutputTokens != 900 || r.ReasoningTokens != 600 || r.Prompt != "テストを追加して" || r.SystemPromptID == "" {
		t.Fatalf("record: %+v", r)
	}
}

func TestDashboardAPI(t *testing.T) {
	up := httptest.NewTLSServer(http.NotFoundHandler())
	defer up.Close()
	env := newTestProxy(t, up)
	id, _ := env.p.store.SaveSystemPrompt("SYS")
	env.p.store.Add(Record{ID: "a1", Time: time.Now(), Client: "Claude Code", UsageFound: true, TotalInputTokens: 10, OutputTokens: 2, SystemPromptID: id})

	get := func(host, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = host
		rr := httptest.NewRecorder()
		env.p.ServeHTTP(rr, req)
		return rr
	}
	if rr := get("127.0.0.1:8899", "/api/recent"); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"requests":1`) {
		t.Fatalf("recent: %d %s", rr.Code, rr.Body)
	}
	if rr := get("localhost:8899", "/api/system-prompts/"+id); rr.Body.String() != "SYS" {
		t.Fatalf("system prompt: %q", rr.Body)
	}
	if rr := get("localhost:8899", "/api/system-prompts/../../ca-key"); rr.Code == 200 {
		t.Fatalf("path traversal not rejected")
	}
	if rr := get("attacker.example:8899", "/api/recent"); rr.Code != http.StatusForbidden {
		t.Fatalf("foreign Host must be rejected, got %d", rr.Code)
	}
	if rr := get("127.0.0.1:8899", "/"); !strings.Contains(rr.Body.String(), "tokscoop") {
		t.Fatalf("index not served")
	}
}
