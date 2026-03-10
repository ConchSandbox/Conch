import argparse
import os
import shutil
import sys
from typing import List, Optional

from .config_loader import SYSTEM_CONFIG_DIR, SYSTEM_CONFIG_PATH, get_package_template_path


def _ensure_directory(path: str) -> None:
    os.makedirs(path, exist_ok=True)


def _parse_args(argv: Optional[List[str]] = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        prog="conch-sdk-init-config",
        description="Initialize /etc/conch/sdk-config.yaml from the template (config/sdk-config.yaml).",
    )
    parser.add_argument(
        "-f",
        "--force",
        action="store_true",
        help="Overwrite existing /etc/conch/sdk-config.yaml if it exists.",
    )
    return parser.parse_args(argv)


def main(argv: Optional[List[str]] = None) -> None:
    if argv is None:
        argv = sys.argv[1:]
    args = _parse_args(argv)
    force = args.force

    template_path = get_package_template_path()
    if not os.path.exists(template_path):
        print(f"Error: template config not found at '{template_path}'.", file=sys.stderr)
        sys.exit(1)

    target_dir = SYSTEM_CONFIG_DIR
    target_path = SYSTEM_CONFIG_PATH

    try:
        _ensure_directory(target_dir)

        if os.path.exists(target_path) and not force:
            print(f"{target_path} already exists. Use --force to overwrite.")
            return

        shutil.copyfile(template_path, target_path)
        print(f"Initialized configuration at: {target_path}")
    except PermissionError:
        print(
            "Error: Permission denied while writing to /etc/conch.\n"
            "Please run this command with sufficient privileges (e.g., using sudo).",
            file=sys.stderr,
        )
        sys.exit(1)
    except OSError as e:
        print(f"Error: failed to write configuration: {e}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
