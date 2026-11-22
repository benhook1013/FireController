"""Prepare the pinned Linux/amd64 offsite transport used by CI and deployment."""

import hashlib
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import zipfile

VERSION = "v1.75.2"
ARCHIVE_SHA256 = "349ac8fba6ff65d6247043f1750cdcb518ec5d500ef91463a10d37c0ccdf3702"
BINARY_SHA256 = "ed7ab1f17876bb006c3df3949a3044959671afd9112362dacaa5dd1647735bc1"


def prepare(output):
    name = "rclone-" + VERSION + "-linux-amd64"
    output = Path(output)
    with tempfile.TemporaryDirectory(prefix=".rclone-", dir=output.parent) as directory:
        archive_path = Path(directory) / "rclone.zip"
        subprocess.run(
            ["curl", "--fail", "--silent", "--show-error", "--location", "--retry", "2",
             "--retry-delay", "2", "--max-time", "90", "--retry-max-time", "90",
             "--max-filesize", str(64 << 20), "--output", str(archive_path),
             f"https://downloads.rclone.org/{VERSION}/{name}.zip"],
            check=True,
        )
        with archive_path.open("rb") as stream:
            raw = stream.read((64 << 20) + 1)
        if hashlib.sha256(raw).hexdigest() != ARCHIVE_SHA256:
            raise ValueError("rclone archive digest mismatch")
        with zipfile.ZipFile(archive_path) as archive:
            binary = archive.read(name + "/rclone")
        if hashlib.sha256(binary).hexdigest() != BINARY_SHA256:
            raise ValueError("rclone executable digest mismatch")
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
    except (OSError, ValueError, subprocess.CalledProcessError, zipfile.BadZipFile) as error:
        raise SystemExit(str(error)) from None
