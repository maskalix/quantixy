#!/bin/bash

# Default timeout in minutes
TIMEOUT_MINUTES=${TIMEOUT_MINUTES:-10}
SERVICES_CONFIG="/etc/quantixy/services.yaml"
NGINX_ACCESS_LOG="/var/log/nginx/access.log"
LOADING_PAGE_PATH=${LOADING_PAGE_PATH:-"/usr/share/nginx/html/loading.html"}
VERBOSE_LOGGING=${VERBOSE_LOGGING:-"false"}

# Export environment variables for the inactivity monitor
export TIMEOUT_MINUTES
export VERBOSE_LOGGING

# Function to start a container
start_container() {
    local domain=$1
    local container_name=$(yq e ".${domain}.container" $SERVICES_CONFIG)
    local container_port=$(yq e ".${domain}.port" $SERVICES_CONFIG)

    if [ -z "$container_name" ] || [ "$container_name" == "null" ]; then
        echo "❌ No container mapping found for domain $domain"
        return
    fi

    # Write current domain being started to a file for the loading page
    echo "$domain" >/usr/share/nginx/html/current_domain.txt

    # Check if container exists (running or stopped)
    if docker ps -a -q -f name="^${container_name}$" | grep -q .; then
        # Container exists, check if it's running
        if ! docker ps -q -f name="^${container_name}$" | grep -q .; then
            echo "⚡ STARTING: Container '$container_name'..."
            docker start $container_name
            if [ $? -eq 0 ]; then
                #echo "✅ SUCCESS: Container '$container_name' started successfully for domain '$domain'"
                # Give the container a moment to start
                sleep 5
            else
                echo "❌ FAILED: Could not start container '$container_name' for domain '$domain'"
            fi
            #else
            #echo "✅ ALREADY RUNNING: Container '$container_name' is already running for domain '$domain'"
        fi
    else
        echo "❌ ERROR: Container '$container_name' does not exist for domain '$domain'!"
        echo "📋 AVAILABLE CONTAINERS:"
        docker ps -a --format "table {{.Names}}\t{{.Status}}\t{{.Image}}"
    fi
}

# Function to stop a container
stop_container() {
    local container_name=$1
    echo "💤 STOPPING CONTAINER: '$container_name' due to inactivity (timeout: ${TIMEOUT_MINUTES} minutes)"
    docker stop $container_name
    if [ $? -eq 0 ]; then
        echo "✅ STOPPED: Container '$container_name' stopped successfully"
    else
        echo "❌ STOP FAILED: Could not stop container '$container_name'"
    fi
}

# Create a timestamp file for each service to track last access
touch_last_access_file() {
    local domain=$1
    local container_name=$(yq e ".${domain}.container" $SERVICES_CONFIG)
    if [ -n "$container_name" ] && [ "$container_name" != "null" ]; then
        mkdir -p /tmp/quantixy_last_access
        touch "/tmp/quantixy_last_access/${container_name}"
    fi
}

