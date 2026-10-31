#!/usr/bin/env python3
"""Package tested native binaries with the exact revision's public guidance."""
import hashlib
import io
import json
from pathlib import Path
import re
import subprocess
import sys
import tarfile


def package(binary_dir, platform, destination, revision):
    root = Path(__file__).resolve().parents[2]
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise ValueError("revision must be a full Git commit")
    if not re.fullmatch(r"(linux|darwin|windows)-(amd64|arm64)", platform):
        raise ValueError("unsupported platform")
    actual = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=root, text=True).strip()
    if actual != revision:
        raise ValueError("package revision does not match this checkout")
    suffix = ".exe" if platform.startswith("windows-") else ""
    binaries = {}
    for name in ("fc", "fcd", "fcmcp"):
        path = Path(binary_dir) / (name + suffix)
        metadata = subprocess.check_output(["go", "version", "-m", str(path)], text=True)
        if "vcs.revision=" + revision + "\n" not in metadata or "vcs.modified=false\n" not in metadata:
            raise ValueError("binary does not identify a clean build of the selected revision: " + name)
        binaries[name + suffix] = path.read_bytes()
    public = subprocess.check_output(["git", "archive", revision, "LICENSE", "docs", "skills"], cwd=root)
    destination = Path(destination)
    destination.mkdir(parents=True, exist_ok=True)
    archive = destination / ("firecontroller-" + platform + ".tar.gz")
    metadata = {
        "source_revision": revision,
        "platform": platform,
        "binary_sha256": {name: hashlib.sha256(body).hexdigest() for name, body in binaries.items()},
    }
    with tarfile.open(archive, "x:gz") as out:
        for name, body in binaries.items():
            info = tarfile.TarInfo("bin/" + name)
            info.mode, info.size = 0o755, len(body)
            out.addfile(info, io.BytesIO(body))
        body = (json.dumps(metadata, indent=2) + "\n").encode()
        info = tarfile.TarInfo("source.json")
        info.mode, info.size = 0o644, len(body)
        out.addfile(info, io.BytesIO(body))
        with tarfile.open(fileobj=io.BytesIO(public)) as source:
            for info in source:
                if not (info.isdir() or info.isfile()):
                    raise ValueError("public guidance contains a non-regular member")
                out.addfile(info, source.extractfile(info) if info.isfile() else None)
    checksum = hashlib.sha256(archive.read_bytes()).hexdigest()
    archive.with_name(archive.name + ".sha256").write_text(checksum + "  " + archive.name + "\n")
    print(archive)


if __name__ == "__main__":
    if len(sys.argv) != 5:
        raise SystemExit("usage: package-clients.py BIN_DIR PLATFORM OUTPUT_DIR REVISION")
    package(*sys.argv[1:])
