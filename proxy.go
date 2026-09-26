package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Proxy struct {
	cfg         *Config
	ca          *CA
	store       *Store
	matcher     *Matcher
	transport   *http.Transport
	upstreamTLS *tls.Config
	upstream    *url.URL
	dial        func(ctx context.Context, network, addr string) (net.Conn, error)
	ui          http.Handler
	log         *log.Logger
}

func newProxy(cfg *Config, ca *CA, store *Store, logger *log.Logger) (*Proxy, error) {
	p := &Proxy{
		cfg:         cfg,
		ca:          ca,
		store:       store,
		matcher:     newMatcher(cfg.ExtraHosts),
		log:         logger,
		upstreamTLS: &tls.Config{},
	}
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	p.dial = d.DialContext

	up := cfg.UpstreamProxy
	if up == "" {
		up = os.Getenv("TOKSCOPE_UPSTREAM_PROXY")
	}
	if up != "" {
		u, err := url.Parse(up)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("upstream_proxy の形式が正しくありません: %q（例: http://proxy.example.com:8080）", up)
		}
		p.upstream = u
	}

	p.transport = &http.Transport{
		Proxy: func(r *http.Request) (*url.URL, error) { return p.upstreamFor(r.URL.Hostname()), nil },
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return p.dial(ctx, network, addr)
		},
		TLSClientConfig:     p.upstreamTLS,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 20 * time.Second,
	}
	p.ui = newUI(p)
	return p, nil
}

func (p *Proxy) upstreamFor(host string) *url.URL {
	if p.upstream == nil || isLoopback(host) {
		return nil
	}
	return p.upstream
}

// ServeHTTP handles three kinds of requests on one port:
// CONNECT (HTTPS proxy), absolute-URI requests (plain HTTP proxy), and
// everything else, which is the dashboard.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		p.handleConnect(w, r)
	case r.URL.IsAbs() && !p.isSelf(r.URL.Host):
		p.handleHTTP(w, r)
	default:
		p.ui.ServeHTTP(w, r)
	}
}

func (p *Proxy) isSelf(hostport string) bool {
	_, port, err := net.SplitHostPort(p.cfg.Listen)
	if err != nil {
		return false
	}
	h, pt, err := net.SplitHostPort(hostport)
	return err == nil && pt == port && (isLoopback(h) || hostport == p.cfg.Listen)
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "tokscoop: hijacking not supported", http.StatusInternalServerError)
		return
	}
	provider := p.matcher.Match(target)
	if provider == "" {
		// Not an AI API: plain tunnel, never decrypted.
		up, err := p.dialTunnel(r.Context(), target)
		if err != nil {
			http.Error(w, "tokscoop: "+err.Error(), http.StatusBadGateway)
			return
		}
		conn, brw, err := hj.Hijack()
		if err != nil {
			up.Close()
			return
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		tunnel(&bufferedConn{Conn: conn, r: brw.Reader}, up)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
	p.serveMITM(&bufferedConn{Conn: conn, r: brw.Reader}, target, provider, r.UserAgent())
}

func (p *Proxy) serveMITM(conn net.Conn, target, provider, userAgent string) {
	host, _, _ := net.SplitHostPort(target)
	tlsConn := tls.Server(conn, &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := hello.ServerName
			if name == "" {
				name = host
			}
			return p.ca.certFor(name)
		},
	})
	_ = tlsConn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		if len(userAgent) > 256 {
			userAgent = userAgent[:256]
		}
		p.log.Printf("%s: クライアントとのTLSハンドシェイクに失敗しました (client=%s, user_agent=%q, ca=%s): %v", host, conn.RemoteAddr(), userAgent, p.ca.CertPath, err)
		p.log.Print("ツールを起動するターミナルで tokscope env の設定を適用し、ツールを再起動してください。Codex は CODEX_CA_CERTIFICATE、Claude Code は NODE_EXTRA_CA_CERTS を確認してください。")
		conn.Close()
		return
	}
	_ = tlsConn.SetDeadline(time.Time{})
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p.handleRequest(w, r, "https", target, provider)
		}),
		ReadHeaderTimeout: 60 * time.Second,
		IdleTimeout:       5 * time.Minute,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	_ = srv.Serve(newOneConnListener(tlsConn))
}

// handleHTTP serves plain-HTTP proxy requests (e.g. a LiteLLM on http://localhost:4000).
func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	port := r.URL.Port()
	if port == "" {
		port = "80"
	}
	provider := p.matcher.Match(net.JoinHostPort(r.URL.Hostname(), port))
	p.handleRequest(w, r, "http", r.URL.Host, provider)
}

