#!/usr/bin/env python3
"""
Inactivity Monitor for Quantixy

This script monitors service inactivity and shuts down containers 
after a specified timeout period (TIMEOUT_MINUTES).

The script runs independently in the background and does not rely on any user interaction.
"""

import os
import time
import logging
import subprocess
import datetime
import sys
from pathlib import Path

from utils import load_services_config

# Configure logging based on VERBOSE_LOGGING environment variable
VERBOSE_LOGGING = os.environ.get('VERBOSE_LOGGING', 'false').lower() in ('true', '1', 'yes')

log_level = logging.INFO if VERBOSE_LOGGING else logging.WARNING

logging.basicConfig(
    level=log_level,
    format='%(asctime)s - %(levelname)s - %(message)s',
    handlers=[logging.StreamHandler(sys.stdout)]
)
logger = logging.getLogger('inactivity_monitor')

# create stdout logger
STDOUT_LOGGER = logging.StreamHandler(sys.stdout) if VERBOSE_LOGGING else None

# Configuration
TIMEOUT_MINUTES = int(os.environ.get('TIMEOUT_MINUTES', '10'))
LAST_ACCESS_DIR = Path('/tmp/quantixy_last_access')
CHECK_INTERVAL = 30  # Check half minute

if VERBOSE_LOGGING:
    logger.info(f"Inactivity monitor started with timeout: {TIMEOUT_MINUTES} minutes")
    logger.info(f"Verbose logging enabled: {VERBOSE_LOGGING}")
else:
    logger.warning(f"Inactivity monitor started (timeout: {TIMEOUT_MINUTES}m, verbose: {VERBOSE_LOGGING})")


def get_containers():
    """Get list of container names from services.yaml using native Python libraries"""
    try:
        services_data = load_services_config()

        if not services_data:
            logger.error("Services configuration is empty or invalid")
            return []

        # Extract containers from the YAML data
        containers = [
            service.get('container')
            for service in services_data.values()
            if isinstance(service, dict) and 'container' in service
        ]
        return [c for c in containers if c]  # Filter out None values
    except Exception as e:
        logger.error(f"Unexpected error: {e}")
        return []

def check_container_running(container_name):
    """Check if container is running using docker CLI"""
    try:
        result = subprocess.run(
            ['docker', 'ps', '-q', '-f', f'name=^{container_name}$'],
            capture_output=True, text=True, check=True
        )
        return bool(result.stdout.strip())
    except subprocess.CalledProcessError as e:
        logger.error(f"Failed to check container {container_name}: {e}")
        return False

def get_last_activity(container_name):
    """Get timestamp of last container activity"""
    last_access_file = LAST_ACCESS_DIR / container_name
    if not last_access_file.exists():
        return None
    
    try:
        # Get file modification time
        mtime = last_access_file.stat().st_mtime
        return datetime.datetime.fromtimestamp(mtime)
    except Exception as e:
        logger.error(f"Error getting last activity for {container_name}: {e}")
        return None

def stop_container(container_name):
    """Stop a container due to inactivity"""
    try:
        logger.info(f"Stopping container {container_name} due to inactivity (timeout: {TIMEOUT_MINUTES} minutes)")
        subprocess.run(['docker', 'stop', container_name], check=True)
        logger.info(f"Container {container_name} stopped successfully")
    except subprocess.CalledProcessError as e:
        logger.error(f"Failed to stop container {container_name}: {e}")

def main():
    """Main monitoring loop"""
    # Ensure last access directory exists
    LAST_ACCESS_DIR.mkdir(exist_ok=True, parents=True)
    
    # Main monitoring loop
    while True:
        now = datetime.datetime.now()
        containers = get_containers()
        
        for container in containers:
            # Only check running containers
            if check_container_running(container):
                last_activity = get_last_activity(container)
                
                # If no activity record exists, create one and continue
                if not last_activity:
                    logger.info(f"No activity record for {container}, creating one")
                    (LAST_ACCESS_DIR / container).touch()
                    continue
                  # Calculate inactivity duration
                inactive_minutes = (now - last_activity).total_seconds() / 60
                
                # Log for monitoring - only when verbose logging is enabled
                if inactive_minutes > TIMEOUT_MINUTES / 2 and VERBOSE_LOGGING:
                    logger.info(f"Container {container} inactive for {inactive_minutes:.1f} minutes (timeout: {TIMEOUT_MINUTES})")
                
                # Stop container if inactive for too long
                if inactive_minutes >= TIMEOUT_MINUTES:
                    stop_container(container)
        
        # Wait for next check
        time.sleep(CHECK_INTERVAL)

if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        logger.info("Inactivity monitor stopped")
    except Exception as e:
        logger.error(f"Error in inactivity monitor: {e}", exc_info=True)