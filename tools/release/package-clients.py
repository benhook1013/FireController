#!/usr/bin/env python3
"""Package native binaries and install release-matched client skills."""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import posixpath
import re
import stat
import subprocess
import sys
import tarfile
import tempfile
from urllib.parse import quote, unquote, urlsplit, urlunsplit


REPOSITORY = "https://github.com/benhook1013/FireController"
OPERATIONAL_SKILLS = ("firecontroller-use", "firecontroller-reviews")
SKILL_ROOTS = {
    "claude-code": Path(".claude/skills"),
    "codex": Path(".agents/skills"),
    "codex-legacy": Path(".codex/skills"),
}



def artifact_lease_options():
    """Preserve an optional local managed-runner lease; never infer remote custody."""
    raw = os.environ.get("FC_ARTIFACT_LEASE_FD")
    if raw is None:
        return {}
    if os.name != "posix" or not re.fullmatch(r"[0-9]{1,10}", raw):
        raise ValueError("invalid inherited artifact lease")
    fd = int(raw)
    if fd < 3:
        raise ValueError("invalid inherited artifact lease")
    try:
        os.fstat(fd)
    except (OSError, OverflowError):
        raise ValueError("unavailable inherited artifact lease") from None
    return {"pass_fds": (fd,)}


def installed_skill_guidance(files, revision):
    """Generate portable skill-local docs from this archive's canonical docs."""
    # The MCP build input lives beside the skill, but is not an installed resource.
    result = {name: body for name, body in files.items()
              if name != "skills/firecontroller-use/mcp_guidance.go"}
    skills = sorted({name.split("/")[1] for name in files
                     if re.fullmatch(r"skills/[^/]+/SKILL\.md", name)})

    def rewrite(body, source, destination, skill):
        def link(value):
            parsed = urlsplit(value)
            if parsed.scheme or parsed.netloc:
                prefix = REPOSITORY + "/blob/"
                if not value.startswith(prefix):
                    return value
                branch, separator, path = parsed.path.removeprefix(
                    "/benhook1013/FireController/blob/").partition("/")
                if not separator or branch not in {"main", "develop", revision}:
                    return value
                target = unquote(path)
            elif parsed.path:
                target = posixpath.normpath(posixpath.join(
                    posixpath.dirname(source), unquote(parsed.path)))
            else:
                return value
            if target.startswith("skills/") and target not in files:
                raise ValueError("skill guidance links to a missing bundled skill: " + target)
            if target == "docs" or target.startswith("docs/"):
                if target not in files and not any(name.startswith(target.rstrip("/") + "/") for name in files):
                    raise ValueError("skill guidance links to missing release documentation: " + target)
                output = "skills/" + skill + "/references/" + target
            elif target.startswith("skills/") and not (
                    skill in OPERATIONAL_SKILLS and target.startswith("skills/firecontroller-onboard/")):
                output = target
            else:
                # Pin source references and optional onboarding to the release.
                return urlunsplit(("https", "github.com", "/benhook1013/FireController/blob/" + revision + "/" + quote(target, safe="/"), parsed.query, parsed.fragment))
            relative = posixpath.relpath(output, posixpath.dirname(destination))
            return urlunsplit(("", "", quote(relative, safe="/"), parsed.query, parsed.fragment))

        def replace(match):
            raw = match.group(2)
            angled = raw.startswith("<") and raw.endswith(">")
            changed = link(raw[1:-1] if angled else raw)
            return match.group(1) + ("<" + changed + ">" if angled else changed)

        text = body.decode("utf-8")
        # Public guidance uses ordinary inline links and reference definitions;
        # leave labels, headings, prose and fragments untouched.
        text = re.sub(r"(\]\(\s*)(<[^>\n]+>|[^\s)]+)", replace, text)
        text = re.sub(r"(?m)^(\s{0,3}\[[^]\n]+\]:\s*)(<[^>\n]+>|\S+)", replace, text)
        return text.encode("utf-8")

    for skill in skills:
        for source, body in files.items():
            if source.startswith("skills/" + skill + "/") and source.endswith(".md"):
                result[source] = rewrite(body, source, source, skill)
            if source.startswith("docs/"):
                destination = "skills/" + skill + "/references/" + source
                if destination in files:
                    raise ValueError("generated skill documentation conflicts with a maintained file")
                result[destination] = rewrite(body, source, destination, skill) if source.endswith(".md") else body
    return result


