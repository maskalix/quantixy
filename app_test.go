package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "s.yaml")
		os.WriteFile(p, []byte(s), 0o600)
		return p
	}

	got, err := loadConfig(write("Example.COM.:\n  container: app\n  port: 80\n  timeout: 30m\nws.example.com:\n  container: ws\n  port: 3000\n  websocket: true\n"),
		10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if s := got["example.com"]; s == nil || s.Host != "app" || s.Protocol != "http" || s.Timeout != 30*time.Minute {
		t.Fatalf("example.com: %+v", s)
	}
	if s := got["ws.example.com"]; s == nil || !s.Websocket || s.Port != 3000 || s.Timeout != 10*time.Minute {
		t.Fatalf("ws.example.com: %+v", s)
	}

	bad := map[string]string{
		"unknown field":  "a.com:\n  container: app\n  port: 80\n  conatiner: x\n",
		"bad container":  "a.com:\n  container: \"app;rm -rf\"\n  port: 80\n",
		"path container": "a.com:\n  container: ../../images\n  port: 80\n",
		"bad port":       "a.com:\n  container: app\n  port: 70000\n",
		"bad protocol":   "a.com:\n  container: app\n  port: 80\n  protocol: gopher\n",
		"bad domain":     "\"a b.com\":\n  container: app\n  port: 80\n",
		"empty":          "",
	}
	for name, y := range bad {
		if _, err := loadConfig(write(y), time.Minute); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// fakeDocker emulates the three Docker API calls; "starting" the container
// brings up a real HTTP backend on addr.
type fakeDocker struct {
	mu      sync.Mutex
	addr    string
	backend *http.Server
	starts  atomic.Int32
	stops   atomic.Int32
	handler http.Handler
	delay   time.Duration
}

func (f *fakeDocker) running() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.backend != nil
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "GET" && r.URL.Path == "/containers/app/json":
		if f.running() {
			io.WriteString(w, `{"Name":"/app","State":{"Running":true}}`)
		} else {
			io.WriteString(w, `{"Name":"/app","State":{"Running":false}}`)
		}
	case r.Method == "POST" && r.URL.Path == "/containers/app/start":
		f.starts.Add(1)
		f.mu.Lock()
		if f.backend == nil {
			srv := &http.Server{Handler: f.handler}
			f.backend = srv
			go func() {
				time.Sleep(f.delay) // connections are refused until "booted"
				ln, err := net.Listen("tcp", f.addr)
				if err != nil {
					panic(err)
				}
				srv.Serve(ln)
			}()
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == "POST" && r.URL.Path == "/containers/app/stop":
		f.stops.Add(1)
		f.mu.Lock()
		if f.backend != nil {
			f.backend.Close()
			f.backend = nil
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"no such container"}`)
	}
}

func freeAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func setup(t *testing.T, websocket bool, trusted ...string) (*App, *fakeDocker, *httptest.Server) {
	t.Helper()
	backendAddr := freeAddr(t)
	fd := &fakeDocker{addr: backendAddr, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Host", r.Host)
		w.Header().Set("X-Got-XFF", r.Header.Get("X-Forwarded-For"))
		w.Header().Set("X-Got-Real-IP", r.Header.Get("X-Real-IP"))
		w.Header().Set("X-Got-Upgrade", r.Header.Get("Upgrade"))
		w.Header().Set("X-Got-XFH", r.Header.Get("X-Forwarded-Host"))
		w.Header().Set("X-Got-XFPort", r.Header.Get("X-Forwarded-Port"))
		w.Header().Set("X-Got-TCIP", r.Header.Get("True-Client-IP"))
		io.WriteString(w, "hello from app")
	})}
	dsrv := httptest.NewServer(fd)
	t.Cleanup(func() {
		dsrv.Close()
		if fd.backend != nil {
			fd.backend.Close()
		}
	})
	d, err := newDocker("tcp://" + dsrv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(backendAddr)
	p, _ := strconv.Atoi(port)
	services := map[string]*Service{
		"app.test": {Container: "app", Host: host, Port: p, Protocol: "http", Websocket: websocket, Timeout: time.Minute},
		"www.test": {Container: "app", Host: host, Port: p, Protocol: "http", Websocket: websocket, Timeout: time.Minute},
	}
	var tp []netip.Prefix
	for _, s := range trusted {
		tp = append(tp, netip.MustParsePrefix(s))
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	app, err := newApp(log, d, services, tp, 5*time.Second, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	return app, fd, srv
}

func get(t *testing.T, srv *httptest.Server, host, accept string, hdr ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/x", nil)
	req.Host = host
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func body(r *http.Response) string { b, _ := io.ReadAll(r.Body); return string(b) }

func TestOnlyWebsocketUpgrades(t *testing.T) {
	_, _, srv := setup(t, true)
	if r := get(t, srv, "app.test", "", "Connection", "Upgrade", "Upgrade", "h2c", "HTTP2-Settings", "AAMAAABkAAQAAP__"); r.Header.Get("X-Got-Upgrade") != "" {
		t.Errorf("h2c upgrade forwarded: %q", r.Header.Get("X-Got-Upgrade"))
	}
}

// A container stopped outside Quantixy: the next request must wait for the
// wake instead of failing.
func TestExternalStop(t *testing.T) {
	app, fd, srv := setup(t, false)
	if r := get(t, srv, "app.test", ""); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	req, _ := http.NewRequest("POST", "http://docker/containers/app/stop", nil)
	fd.ServeHTTP(httptest.NewRecorder(), req) // "docker stop" behind our back
	if !app.routes["app.test"].ready.Load() {
		t.Fatal("test setup: route should still look ready")
	}
	if r := get(t, srv, "app.test", ""); r.StatusCode != 200 || fd.starts.Load() != 2 {
		t.Fatalf("after external stop: %d, starts %d", r.StatusCode, fd.starts.Load())
	}
}

// An upstream that ignores the half-close after the client hangs up must not
// keep the WebSocket (and so the container) alive forever.
func TestWebsocketClosesWhenClientLeaves(t *testing.T) {
	app, fd, srv := setup(t, true)
	hold := make(chan struct{})
	defer close(hold)
	fd.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, _ := w.(http.Hijacker).Hijack()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		rw.Flush()
		io.Copy(io.Discard, conn) // sees EOF on the half-close but never closes its side
		<-hold
		conn.Close()
	})

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(conn, "GET /ws HTTP/1.1\r\nHost: app.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	buf := make([]byte, 12)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "HTTP/1.1 101" {
		t.Fatalf("handshake: %q %v", buf, err)
	}
	if app.ctrs[0].inflight.Load() != 1 {
		t.Fatal("open websocket not counted as in flight")
	}
	conn.(*net.TCPConn).CloseWrite() // like docker-proxy: half-close, keep reading
	defer conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for app.ctrs[0].inflight.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("websocket still in flight after client closed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWakeAndProxy(t *testing.T) {
	_, fd, srv := setup(t, false)

	// API client: waits for the container, then gets proxied.
	r := get(t, srv, "app.test:443", "application/json", "X-Forwarded-For", "6.6.6.6", "X-Real-IP", "6.6.6.6", "Upgrade", "websocket", "Connection", "Upgrade")
	if r.StatusCode != 200 || body(r) != "hello from app" {
		t.Fatalf("status %d", r.StatusCode)
	}
	if fd.starts.Load() != 1 {
		t.Fatalf("starts = %d", fd.starts.Load())
	}
	if r.Header.Get("X-Host") != "app.test:443" {
		t.Errorf("Host not preserved: %q", r.Header.Get("X-Host"))
	}
	if xff := r.Header.Get("X-Got-XFF"); xff != "127.0.0.1" {
		t.Errorf("spoofed XFF passed through: %q", xff)
	}
	if ip := r.Header.Get("X-Got-Real-IP"); ip != "127.0.0.1" {
		t.Errorf("spoofed X-Real-IP passed through: %q", ip)
	}
	if u := r.Header.Get("X-Got-Upgrade"); u != "" {
		t.Errorf("Upgrade forwarded with websocket disabled: %q", u)
	}

	// Second domain on the same container: no second start.
	if r := get(t, srv, "WWW.test", "*/*"); r.StatusCode != 200 {
		t.Fatalf("www status %d", r.StatusCode)
	}
	if fd.starts.Load() != 1 {
		t.Fatalf("starts = %d", fd.starts.Load())
	}
}

func TestUntrustedHeadersDropped(t *testing.T) {
	_, _, srv := setup(t, false)
	r := get(t, srv, "app.test", "", "X-Forwarded-Host", "evil.example", "X-Forwarded-Port", "8443", "True-Client-IP", "1.2.3.4")
	if v := r.Header.Get("X-Got-XFH"); v != "app.test" {
		t.Errorf("X-Forwarded-Host = %q", v)
	}
	if v := r.Header.Get("X-Got-XFPort") + r.Header.Get("X-Got-TCIP"); v != "" {
		t.Errorf("spoofable headers passed through: %q", v)
	}
}

func TestTrustedProxy(t *testing.T) {
	_, _, srv := setup(t, true, "127.0.0.0/8", "10.0.0.0/8")
	// The client forged "1.2.3.4"; the front proxy (trusted) appended the real client,
	// an internal hop (trusted) appended the front proxy.
	r := get(t, srv, "app.test", "", "X-Forwarded-For", "1.2.3.4, 203.0.113.7, 10.0.0.5", "X-Real-IP", "1.2.3.4", "X-Forwarded-Host", "evil.example")
	if xff := r.Header.Get("X-Got-XFF"); xff != "1.2.3.4, 203.0.113.7, 10.0.0.5, 127.0.0.1" {
		t.Errorf("XFF = %q", xff)
	}
	if ip := r.Header.Get("X-Got-Real-IP"); ip != "203.0.113.7" {
		t.Errorf("X-Real-IP = %q", ip)
	}
	if v := r.Header.Get("X-Got-XFH"); v != "app.test" {
		t.Errorf("X-Forwarded-Host = %q", v)
	}
}

// Docker resolves an unknown name as an ID prefix; that container must not be touched.
func TestNameMustMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"Name":"/someone-else","State":{"Running":true}}`)
	}))
	defer srv.Close()
	d, _ := newDocker("tcp://" + srv.Listener.Addr().String())
	if _, err := d.Running(context.Background(), "cafe"); err != errNoContainer {
		t.Fatalf("err = %v", err)
	}
}

