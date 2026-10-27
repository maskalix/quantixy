package main

import (
	"bytes"
	"context"
	"crypto/tls"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed loading.html
var builtinLoading string

const (
	htmlWait    = 2 * time.Second  // how long a browser waits before it gets the loading page
	failBackoff = 10 * time.Second // after a failed start, don't hit Docker again for this long
)

var errStarting = errors.New("still starting")

// monotonic clock, immune to wall-clock jumps
var epoch = time.Now()

func now() int64 { return int64(time.Since(epoch)) }

// container is the lifecycle state shared by every domain pointing at it.
type container struct {
	name     string
	timeout  time.Duration
	lastSeen atomic.Int64
	inflight atomic.Int64
	mu       sync.Mutex // serializes docker start/stop
	routes   []*route
}

func (c *container) touch()              { c.lastSeen.Store(now()) }
func (c *container) idle() time.Duration { return time.Duration(now() - c.lastSeen.Load()) }

// route is one domain → container:port.
type route struct {
	domain    string
	addr      string
	websocket bool
	ctr       *container
	proxy     *httputil.ReverseProxy
	ready     atomic.Bool

	wmu  sync.Mutex
	wake *wakeCall
}

type wakeCall struct {
	done chan struct{}
	err  error
}

type App struct {
	log          *slog.Logger
	docker       *Docker
	routes       map[string]*route
	ctrs         []*container
	trusted      []netip.Prefix
	startTimeout time.Duration
	page         *template.Template
	customPage   bool
}

func newApp(log *slog.Logger, d *Docker, services map[string]*Service, trusted []netip.Prefix, startTimeout time.Duration, pagePath string) (*App, error) {
	a := &App{log: log, docker: d, routes: map[string]*route{}, trusted: trusted, startTimeout: startTimeout}

	src := builtinLoading
	if pagePath != "" {
		b, err := os.ReadFile(pagePath)
		if err != nil {
			return nil, fmt.Errorf("LOADING_PAGE_PATH: %w", err)
		}
		src, a.customPage = string(b), true
	}
	t, err := template.New("loading").Parse(src)
	if err != nil {
		return nil, fmt.Errorf("loading page: %w", err)
	}
	a.page = t

	ctrs := map[string]*container{}
	domains := make([]string, 0, len(services))
	for d := range services {
		domains = append(domains, d)
	}
	slices.Sort(domains)
	for _, domain := range domains {
		s := services[domain]
		c := ctrs[s.Container]
		if c == nil {
			c = &container{name: s.Container}
			c.touch()
			ctrs[s.Container] = c
			a.ctrs = append(a.ctrs, c)
		}
		c.timeout = max(c.timeout, s.Timeout) // shared container: the most patient domain wins
		rt := &route{
			domain:    domain,
			addr:      net.JoinHostPort(s.Host, strconv.Itoa(s.Port)),
			websocket: s.Websocket,
			ctr:       c,
		}
		rt.proxy = a.newProxy(rt, s)
		c.routes = append(c.routes, rt)
		a.routes[domain] = rt
	}
	return a, nil
}

func (a *App) newProxy(rt *route, s *Service) *httputil.ReverseProxy {
	target := &url.URL{Scheme: s.Protocol, Host: rt.addr}
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy: nil, // never route upstream traffic through HTTP(S)_PROXY
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return fullCloseConn{c}, nil
		},
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: s.InsecureSkipVerify},
	}
	return &httputil.ReverseProxy{
		Transport: tr,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			a.setForwarded(pr)
		},
		ErrorLog: slog.NewLogLogger(a.log.Handler(), slog.LevelDebug),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return // client went away
			}
			var op *net.OpError
			if errors.As(err, &op) && op.Op == "dial" {
				// Stopped behind our back (or crashed): wake it again.
				a.log.Debug("upstream unreachable", "domain", rt.domain, "err", err)
				rt.ready.Store(false)
				// Nothing reached the upstream: let ServeHTTP wait for the wake
				// and retry a body-less request (r is the outgoing request here).
				if retry, ok := r.Context().Value(retryKey{}).(*bool); ok && r.Body == nil {
					*retry = true
					return
				}
				rt.trigger(a)
				a.unavailable(w, r, rt, errStarting)
				return
			}
			a.log.Warn("proxy error", "domain", rt.domain, "err", err)
			http.Error(w, "502 bad gateway", http.StatusBadGateway)
		},
	}
}

