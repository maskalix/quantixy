<img src="github/logo.png" alt="Logo" width="150"/>

# Quantixy — Schrödinger’s Proxy
Quantixy is proxy which auto-sleeps and wakes Docker containers when the website is reached. Containers are both running and not running until someone checks.

## DOCKER HUB
https://hub.docker.com/r/maskalicz/quantixy

```
services:
  quantixy:
    image: maskalicz/quantixy:latest
    container_name: quantixy
    ports:
      - 8888:80
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./services.yaml:/etc/quantixy/services.yaml
    environment:
      - TIMEOUT_MINUTES=1 # Time (in minutes) after which containers will shutdown (after inactivity)
      - VERBOSE_LOGGING=false
      - QUANTIXY__exemple.com__container: test
      - QUANTIXY__exemple.com__port: 1234
      - QUANTIXY__exemple.com__protocol: http
      - QUANTIXY__exemple.com__websocket: true
      #- LOADING_PAGE_PATH= # If you want custom loading 
```

## Fast-guide for those, who know
-   Add service to services.yaml
-   In other reverseproxy (preferably NGINX) set servername as domain you want and the proxy_pass to http://quantixy:8888 (or the IP of Quantixy) and you're good to go

## 🚀 Features

-   **Auto-start and auto-shutdown**: Go to the website and it autostart the container and after certain time stops it
-   **Dynamic Service Routing**: Automatically routes requests to containerized services based on domain configuration
-   **Graceful Failover**: Serves a loading page when services are unavailable - starting (502, 503, 504 errors)
-   **WebSocket Support**: Built-in support for WebSocket connections

## 📋 Prerequisites

-   Docker and Docker Compose
-   Access to modify nginx configuration files

## 🛠️ Installation

-   Preffered Docker image on Docker Hub

1. Clone the repository:

```bash
git clone https://github.com/yourusername/quantixy.git
cd quantixy
```

2. Configure your services in `services.yaml`

3. Start the nginx proxy:

```bash
docker-compose up -d
```

## 📁 Project Structure

```
quantixy/
├── nginx/
│   └── conf.d/
│       └── default.conf      # Main nginx configuration
├── html/
│   └── loading.html          # Fallback loading page
├── logs/                     # Nginx logs directory
├── services.yaml             # Service configuration (planned)
└── README.md
```

## ⚙️ Configuration

### Nginx Configuration

The main configuration is located in `nginx/conf.d/default.conf` and includes:

-   **Default Server Block**: Handles unmatched requests and serves loading page
-   **Health Check Endpoint**: Available at `/health` for monitoring
-   **Dynamic Server Blocks**: Generated based on `services.yaml` configuration

### Service Configuration

Services are configured through `services.yaml` (implementation pending) or environment variables. Example structure:

#### service.yaml:
```yaml
services:
    - domain: example.com
      container: my_example_app
      port: 8000
      websocket: false

    - domain: ws.example.com
      container: my_ws_app
      port: 3000
      websocket: true
```

#### Environment Variables:
```yaml
QUANTIXY__domain.do__container: container_name
QUANTIXY__domain.do__port: 80 # container_port
QUANTIXY__domain.do__protocol: http # container_service: http or https
QUANTIXY__domain.do__websocket: true # container_use_websocket: true or false
```

## 🔗 Endpoints

| Endpoint  | Description                                               |
| --------- | --------------------------------------------------------- |
| `/`       | Default route - serves configured service or loading page |
| `/health` | Health check endpoint (returns 200 OK)                    |

## 🏗️ Architecture

Quantixy uses nginx as a reverse proxy to route incoming requests to appropriate Docker containers. When a service is unavailable, it gracefully falls back to a loading page instead of showing error messages to users.

### Request Flow

1. Client makes request to configured domain
2. Nginx attempts to proxy to corresponding container
3. If container is available: Request is forwarded
4. If container is unavailable: Loading page is served

## 🐳 Docker Integration

The system is designed to work with Docker containers. Each service should:

-   Expose its port internally to the Docker network
-   Be accessible by the configured container name
-   Handle graceful shutdowns for zero-downtime deployments

## 📊 Monitoring

-   Access logs: `/var/log/nginx/access.log`
-   Error logs: `/var/log/nginx/error.log`
-   Health check: `http://your-domain/health`

## 🔧 Development

### Adding a New Service

1. Add service configuration to `services.yaml`
2. Regenerate nginx configuration (automation pending)
3. Reload nginx configuration
4. Deploy your containerized service

### WebSocket Services

For WebSocket-enabled services, ensure your configuration includes:

```nginx
proxy_http_version 1.1;
proxy_set_header Upgrade $http_upgrade;
proxy_set_header Connection "upgrade";
```

## 🆘 Troubleshooting

### Common Issues

**Service returns 502 Bad Gateway**

-   Check if the target container is running
-   Verify container name matches configuration
-   Check container port is correctly exposed

**Loading page not displaying**

-   Ensure `/usr/share/nginx/html/loading.html` exists
-   Check nginx error logs for file permission issues

**Health check failing**

-   Verify nginx is running
-   Check if port 80 is accessible
-   Review nginx error logs

## 📞 Support

For support and questions:

-   Open an issue on GitHub
-   Check the troubleshooting section above
-   Review nginx error logs for detailed error information

---

Built with ❤️ by LNLN (LINE by LINE cooked by Martin Skalicky)