func TestLimitListener(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	l := limitListener(ln, 1)
	defer l.Close()
	accepted := make(chan net.Conn, 2)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	c1, _ := net.Dial("tcp", ln.Addr().String())
	defer c1.Close()
	c2, _ := net.Dial("tcp", ln.Addr().String())
	defer c2.Close()
	first := <-accepted
	select {
	case <-accepted:
		t.Fatal("accepted a second connection over the limit")
	case <-time.After(100 * time.Millisecond):
	}
	first.Close()
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(time.Second):
		t.Fatal("slot not released on Close")
	}
}

func TestLoadingPage(t *testing.T) {
	_, fd, srv := setup(t, false)
	fd.delay = 3 * time.Second

	r := get(t, srv, "app.test", "text/html,application/xhtml+xml")
	b := body(r)
	if r.StatusCode != 503 || !strings.Contains(b, "Starting services for <b>app.test</b>") {
		t.Fatalf("status %d body %q", r.StatusCode, b)
	}
	if !strings.Contains(r.Header.Get("Content-Security-Policy"), "default-src 'none'") || r.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("missing security headers: %v", r.Header)
	}
	// Once up, the browser's reload is proxied.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r := get(t, srv, "app.test", "text/html"); r.StatusCode == 200 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("never became ready")
}