func (p *Proxy) handleRequest(w http.ResponseWriter, r *http.Request, scheme, authority, provider string) {
	u := &url.URL{
		Scheme:   scheme,
		Host:     trimDefaultPort(scheme, authority),
		Path:     r.URL.Path,
		RawPath:  r.URL.RawPath, // keep the client's encoding: SigV4 signs it
		RawQuery: r.URL.RawQuery,
	}
	if isUpgrade(r) {
		p.handleUpgrade(w, r, u, provider)
		return
	}
	var api, pathModel string
	var streamHint bool
	if provider != "" && r.Method == http.MethodPost {
		api, pathModel, streamHint = detectAPI(provider, r.URL.Path)
	}
	if api == "" {
		p.passthrough(w, r, u)
		return
	}
	p.intercept(w, r, u, provider, api, pathModel, streamHint)
}

func (p *Proxy) outgoing(r *http.Request, u *url.URL) *http.Request {
	out := r.Clone(r.Context())
	out.URL = u
	out.RequestURI = ""
	out.Host = r.Host
	if out.Host == "" {
		out.Host = u.Host
	}
	out.TransferEncoding = nil
	removeHopHeaders(out.Header)
	out.Header.Del("Expect")
	return out
}

func (p *Proxy) passthrough(w http.ResponseWriter, r *http.Request, u *url.URL) {
	resp, err := p.transport.RoundTrip(p.outgoing(r, u))
	if err != nil {
		p.log.Printf("%s: %v", u.Host, err)
		http.Error(w, "tokscoop: upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_ = streamCopy(w, resp.Body, nil)
}

func (p *Proxy) intercept(w http.ResponseWriter, r *http.Request, u *url.URL, provider, api, pathModel string, streamHint bool) {
	start := time.Now()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "tokscoop: failed to read request body", http.StatusBadRequest)
		return
	}
	ri := parseRequest(api, body)

	out := p.outgoing(r, u)
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	out.ContentLength = int64(len(body))
	if !isSigV4(r) {
		// Let Go negotiate gzip and decompress transparently so the usage
		// can be read (the client then receives an uncompressed body).
		// Signed AWS requests are left byte-for-byte intact instead.
		out.Header.Del("Accept-Encoding")
	}

	rec := Record{
		ID:       newID(),
		Time:     start,
		Client:   detectClient(r.Header),
		Provider: provider,
		API:      api,
		Host:     u.Hostname(),
		Path:     r.URL.Path, // never the query string: it may hold an API key
	}
	p.applyRequest(&rec, ri, pathModel, streamHint)

	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		rec.Status = http.StatusBadGateway
		rec.Error = err.Error()
		rec.DurationMs = time.Since(start).Milliseconds()
		p.store.Add(rec)
		http.Error(w, "tokscoop: upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	sink := newUsageSink(api, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Encoding"))
	copyErr := streamCopy(w, resp.Body, sink)
	usage := sink.Finish()
	if !usage.Found {
		usage = bedrockHeaderUsage(resp.Header, usage)
	}
	rec.Status = resp.StatusCode
	rec.DurationMs = time.Since(start).Milliseconds()
	applyUsage(&rec, usage)
	if copyErr != nil {
		rec.Error = "途中で切断されました: " + copyErr.Error()
	}
	p.store.Add(rec)
}

func (p *Proxy) applyRequest(rec *Record, ri ReqInfo, pathModel string, streamHint bool) {
	rec.Model = firstNonEmpty(ri.Model, pathModel)
	rec.Stream = ri.Stream || streamHint
	rec.Messages = ri.Messages
	rec.PromptKind = ri.PromptKind
	if p.cfg.savePrompt() {
		rec.Prompt = truncateRunes(ri.Prompt, p.cfg.PromptMaxChars)
	}
	if p.cfg.saveSystemPrompt() && ri.System != "" {
		id, err := p.store.SaveSystemPrompt(ri.System)
		if err != nil {
			p.log.Printf("システムプロンプトを保存できませんでした: %v", err)
			return
		}
		rec.SystemPromptID = id
		rec.SystemPromptChars = utf8.RuneCountInString(ri.System)
	}
}

