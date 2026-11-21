#!/usr/bin/env python3
"""Host side of the fixed FireController deploy. Run by deploy.py, not by hand.

Steps, all under one deploy lock plus the shared backup phase lock (managed
backups and the offsite batch wait while a deploy holds it):

1. Snapshot the live database (SQLite online backup) as the rollback point.
2. Rehearse the new binary's migrations on a copy of that snapshot.
3. Install the new fcd for the backup helpers and repoint their drop-ins.
4. Drain and replace the deployment image (kubernetes.py), then verify.
"""
import argparse
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.request

HERE = Path(__file__).resolve().parent
DATA_DB = Path("/var/lib/firecontroller/data/firecontroller.sqlite")
SNAPSHOTS = Path("/var/lib/firecontroller/predeploy")
WORK = Path("/var/lib/firecontroller/predeploy-work")
HELPER_RELEASES = Path("/var/lib/firecontroller/operator-offsite-v1/releases")
PHASE_LOCK = Path("/var/lib/firecontroller/operator-offsite-v1/coordination/phase.lock")
DEPLOY_LOCK = Path("/root/firecontroller-deploy/deploy.lock")
INSTANCE_FILE = Path("/root/firecontroller-operations/dot-deploy/instance")
CREDENTIAL = Path("/var/lib/firecontroller/data/bootstrap-admin.token")
URL = "https://controller.firedevops.net"
DROPINS = {
    "backup-health": Path("/etc/systemd/system/firecontroller-backup-health.service.d/50-develop-release.conf"),
    "offsite-batch": Path("/etc/systemd/system/firecontroller-offsite-batch.service.d/60-backup-phase.conf"),
}
KEEP_SNAPSHOTS = 2
KUBECTL = ["k3s", "kubectl", "-n", "firecontroller"]
SHA = re.compile(r"[0-9a-f]{40}")
HEX64 = re.compile(r"[0-9a-f]{64}")


class DeployError(RuntimeError):
    pass


