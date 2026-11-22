#!/usr/bin/env python3
"""Select and launch one source-bound FireController backup-helper generation.

The systemd drop-ins invoke this stable launcher with the original ExecStart
argv encoded as data. Each fresh process takes shared custody of the stable
generation lease before reading the private selector, checks the selected
immutable fcd, substitutes only the known fcd/hash fields, and replaces itself
with the original executable (without a shell), retaining its lease through
process exit.
"""

import argparse
import base64
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys
import tempfile

SOURCE = re.compile(r"[0-9a-f]{40}\Z")
SHA256 = re.compile(r"[0-9a-f]{64}\Z")
IMAGE_REFERENCE = re.compile(r"localhost/[a-z0-9._/-]+@sha256:[0-9a-f]{64}\Z")
HEALTH_SCRIPT = re.compile(r"publish-backup-health\.py\Z")
RELEASE_REFERENCE = re.compile(r"/releases/([0-9a-f]{40})/")
SYSTEMD_FORBIDDEN = set("'\"\\$%")
LEASE_MODE = 0o640
MANAGER_FENCE_CONTENT = b"firecontroller-helper-manager-fenced-v1\n"


class GenerationError(RuntimeError):
    """A malformed selector or an unsupported systemd launch shape."""


def _check_selector_directory(path):
    directory = Path(path).parent
    try:
        info = directory.lstat()
    except OSError as error:
        raise GenerationError("generation selector directory is unavailable") from error
    if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode) or info.st_mode & 0o022:
        raise GenerationError("generation selector directory must be trusted and not group/world writable")


def lease_path(selector):
    """Derive the stable launch lease beside the private generation selector."""
    return Path(selector).with_name(Path(selector).name + ".lease")


def manager_fence_path(selector):
    """Return the private record proving first-conversion systemd reconciliation."""
    return Path(selector).with_name(Path(selector).name + ".manager-fenced")


def _phase_lock_info(path):
    path = Path(path)
    try:
        info = path.lstat()
    except OSError as error:
        raise GenerationError("existing backup phase lock is unavailable") from error
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise GenerationError("backup phase lock must be a regular non-symlink file")
    return info


def _validate_lease_info(info, path, phase_gid=None):
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise GenerationError("helper generation lease must be a regular non-symlink file")
    if stat.S_IMODE(info.st_mode) != LEASE_MODE:
        raise GenerationError("helper generation lease must grant private owner-write and group-read access")
    if phase_gid is not None and info.st_gid != phase_gid:
        raise GenerationError("helper generation lease group differs from its selector or phase lock")


def _selector_group(selector):
    path = Path(selector)
    try:
        info = path.lstat()
    except OSError as error:
        raise GenerationError("generation selector is unavailable") from error
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise GenerationError("generation selector must be a regular non-symlink file")
    mode = stat.S_IMODE(info.st_mode)
    if mode & 0o222 or mode & 0o007 or not mode & 0o440:
        raise GenerationError("generation selector must be private and read-only")
    return info.st_gid


def validate_generation_lease(selector, phase_gid=None):
    """Fail closed unless the stable lease has its private, expected file shape."""
    path = lease_path(selector)
    _check_selector_directory(path)
    try:
        info = path.lstat()
    except OSError as error:
        raise GenerationError("helper generation lease is missing: " + str(path)) from error
    _validate_lease_info(info, path, phase_gid)
    return path


