#!/usr/bin/env python3
"""Deploy the pushed develop commit to the FireController host. One command, same steps every time.

    python3 tools/deploy/deploy.py            # deploy origin/develop
    python3 tools/deploy/deploy.py --dry-run  # build and check only

Build and verify the exact image and installation payload from origin/develop.
The host verifies the package before starting the guarded rollout.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tempfile

HERE = Path(__file__).resolve().parent
REPO = HERE.parent.parent
HOST = "root@77.42.29.156"
REMOTE_ROOT = "/root/firecontroller-deploy"
INSTALL = REPO / "tools" / "install"
CONTEXT = ("Dockerfile", ".dockerignore", "go.mod", "go.sum", "LICENSE", "cmd", "internal")
RECEIVER = (
    "generation_package.py",
    "deploy_helper_consumer.py",
    "deploy_helper_package.py",
    "platform_prerequisites.py",
    "lifecycle_package.py",
)


def run(args, **kw):
    return subprocess.run(args, check=True, text=True, **kw)


def out(args, **kw):
    return run(args, capture_output=True, **kw).stdout.strip()


def require_clean_develop():
    run(["git", "-C", str(REPO), "fetch", "-q", "origin", "develop"])
    if out(["git", "-C", str(REPO), "status", "--porcelain", "--untracked-files=no"]):
        raise SystemExit("refusing: tracked changes in the checkout")
    head = out(["git", "-C", str(REPO), "rev-parse", "HEAD"])
    if head != out(["git", "-C", str(REPO), "rev-parse", "origin/develop"]):
        raise SystemExit("refusing: HEAD is not origin/develop; push first")
    return head


def parse_digest(listing, name):
    """Return the manifest digest of `name` from `ctr images ls` output."""
    for line in listing.splitlines()[1:]:
        cols = line.split()
        if len(cols) >= 3 and cols[0] == name and re.fullmatch(r"sha256:[0-9a-f]{64}", cols[2]):
            return cols[2]
    raise SystemExit("imported image not found on host: " + name)


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--host", default=HOST)
    p.add_argument("--dry-run", action="store_true", help="build and verify the image and package locally")
    p.add_argument("--drain-seconds", type=int, default=600)
    p.add_argument("--recover-crashloop", action="store_true", help="replace a verified offline, crash-looping Controller")
    p.add_argument("--memory-limit", default="1536Mi", help="controller container memory limit, applied with every rollout")
    p.add_argument("--retention-mode", choices=["dry-run", "enabled"], help="explicitly set database retention mode on the live Controller; omit to preserve current mode")
    p.add_argument("--remote-mcp-mode", choices=["enable", "disable"], help="explicit MCP opt-in; enable only after staging the exact proxy location; omit to preserve current args")
    a = p.parse_args(argv)

    source = require_clean_develop()
    tag = "localhost/firecontroller:" + source
    print("building", tag, flush=True)
    sys.path.insert(0, str(HERE))
    from browser_asset_binding import source_binding
    with tempfile.TemporaryDirectory(prefix="fc-deploy-") as tmp:
        tmp = Path(tmp)
        metadata = tmp / "image.json"
        context = tmp / "context.tar"
        config = tmp / "docker-config"
        config.mkdir()
        git = ["git", "--no-replace-objects", "-C", str(REPO)]
        run(git + ["archive", "--output", str(context), source, *CONTEXT])
        with context.open("rb") as stream:
            run(["docker", "buildx", "build", "-q", "--platform", "linux/amd64", "--load", "--provenance=false",
                 "--metadata-file", str(metadata), "-t", tag, "--label", "org.opencontainers.image.revision=" + source,
                 "--label", "org.opencontainers.image.source=https://github.com/benhook1013/FireController", "-"],
                stdin=stream, stdout=subprocess.DEVNULL, env={**os.environ, "DOCKER_CONFIG": str(config)})
        digest = json.loads(metadata.read_text()).get("containerimage.digest", "")
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
            raise SystemExit("build did not report an immutable image digest")
        image = "localhost/firecontroller@" + digest
        cid = out(["docker", "create", "--pull=never", "--network=none", image])
        try:
            run(["docker", "cp", cid + ":/fcd", str(tmp / "fcd")])
        finally:
            run(["docker", "rm", "--volumes", cid], stdout=subprocess.DEVNULL)
        fcd_sha = hashlib.sha256((tmp / "fcd").read_bytes()).hexdigest()
        (tmp / "assets.json").write_text(json.dumps(source_binding(REPO, source)))
        rclone_sha = out([sys.executable, "-I", str(INSTALL / "qualified_rclone.py"), str(tmp / "rclone")])
        package = tmp / "generation.tar"
        package_args = ["--source", source, "--image", image]
        receipt = json.loads(out([sys.executable, "-I", str(INSTALL / "generation_package.py"),
                                  "assemble", "--repository", str(REPO), *package_args, "--fcd-sha256", fcd_sha,
                                  "--rclone", str(tmp / "rclone"), "--rclone-sha256", rclone_sha,
                                  "--output", str(package)]))
        package_sha = receipt.get("package_sha256", "")
        if not re.fullmatch(r"[0-9a-f]{64}", package_sha):
            raise SystemExit("generation package did not report an immutable digest")
        print("generation package sha256", package_sha, flush=True)
        print("fcd sha256", fcd_sha, flush=True)
        if a.dry_run:
            run([sys.executable, "-I", str(INSTALL / "generation_package.py"), "materialize", *package_args,
                 "--package", str(package), "--package-sha256", package_sha, "--output", str(tmp / "payload")])
            print("dry run: built and verified image and generation package")
            return 0

        ssh = ["ssh", "-o", "BatchMode=yes", a.host]
        run(ssh + ["install -d -m 0700 " + shlex.quote(REMOTE_ROOT)])
        remote_prefix = REMOTE_ROOT + "/" + source + "."
        remote = out(ssh + ["mktemp -d " + shlex.quote(remote_prefix + "XXXXXXXX")])
        if not re.fullmatch(re.escape(remote_prefix) + r"[A-Za-z0-9]{8}", remote):
            raise SystemExit("host did not return the expected release staging directory")
        payload = remote + "/payload"
        for filename in RECEIVER:
            with (tmp / filename).open("wb") as stream:
                run(git + ["show", source + ":tools/install/" + filename], stdout=stream)
        run(["scp", "-q", "-o", "BatchMode=yes"] + [str(tmp / f) for f in RECEIVER]
            + [str(package), str(tmp / "assets.json"), a.host + ":" + remote + "/"])
        materialize = ["python3", "-I", remote + "/generation_package.py", "materialize", *package_args,
                       "--package", remote + "/generation.tar", "--package-sha256", package_sha, "--output", payload]
        run(ssh + [shlex.join(materialize)])

        print("importing image on host", flush=True)
        save = subprocess.Popen(["docker", "save", tag], stdout=subprocess.PIPE)
        try:
            run(ssh + ["k3s ctr -n k8s.io images import -"], stdin=save.stdout, stdout=subprocess.DEVNULL)
        finally:
            save.stdout.close()
            save_code = save.wait()
        if save_code != 0:
            raise SystemExit("docker save failed")
        imported = parse_digest(out(ssh + ["k3s ctr -n k8s.io images ls name==" + shlex.quote(tag)]), tag)
        if imported != digest:
            raise SystemExit("host image digest differs from the verified generation package")
        # An imported OCI index gets no CRI repo digest from its tag alone; name
        # the digest reference explicitly so the deployment can select it.
        run(ssh + ["k3s ctr -n k8s.io images tag --force " + shlex.quote(tag) + " " + shlex.quote(image)], stdout=subprocess.DEVNULL)

        host_cmd = ["python3", "-I", payload + "/deploy_host.py", "--source", source, "--image", image,
                    "--fcd", payload + "/fcd", "--fcd-sha256", fcd_sha, "--assets", remote + "/assets.json",
                    "--drain-seconds", str(a.drain_seconds), "--memory-limit", a.memory_limit]
        if a.retention_mode is not None:
            host_cmd.extend(["--retention-mode", a.retention_mode])
        if a.remote_mcp_mode is not None:
            host_cmd.extend(["--remote-mcp-mode", a.remote_mcp_mode])
        if a.recover_crashloop:
            host_cmd.append("--recover-crashloop")
        return subprocess.run(ssh + [shlex.join(host_cmd)]).returncode


if __name__ == "__main__":
    sys.exit(main())
