#!/usr/bin/env python3
import time
import subprocess
import re
import os
import sys
from pathlib import Path

# Configuration
SERVICES_CONFIG = "/etc/quantixy/services.yaml"
NGINX_ACCESS_LOG = "/var/log/nginx/access.log"
TIMEOUT_MINUTES = int(os.getenv('TIMEOUT_MINUTES', '10'))

def load_services_config():
    """Load and return the services configuration from YAML using yq"""
    try:
        print("🔧 Loading services config using yq...")
        # Use yq to get just the domain names first
        result = subprocess.run(['yq', 'e', 'keys | .[]', SERVICES_CONFIG], 
                              capture_output=True, text=True, timeout=10)
        if result.returncode != 0:
            print(f"❌ Failed to read domain keys: {result.stderr}")
            return {}
        
        domains = [line.strip() for line in result.stdout.split('\n') if line.strip()]
        print(f"📋 Found domains: {domains}")
        
        config = {}
        for domain in domains:
            # Get container name for this domain
            result = subprocess.run(['yq', 'e', f'."{domain}".container', SERVICES_CONFIG], 
                                  capture_output=True, text=True, timeout=5)
            if result.returncode == 0 and result.stdout.strip():
                container = result.stdout.strip()
                
                # Get port for this domain
                result = subprocess.run(['yq', 'e', f'."{domain}".port', SERVICES_CONFIG], 
                                      capture_output=True, text=True, timeout=5)
                port = result.stdout.strip() if result.returncode == 0 else '80'
                
                config[domain] = {
                    'container': container,
                    'port': port
                }
                print(f"   {domain} -> {container}:{port}")
        
        return config
    except Exception as e:
        print(f"❌ Failed to load services config: {e}")
        import traceback
        traceback.print_exc()
        return {}

def start_container(domain, services_config):
    """Start a container for the given domain"""
    if domain not in services_config:
        print(f"❌ Domain {domain} not found in services config")
        return False
    
    container_name = services_config[domain].get('container')
    if not container_name:
        print(f"❌ No container mapping found for domain {domain}")
        return False
    
    #print(f"🚀 ATTEMPTING TO START: Container '{container_name}' for domain '{domain}'")
    
    # Write current domain being started to a file for the loading page
    try:
        with open('/usr/share/nginx/html/current_domain.txt', 'w') as f:
            f.write(domain)
    except Exception as e:
        print(f"⚠️ Could not write current domain file: {e}")
    
    # Check if container exists
    try:
        result = subprocess.run(['docker', 'ps', '-a', '-q', '-f', f'name=^{container_name}$'], 
                              capture_output=True, text=True)
        if not result.stdout.strip():
            print(f"❌ ERROR: Container '{container_name}' does not exist for domain '{domain}'!")
            return False
        
        # Check if container is running
        result = subprocess.run(['docker', 'ps', '-q', '-f', f'name=^{container_name}$'], 
                              capture_output=True, text=True)
        if result.stdout.strip():
            #print(f"✅ ALREADY RUNNING: Container '{container_name}' is already running for domain '{domain}'")
            return True
        
        # Container exists but is stopped, start it
        #print(f"📦 CONTAINER STATUS: '{container_name}' exists but is stopped")
        #print(f"⚡ STARTING: Container '{container_name}'...")
        
        result = subprocess.run(['docker', 'start', container_name], 
                              capture_output=True, text=True)
        if result.returncode == 0:
            #print(f"✅ SUCCESS: Container '{container_name}' started successfully for domain '{domain}'")
            return True
        else:
            print(f"❌ FAILED: Could not start container '{container_name}' for domain '{domain}'")
            print(f"Error: {result.stderr}")
            return False
            
    except Exception as e:
        print(f"❌ Error checking/starting container: {e}")
        return False

def touch_last_access_file(domain, services_config):
    """Update the last access time for a domain's container"""
    if domain not in services_config:
        return
    
    container_name = services_config[domain].get('container')
    if not container_name:
        return
    
    try:
        access_dir = Path('/tmp/quantixy_last_access')
        access_dir.mkdir(exist_ok=True)
        access_file = access_dir / container_name
        access_file.touch()
    except Exception as e:
        print(f"⚠️ Could not update last access file: {e}")

