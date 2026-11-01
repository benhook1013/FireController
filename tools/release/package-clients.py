#!/usr/bin/env python3
"""Package tested native binaries with the exact revision's public guidance."""
import hashlib
import io
import json
from pathlib import Path
import posixpath
import re
import subprocess
import sys
import tarfile
from urllib.parse import quote, unquote, urlsplit, urlunsplit


REPOSITORY = "https://github.com/benhook1013/FireController"


def installed_skill_guidance(files, revision):
    """Generate portable skill-local docs from this archive's canonical docs."""
    result = dict(files)
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
            if target == "docs" or target.startswith("docs/"):
                if target not in files and not any(name.startswith(target.rstrip("/") + "/") for name in files):
                    raise ValueError("skill guidance links to missing release documentation: " + target)
                output = "skills/" + skill + "/references/" + target
            elif target.startswith("skills/"):
                if target not in files:
                    raise ValueError("skill guidance links to a missing bundled skill: " + target)
                output = target
            else:
                # Source/test references remain accessible at the exact release,
                # without copying application code into an installed skill.
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


if __name__ == "__main__":
    if len(sys.argv) != 5:
        raise SystemExit("usage: package-clients.py BIN_DIR PLATFORM OUTPUT_DIR REVISION")
    package(*sys.argv[1:])