// fullCloseConn: when a WebSocket client hangs up, ReverseProxy only half-closes
// the upstream and waits for it to close too. Many apps never do, which would
// leak the connection and keep the container "busy" forever.
type fullCloseConn struct{ net.Conn }

func (c fullCloseConn) CloseWrite() error { return c.Conn.Close() }

// Client-IP and proxy headers that apps commonly trust; from an untrusted peer
// they are dropped (ReverseProxy itself only strips Forwarded and X-Forwarded-For/Host/Proto).
var spoofable = []string{"X-Real-IP", "True-Client-IP", "X-Client-IP", "X-Cluster-Client-IP", "CF-Connecting-IP", "Fastly-Client-IP", "X-Original-URL", "X-Rewrite-URL"}

// setForwarded sets X-Forwarded-For/Host/Proto and X-Real-IP. Of what the client
// sent, only the X-Forwarded-For chain and X-Forwarded-Proto are kept, and only
// when the direct peer is in TRUSTED_PROXIES.
func (a *App) setForwarded(pr *httputil.ProxyRequest) {
	peer, _ := netip.ParseAddrPort(pr.In.RemoteAddr)
	ip := peer.Addr().Unmap()
	if !a.isTrusted(ip) {
		for k := range pr.Out.Header {
			if strings.HasPrefix(k, "X-Forwarded-") {
				delete(pr.Out.Header, k)
			}
		}
		for _, k := range spoofable {
			pr.Out.Header.Del(k)
		}
		pr.SetXForwarded()
		pr.Out.Header.Set("X-Real-IP", ip.String())
		return
	}
	xff := pr.In.Header["X-Forwarded-For"]
	pr.Out.Header["X-Forwarded-For"] = xff
	pr.SetXForwarded() // X-Forwarded-Host = Host, which the front proxy must keep
	if v := pr.In.Header.Get("X-Forwarded-Proto"); v == "http" || v == "https" {
		pr.Out.Header.Set("X-Forwarded-Proto", v)
	}
	pr.Out.Header.Set("X-Real-IP", a.clientIP(ip, xff).String())
}

// clientIP walks X-Forwarded-For from the right, skipping trusted proxies: the
// first untrusted address is the client. Anything left of it is client-supplied.
func (a *App) clientIP(peer netip.Addr, xff []string) netip.Addr {
	hops := strings.Split(strings.Join(xff, ","), ",")
	ip := peer
	for i := len(hops) - 1; i >= 0 && a.isTrusted(ip); i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		ip = hop.Unmap()
	}
	return ip
}

func (a *App) isTrusted(ip netip.Addr) bool {
	for _, p := range a.trusted {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func normHost(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt := a.routes[normHost(r.Host)]
	if rt == nil {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte("OK\n"))
			return
		}
		http.Error(w, "404 unknown host", http.StatusNotFound)
		return
	}

	c := rt.ctr
	c.touch()
	c.inflight.Add(1)
	defer func() {
		c.touch() // long requests / websockets count as activity until they end
		c.inflight.Add(-1)
	}()

	if !rt.ready.Load() {
		if err := a.waitReady(r, rt); err != nil {
			a.unavailable(w, r, rt, err)
			return
		}
	}

	// Only WebSocket upgrades, and only where enabled: anything else (h2c, …)
	// could tunnel past the front proxy's rules.
	if !rt.websocket || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		r.Header.Del("Upgrade")
	}
	a.log.Debug("proxy", "domain", rt.domain, "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
	retry := new(bool)
	rt.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), retryKey{}, retry)))
	if *retry { // container was stopped behind our back; retry once it's up again
		if err := a.waitReady(r, rt); err != nil {
			a.unavailable(w, r, rt, err)
			return
		}
		rt.proxy.ServeHTTP(w, r)
	}
}

type retryKey struct{}