# Function to generate NGINX config from services.yaml
generate_nginx_config() {
    local config_file="/etc/nginx/conf.d/default.conf"

    # Create the main config with log format
    cat >"/etc/nginx/nginx.conf" <<'EOF'
user nginx;
worker_processes auto;
error_log /var/log/nginx/error.log notice;
pid /var/run/nginx.pid;

events {
    worker_connections 1024;
}

http {
    include /etc/nginx/mime.types;
    default_type application/octet-stream;

    # Custom log format to include host
    log_format main_with_host '$remote_addr - $remote_user [$time_local] "$request" '
                              '$status $body_bytes_sent "$http_referer" '
                              '"$http_user_agent" "$host"';

    sendfile on;
    tcp_nopush on;
    keepalive_timeout 65;
    gzip on;

    include /etc/nginx/conf.d/*.conf;
}
EOF

    # Start with the default server block
    cat >"$config_file" <<'EOF'
server {
    listen 80 default_server;
    server_name _;
    
    access_log /var/log/nginx/access.log main_with_host;
    error_log /var/log/nginx/error.log;

    # Default location - serve loading page
    location / {
        root /usr/share/nginx/html;
        try_files /loading.html =404;
    }

    # Health check endpoint
    location /health {
        access_log off;
        return 200 "OK\n";
        add_header Content-Type text/plain;
    }
}

EOF

    # Generate server blocks for each domain in services.yaml
    #echo "🔧 DEBUG: Reading domains from services.yaml..."
    #echo "🔧 DEBUG: Services config content:"
    #cat "$SERVICES_CONFIG"

    # Use a temporary file to build the dynamic configs
    temp_config="/tmp/dynamic_servers.conf"
    >"$temp_config" # Clear temp file

    # Get all domains and process them
    yq e 'keys | .[]' $SERVICES_CONFIG | while IFS= read -r domain; do
        if [ -n "$domain" ]; then
            #echo "🔧 DEBUG: Processing domain: '$domain'"
            container_name=$(yq e ".[\"${domain}\"].container" $SERVICES_CONFIG)
            port=$(yq e ".[\"${domain}\"].port" $SERVICES_CONFIG)
            protocol=$(yq e ".[\"${domain}\"].protocol // \"http\"" $SERVICES_CONFIG)
            websocket=$(yq e ".[\"${domain}\"].websocket // false" $SERVICES_CONFIG)

            #echo "🔧 DEBUG: Domain '$domain' -> container: '$container_name', port: '$port'"

            if [ -n "$container_name" ] && [ "$container_name" != "null" ]; then
                #echo "Generating NGINX config for domain: $domain -> $container_name:$port"

                cat >>"$temp_config" <<EOF
server {
    listen 80;
    server_name $domain;
    
    access_log /var/log/nginx/access.log main_with_host;
    error_log /var/log/nginx/error.log;

    # DNS resolver for dynamic upstream resolution
    resolver 127.0.0.11 valid=30s;
    
    location / {
        # Use variable to enable dynamic resolution and prevent startup failures
        set \$upstream ${protocol}://${container_name}:${port};
        
        proxy_pass \$upstream;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
        
EOF

                # Add WebSocket support if enabled
                if [ "$websocket" = "true" ]; then
                    cat >>"$temp_config" <<'EOF'
        # WebSocket support
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        
EOF
                fi

                cat >>"$temp_config" <<'EOF'
        # Connection settings with shorter timeouts for faster failover
        proxy_connect_timeout 2s;
        proxy_send_timeout 10s;
        proxy_read_timeout 10s;
        
        # Don't cache anything
        proxy_no_cache 1;
        proxy_cache_bypass 1;
        add_header Cache-Control "no-cache, no-store, must-revalidate";
        add_header Pragma "no-cache";
        add_header Expires "0";
        
        # If proxy fails, serve loading page
        proxy_intercept_errors on;
        error_page 502 503 504 @loading;
    }
    
    # Loading page fallback for this domain
    location @loading {
        root /usr/share/nginx/html;
        try_files /loading.html =404;
    }
}

EOF
                #echo "✅ Successfully generated config for: $domain"
                #else
                #echo "❌ No container found for domain: $domain"
            fi
        fi
    done

    # Append the dynamic configs to the main config file
    if [ -f "$temp_config" ] && [ -s "$temp_config" ]; then
        echo "🔧 DEBUG: Appending dynamic configs to main config file"
        cat "$temp_config" >>"$config_file"
        rm "$temp_config"
        #echo "✅ Successfully generated dynamic configurations"
        #else
        #echo "❌ No dynamic configurations were generated"
    fi
}

# Generate NGINX configuration from services.yaml
echo "Generating NGINX configuration from services.yaml..."
generate_nginx_config

# Ensure log file exists before starting NGINX
touch $NGINX_ACCESS_LOG

# Remove the symlink and create a real log file
if [ -L "$NGINX_ACCESS_LOG" ]; then
    rm "$NGINX_ACCESS_LOG"
    touch "$NGINX_ACCESS_LOG"
fi

# Initialize NGINX - this will also create the log file if it doesn't exist
nginx -g 'daemon off;' &
NGINX_PID=$!
echo "NGINX started with PID $NGINX_PID"

# Give Nginx a moment to fully initialize
sleep 5

# Force reload NGINX to ensure fresh config
nginx -s reload
echo "NGINX configuration reloaded"

echo "Monitoring NGINX access log: $NGINX_ACCESS_LOG"
echo "Services configuration: $SERVICES_CONFIG"
echo "Timeout set to: $TIMEOUT_MINUTES minutes"

# Initialize last access times for all configured containers
yq e 'keys | .[]' $SERVICES_CONFIG | while read domain; do
    touch_last_access_file "$domain"
done

# Start periodic container cleanup in background
(
    while true; do
        sleep 60 # Check every minute
        # Iterate over configured domains
        yq e 'keys | .[]' $SERVICES_CONFIG | while read domain; do
            container_name=$(yq e ".${domain}.container" $SERVICES_CONFIG)
            if [ -z "$container_name" ] || [ "$container_name" == "null" ]; then
                continue
            fi

            # Check if container is running
            if docker ps -q -f name="^${container_name}$" | grep -q .; then
                last_access_file="/tmp/quantixy_last_access/${container_name}"
                if [ -f "$last_access_file" ]; then
                    last_access_time=$(stat -c %Y "$last_access_file")
                    current_time=$(date +%s)
                    inactive_time=$(((current_time - last_access_time) / 60))

                    if [ "$inactive_time" -ge "$TIMEOUT_MINUTES" ]; then
                        echo "🔍 TIMEOUT CHECK: Container '$container_name' for domain '$domain' has been inactive for $inactive_time minutes (limit: $TIMEOUT_MINUTES)"
                        stop_container "$container_name"
                    else
                        echo "⏰ ACTIVITY CHECK: Container '$container_name' for domain '$domain' is active (last access: $inactive_time minutes ago)"
                    fi
                else
                    # If last access file doesn't exist but container is running,
                    # it might have been started manually or an edge case.
                    # We'll create the access file to start tracking.
                    echo "📝 CREATING TRACKING: Last access file for container '$container_name' (domain: '$domain') not found. Creating it now."
                    touch_last_access_file "$domain"
                fi
            fi
        done
    done
) &

# Start Python log monitoring (replacing bash monitoring)
echo "🔍 Starting Python log monitoring..."
python3 /app/log_monitor.py &

# Start inactivity monitor in background
echo "Starting inactivity monitor (timeout: ${TIMEOUT_MINUTES} minutes, verbose: ${VERBOSE_LOGGING})..."
python3 /app/inactivity_monitor.py &

echo "Timeout set to: $TIMEOUT_MINUTES minutes"
echo "Verbose logging: $VERBOSE_LOGGING"
echo "🪄 Quantixy is now running"

# Keep the container running
if [ "$VERBOSE_LOGGING" = "true" ]; then
    # Show nginx logs in verbose mode
    tail -f /var/log/nginx/access.log
else
    # Keep container alive without showing logs
    while true; do
        sleep 3600 # Sleep for 1 hour, then repeat
    done
fi