// handleUpgrade relays WebSocket (and other Upgrade) requests. For OpenAI
// Responses over WebSocket (used by recent Codex versions) the frames are
// also parsed to record usage.
func (p *Proxy) handleUpgrade(w http.ResponseWriter, r *http.Request, u *url.URL, provider string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "tokscoop: upgrade not supported", http.StatusInternalServerError)
		return
	}
	addr := u.Host
	if u.Port() == "" {
		port := "80"
		if u.Scheme == "https" {
			port = "443"
		}
		addr = net.JoinHostPort(u.Hostname(), port)
	}
	up, err := p.dialTunnel(r.Context(), addr)
	if err != nil {
		http.Error(w, "tokscoop: upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	if u.Scheme == "https" {
		cfg := p.upstreamTLS.Clone()
		cfg.ServerName = u.Hostname()
		cfg.NextProtos = []string{"http/1.1"}
		tc := tls.Client(up, cfg)
		if err := tc.HandshakeContext(r.Context()); err != nil {
			up.Close()
			http.Error(w, "tokscoop: upstream TLS error: "+err.Error(), http.StatusBadGateway)
			return
		}
		up = tc
	}

	var sess *wsSession
	if provider != "" && isWebSocket(r) && strings.HasSuffix(r.URL.Path, "/responses") {
		sess = newWSSession(p, r, provider, u.Hostname())
	}
	out := r.Clone(context.Background())
	out.URL = &url.URL{Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery}
	out.RequestURI = ""
	out.Host = r.Host
	out.Header.Del("Proxy-Connection")
	out.Header.Del("Proxy-Authorization")
	if sess != nil {
		out.Header.Del("Sec-WebSocket-Extensions") // no permessage-deflate, so frames stay readable
	}
	_ = up.SetDeadline(time.Now().Add(60 * time.Second))
	if err := out.Write(up); err != nil {
		up.Close()
		http.Error(w, "tokscoop: upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	ubr := bufio.NewReader(up)
	resp, err := http.ReadResponse(ubr, out)
	if err != nil {
		up.Close()
		http.Error(w, "tokscoop: upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	_ = up.SetDeadline(time.Time{})

	conn, cbr, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = resp.Write(conn)
		resp.Body.Close()
		conn.Close()
		up.Close()
		return
	}
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 %s\r\n", resp.Status)
	_ = resp.Header.Write(conn)
	_, _ = io.WriteString(conn, "\r\n")

	client := &bufferedConn{Conn: conn, r: cbr.Reader}
	upstream := &bufferedConn{Conn: up, r: ubr}
	if sess == nil {
		tunnel(client, upstream)
		return
	}
	tunnelTee(client, upstream, sess.clientSink(), sess.serverSink())
}

// dialTunnel opens a raw TCP connection to addr, through the upstream proxy
// when one is configured.
func (p *Proxy) dialTunnel(ctx context.Context, addr string) (net.Conn, error) {
	host, _, _ := net.SplitHostPort(addr)
	up := p.upstreamFor(host)
	if up == nil {
		return p.dial(ctx, "tcp", addr)
	}
	proxyAddr := up.Host
	if up.Port() == "" {
		port := "80"
		if up.Scheme == "https" {
			port = "443"
		}
		proxyAddr = net.JoinHostPort(up.Hostname(), port)
	}
	c, err := p.dial(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	if up.Scheme == "https" {
		tc := tls.Client(c, &tls.Config{ServerName: up.Hostname()})
		if err := tc.HandshakeContext(ctx); err != nil {
			c.Close()
			return nil, err
		}
		c = tc
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: http.Header{}}
	if up.User != nil {
		pw, _ := up.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(up.User.Username()+":"+pw)))
	}
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if err := req.Write(c); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		c.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		c.Close()
		return nil, fmt.Errorf("upstream proxy refused CONNECT %s: %s", addr, resp.Status)
	}
	_ = c.SetDeadline(time.Time{})
	return &bufferedConn{Conn: c, r: br}, nil
}

// ---- helpers ----

func tunnel(a, b net.Conn) { tunnelTee(a, b, nil, nil) }

// tunnelTee copies both directions; aToB / bToA (optional) also receive a
// copy of the bytes for inspection.
func tunnelTee(a, b net.Conn, aToB, bToA io.Writer) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn, sink io.Writer) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
				if sink != nil {
					_, _ = sink.Write(buf[:n])
				}
			}
			if err != nil {
				return
			}
		}
	}
	go cp(b, a, aToB)
	go cp(a, b, bToA)
	<-done
	a.Close()
	b.Close()
	<-done
}

// streamCopy writes src to the client, flushing after every read so that
// streamed tokens arrive without delay, and tees the bytes into sink.
func streamCopy(w http.ResponseWriter, src io.Reader, sink io.Writer) error {
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if sink != nil {
				_, _ = sink.Write(buf[:n])
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// oneConnListener lets http.Server serve a single, already-accepted conn.
type oneConnListener struct {
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
	addr   net.Addr
}

func newOneConnListener(c net.Conn) *oneConnListener {
	l := &oneConnListener{ch: make(chan net.Conn, 1), closed: make(chan struct{}), addr: c.LocalAddr()}
	l.ch <- &trackedConn{Conn: c, l: l}
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.addr }

type trackedConn struct {
	net.Conn
	l *oneConnListener
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.l.Close()
	return err
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func removeHopHeaders(h http.Header) {
	for _, f := range h.Values("Connection") {
		for _, sf := range strings.Split(f, ",") {
			if sf = textproto.TrimString(sf); sf != "" {
				h.Del(sf)
			}
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

func copyHeader(dst, src http.Header) {
	h := src.Clone()
	removeHopHeaders(h)
	for k, vv := range h {
		dst[k] = vv
	}
}

func isUpgrade(r *http.Request) bool {
	for _, v := range r.Header.Values("Connection") {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), "upgrade") {
				return r.Header.Get("Upgrade") != ""
			}
		}
	}
	return false
}

func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func isSigV4(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-") || r.Header.Get("X-Amz-Date") != ""
}

func trimDefaultPort(scheme, hostport string) string {
	h, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		if strings.Contains(h, ":") {
			return "[" + h + "]"
		}
		return h
	}
	return hostport
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func truncateRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}
