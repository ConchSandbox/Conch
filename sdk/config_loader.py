import os
from typing import Dict, Any, Optional, List

import yaml

def _candidate_paths_from_env() -> List[str]:
    paths: List[str] = []
    env_path = os.getenv("CONCH_SDK_CONFIG")
    if env_path:
        paths.append(env_path)
    return paths

def _candidate_paths_from_user() -> List[str]:
    paths: List[str] = []
    home = os.path.expanduser("~")
    xdg_config_home = os.getenv("XDG_CONFIG_HOME", os.path.join(home, ".config"))
    user_path = os.path.join(xdg_config_home, "conch", "sdk-config.yaml")
    paths.append(user_path)
    return paths

def _candidate_paths_from_system() -> List[str]:
    return ["/etc/conch/sdk-config.yaml"]

def _candidate_paths_from_repo_configs() -> List[str]:
    # repo configs/ directory as fallback (for development/source code running scenarios)
    here = os.path.dirname(__file__)
    repo_root = os.path.abspath(os.path.join(here, ".."))
    return [os.path.join(repo_root, "config", "sdk-config.yaml")]

def _find_default_config_path() -> str:
    """
    find the default config path in the order of priority:
    1. environment variable CONCH_SDK_CONFIG
    2. user-level config ~/.config/conch/sdk-config.yaml (or $XDG_CONFIG_HOME)
    3. system-level config /etc/conch/sdk-config.yaml
    4. repo-level default config configs/sdk-config.yaml
    """
    search_order = (
        _candidate_paths_from_env()
        + _candidate_paths_from_user()
        + _candidate_paths_from_system()
        + _candidate_paths_from_repo_configs()
    )

    for path in search_order:
        if path and os.path.exists(path):
            return path

    raise FileNotFoundError(
        "Conch SDK configuration file not found. "
        "Tried (in order):\n"
        "  1) $CONCH_SDK_CONFIG\n"
        "  2) $XDG_CONFIG_HOME/conch/sdk-config.yaml (or ~/.config/conch/sdk-config.yaml)\n"
        "  3) /etc/conch/sdk-config.yaml\n"
        "  4) <repo>/configs/sdk-config.yaml\n"
        "Please create one based on the default template (configs/sdk-config.yaml) "
        "or run the CLI to initialize /etc configuration."
    )

def load_config(config_path: Optional[str] = None) -> Dict[str, Any]:
    """
    load config:
    - when config_path is explicitly provided, use it;
    - otherwise, load the config from the default path in the order of priority.
    """
    resolved_path = config_path or _find_default_config_path()

    if not os.path.exists(resolved_path):
        raise FileNotFoundError(f"Configuration file not found: {resolved_path}")

    with open(resolved_path, "r", encoding="utf-8") as f:
        try:
            cfg = yaml.safe_load(f)
        except yaml.YAMLError as e:
            raise ValueError(f"Failed to parse configuration file '{resolved_path}': {e}")

    if not isinstance(cfg, dict):
        raise ValueError(f"Configuration file '{resolved_path}' must contain a YAML mapping at the top level.")

    return cfg
