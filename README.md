<img src="github/logo.png" alt="Logo" width="150"/>

# Quantixy — Schrödinger's Proxy

A reverse proxy that starts a Docker container on the first request and stops it again when nobody has used it for a while. Until someone looks, your containers are both running and not running.

One static Go binary (~10 MB image, `FROM scratch`), no nginx, no shell, no Python, no docker CLI.

> The original nginx + bash + Python version lives on the [`archive/v1-nginx`](https://github.com/maskalix/quantixy/tree/archive/v1-nginx) branch.

## Quick start

Use the [`docker-compose.yml`](docker-compose.yml) from this repo. It runs Quantixy next to a locked-down [socket proxy](https://github.com/wollomatic/socket-proxy), so Quantixy itself never touches the Docker socket:

```bash
export DOCKER_GID=$(getent group docker | cut -d: -f3)  # lets the socket proxy read docker.sock
docker compose up -d
```

What to adjust in it:

- `ports: 8888:8080` and `PORT=8080`: the host port, and the port Quantixy listens on inside the container (they must match on the container side).
- the `apps` network: an existing network your app containers are on. Quantixy reaches them by container name.

`services.yaml`:

```yaml
example.com:
  container: my_app   # container to start/stop (required)
  port: 8000          # port inside the container (required)

ws.example.com:
  container: my_ws_app
  port: 3000
  websocket: true     # allow WebSocket upgrades (default false)
  protocol: https     # upstream protocol (default http)
  timeout: 30m        # per-service idle timeout (default TIMEOUT_MINUTES)
  insecure_skip_verify: true # accept a self-signed upstream cert
  host: my-ws-alias   # upstream hostname (default: the container name)
```

Then point your front proxy (nginx, Caddy, …) at `http://quantixy:<PORT>` for these domains, keeping the `Host` header.

## How it works

1. A request arrives; the `Host` header picks the service. Unknown hosts get `404`.
2. If the container isn't known to be up, Quantixy starts it through the Docker API and waits until its port accepts connections. Concurrent requests share one start.
3. Browsers (`GET` + `Accept: text/html`) wait up to 2 s, then get a loading page that reloads itself every 2 s. API and WebSocket clients wait until the container is ready (up to `START_TIMEOUT`), then get proxied.
4. Every 15 s, containers with no open connections and no request for longer than the timeout are stopped.
5. If a container is stopped behind Quantixy's back, the next request notices (connection refused) and wakes it again.

## Environment

| Variable | Default | |
| --- | --- | --- |
| `TIMEOUT_MINUTES` | `10` | Idle time before a container is stopped |
| `START_TIMEOUT` | `120s` | Max time to wait for a container's port to come up |
| `PORT` | `8080` | Port Quantixy listens on inside the container (ports below 1024 work too, Docker allows them for non-root) |
| `CONFIG` | `/etc/quantixy/services.yaml` | Services file |
| `TRUSTED_PROXIES` | _(none)_ | Comma-separated exact IPs (or CIDRs) of your front proxy, so apps get the real client IP. See Security. |
| `MAX_CONNECTIONS` | `1024` | Max open client connections (incl. WebSockets and requests waiting for a start) |
| `LOADING_PAGE_PATH` | _(built-in)_ | Custom loading page (Go `html/template`; `{{.Domain}}`, `{{.Failed}}`) |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | The compose file sets `tcp://docker-proxy:2375` |
| `VERBOSE_LOGGING` | `false` | Log every proxied request |

`GET /health` on any non-configured host returns `200 OK`; the image's `HEALTHCHECK` uses `quantixy -health`.

## Security

Access to the Docker socket is equivalent to root on the host, so Quantixy doesn't get it:

- **Socket proxy:** only the socket proxy mounts `docker.sock`. It sits on an `internal` network and accepts connections only from the `quantixy` container. It allows exactly `GET /containers/<name>/json` and `POST /containers/<name>/start|stop`; everything else gets 403, including list, create, exec, kill, delete, logs and images. Even a compromised Quantixy could only inspect, start and stop containers, and never create or exec into one.
  - To restrict it further to your own containers (inspect output includes a container's env), replace the name pattern in both `-allow…` lines with your names, e.g. `(/v1\.[0-9]+)?/containers/(app1|app2)/json`.
- **Docker calls:** Quantixy only calls those three endpoints, and only for container names listed in `services.yaml`.
  - Names are validated (`[a-zA-Z0-9][a-zA-Z0-9_.-]*`) and path-escaped. Request data never reaches the Docker API.
  - Inspect must return exactly that name. Docker would otherwise fall back to a container whose *ID* starts with the name.
- **Container:** non-root user (65532), `scratch` image (no shell, no package manager), `read_only`, `cap_drop: ALL`, `no-new-privileges`. The toolchain is pinned to a Go release without known standard-library vulnerabilities (govulncheck clean).
- **Config:** typos (unknown keys) are errors instead of being silently ignored.
- **Forwarding headers:**
  - From clients outside `TRUSTED_PROXIES`, all `X-Forwarded-*`, `X-Real-IP`, `True-Client-IP`, `X-Original-URL` and similar headers are dropped and set fresh.
  - From a trusted proxy, only the `X-Forwarded-For` chain and `X-Forwarded-Proto` are kept. `X-Real-IP` is the first untrusted address from the right of `X-Forwarded-For`, so a client can't forge it. `X-Forwarded-Host` is always the request's Host.
  - Put exact front-proxy IPs in `TRUSTED_PROXIES`, not whole Docker ranges.
- **Upgrades:** only `Upgrade: websocket`, and only on services with `websocket: true`. h2c and other upgrades are stripped, so they can't tunnel past the front proxy's rules. When either side of a WebSocket closes, both connections are closed, so half-open sockets can't pile up.
- **Resource limits:**
  - At most `MAX_CONNECTIONS` client connections (1024 by default; more wait in the kernel backlog).
  - 32 KB max headers, 10 s to send them (slowloris), 120 s idle keep-alive, 3 s upstream dial.
  - Upstream connections ignore `HTTP(S)_PROXY`.
  - A failed start backs off for 10 s, so floods don't hammer the Docker daemon.
- **Loading page:** no JavaScript and no third-party requests. Strict CSP, `no-store`, `nosniff`, `frame-ancestors 'none'`. It shows only the configured domain, never anything from the request.

Why not `tecnativa/docker-socket-proxy`? It can't allow start/stop without also setting `POST=1`, and `POST=1` with `CONTAINERS=1` also lets through container create and exec. That is root on the host again.

**Expose Quantixy only to your front proxy.** Anyone who reaches its port directly skips the front proxy's auth and rate limits. Anyone who can reach a configured domain can wake its container — that's the point — so the front proxy is the place for auth and rate limits.

## Build

```bash
go test -race ./...
docker build -t quantixy .
```

---

Built with ❤️ by LNLN (LINE by LINE cooked by Martin Skalicky)
