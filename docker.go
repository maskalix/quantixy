package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var errNoContainer = errors.New("container does not exist")

// Docker is a minimal Docker Engine API client: inspect, start, stop.
// Only container names validated by loadConfig ever reach it.
type Docker struct {
	base   string
	client *http.Client
}

func newDocker(host string) (*Docker, error) {
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("DOCKER_HOST: %w", err)
	}
	tr := &http.Transport{Proxy: nil, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second}
	d := &Docker{client: &http.Client{Transport: tr, Timeout: 30 * time.Second}}
	switch u.Scheme {
	case "unix":
		sock := u.Path
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}
		d.base = "http://docker"
	case "tcp", "http":
		d.base = "http://" + u.Host
	default:
		return nil, fmt.Errorf("DOCKER_HOST: unsupported scheme %q (unix:// or tcp://)", u.Scheme)
	}
	return d, nil
}

func (d *Docker) do(ctx context.Context, method, path string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, err
}

func apiErr(op string, code int, body []byte) error {
	var m struct{ Message string }
	_ = json.Unmarshal(body, &m)
	return fmt.Errorf("docker %s: HTTP %d: %s", op, code, strings.TrimSpace(m.Message))
}

// Running reports whether the container is running (or restarting).
func (d *Docker) Running(ctx context.Context, name string) (bool, error) {
	code, body, err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json")
	if err != nil {
		return false, err
	}
	switch code {
	case http.StatusOK:
	case http.StatusNotFound:
		return false, errNoContainer
	default:
		return false, apiErr("inspect", code, body)
	}
	var c struct {
		Name  string
		State struct{ Running, Restarting bool }
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return false, err
	}
	// Docker falls back to ID-prefix matching when no container has this name:
	// never act on some other container whose ID happens to start with it.
	if c.Name != "/"+name {
		return false, errNoContainer
	}
	return c.State.Running || c.State.Restarting, nil
}

func (d *Docker) Start(ctx context.Context, name string) error {
	code, body, err := d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/start")
	if err != nil {
		return err
	}
	switch code {
	case http.StatusNoContent, http.StatusNotModified:
		return nil
	case http.StatusNotFound:
		return errNoContainer
	}
	return apiErr("start", code, body)
}

func (d *Docker) Stop(ctx context.Context, name string) error {
	code, body, err := d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/stop?t=10")
	if err != nil {
		return err
	}
	switch code {
	case http.StatusNoContent, http.StatusNotModified:
		return nil
	case http.StatusNotFound:
		return errNoContainer
	}
	return apiErr("stop", code, body)
}
