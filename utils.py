import logging
import os
import sys

import yaml

SERVICES_CONFIG = '/etc/quantixy/services.yaml'
ENV_VAR_PREFIX = 'QUANTIXY__'

# convert following env var to yaml and load it. Or load SERVICES_CONFIG if it exists
# QUANTIXY__domain.exemple__container=test
# QUANTIXY__domain.exemple__port=1234
# QUANTIXY__domain.exemple__protocol=http
# QUANTIXY__domain.exemple__websocket=true

SERVICES_DATA_FROM_ENV_VAR = {}

def load_services_config():
    # get all env vars that start with QUANTIXY_

    # if SERVICES_DATA_FROM_ENV_VAR is empty, load from environment variables
    if not SERVICES_DATA_FROM_ENV_VAR:
        for key, value in os.environ.items():
            if key.startswith(ENV_VAR_PREFIX):
                parts = key[len(ENV_VAR_PREFIX):].split('__')
                domain = '.'.join(parts[:-1])
                if domain not in SERVICES_DATA_FROM_ENV_VAR:
                    SERVICES_DATA_FROM_ENV_VAR[domain] = {}
                SERVICES_DATA_FROM_ENV_VAR[domain][parts[-1].lower()] = value

    services_data = SERVICES_DATA_FROM_ENV_VAR.copy()

    # load from SERVICES_CONFIG if it exists
    if os.path.exists(SERVICES_CONFIG):
        try:
            with open(SERVICES_CONFIG, 'r') as file:
                config_data = yaml.safe_load(file)
                for domain, service in config_data.items():
                    if domain not in services_data:
                        services_data[domain] = {}
                    services_data[domain].update(service)
        except FileNotFoundError:
            print(f"❌ Services config file not found: {SERVICES_CONFIG}")
        except yaml.YAMLError as e:
            print(f"❌ Failed to parse YAML file: {e}")

    if os.environ.get('VERBOSE_LOGGING', 'false').lower() in ('true', '1', 'yes'):
        logger = logging.getLogger('debug')
        logger.info("🔧 Loaded services configuration:")
        logger.info(yaml.dump(services_data, default_flow_style=False))

    return services_data