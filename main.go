// Quantixy — Schrödinger's proxy: a reverse proxy that starts Docker
// containers on the first request and stops them again when idle.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	health := flag.Bool("health", false, "probe the local /health endpoint and exit (for HEALTHCHECK)")
	flag.Parse()

	port, err := strconv.Atoi(env("PORT", "8080"))
	if err != nil || port < 1 || port > 65535 {
		fmt.Fprintln(os.Stderr, "quantixy: PORT must be 1-65535")
		os.Exit(1)
	}
	listen := ":" + strconv.Itoa(port)
	if *health {
		os.Exit(probe(listen))
	}
	if err := run(listen); err != nil {
		fmt.Fprintln(os.Stderr, "quantixy:", err)
		os.Exit(1)
	}
}

func run(listen string) error {
	verbose, _ := strconv.ParseBool(env("VERBOSE_LOGGING", "false"))
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	minutes, err := strconv.Atoi(env("TIMEOUT_MINUTES", "10"))
	if err != nil || minutes < 1 {
		return fmt.Errorf("TIMEOUT_MINUTES must be a whole number ≥ 1")
	}
	maxConns, err := strconv.Atoi(env("MAX_CONNECTIONS", "1024"))
	if err != nil || maxConns < 1 {
		return fmt.Errorf("MAX_CONNECTIONS must be a whole number ≥ 1")
	}
	startTimeout, err := time.ParseDuration(env("START_TIMEOUT", "120s"))
	if err != nil || startTimeout < time.Second {
		return fmt.Errorf("START_TIMEOUT must be a duration ≥ 1s (e.g. 90s)")
	}
	trusted, err := parsePrefixes(os.Getenv("TRUSTED_PROXIES"))
	if err != nil {
		return fmt.Errorf("TRUSTED_PROXIES: %w", err)
	}
	cfgPath := env("CONFIG", "/etc/quantixy/services.yaml")
	services, err := loadConfig(cfgPath, time.Duration(minutes)*time.Minute)
	if err != nil {
		return err
	}
	docker, err := newDocker(os.Getenv("DOCKER_HOST"))
	if err != nil {
		return err
	}
	app, err := newApp(log, docker, services, trusted, startTimeout, os.Getenv("LOADING_PAGE_PATH"))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	for d, s := range services {
		log.Info("service", "domain", d, "container", s.Container, "upstream", s.Protocol+"://"+net.JoinHostPort(s.Host, strconv.Itoa(s.Port)), "websocket", s.Websocket, "timeout", s.Timeout)
	}
	go app.reaper(ctx, 15*time.Second)

	srv := &http.Server{
		Addr:              listen,
		Handler:           app,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()

	log.Info("quantixy listening", "addr", listen, "services", len(services), "idle_timeout", time.Duration(minutes)*time.Minute, "trusted_proxies", len(trusted))
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	if err := srv.Serve(limitListener(ln, maxConns)); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// limitListener caps open client connections (including waiting requests and
// WebSockets) so a flood can't exhaust memory; extra connections wait in the
// kernel backlog.
func limitListener(ln net.Listener, n int) net.Listener {
	return &limitedListener{Listener: ln, sem: make(chan struct{}, n)}
}

type limitedListener struct {
	net.Listener
	sem chan struct{}
}

func (l *limitedListener) Accept() (net.Conn, error) {
	l.sem <- struct{}{}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitedConn{Conn: c, release: sync.OnceFunc(func() { <-l.sem })}, nil
}

type limitedConn struct {
	net.Conn
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// parsePrefixes parses a comma/space separated list of CIDRs or single IPs.
func parsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
			continue
		}
		ip, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP or CIDR", f)
		}
		out = append(out, netip.PrefixFrom(ip, ip.BitLen()))
	}
	return out, nil
}

func probe(listen string) int {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return 1
	}
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+port+"/health", nil)
	req.Host = "_" // never matches a configured domain
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
