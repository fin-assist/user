#!/usr/bin/env python3
"""Resolve the immutable shared CI source; copied unchanged into consumer scripts/."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile

SOURCE_URL = "https://github.com/fin-assist/go-service-template.git"
CONFIG_NAME = ".finassist-ci.json"


def unique_object(pairs):
    """Reject ambiguous manifests rather than silently accepting duplicate keys."""
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("Duplicate shared CI configuration key.")
        result[key] = value
    return result


def read_config(root: Path) -> dict:
    """Read the small allowlisted consumer manifest without accepting shell syntax."""
    path = root / CONFIG_NAME
    if path.stat().st_size > 4096:
        raise ValueError("Shared CI configuration is too large.")
    config = json.loads(path.read_text(), object_pairs_hook=unique_object)
    if not isinstance(config, dict) or set(config) - {"revision", "profile", "gosec_g115", "squawk_smallint"}:
        raise ValueError("Unknown shared CI configuration fields.")
    if not isinstance(config.get("revision"), str) or not re.fullmatch(r"[0-9a-f]{40}", config["revision"]):
        raise ValueError("Shared CI revision must be a full lowercase commit SHA.")
    if config.get("profile") not in ("service", "library"):
        raise ValueError("Shared CI profile must be service or library.")
    for key in ("gosec_g115", "squawk_smallint"):
        if key in config and type(config[key]) is not bool:
            raise ValueError("Shared CI overrides must be booleans.")
        config.setdefault(key, False)
    return config


def git(root: Path, *args: str) -> str:
    """Run bounded git without shell evaluation, hooks, prompts or external config."""
    env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
    env.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull, GIT_TERMINAL_PROMPT="0")
    try:
        result = subprocess.run(
            ["git", "-c", "core.hooksPath=" + os.devnull, "-c", "core.fsmonitor=false", "-c", "protocol.file.allow=never", *args],
            cwd=root, env=env, check=True, capture_output=True, text=True, timeout=120)
    except (subprocess.SubprocessError, OSError) as error:
        # Git stderr can contain local credential helpers or authenticated URL rewrites.
        raise ValueError("Unable to obtain or verify the pinned shared CI source.") from error
    return result.stdout.strip()


def validate_cache(cache: Path, revision: str) -> Path:
    """Reject modified working trees and incorrect origin or commit identities."""
    if cache.is_symlink() or not cache.is_dir() or not (cache / ".git").is_dir() or (cache / ".git").is_symlink():
        raise ValueError("Invalid shared CI cache directory.")
    if git(cache, "rev-parse", "HEAD") != revision:
        raise ValueError("Shared CI cache revision differs from the manifest.")
    if git(cache, "config", "--get", "remote.origin.url") != SOURCE_URL:
        raise ValueError("Shared CI cache has an unexpected source repository.")
    if any(line.startswith(tuple("abcdefghijklmnopqrstuvwxyzS")) for line in git(cache, "ls-files", "-v").splitlines()):
        raise ValueError("Shared CI cache has hidden index entries; remove that revision cache and retry.")
    if git(cache, "status", "--porcelain", "--untracked-files=all", "--ignored=matching"):
        raise ValueError("Shared CI cache has local modifications; remove that revision cache and retry.")
    common = cache / "common"
    if common.is_symlink() or not (common / "render_lint.py").is_file():
        raise ValueError("Pinned shared CI source is incomplete.")
    return common


def source_dir(root: Path, config: dict, source: Path | None = None) -> Path:
    """Resolve an explicit local source or an independently verified pinned checkout."""
    if source is not None:
        if os.environ.get("GITHUB_ACTIONS") == "true" and os.environ.get("GITHUB_EVENT_NAME") not in {"push", "pull_request"}:
            raise ValueError("Local source overrides are restricted to ordinary push or pull_request CI.")
        common = source.resolve()
        if not common.is_dir() or not (common / "render_lint.py").is_file() or not (common / "configs").is_dir():
            raise ValueError("Local source must be the shared common directory.")
        return common
    cache_root = root / ".finassist-ci"
    if cache_root.is_symlink():
        raise ValueError("Shared CI cache root must not be a symbolic link.")
    cache_root.mkdir(exist_ok=True)
    revision = config["revision"]
    cache = cache_root / revision
    if cache.exists() or cache.is_symlink():
        return validate_cache(cache, revision)
    staging = Path(tempfile.mkdtemp(prefix=".fetch-", dir=cache_root))
    try:
        template = staging / "empty-template"
        template.mkdir()
        git(staging, "init", "--quiet", "--template=" + str(template))
        template.rmdir()
        git(staging, "remote", "add", "origin", SOURCE_URL)
        git(staging, "fetch", "--quiet", "--depth=1", "origin", revision)
        git(staging, "checkout", "--quiet", "--detach", "FETCH_HEAD")
        validate_cache(staging, revision)
        try:
            staging.rename(cache)
        except OSError:
            if not cache.is_dir():
                raise
        return validate_cache(cache, revision)
    finally:
        if staging.exists():
            shutil.rmtree(staging)


def main() -> int:
    """Print the common directory, or render shared configs for the caller root."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", nargs="?", choices=["render"])
    parser.add_argument("--source", type=Path, help="Explicit local common/ source for development and unprivileged CI")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    try:
        common = source_dir(root, read_config(root), args.source)
        if args.command == "render":
            subprocess.run([sys.executable, "-B", str(common / "render_lint.py"), "--root", str(root)],
                           check=True, timeout=60)
        else:
            print(common)
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print("Shared CI bootstrap failed: " + str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
