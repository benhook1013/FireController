"""Prepare the pinned Linux/amd64 offsite transport used by CI and deployment."""

import hashlib
import io
import os
from pathlib import Path
import sys
import tempfile
import urllib.request
import zipfile

VERSION = "v1.75.2"
ARCHIVE_SHA256 = "349ac8fba6ff65d6247043f1750cdcb518ec5d500ef91463a10d37c0ccdf3702"
BINARY_SHA256 = "ed7ab1f17876bb006c3df3949a3044959671afd9112362dacaa5dd1647735bc1"


def prepare(output):
    name = "rclone-" + VERSION + "-linux-amd64"
    with urllib.request.urlopen(f"https://downloads.rclone.org/{VERSION}/{name}.zip", timeout=30) as response:
        raw = response.read((64 << 20) + 1)
    if hashlib.sha256(raw).hexdigest() != ARCHIVE_SHA256:
        raise ValueError("rclone archive digest mismatch")
    with zipfile.ZipFile(io.BytesIO(raw)) as archive:
        binary = archive.read(name + "/rclone")
    if hashlib.sha256(binary).hexdigest() != BINARY_SHA256:
        raise ValueError("rclone executable digest mismatch")
    output = Path(output)
    with tempfile.TemporaryDirectory(prefix=".rclone-", dir=output.parent) as directory:
        temporary = Path(directory) / "rclone"
        temporary.write_bytes(binary)
        temporary.chmod(0o555)
        os.link(temporary, output, follow_symlinks=False)
    return BINARY_SHA256


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("usage: qualified_rclone.py OUTPUT")
    try:
        print(prepare(sys.argv[1]))
    except (OSError, ValueError, zipfile.BadZipFile) as error:
        raise SystemExit(str(error)) from None