func TestUnknownHostAndHealth(t *testing.T) {
	_, fd, srv := setup(t, false)
	if r := get(t, srv, "evil.test", "text/html"); r.StatusCode != 404 {
		t.Errorf("unknown host: %d", r.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/health", nil)
	req.Host = "_"
	r, err := srv.Client().Do(req)
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("health: %v %v", err, r)
	}
	r.Body.Close()
	if fd.starts.Load() != 0 {
		t.Error("unknown host started a container")
	}
}

func TestIdleStop(t *testing.T) {
	app, fd, srv := setup(t, false)
	if r := get(t, srv, "app.test", ""); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	c := app.ctrs[0]

	app.maybeStop(context.Background(), c) // not idle yet
	if fd.stops.Load() != 0 {
		t.Fatal("stopped a busy container")
	}
	c.timeout = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	c.inflight.Add(1) // e.g. an open websocket
	app.maybeStop(context.Background(), c)
	if fd.stops.Load() != 0 {
		t.Fatal("stopped a container with an open connection")
	}
	c.inflight.Add(-1)
	app.maybeStop(context.Background(), c)
	if fd.stops.Load() != 1 || fd.running() {
		t.Fatal("idle container not stopped")
	}
	// Next request wakes it again.
	if r := get(t, srv, "app.test", ""); r.StatusCode != 200 || fd.starts.Load() != 2 {
		t.Fatalf("re-wake: %d, starts %d", r.StatusCode, fd.starts.Load())
	}
}