def extract_host_from_log_line(line):
    """Extract the host from NGINX log line with custom format"""
    # The host is the last field in quotes
    match = re.search(r'"([^"]+)"$', line)
    if match:
        return match.group(1)
    return None

def monitor_logs():
    """Monitor NGINX access logs and start containers on demand"""
    print("🔍 Starting Python log monitoring...")
    print(f"🔧 Python version: {sys.version}")
    
    # Test subprocess
    try:
        result = subprocess.run(['yq', '--version'], capture_output=True, text=True, timeout=5)
        print(f"✅ yq version: {result.stdout.strip()}")
    except Exception as e:
        print(f"❌ yq test failed: {e}")
    
    # Load services configuration
    print("📋 Loading services configuration...")
    services_config = load_services_config()
    if not services_config:
        print("❌ No services configuration found, exiting")
        return
    
    print("✅ Services config loaded:")
    for domain, config in services_config.items():
        container = config.get('container', 'unknown')
        port = config.get('port', 'unknown')
        print(f"   {domain} -> {container}:{port}")
    
    # Check if log file exists
    log_file = Path(NGINX_ACCESS_LOG)
    if not log_file.exists():
        print(f"⏳ Waiting for log file {NGINX_ACCESS_LOG} to be created...")
        while not log_file.exists():
            time.sleep(1)
    
    print(f"📝 Monitoring {NGINX_ACCESS_LOG} for new entries...")
    
    # Track last position in file
    last_position = log_file.stat().st_size
    
    while True:
        try:
            current_size = log_file.stat().st_size
            
            if current_size > last_position:
                # New content added to file
                with open(log_file, 'r') as f:
                    f.seek(last_position)
                    new_lines = f.readlines()
                
                for line in new_lines:
                    line = line.strip()
                    if not line:
                        continue
                    
                    #print(f"📝 Log entry: {line}")
                    
                    # Extract host from log line
                    host = extract_host_from_log_line(line)
                    if not host:
                        print("⚠️ Could not extract host from log line")
                        continue
                    
                    #print(f"🌐 DOMAIN DETECTED: {host}")
                    
                    # Check if domain is configured
                    if host in services_config:
                        #print(f"✅ CONFIGURED DOMAIN: {host} is configured in services.yaml")
                        container_name = services_config[host].get('container')
                        #print(f"🐳 STARTING CONTAINER: {container_name} for domain {host}")
                        
                        # Start the container
                        if start_container(host, services_config):
                            touch_last_access_file(host, services_config)
                        
                        # Check for 502 errors
                        if ' 502 ' in line:
                            print(f"🚨 502 ERROR detected for host: {host}")
                            print(f"🔄 RETRY STARTING CONTAINER: {container_name} for domain {host}")
                            start_container(host, services_config)
                    else:
                        print(f"❌ UNCONFIGURED DOMAIN: {host} is not configured in services.yaml")
                
                last_position = current_size
            
            time.sleep(0.5)  # Check every 500ms
            
        except Exception as e:
            print(f"❌ Error monitoring logs: {e}")
            time.sleep(5)  # Wait before retrying

if __name__ == "__main__":
    try:
        print("🐍 Python log monitor starting...")
        print(f"📁 Working directory: {os.getcwd()}")
        print(f"📄 Services config path: {SERVICES_CONFIG}")
        print(f"📄 Access log path: {NGINX_ACCESS_LOG}")
        
        # Test if files exist
        if os.path.exists(SERVICES_CONFIG):
            print(f"✅ Services config file exists")
        else:
            print(f"❌ Services config file does not exist!")
            
        if os.path.exists(NGINX_ACCESS_LOG):
            print(f"✅ Access log file exists")
        else:
            print(f"❌ Access log file does not exist!")
        
        monitor_logs()
    except Exception as e:
        print(f"💥 FATAL ERROR in Python monitor: {e}")
        import traceback
        traceback.print_exc()
        sys.exit(1)