def ensure_generation_lease(selector, phase_lock):
    """Provision the stable lease once; an existing inode is never replaced."""
    path = lease_path(selector)
    _check_selector_directory(path)
    phase = _phase_lock_info(phase_lock)
    if not path.exists() and not path.is_symlink():
        fence_path = manager_fence_path(selector)
        if fence_path.exists() or fence_path.is_symlink():
            raise GenerationError("fenced helper installation is missing its stable lease; refusing to recreate it")
    try:
        fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), LEASE_MODE)
    except FileExistsError:
        validate_generation_lease(selector, phase.st_gid)
        return path
    except OSError as error:
        raise GenerationError("could not provision helper generation lease: " + str(error)) from error
    try:
        os.fchown(fd, os.geteuid(), phase.st_gid)
        os.fchmod(fd, LEASE_MODE)
        os.fsync(fd)
        info = os.fstat(fd)
        _validate_lease_info(info, path, phase.st_gid)
        directory = os.open(path.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    except BaseException:
        os.close(fd)
        # Do not unlink or replace a possibly observed lease inode. A partial
        # provisioning failure is deliberately an operator reconciliation case.
        raise
    os.close(fd)
    return path


def _acquire_generation_lease(selector, exclusive, phase_gid=None):
    path = validate_generation_lease(selector, phase_gid)
    flags = os.O_RDWR if exclusive else os.O_RDONLY
    flags |= getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        fd = os.open(path, flags)
        info = os.fstat(fd)
        _validate_lease_info(info, path, phase_gid)
        named = path.lstat()
        if (info.st_dev, info.st_ino) != (named.st_dev, named.st_ino):
            raise GenerationError("helper generation lease inode changed while opening")
        fcntl.flock(fd, fcntl.LOCK_EX if exclusive else fcntl.LOCK_SH)
        selector_gid = _selector_group(selector)
        if phase_gid is not None and selector_gid != phase_gid:
            raise GenerationError("generation selector group differs from the backup phase lock")
        _validate_lease_info(info, path, selector_gid if phase_gid is None else phase_gid)
        # Keep the exact stable inode open through exec so deploy/rollback
        # cannot pass an already-started helper before that process exits.
        if not exclusive:
            os.set_inheritable(fd, True)
        return fd
    except BaseException:
        if "fd" in locals():
            os.close(fd)
        raise


def acquire_generation_shared(selector):
    """Take shared custody before selector access; caller retains fd through exec."""
    return _acquire_generation_lease(selector, exclusive=False)


def acquire_generation_exclusive(selector, phase_gid=None):
    """Take exclusive custody on the stable generation inode."""
    return _acquire_generation_lease(selector, exclusive=True, phase_gid=phase_gid)


def manager_fenced(selector, phase_lock):
    """Validate the durable first-conversion fence, if one has been recorded."""
    path = manager_fence_path(selector)
    _check_selector_directory(path)
    try:
        info = path.lstat()
    except FileNotFoundError:
        return False
    except OSError as error:
        raise GenerationError("helper manager fence cannot be read") from error
    phase = _phase_lock_info(phase_lock)
    if (not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode)
            or stat.S_IMODE(info.st_mode) != 0o440 or info.st_gid != phase.st_gid):
        raise GenerationError("helper manager fence has invalid private permissions or group")
    try:
        content = path.read_bytes()
    except OSError as error:
        raise GenerationError("helper manager fence cannot be read") from error
    if content != MANAGER_FENCE_CONTENT:
        raise GenerationError("helper manager fence content is invalid")
    return True