def package(binary_dir, platform, destination, revision):
    artifact_lease_options()  # Refuse invalid enrollment before packaging writes.
    root = Path(__file__).resolve().parents[2]
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise ValueError("revision must be a full Git commit")
    if not re.fullmatch(r"(linux|darwin|windows)-(amd64|arm64)", platform):
        raise ValueError("unsupported platform")
    actual = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=root, text=True, **artifact_lease_options()).strip()
    if actual != revision:
        raise ValueError("package revision does not match this checkout")
    suffix = ".exe" if platform.startswith("windows-") else ""
    binaries = {}
    for name in ("fc", "fcd", "fcmcp"):
        path = Path(binary_dir) / (name + suffix)
        metadata = subprocess.check_output(["go", "version", "-m", str(path)], text=True, **artifact_lease_options())
        if "vcs.revision=" + revision + "\n" not in metadata or "vcs.modified=false\n" not in metadata:
            raise ValueError("binary does not identify a clean build of the selected revision: " + name)
        binaries[name + suffix] = path.read_bytes()
    public = subprocess.check_output(["git", "archive", revision, "LICENSE", "docs", "skills"], cwd=root, **artifact_lease_options())
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
            files = {}
            for info in source:
                if not (info.isdir() or info.isfile()):
                    raise ValueError("public guidance contains a non-regular member")
                if info.isfile():
                    files[info.name] = source.extractfile(info).read()
            generated = installed_skill_guidance(files, revision)
            for name, body in generated.items():
                info = source.getmember(name) if name in files else tarfile.TarInfo(name)
                if name not in files:
                    info.mode = 0o644
                info.size = len(body)
                out.addfile(info, io.BytesIO(body))
    checksum = hashlib.sha256(archive.read_bytes()).hexdigest()
    archive.with_name(archive.name + ".sha256").write_text(checksum + "  " + archive.name + "\n")
    print(archive)


def release_skill_files(archive, checksum, revision):
    """Read only the two operational skills from the exact verified bundle."""
    if not re.fullmatch(r"[0-9a-f]{64}", checksum) or not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise ValueError("provide the exact archive SHA-256 and full source revision")
    files = {}
    seen = set()
    source = None
    verified = Path(archive).read_bytes()
    if hashlib.sha256(verified).hexdigest() != checksum:
        raise ValueError("client archive SHA-256 mismatch")
    with tarfile.open(fileobj=io.BytesIO(verified), mode="r|gz") as bundle:
        for member in bundle:
            if member.name == "source.json":
                if source is not None or not member.isfile():
                    raise ValueError("invalid client bundle source manifest")
                source = json.load(bundle.extractfile(member))
                if not isinstance(source, dict) or source.get("source_revision") != revision:
                    raise ValueError("client bundle source revision mismatch")
                continue
            parts = PurePosixPath(member.name).parts
            if len(parts) < 2 or parts[0] != "skills" or parts[1] not in OPERATIONAL_SKILLS:
                continue
            if (len(parts) < 3 or not member.isfile() or member.name != "/".join(parts)
                    or any(part.endswith((".", " ")) or re.search(r'[\\:<>|?*"]', part) for part in parts)):
                raise ValueError("unexpected member in client skill: " + member.name)
            key = member.name.casefold()
            if key in seen:
                raise ValueError("duplicate member in client skill: " + member.name)
            seen.add(key)
            files[Path(*parts[1:])] = bundle.extractfile(member).read()
    if source is None:
        raise ValueError("client bundle source manifest is missing")
    for relative in files:
        if any(parent.as_posix().casefold() in seen
               for parent in (Path("skills") / relative).parents):
            raise ValueError("a skill file is also a directory: " + str(relative))
    for skill in OPERATIONAL_SKILLS:
        if Path(skill, "SKILL.md") not in files:
            raise ValueError("client bundle skill is missing: " + skill)
    return files


def path_kind(path):
    try:
        info = path.lstat()
    except FileNotFoundError:
        return "missing"
    if stat.S_ISLNK(info.st_mode):
        return "link"
    if getattr(info, "st_file_attributes", 0) & getattr(stat, "FILE_ATTRIBUTE_REPARSE_POINT", 0):
        return "special"
    if stat.S_ISDIR(info.st_mode):
        return "directory"
    if stat.S_ISREG(info.st_mode):
        return "file"
    return "special"


def check_parents(root, relative):
    directory = root
    for component in relative.parent.parts:
        directory /= component
        if path_kind(directory) not in {"missing", "directory"}:
            raise ValueError("skill destination crosses a non-directory or link: " + str(directory))


def generation_matches(generation, revision):
    manifest = generation / "source.json"
    if path_kind(manifest) != "file":
        return False
    source = json.loads(manifest.read_text(encoding="utf-8"))
    return isinstance(source, dict) and source.get("source_revision") == revision


def skill_link_target(entry):
    target = os.path.abspath(entry.parent / entry.readlink())
    if os.name == "nt":
        # readlink can return the Windows extended-length substitution path.
        if target.startswith("\\\\?\\UNC\\"):
            target = "\\\\" + target[8:]
        elif target.startswith("\\\\?\\"):
            target = target[4:]
    return Path(target)


def existing_managed_link(entry, current):
    """Accept only the selected current link or its positively identified predecessor."""
    target = skill_link_target(entry)
    if target == current / "skills" / entry.name:
        return True
    generation = target.parent.parent
    return (
        target == generation / "skills" / entry.name
        and generation.parent == current.parent
        and re.fullmatch(r"[0-9a-f]{40}", generation.name) is not None
        and path_kind(generation) == "directory"
        and path_kind(generation / "skills") == "directory"
        and path_kind(target) == "directory"
        and generation_matches(generation, generation.name)
    )


