import os
import yaml
from typing import Dict, Any

DEFAULT_CONFIG_PATH = '/etc/conch/sdk-config.yaml'

def load_config(config_path: str = None) -> Dict[str, Any]:
    config_path = config_path or DEFAULT_CONFIG_PATH

    if not os.path.exists(config_path):
        raise FileNotFoundError(f"Configuration file not found: {config_path}")

    with open(config_path, "r", encoding="utf-8") as f:
        try:
            config = yaml.safe_load(f)
        except yaml.YAMLError as e:
            raise ValueError(f"Failed to parse configuration file: {e}")

    return config

config = load_config()
