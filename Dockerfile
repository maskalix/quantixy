FROM nginx:stable-alpine

# Install required packages
RUN apk add --no-cache \
    docker-cli \
    bash \
    curl \
    yq \
    python3

COPY requirements.txt /app/requirements.txt

# pip install
RUN python3 -m ensurepip && \
    pip3 install --no-cache --upgrade pip && \
    pip3 install -r requirements.txt

# Create required directories and log files
RUN mkdir -p /etc/quantixy /tmp/quantixy_last_access /app /var/log/nginx && \
    touch /var/log/nginx/access.log /var/log/nginx/error.log

# Copy NGINX configuration
COPY nginx/conf.d/default.conf /etc/nginx/conf.d/default.conf

# Copy HTML loading page
COPY html/loading.html /usr/share/nginx/html/loading.html

# Copy the entire html folder
COPY html/ /app/html/

# Copy services.yaml
COPY services.yaml /etc/quantixy/services.yaml

# Copy Python log monitor
COPY log_monitor.py /app/log_monitor.py

# Copy inactivity monitor
COPY inactivity_monitor.py /app/inactivity_monitor.py

# Copy entrypoint script and make it executable
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Expose port 80
EXPOSE 80

# Run entrypoint script
ENTRYPOINT ["/entrypoint.sh"]