def install_skills(archive, checksum, revision, client, scope_root, *, managed_current=None, apply=False):
    files = release_skill_files(archive, checksum, revision)
    root = Path(scope_root).resolve(strict=True)
    if not root.is_dir():
        raise ValueError("the selected client scope root must be a directory")
    layout = SKILL_ROOTS[client]
    destination = root / layout
    actions = []

    if managed_current is not None:
        current = Path(os.path.abspath(managed_current))
        generation = current.resolve(strict=True)
        if not generation_matches(generation, revision):
            raise ValueError("managed current path does not select the requested source revision")
        for relative, body in files.items():
            relative = Path("skills") / relative
            check_parents(generation, relative)
            installed = generation / relative
            if path_kind(installed) != "file" or installed.read_bytes() != body:
                raise ValueError("managed generation differs from the verified bundle: " + str(relative))

    for skill in OPERATIONAL_SKILLS:
        entry = destination / skill
        check_parents(root, layout / skill)
        kind = path_kind(entry)
        if managed_current is not None:
            target = current / "skills" / skill
            if kind == "link" and existing_managed_link(entry, current):
                if skill_link_target(entry) != target:
                    actions.append(("link", entry, target))
            elif kind == "missing":
                actions.append(("link", entry, target))
            else:
                raise ValueError("managed skill destination is occupied: " + str(entry))
            continue
        if kind not in {"missing", "directory"}:
            raise ValueError("skill destination is occupied: " + str(entry))
        manifest = entry / "SKILL.md"
        has_installed_manifest = path_kind(manifest) == "file"
        if has_installed_manifest:
            header = re.match(r"\A---[ \t]*\n(.*?)^---[ \t]*(?:\n|$)", manifest.read_text(encoding="utf-8"), re.M | re.S)
            if header is None or not re.search(r"(?m)^name:[ \t]*" + re.escape(skill) + r"[ \t]*$", header[1]):
                raise ValueError("skill manifest belongs to a different skill: " + str(manifest))
        for relative, body in files.items():
            if relative.parts[0] != skill:
                continue
            check_parents(root, layout / relative)
            path = destination / relative
            kind = path_kind(path)
            if kind not in {"missing", "file"}:
                raise ValueError("skill file destination is occupied: " + str(path))
            if kind == "file":
                if path.read_bytes() == body:
                    continue
                if not has_installed_manifest:
                    raise ValueError("existing skill file has no matching installed manifest: " + str(path))
            actions.append(("copy", path, body))

    for action, path, payload in actions:
        if action == "link" or path.name == "SKILL.md":
            print(f"{action} {path}")
        if not apply:
            continue
        check_parents(root, path.relative_to(root))
        path.parent.mkdir(parents=True, exist_ok=True)
        # Replace one checked entry atomically. Leave unrelated paths alone.
        with tempfile.TemporaryDirectory(prefix=".firecontroller-skill-", dir=path.parent) as work:
            replacement = Path(work) / path.name
            if action == "link":
                if current.resolve(strict=True) != generation:
                    raise ValueError("managed current generation changed during installation")
                kind = path_kind(path)
                if kind != "missing" and not (kind == "link" and existing_managed_link(path, current)):
                    raise ValueError("managed skill destination changed: " + str(path))
                replacement.symlink_to(payload, target_is_directory=True)
            else:
                if path_kind(path) not in {"missing", "file"}:
                    raise ValueError("skill file destination changed: " + str(path))
                replacement.write_bytes(payload)
            os.replace(replacement, path)
    if managed_current is not None and current.resolve(strict=True) != generation:
        raise ValueError("managed current generation changed during installation")
    print(f"{'installed' if apply else 'planned'} {len(actions)} changes for {revision} in {destination}")


def install_skill_command(arguments):
    parser = argparse.ArgumentParser(
        prog="package-clients.py install-skills",
        description="Install the operational skills from one verified client bundle.",
    )
    parser.add_argument("--archive", type=Path, required=True)
    parser.add_argument("--sha256", required=True)
    parser.add_argument("--source", required=True)
    parser.add_argument("--client", choices=SKILL_ROOTS, required=True)
    parser.add_argument("--scope-root", type=Path, required=True, help="explicit user home or repository root")
    parser.add_argument("--managed-current", type=Path, help="existing active generation pointer for managed links")
    parser.add_argument("--apply", action="store_true", help="apply changes after checking all destinations (default is dry run)")
    parsed = parser.parse_args(arguments)
    try:
        install_skills(parsed.archive, parsed.sha256, parsed.source, parsed.client, parsed.scope_root,
                       managed_current=parsed.managed_current, apply=parsed.apply)
    except (OSError, ValueError, tarfile.TarError) as error:
        parser.exit(1, f"install-skills: {error}\n")


if __name__ == "__main__":
    if sys.argv[1:2] == ["install-skills"]:
        install_skill_command(sys.argv[2:])
    elif len(sys.argv) == 5:
        package(*sys.argv[1:])
    else:
        raise SystemExit("usage: package-clients.py BIN_DIR PLATFORM OUTPUT_DIR REVISION or package-clients.py install-skills --help")
