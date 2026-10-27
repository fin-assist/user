#!/usr/bin/env python3
"""Extract a stage binary from the already built service image (FIN-148)."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess


def docker(*args):
    return subprocess.run(["docker", *args], check=True, text=True,
                          capture_output=True, timeout=60).stdout.strip()


def package(service, image, output, environ=os.environ):
    """Bind a binary checksum to the exact image, source and Actions run."""
    if not re.fullmatch(r"[a-z][a-z0-9-]*", service):
        raise ValueError("invalid service name")
    sha = environ["GITHUB_SHA"]
    repository = environ["GITHUB_REPOSITORY"]
    run_id = environ["GITHUB_RUN_ID"]
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("expected full source SHA")
    if not re.fullmatch(r"fin-assist/[a-z][a-z0-9-]*", repository):
        raise ValueError("unexpected repository")
    if not run_id.isdigit():
        raise ValueError("invalid workflow run")
    image_id = docker("image", "inspect", "--format", "{{.Id}}", image)
    platform = docker("image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", image)
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id) or platform != "linux/amd64":
        raise ValueError("expected a linux/amd64 image with a content ID")
    output.mkdir(parents=True, exist_ok=False)
    container = docker("create", image_id)
    try:
        docker("cp", f"{container}:/{service}", str(output / service))
    finally:
        docker("rm", container)
    binary = output / service
    if binary.is_symlink() or not binary.is_file() or binary.stat().st_size == 0:
        raise ValueError("image binary is not a nonempty regular file")
    binary.chmod(0o755)
    hasher = hashlib.sha256()
    with binary.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            hasher.update(chunk)
    digest = hasher.hexdigest()
    modules = re.findall(r"github.com/fin-assist/([^\s]+)\s+(v[^\s]+)", Path("go.mod").read_text())
    manifest = dict(schema_version=1, service=service, repository=repository, source_sha=sha,
                    run_id=int(run_id), run_attempt=int(environ.get("GITHUB_RUN_ATTEMPT", "1")),
                    image_id=image_id, platform=platform, binary_sha256=digest,
                    modules=dict(modules))
    (output / "manifest.json").write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
    (output / "SHA256SUMS").write_text(f"{digest}  {service}\n")
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("service")
    parser.add_argument("image")
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    package(args.service, args.image, args.output)


if __name__ == "__main__":
    main()
