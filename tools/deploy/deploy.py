#!/usr/bin/env python3
"""Deploy the pushed develop commit to the FireController host. One command, same steps every time.

    python3 tools/deploy/deploy.py            # deploy origin/develop
    python3 tools/deploy/deploy.py --dry-run  # build and check only

Locally: require a clean checkout at origin/develop, build the image from the
repository Dockerfile, extract its fcd and the browser asset binding. Then ship
the image and host script over SSH and run deploy_host.py, which snapshots the
database, rehearses migrations, moves the backup helpers, drains and swaps the
image, and verifies the live service. No per-release operation files.
"""
import argparse
import json
import os
import hashlib
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
SHIPPED = ["deploy_host.py", "kubernetes.py", "browser_asset_binding.py"]


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
    p.add_argument("--dry-run", action="store_true", help="build and extract only; touch nothing remote")
    p.add_argument("--drain-seconds", type=int, default=600)
    a = p.parse_args(argv)

    source = require_clean_develop()
    tag = "localhost/firecontroller:" + source
    print("building", tag, flush=True)
    # Base images are public; an empty client config avoids host credential helpers.
    with tempfile.TemporaryDirectory(prefix="fc-docker-config-") as config:
        run(["docker", "build", "-q", "-t", tag, "--label", "org.opencontainers.image.revision=" + source,
             "--label", "org.opencontainers.image.source=https://github.com/benhook1013/FireController", str(REPO)],
            stdout=subprocess.DEVNULL, env={**os.environ, "DOCKER_CONFIG": config})

    sys.path.insert(0, str(HERE))
    from browser_asset_binding import source_binding
    with tempfile.TemporaryDirectory(prefix="fc-deploy-") as tmp:
        tmp = Path(tmp)
        cid = out(["docker", "create", tag])
        try:
            run(["docker", "cp", cid + ":/fcd", str(tmp / "fcd")])
        finally:
            run(["docker", "rm", cid], stdout=subprocess.DEVNULL)
        fcd_sha = hashlib.sha256((tmp / "fcd").read_bytes()).hexdigest()
        (tmp / "assets.json").write_text(json.dumps(source_binding(REPO, source)))
        print("fcd sha256", fcd_sha, flush=True)
        if a.dry_run:
            print("dry run: built and checked; nothing sent to the host")
            return 0

        remote = REMOTE_ROOT + "/" + source
        ssh = ["ssh", "-o", "BatchMode=yes", a.host]
        run(ssh + ["install -d -m 0700 " + shlex.quote(REMOTE_ROOT) + " " + shlex.quote(remote)])
        run(["scp", "-q", "-o", "BatchMode=yes"] + [str(HERE / f) for f in SHIPPED] + [str(tmp / "fcd"), str(tmp / "assets.json"), a.host + ":" + remote + "/"])

        print("importing image on host", flush=True)
        save = subprocess.Popen(["docker", "save", tag], stdout=subprocess.PIPE)
        run(ssh + ["k3s ctr -n k8s.io images import -"], stdin=save.stdout, stdout=subprocess.DEVNULL)
        save.stdout.close()
        if save.wait() != 0:
            raise SystemExit("docker save failed")
        digest = parse_digest(out(ssh + ["k3s ctr -n k8s.io images ls name==" + shlex.quote(tag)]), tag)
        image = "localhost/firecontroller@" + digest

        host_cmd = ["python3", "-I", remote + "/deploy_host.py", "--source", source, "--image", image,
                    "--fcd", remote + "/fcd", "--fcd-sha256", fcd_sha, "--assets", remote + "/assets.json",
                    "--drain-seconds", str(a.drain_seconds)]
        code = subprocess.run(ssh + [" ".join(shlex.quote(x) for x in host_cmd)]).returncode
        if code == 0:
            # Keep this and the previous deploy bundle; drop older ones.
            prune = ("cd " + REMOTE_ROOT + " && ls -1dt */ | tail -n +3 | xargs -r rm -rf --")
            run(ssh + [prune])
        return code


if __name__ == "__main__":
    sys.exit(main())
