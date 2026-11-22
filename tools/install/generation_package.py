"""Prepare and verify an immutable Linux/amd64 installation payload."""

import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import re
import stat
import struct
import subprocess
import tarfile
import tempfile

import deploy_helper_consumer
import deploy_helper_package
import platform_prerequisites
from deploy_helper_package import Invalid

SOURCE = re.compile(r"[0-9a-f]{40}")
DIGEST = re.compile(r"[0-9a-f]{64}")
IMAGE = re.compile(r"[a-z0-9][a-z0-9.:-]*(/[a-z0-9][a-z0-9._-]*)+@sha256:[0-9a-f]{64}")
HELPERS = "deploy-helpers.tar"
MANIFEST = "manifest.json"
MODES = {"fcd": 0o555, "rclone": 0o555, HELPERS: 0o444, MANIFEST: 0o444}
LIMITS = {
    "fcd": platform_prerequisites.MAX_EXECUTABLE,
    "rclone": platform_prerequisites.MAX_EXECUTABLE,
    HELPERS: deploy_helper_consumer.MAX_ARCHIVE,
    MANIFEST: 65536,
}


def _validate_pins(source, image, *digests):
    if not isinstance(source, str) or not SOURCE.fullmatch(source):
        raise Invalid("full Git source revision required")
    if not isinstance(image, str) or not IMAGE.fullmatch(image):
        raise Invalid("image reference must select an immutable SHA-256 digest")
    if any(not isinstance(digest, str) or not DIGEST.fullmatch(digest) for digest in digests):
        raise Invalid("exact SHA-256 digest required")


def _sha256(file):
    digest = hashlib.sha256()
    for block in iter(lambda: file.read(1 << 20), b""):
        digest.update(block)
    return digest.hexdigest()