def write_manager_fence(selector, phase_lock):
    """Create, but never replace, the first-conversion systemd fence record."""
    path = manager_fence_path(selector)
    _check_selector_directory(path)
    phase = _phase_lock_info(phase_lock)
    try:
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o440)
    except FileExistsError:
        if not manager_fenced(selector, phase_lock):
            raise GenerationError("helper manager fence disappeared during creation")
        return
    except OSError as error:
        raise GenerationError("could not create helper manager fence: " + str(error)) from error
    try:
        os.fchown(fd, os.geteuid(), phase.st_gid)
        os.fchmod(fd, 0o440)
        with os.fdopen(fd, "wb") as stream:
            fd = -1
            stream.write(MANAGER_FENCE_CONTENT)
            stream.flush()
            os.fsync(stream.fileno())
        directory = os.open(path.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    except BaseException:
        if fd >= 0:
            os.close(fd)
        raise
    if not manager_fenced(selector, phase_lock):
        raise GenerationError("helper manager fence was not retained")


def sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _generation(value, label):
    if not isinstance(value, dict) or set(value) != {"source", "sha256"}:
        raise GenerationError(label + " generation must contain only source and sha256")
    source = value["source"]
    digest = value["sha256"]
    if not isinstance(source, str) or SOURCE.fullmatch(source) is None:
        raise GenerationError(label + " source is malformed")
    if not isinstance(digest, str) or SHA256.fullmatch(digest) is None:
        raise GenerationError(label + " sha256 is malformed")
    return {"source": source, "sha256": digest}


def make_record(current, previous=None, pending=None):
    if pending is not None:
        if not isinstance(pending, dict) or set(pending) != {"candidate", "image", "old_image"}:
            raise GenerationError("pending helper rollout has an unsupported shape")
        if not all(isinstance(pending[key], str) and IMAGE_REFERENCE.fullmatch(pending[key])
                   for key in ("image", "old_image")):
            raise GenerationError("pending helper rollout must bind both exact images")
        pending = {**pending, "candidate": _generation(pending["candidate"], "candidate")}
    record = {
        "version": 1,
        "current": _generation(current, "current"),
        "previous": None if previous is None else _generation(previous, "previous"),
        "pending": pending,
    }
    return record


def _decode_record(data):
    def unique_object(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise GenerationError("generation record contains a duplicate key")
            result[key] = value
        return result

    try:
        value = json.loads(data, object_pairs_hook=unique_object)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise GenerationError("generation record is not valid UTF-8 JSON") from error
    if not isinstance(value, dict) or set(value) != {"version", "current", "previous", "pending"}:
        raise GenerationError("generation record has an unsupported shape")
    if type(value["version"]) is not int or value["version"] != 1:
        raise GenerationError("generation record version is unsupported")
    return make_record(value["current"], value["previous"], value["pending"])


def read_record(path):
    path = Path(path)
    _check_selector_directory(path)
    try:
        info = path.lstat()
    except OSError as error:
        raise GenerationError("generation selector cannot be read: " + str(path)) from error
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise GenerationError("generation selector must be a regular non-symlink file")
    mode = stat.S_IMODE(info.st_mode)
    if mode & 0o222 or mode & 0o007 or not mode & 0o440:
        raise GenerationError("generation selector must be private and read-only")
    try:
        return _decode_record(path.read_bytes())
    except OSError as error:
        raise GenerationError("generation selector cannot be read: " + str(path)) from error


def _record_bytes(record):
    record = make_record(record["current"], record["previous"], record["pending"])
    return (json.dumps(record, sort_keys=True, separators=(",", ":")) + "\n").encode("ascii")


def write_record(path, current, previous=None, access_from=None, *, pending=None):
    """Atomically replace one selector, preserving its private read audience."""
    path = Path(path)
    _check_selector_directory(path)
    record = make_record(current, previous, pending)
    if path.exists() or path.is_symlink():
        old = path.lstat()
        if not stat.S_ISREG(old.st_mode) or stat.S_ISLNK(old.st_mode):
            raise GenerationError("generation selector must be a regular non-symlink file")
        uid, gid = old.st_uid, old.st_gid
        mode = stat.S_IMODE(old.st_mode) & 0o440
    else:
        if access_from is None:
            raise GenerationError("initial selector needs the existing phase-lock access identity")
        access = Path(access_from).stat()
        # Keep mutation authority with the deploy owner while sharing read-only
        # access with the existing phase-lock group.
        uid, gid = os.geteuid(), access.st_gid
        mode = 0o440
    if mode == 0:
        raise GenerationError("phase lock grants no read access for the private selector")

    fd, name = tempfile.mkstemp(prefix="." + path.name + ".", suffix=".tmp", dir=str(path.parent))
    temporary = Path(name)
    try:
        os.fchown(fd, uid, gid)
        os.fchmod(fd, mode)
        with os.fdopen(fd, "wb") as stream:
            fd = -1
            stream.write(_record_bytes(record))
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    except BaseException:
        if fd >= 0:
            os.close(fd)
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass
        raise
    return record


def verify_generation(generation, releases):
    generation = _generation(generation, "selected")
    root = Path(releases)
    release = root / generation["source"]
    try:
        root_info = root.lstat()
        release_info = release.lstat()
    except OSError as error:
        raise GenerationError("selected helper release is missing: " + generation["source"]) from error
    if not stat.S_ISDIR(root_info.st_mode) or stat.S_ISLNK(root_info.st_mode):
        raise GenerationError("helper releases root must be a regular non-symlink directory")
    if not stat.S_ISDIR(release_info.st_mode) or stat.S_ISLNK(release_info.st_mode):
        raise GenerationError("selected helper source must be a regular non-symlink directory")
    binary = release / "fcd"
    try:
        info = binary.lstat()
    except OSError as error:
        raise GenerationError("selected helper release is missing: " + generation["source"]) from error
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise GenerationError("selected fcd must be a regular non-symlink file")
    if info.st_mode & 0o222:
        raise GenerationError("selected fcd must be immutable to its owner and service group")
    if not info.st_mode & 0o111:
        raise GenerationError("selected fcd is not executable")
    if sha256(binary) != generation["sha256"]:
        raise GenerationError("selected fcd does not match its recorded sha256")
    return binary


def verify_record(record, releases, include_previous=True):
    record = make_record(record["current"], record["previous"], record["pending"])
    verify_generation(record["current"], releases)
    if include_previous and record["previous"] is not None:
        verify_generation(record["previous"], releases)
    return record


def _check_argv(argv):
    if not isinstance(argv, list) or not argv or any(not isinstance(value, str) or "\0" in value for value in argv):
        raise GenerationError("ExecStart argv is malformed")
    if not argv[0].startswith("/"):
        raise GenerationError("ExecStart executable must be an absolute path")
    return argv


def _source_from_fcd(path, releases):
    expected = str(Path(releases)) + "/"
    if not path.startswith(expected):
        raise GenerationError("fcd argument is outside the configured helper releases")
    suffix = path[len(expected):]
    match = re.fullmatch(r"([0-9a-f]{40})/fcd", suffix)
    if match is None:
        raise GenerationError("fcd argument is not an exact source release path")
    return match.group(1)


def validate_template(role, argv, releases):
    """Validate one supported legacy launch and return its old fcd binding."""
    argv = _check_argv(argv)
    releases = str(Path(releases))
    if role == "health":
        if len(argv) < 6 or argv[0] != "/usr/bin/python3":
            raise GenerationError("backup-health ExecStart is not the supported Python launch shape")
        script = Path(argv[1])
        if not script.is_absolute() or script.parent.parent != Path(releases):
            raise GenerationError("backup-health script is outside the configured helper releases")
        if HEALTH_SCRIPT.fullmatch(script.name) is None or SOURCE.fullmatch(script.parent.name) is None:
            raise GenerationError("backup-health script is not a known source release")
        fcd_flags = [index for index, value in enumerate(argv) if value == "--fcd"]
        hash_flags = [index for index, value in enumerate(argv) if value == "--sha256"]
        if len(fcd_flags) != 1 or len(hash_flags) != 1:
            raise GenerationError("backup-health ExecStart must contain exactly one --fcd and --sha256")
        fcd_index, hash_index = fcd_flags[0], hash_flags[0]
        if fcd_index + 1 >= len(argv) or hash_index + 1 >= len(argv):
            raise GenerationError("backup-health ExecStart has an incomplete fcd/hash option")
        source = _source_from_fcd(argv[fcd_index + 1], releases)
        digest = argv[hash_index + 1]
        if SHA256.fullmatch(digest) is None:
            raise GenerationError("backup-health --sha256 value is malformed")
        return {"source": source, "sha256": digest}
    if role == "batch":
        if len(argv) < 4 or argv[1] != "backup-push-batch" or argv[2] != "--config":
            raise GenerationError("offsite-batch ExecStart is not the supported fcd batch launch shape")
        source = _source_from_fcd(argv[0], releases)
        if not argv[3].startswith("/") or "--config" in argv[4:]:
            raise GenerationError("offsite-batch ExecStart must have one absolute --config path")
        return {"source": source, "sha256": None}
    raise GenerationError("unknown helper role: " + str(role))


def _encode_argv(argv):
    encoded = json.dumps(_check_argv(argv), ensure_ascii=True, separators=(",", ":")).encode("ascii")
    return base64.urlsafe_b64encode(encoded).decode("ascii")


def decode_argv(encoded):
    if not isinstance(encoded, str) or len(encoded) > 131072:
        raise GenerationError("encoded ExecStart argv is malformed or too large")
    try:
        raw = base64.b64decode(encoded.encode("ascii"), altchars=b"-_", validate=True)
        argv = json.loads(raw.decode("ascii"))
    except (UnicodeEncodeError, UnicodeDecodeError, ValueError, json.JSONDecodeError) as error:
        raise GenerationError("encoded ExecStart argv is malformed") from error
    return _check_argv(argv)


def systemd_execstart(text):
    """Return the one reset and one command line accepted for these drop-ins."""
    lines = text.splitlines(keepends=True)
    section = None
    resets = []
    commands = []
    for index, raw in enumerate(lines):
        if raw.endswith("\r\n") or "\r" in raw:
            raise GenerationError("CRLF or embedded carriage return in helper drop-in is unsupported")
        line = raw[:-1] if raw.endswith("\n") else raw
        stripped = line.strip()
        if not stripped or stripped.startswith(("#", ";")):
            continue
        if stripped.startswith("[") and stripped.endswith("]"):
            section = stripped[1:-1]
            continue
        directive = re.match(r"^\s*(Exec[A-Za-z]+)(?:\s|=)", line)
        if directive is not None:
            if directive.group(1) != "ExecStart":
                raise GenerationError("helper drop-in has an extra systemd command directive")
            if not line.startswith("ExecStart=") or section != "Service":
                raise GenerationError("helper drop-in has an unsupported ExecStart directive")
            value = line[len("ExecStart="):]
            if value == "":
                resets.append(index)
            else:
                commands.append((index, value))
    if len(resets) != 1 or len(commands) != 1:
        raise GenerationError("helper drop-in must contain one ExecStart reset and one command")
    if resets[0] >= commands[0][0]:
        raise GenerationError("helper drop-in must reset ExecStart before its single command")
    return lines, resets[0], commands[0][0], commands[0][1]


def parse_systemd_argv(command):
    if not command or command != command.strip() or any(character in SYSTEMD_FORBIDDEN for character in command):
        raise GenerationError("ExecStart quoting, expansion, escaping or whitespace is unsupported")
    if any(ord(character) < 0x20 or ord(character) == 0x7f for character in command):
        raise GenerationError("ExecStart contains a control character")
    argv = command.split(" ")
    if any(not value for value in argv):
        raise GenerationError("ExecStart must use single literal spaces between arguments")
    return _check_argv(argv)


def wrapper_argv(role, argv, launcher, selector, releases):
    validate_template(role, argv, releases)
    return ["/usr/bin/python3", str(launcher), "launch", "--role", role,
            "--selector", str(selector), "--releases", str(releases),
            "--argv-b64", _encode_argv(argv)]


def _decode_wrapper(argv, role, launcher, selector, releases):
    expected = ["/usr/bin/python3", str(launcher), "launch", "--role", role,
                "--selector", str(selector), "--releases", str(releases), "--argv-b64"]
    if len(argv) != len(expected) + 1 or argv[:-1] != expected:
        raise GenerationError("helper wrapper does not use the stable shared selector")
    template = decode_argv(argv[-1])
    validate_template(role, template, releases)
    return template


def classify_dropin(text, role, launcher, selector, releases):
    lines, _, command_index, command = systemd_execstart(text)
    argv = parse_systemd_argv(command)
    if len(argv) >= 3 and argv[0] == "/usr/bin/python3" and argv[1] == str(launcher):
        template = _decode_wrapper(argv, role, launcher, selector, releases)
        return {"state": "wrapper", "argv": template}
    validate_template(role, argv, releases)
    return {"state": "legacy", "argv": argv}


def render_dropin(text, role, argv, launcher, selector, releases):
    lines, _, command_index, _ = systemd_execstart(text)
    wrapper = wrapper_argv(role, argv, launcher, selector, releases)
    command = " ".join(wrapper)
    if parse_systemd_argv(command) != wrapper:
        raise GenerationError("stable wrapper paths are not safe literal systemd arguments")
    old = lines[command_index]
    newline = "\n" if old.endswith("\n") else ""
    lines[command_index] = "ExecStart=" + command + newline
    return "".join(lines)


def coherent_template(role, argv, releases):
    binding = validate_template(role, argv, releases)
    source = binding["source"]
    root = Path(releases)
    release = root / source
    try:
        root_info = root.lstat()
        release_info = release.lstat()
        if not stat.S_ISDIR(root_info.st_mode) or stat.S_ISLNK(root_info.st_mode):
            raise GenerationError("helper releases root must be a regular non-symlink directory")
        if not stat.S_ISDIR(release_info.st_mode) or stat.S_ISLNK(release_info.st_mode):
            raise GenerationError("old helper source must be a regular non-symlink directory")
        binary = release / "fcd"
        info = binary.lstat()
    except OSError as error:
        raise GenerationError("old helper release is missing: " + source) from error
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise GenerationError("old fcd must be a regular non-symlink file")
    if info.st_mode & 0o222:
        raise GenerationError("old fcd must be immutable to its owner and service group")
    if not info.st_mode & 0o111:
        raise GenerationError("old fcd is not executable")
    digest = sha256(binary)
    if binding["sha256"] is not None and binding["sha256"] != digest:
        raise GenerationError("backup-health sha256 does not match its old fcd source")
    return {"source": source, "sha256": digest}


def rewrite_argv(role, argv, current, releases):
    """Replace precisely the role's known fcd argument and health digest."""
    binding = validate_template(role, argv, releases)
    current = _generation(current, "selected")
    rewritten = list(argv)
    target = str(Path(releases) / current["source"] / "fcd")
    if role == "health":
        fcd_index = rewritten.index("--fcd")
        hash_index = rewritten.index("--sha256")
        rewritten[fcd_index + 1] = target
        rewritten[hash_index + 1] = current["sha256"]
    else:
        rewritten[0] = target
    # Revalidate the target generation's path shape without comparing it to the
    # original template, whose fcd source is intentionally retained as argv data.
    if role == "health":
        if _source_from_fcd(rewritten[rewritten.index("--fcd") + 1], releases) != current["source"]:
            raise GenerationError("selected health fcd path was not substituted exactly")
        if rewritten[rewritten.index("--sha256") + 1] != current["sha256"]:
            raise GenerationError("selected health sha256 was not substituted exactly")
    elif _source_from_fcd(rewritten[0], releases) != current["source"]:
        raise GenerationError("selected batch fcd path was not substituted exactly")
    return rewritten


def launch(role, selector, releases, encoded_argv):
    # Shared custody starts before selector inspection and survives os.execv.
    # A deploy/rollback holding the exclusive lease can therefore switch only
    # after every launched helper has exited.
    lease_fd = acquire_generation_shared(selector)
    try:
        record = read_record(selector)
        if record["pending"] is not None:
            raise GenerationError("helper launch held until the pending controller rollout is reconciled")
        verify_generation(record["current"], releases)
        template = decode_argv(encoded_argv)
        argv = rewrite_argv(role, template, record["current"], releases)
        os.execv(argv[0], argv)
    except BaseException:
        os.close(lease_fd)
        raise


def referenced_sources(text, launcher, selector, releases):
    """Return release references hidden inside a generated systemd wrapper."""
    if str(launcher) not in text:
        return set()
    _, _, _, command = systemd_execstart(text)
    wrapper = parse_systemd_argv(command)
    if len(wrapper) < 6 or wrapper[0] != "/usr/bin/python3" or wrapper[1] != str(launcher):
        raise GenerationError("drop-in refers to the helper launcher in an unsupported shape")
    if wrapper[3] != "--role" or len(wrapper) < 5:
        raise GenerationError("helper wrapper role is malformed")
    role = wrapper[4]
    template = _decode_wrapper(wrapper, role, launcher, selector, releases)
    return {source for value in template for source in RELEASE_REFERENCE.findall(value)}


def rollback_previous(selector, releases, phase_lock):
    """Select the exact previous fcd under generation-then-phase custody."""
    lock_path = Path(phase_lock)
    phase_info = _phase_lock_info(lock_path)
    lease_fd = acquire_generation_exclusive(selector, phase_info.st_gid)
    try:
        with lock_path.open("r+") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            record = verify_record(read_record(selector), releases)
            if record["pending"] is not None:
                raise GenerationError("reconcile the pending controller rollout before selecting another generation")
            if record["previous"] is None:
                raise GenerationError("there is no retained previous helper generation")
            write_record(selector, record["previous"], record["current"])
    finally:
        os.close(lease_fd)
    return record["previous"]


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    launch_parser = commands.add_parser("launch")
    launch_parser.add_argument("--role", choices=("health", "batch"), required=True)
    launch_parser.add_argument("--selector", required=True, type=Path)
    launch_parser.add_argument("--releases", required=True, type=Path)
    launch_parser.add_argument("--argv-b64", required=True)
    rollback_parser = commands.add_parser("select-previous")
    rollback_parser.add_argument("--selector", required=True, type=Path)
    rollback_parser.add_argument("--releases", required=True, type=Path)
    rollback_parser.add_argument("--phase-lock", required=True, type=Path)
    args = parser.parse_args(argv)
    try:
        if args.command == "launch":
            launch(args.role, args.selector, args.releases, args.argv_b64)
        else:
            selected = rollback_previous(args.selector, args.releases, args.phase_lock)
            print(json.dumps({"source": selected["source"], "sha256": selected["sha256"]}, sort_keys=True))
        return 0
    except (GenerationError, OSError) as error:
        print("helper generation error: " + str(error), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