def log(event, **fields):
    print(json.dumps({"time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "event": event, **fields}), flush=True)


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def flock_wait(path, timeout, sleep=time.sleep, clock=time.monotonic):
    """Open an existing lock file and take an exclusive flock, waiting up to timeout."""
    f = open(path, "r+")
    deadline = clock() + timeout
    while True:
        try:
            fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return f
        except BlockingIOError:
            if clock() >= deadline:
                f.close()
                raise DeployError("timed out waiting for " + str(path) + "; nothing was changed")
            sleep(5)


def snapshot(source, root, label, keep=KEEP_SNAPSHOTS):
    """Online-backup the live database into root/<label>/ and keep the newest `keep` snapshots."""
    root.mkdir(mode=0o700, exist_ok=True)
    target_dir = root / label
    target_dir.mkdir(mode=0o700)
    target = target_dir / "firecontroller.sqlite"
    src = sqlite3.connect("file:" + str(source) + "?mode=ro", uri=True)
    dst = sqlite3.connect(str(target))
    try:
        src.backup(dst)
    finally:
        dst.close()
        src.close()
    os.chmod(target, 0o600)
    prune_snapshots(root, keep)
    return target


def prune_snapshots(root, keep):
    dirs = sorted((p for p in root.iterdir() if p.is_dir() and not p.is_symlink()), key=lambda p: p.name)
    for old in dirs[:-keep] if keep > 0 else dirs:
        shutil.rmtree(old)


def rehearse_migration(fcd, snapshot_path, work_root=WORK):
    """Run `fcd migrate` on a private copy of the snapshot, then delete the copy."""
    work_root.mkdir(mode=0o700, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="fc-migrate-", dir=str(work_root)) as work:
        os.chmod(work, 0o700)
        shutil.copy2(snapshot_path, Path(work) / "firecontroller.sqlite")
        p = subprocess.run([str(fcd), "migrate", "--data-dir", work, "--timeout", "10m"], capture_output=True, text=True, timeout=660)
        if p.returncode != 0:
            raise DeployError("migration rehearsal failed; live service unchanged: " + p.stderr[-400:])
        result = json.loads(p.stdout)
        if result.get("state") != "migrated" or result.get("quick_check") != "ok":
            raise DeployError("migration rehearsal reported " + p.stdout[:200])
        return result


def repoint_dropin(text, source, fcd_sha):
    """Point a helper drop-in at releases/<source>/fcd (and its --sha256 when present)."""
    pattern = re.compile(r"/releases/[0-9a-f]{40}/fcd\b")
    if len(pattern.findall(text)) != 1:
        raise DeployError("helper drop-in must name exactly one fcd release path")
    out = pattern.sub("/releases/" + source + "/fcd", text)
    digests = re.findall(r"--sha256 [0-9a-f]{64}", out)
    if len(digests) > 1:
        raise DeployError("helper drop-in names more than one --sha256")
    if digests:
        out = out.replace(digests[0], "--sha256 " + fcd_sha)
    return out


def install_helpers(fcd, source, fcd_sha, releases=HELPER_RELEASES, dropins=DROPINS, reload=True):
    target = releases / source / "fcd"
    if target.exists():
        if sha256(target) != fcd_sha:
            raise DeployError("existing helper binary for this source has a different hash")
    else:
        target.parent.mkdir(mode=0o755)
        tmp = target.with_suffix(".tmp")
        shutil.copy2(fcd, tmp)
        os.chmod(tmp, 0o555)
        if sha256(tmp) != fcd_sha:
            raise DeployError("helper binary copy hash mismatch")
        tmp.replace(target)
    changed = []
    for name, path in dropins.items():
        before = path.read_text()
        after = repoint_dropin(before, source, fcd_sha)
        if after != before:
            tmp = path.with_suffix(".deploy.tmp")
            tmp.write_text(after)
            os.chmod(tmp, path.stat().st_mode & 0o777)
            tmp.replace(path)
            changed.append(name)
    if reload and changed:
        subprocess.run(["systemctl", "daemon-reload"], check=True, timeout=60)
    return changed


def kubectl_json(*args):
    return json.loads(subprocess.run(KUBECTL + list(args) + ["-o", "json"], check=True, capture_output=True, text=True, timeout=60).stdout)


def controller_image(deployment):
    return next(c["image"] for c in deployment["spec"]["template"]["spec"]["containers"] if c["name"] == "controller")


def verify(source, image):
    d = kubectl_json("get", "deployment", "firecontroller")
    annotations = d["spec"]["template"]["metadata"].get("annotations", {})
    if annotations.get("firecontroller.dev/source-commit") != source or controller_image(d) != image:
        raise DeployError("deployment does not name the new source and image")
    if d["status"].get("readyReplicas") != 1 or d["status"].get("updatedReplicas") != 1:
        raise DeployError("new pod is not ready")
    pods = kubectl_json("get", "pods", "-l", ",".join(k + "=" + v for k, v in d["spec"]["selector"]["matchLabels"].items()))["items"]
    running = [p for p in pods if p["status"].get("phase") == "Running" and not p["metadata"].get("deletionTimestamp")]
    if len(running) != 1:
        raise DeployError("expected exactly one running controller pod")
    status = next(c for c in running[0]["status"]["containerStatuses"] if c["name"] == "controller")
    digest = image.split("@", 1)[1]
    if not status.get("ready") or digest not in status.get("imageID", ""):
        raise DeployError("running controller is not the deployed image")
    with urllib.request.urlopen(URL + "/readyz", timeout=10) as r:
        if r.status != 200:
            raise DeployError("readyz failed")
    return {"pod": running[0]["metadata"]["name"], "restarts": status.get("restartCount")}


def verify_assets(binding_path):
    if binding_path is None:
        return None
    sys.path.insert(0, str(HERE))
    from browser_asset_binding import verify_live
    binding = json.loads(Path(binding_path).read_text())

    def fetch(path):
        with urllib.request.urlopen(URL + path, timeout=10) as r:
            return r.read()
    return verify_live(binding, binding["source"], fetch("/"), fetch)


def prune_after_deploy(keep_images, releases=HELPER_RELEASES, dropins=None, keep_recent=2):
    """Remove FireController images and helper binaries no longer in use.

    Keeps the given image digests (current and previous), helper releases named
    by any FireController systemd unit or drop-in, and the newest `keep_recent`.
    """
    removed_images = 0
    listing = subprocess.run(["k3s", "ctr", "-n", "k8s.io", "images", "ls"], check=True, capture_output=True, text=True, timeout=60).stdout
    for line in listing.splitlines()[1:]:
        cols = line.split()
        if len(cols) >= 3 and cols[0].startswith("localhost/firecontroller") and cols[2] not in keep_images:
            subprocess.run(["k3s", "ctr", "-n", "k8s.io", "images", "rm", cols[0]], capture_output=True, timeout=60)
            removed_images += 1
    referenced = set()
    units = list(Path("/etc/systemd/system").glob("firecontroller-*.service")) + list(Path("/etc/systemd/system").glob("firecontroller-*.service.d/*.conf"))
    for unit in (dropins if dropins is not None else units):
        referenced |= set(re.findall(r"/releases/([0-9a-f]{40})/", Path(unit).read_text()))
    dirs = sorted((d for d in releases.iterdir() if d.is_dir() and not d.is_symlink()), key=lambda d: d.stat().st_mtime)
    keep = referenced | {d.name for d in dirs[-keep_recent:]}
    removed_releases = [d.name for d in dirs if d.name not in keep]
    for name in removed_releases:
        shutil.rmtree(releases / name)
    return {"images_removed": removed_images, "helper_releases_removed": len(removed_releases)}


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--source", required=True)
    p.add_argument("--image", required=True, help="localhost/firecontroller@sha256:...")
    p.add_argument("--fcd", required=True, type=Path)
    p.add_argument("--fcd-sha256", required=True)
    p.add_argument("--assets", type=Path, help="browser asset binding JSON from deploy.py")
    p.add_argument("--drain-seconds", type=int, default=600)
    p.add_argument("--lock-wait", type=int, default=1200, help="seconds to wait for a running backup")
    p.add_argument("--memory-limit", default="1536Mi", help="controller container memory limit, applied with the image")
    p.add_argument("--retention-mode", choices=["dry-run", "enabled"])
    p.add_argument("--remote-mcp-mode", choices=["enable", "disable"], help="explicit MCP opt-in; enable only after staging the exact proxy location; omit to preserve current args")
    a = p.parse_args(argv)
    if not SHA.fullmatch(a.source) or not HEX64.fullmatch(a.fcd_sha256) or not re.fullmatch(r"localhost/[a-z0-9._/-]+@sha256:[0-9a-f]{64}", a.image):
        p.error("source, fcd hash or image reference is malformed")
    if sha256(a.fcd) != a.fcd_sha256:
        p.error("fcd binary does not match its hash")

    DEPLOY_LOCK.parent.mkdir(mode=0o700, exist_ok=True)
    DEPLOY_LOCK.touch(mode=0o600, exist_ok=True)
    with contextlib.ExitStack() as held:
        held.enter_context(flock_wait(DEPLOY_LOCK, 0))
        log("waiting_for_backup_phase_lock")
        held.enter_context(flock_wait(PHASE_LOCK, a.lock_wait))
        before = kubectl_json("get", "deployment", "firecontroller")
        old_source = before["spec"]["template"]["metadata"].get("annotations", {}).get("firecontroller.dev/source-commit", "unknown")
        old_image = controller_image(before)
        log("locked", previous_source=old_source, previous_image=old_image)

        label = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime()) + "-" + old_source[:8]
        snap = snapshot(DATA_DB, SNAPSHOTS, label)
        log("snapshot", path=str(snap), bytes=snap.stat().st_size)

        log("migration_rehearsed", **rehearse_migration(a.fcd, snap))

        log("helpers", repointed=install_helpers(a.fcd, a.source, a.fcd_sha256))

        instance = INSTANCE_FILE.read_text().strip()
        rollout_cmd = [sys.executable, "-I", str(HERE / "kubernetes.py"), "--image", a.image, "--source-revision", a.source,
                       "--url", URL, "--instance-id", instance, "--credential-file", str(CREDENTIAL),
                       "--drain-seconds", str(a.drain_seconds), "--memory-limit", a.memory_limit]
        if a.retention_mode is not None:
            rollout_cmd.extend(["--retention-mode", a.retention_mode])
        if a.remote_mcp_mode is not None:
            rollout_cmd.extend(["--remote-mcp-mode", a.remote_mcp_mode])
        rollout_cmd.extend(["--kubectl", "k3s", "kubectl"])
        rollout = subprocess.run(rollout_cmd, capture_output=True, text=True, timeout=a.drain_seconds + 600)
        if rollout.returncode != 0:
            log("rollout_failed", stderr=rollout.stderr[-600:], rollback_image=old_image, rollback_snapshot=str(snap))
            return 1
        log("rolled_out", result=rollout.stdout.strip()[-300:])
        checked = verify(a.source, a.image)
        assets = verify_assets(a.assets)
        log("verified", source=a.source, image=a.image, assets=assets, **checked)
        try:
            log("pruned", **prune_after_deploy({a.image.split("@", 1)[1], old_image.split("@", 1)[1]}))
        except (OSError, subprocess.SubprocessError) as e:
            log("prune_skipped", error=str(e)[:200])  # the deploy itself already succeeded
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except DeployError as e:
        log("failed", error=str(e))
        sys.exit(1)