def _open_regular(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    stream = os.fdopen(descriptor, "rb")
    if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
        stream.close()
        raise Invalid("regular payload file required")
    return stream


def _check_executable(file, role):
    header = file.read(20)
    if (len(header) != 20 or header[:7] != b"\x7fELF\x02\x01\x01"
            or struct.unpack_from("<H", header, 16)[0] not in (2, 3)
            or struct.unpack_from("<H", header, 18)[0] != 62):
        raise Invalid(role + " must be a Linux/amd64 ELF executable")
    file.seek(0)


def _add_member(archive, name, file, size):
    member = tarfile.TarInfo(name)
    member.size = size
    member.mode = MODES[name]
    archive.addfile(member, file)


def _receipt(source, image, digest):
    return {"source": source, "image": image, "package_sha256": digest,
            "payload_verified": True, "installation_ready": False}


def _verify_payload(stream, expected_sha256, source, image):
    if _sha256(stream) != expected_sha256:
        raise Invalid("generation package digest mismatch")
    stream.seek(0)
    with tarfile.open(fileobj=stream, mode="r:") as archive:
        members = {}
        for member in archive:
            name = member.name
            if (name not in MODES or name in members or not member.isfile()
                    or member.mode != MODES[name] or not 0 < member.size <= LIMITS[name]):
                raise Invalid("unexpected generation package member")
            members[name] = member
        if set(members) != set(MODES):
            raise Invalid("incomplete generation package")
        with archive.extractfile(members[MANIFEST]) as file:
            manifest = json.load(file, object_pairs_hook=deploy_helper_consumer.pairs)
        if (not isinstance(manifest, dict) or set(manifest) != {"schema", "source", "image", "platform", "sha256"}
                or type(manifest["schema"]) is not int or manifest["schema"] != 1
                or (manifest["source"], manifest["image"], manifest["platform"]) != (source, image, "linux/amd64")):
            raise Invalid("generation manifest identity mismatch")
        hashes = manifest["sha256"]
        if not isinstance(hashes, dict) or set(hashes) != {"fcd", "rclone", HELPERS}:
            raise Invalid("generation manifest has an incomplete payload inventory")
        _validate_pins(source, image, *hashes.values())
        for name in ("fcd", "rclone"):
            with archive.extractfile(members[name]) as file:
                _check_executable(file, name)
                if _sha256(file) != hashes[name]:
                    raise Invalid(name + " digest mismatch")
        with archive.extractfile(members[HELPERS]) as file:
            deploy_helper_consumer.verify(file.read(), hashes[HELPERS], source)
    return _receipt(source, image, expected_sha256)


def verify(package, expected_sha256, source, image):
    """Verify the exact payload without extracting or executing it."""
    _validate_pins(source, image, expected_sha256)
    try:
        with _open_regular(package) as stream:
            info = os.fstat(stream.fileno())
            if not 0 < info.st_size <= sum(LIMITS.values()) + (1 << 20):
                raise Invalid("bounded regular generation package required")
            receipt = _verify_payload(stream, expected_sha256, source, image)
            if platform_prerequisites.identity(info) != platform_prerequisites.identity(os.fstat(stream.fileno())):
                raise Invalid("generation package changed during verification")
            return receipt
    except (OSError, tarfile.TarError, UnicodeError, json.JSONDecodeError) as error:
        raise Invalid("generation package unreadable or invalid") from error


def _docker(*args):
    try:
        return subprocess.run(["docker", *args], check=True, capture_output=True, text=True, timeout=60).stdout.strip()
    except (OSError, subprocess.SubprocessError) as error:
        raise Invalid("local Docker image operation failed: " + " ".join(args[:2])) from error


def _extract_fcd(image, source, output):
    format_string = ('[{{json .Id}},{{json .RepoDigests}},'
                     '{{json (index .Config.Labels "org.opencontainers.image.revision")}},'
                     '{{json .Os}},{{json .Architecture}}]')
    try:
        image_id, references, revision, os_name, architecture = json.loads(
            _docker("image", "inspect", "--format", format_string, image))
    except (TypeError, ValueError) as error:
        raise Invalid("local Docker image identity unavailable") from error
    if (not isinstance(image_id, str) or not image_id.startswith("sha256:") or not DIGEST.fullmatch(image_id[7:])
            or not isinstance(references, list) or image not in references
            or (revision, os_name, architecture) != (source, "linux", "amd64")):
        raise Invalid("local Docker image does not match the exact release and platform")
    container = _docker("create", "--pull=never", "--platform=linux/amd64", "--network=none", image_id)
    if not DIGEST.fullmatch(container):
        raise Invalid("local Docker did not return a container identity")
    try:
        _docker("cp", container + ":/fcd", str(output))
    finally:
        _docker("rm", "--volumes", container)


def assemble(repository, source, image, fcd_sha256, rclone, rclone_sha256, output):
    """Extract the selected image's daemon and publish one verified payload."""
    _validate_pins(source, image, fcd_sha256, rclone_sha256)
    with tempfile.TemporaryDirectory(prefix="firecontroller-package-image-") as directory:
        fcd = Path(directory) / "fcd"
        _extract_fcd(image, source, fcd)
        executables = {"fcd": (fcd, fcd_sha256), "rclone": (Path(rclone), rclone_sha256)}
        return _assemble_payload(repository, source, image, executables, Path(output))


def _assemble_payload(repository, source, image, executables, output):
    try:
        pins = {role: platform_prerequisites.executable_pin(str(path.absolute()), digest)
                for role, (path, digest) in executables.items()}
    except ValueError as error:
        raise Invalid(str(error)) from error
    try:
        helpers, _ = deploy_helper_package.assemble(repository, source)
    except (OSError, subprocess.SubprocessError) as error:
        raise Invalid("deployment helpers unavailable at the exact source revision") from error
    manifest = {
        "schema": 1, "source": source, "image": image, "platform": "linux/amd64",
        "sha256": {**{role: digest for role, (_, digest) in executables.items()},
                   HELPERS: hashlib.sha256(helpers).hexdigest()},
    }
    manifest_bytes = (json.dumps(manifest, sort_keys=True, separators=(",", ":")) + "\n").encode("ascii")
    try:
        descriptor, name = tempfile.mkstemp(prefix="." + output.name + ".", dir=output.parent)
        temporary = Path(name)
        try:
            with os.fdopen(descriptor, "w+b") as stream:
                with tarfile.open(fileobj=stream, mode="w", format=tarfile.USTAR_FORMAT) as archive:
                    _add_member(archive, MANIFEST, io.BytesIO(manifest_bytes), len(manifest_bytes))
                    for role, (path, _) in executables.items():
                        with _open_regular(path) as file:
                            _add_member(archive, role, file, pins[role]["size"])
                    _add_member(archive, HELPERS, io.BytesIO(helpers), len(helpers))
                stream.flush()
                os.fchmod(stream.fileno(), 0o444)
                os.fsync(stream.fileno())
                stream.seek(0)
                digest = _sha256(stream)
                stream.seek(0)
                receipt = _verify_payload(stream, digest, source, image)
            os.link(temporary, output, follow_symlinks=False)
            parent = os.open(output.parent, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(parent)
            finally:
                os.close(parent)
            return receipt
        finally:
            temporary.unlink(missing_ok=True)
    except (OSError, tarfile.TarError) as error:
        raise Invalid("cannot publish generation package at the requested path") from error


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    build = commands.add_parser("assemble")
    check = commands.add_parser("verify")
    for command in (build, check):
        command.add_argument("--source", required=True)
        command.add_argument("--image", required=True)
    for flag in ("repository", "fcd-sha256", "rclone", "rclone-sha256", "output"):
        build.add_argument("--" + flag, required=True)
    check.add_argument("--package", required=True)
    check.add_argument("--package-sha256", required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == "assemble":
            result = assemble(args.repository, args.source, args.image, args.fcd_sha256,
                              args.rclone, args.rclone_sha256, args.output)
        else:
            result = verify(args.package, args.package_sha256, args.source, args.image)
    except Invalid as error:
        parser.exit(1, "generation package: " + str(error) + "\n")
    print(json.dumps(result, sort_keys=True))


if __name__ == "__main__":
    main()