// waitReady wakes the route and waits for it: briefly for browsers (they get
// the loading page instead), up to START_TIMEOUT for everything else.
func (a *App) waitReady(r *http.Request, rt *route) error {
	wait := a.startTimeout
	if wantsHTML(r) {
		wait = htmlWait
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	return rt.ensure(ctx, a)
}

func wantsHTML(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		r.Header.Get("Upgrade") == "" &&
		strings.Contains(r.Header.Get("Accept"), "text/html")
}

// unavailable answers while the container is starting (or failed to start):
// browsers get the loading page, everything else a plain 503.
func (a *App) unavailable(w http.ResponseWriter, r *http.Request, rt *route, err error) {
	if r.Context().Err() != nil {
		return
	}
	failed := !errors.Is(err, errStarting)
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Retry-After", "2")
	if !wantsHTML(r) {
		msg := "503 service starting"
		if failed {
			msg = "503 service unavailable"
		}
		http.Error(w, msg, http.StatusServiceUnavailable)
		return
	}

	var body bytes.Buffer
	if err := a.page.Execute(&body, struct {
		Domain string
		Failed bool
	}{rt.domain, failed}); err != nil {
		a.log.Error("loading page", "err", err)
		http.Error(w, "503 service starting", http.StatusServiceUnavailable)
		return
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	if !a.customPage {
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write(body.Bytes())
}

// trigger starts waking the route unless a wake is already in progress
// (or failed less than failBackoff ago).
func (rt *route) trigger(a *App) *wakeCall {
	rt.wmu.Lock()
	defer rt.wmu.Unlock()
	if rt.wake == nil {
		c := &wakeCall{done: make(chan struct{})}
		rt.wake = c
		go a.wake(rt, c)
	}
	return rt.wake
}

func (rt *route) ensure(ctx context.Context, a *App) error {
	c := rt.trigger(a)
	select {
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return errStarting
	}
}

func (a *App) wake(rt *route, call *wakeCall) {
	ctx, cancel := context.WithTimeout(context.Background(), a.startTimeout)
	defer cancel()
	err := a.start(ctx, rt)
	if err == nil {
		rt.ready.Store(true)
	} else {
		a.log.Error("wake failed", "domain", rt.domain, "container", rt.ctr.name, "err", err)
	}
	call.err = err
	close(call.done)

	clear := func() {
		rt.wmu.Lock()
		if rt.wake == call {
			rt.wake = nil
		}
		rt.wmu.Unlock()
	}
	if err == nil {
		clear()
	} else {
		time.AfterFunc(failBackoff, clear)
	}
}

// start starts the container if needed and waits until the upstream port accepts connections.
func (a *App) start(ctx context.Context, rt *route) error {
	c := rt.ctr
	c.mu.Lock()
	defer c.mu.Unlock()

	running, err := a.docker.Running(ctx, c.name)
	if err != nil {
		return err
	}
	began := time.Now()
	if !running {
		a.log.Info("starting container", "container", c.name, "domain", rt.domain)
		if err := a.docker.Start(ctx, c.name); err != nil {
			return err
		}
	}
	c.touch()

	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if dialOK(ctx, rt.addr) {
			if !running {
				a.log.Info("container ready", "container", c.name, "domain", rt.domain, "took", time.Since(began).Round(time.Millisecond))
			}
			return nil
		}
		if up, err := a.docker.Running(ctx, c.name); err == nil && !up {
			return errors.New("container exited during startup")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s not reachable after %s", rt.addr, a.startTimeout)
		case <-tick.C:
		}
	}
}

func dialOK(ctx context.Context, addr string) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// reaper stops containers that have been idle longer than their timeout.
func (a *App) reaper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, c := range a.ctrs {
				a.maybeStop(ctx, c)
			}
		}
	}
}

func (a *App) maybeStop(ctx context.Context, c *container) {
	busy := func() bool { return c.inflight.Load() > 0 || c.idle() < c.timeout }
	if busy() || !c.mu.TryLock() { // locked = starting right now
		return
	}
	defer c.mu.Unlock()
	if busy() {
		return
	}
	running, err := a.docker.Running(ctx, c.name)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			a.log.Warn("inspect failed", "container", c.name, "err", err)
		}
		return
	}
	for _, rt := range c.routes {
		rt.ready.Store(false)
	}
	if !running {
		return
	}
	a.log.Info("stopping idle container", "container", c.name, "idle", c.idle().Round(time.Second))
	if err := a.docker.Stop(ctx, c.name); err != nil {
		a.log.Error("stop failed", "container", c.name, "err", err)
	}
}
