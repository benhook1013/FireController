#!/usr/bin/env python3
"""Host side of the fixed FireController deploy. Run by deploy.py, not by hand.

Steps, all under one deploy lock, the helper-generation lease and the shared
backup phase lock (managed backups and the offsite batch wait while a deploy
holds it):

1. Snapshot the live database (SQLite online backup) as the rollback point.
2. Rehearse the new binary's migrations on a copy of that snapshot.
3. Stage the immutable helper generation and reconcile both stable role wrappers, keeping the verified old selection.
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
import stat
import subprocess
import sys
import tempfile
import time
import urllib.request

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import helper_generation

DATA_DB = Path("/var/lib/firecontroller/data/firecontroller.sqlite")
SNAPSHOTS = Path("/var/lib/firecontroller/predeploy")
WORK = Path("/var/lib/firecontroller/predeploy-work")
HELPER_RELEASES = Path("/var/lib/firecontroller/operator-offsite-v1/releases")
PHASE_LOCK = Path("/var/lib/firecontroller/operator-offsite-v1/coordination/phase.lock")
HELPER_ROOT = HELPER_RELEASES.parent
HELPER_LAUNCHER = HELPER_ROOT / "helper_generation.py"
HELPER_SELECTOR = HELPER_ROOT / "helper-generation.json"
DEPLOY_LOCK = Path("/root/firecontroller-deploy/deploy.lock")
INSTANCE_FILE = Path("/root/firecontroller-operations/dot-deploy/instance")
CREDENTIAL = Path("/var/lib/firecontroller/data/bootstrap-admin.token")
URL = "https://controller.firedevops.net"
DROPINS = {
    "backup-health": Path("/etc/systemd/system/firecontroller-backup-health.service.d/50-develop-release.conf"),
    "offsite-batch": Path("/etc/systemd/system/firecontroller-offsite-batch.service.d/60-backup-phase.conf"),
}
HELPER_UNITS = {
    "backup-health": "firecontroller-backup-health.service",
    "offsite-batch": "firecontroller-offsite-batch.service",
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


def _verify_helper_unit(unit, expected_argv, require_inactive):
    """Check the command systemd actually loaded, without logging its arguments."""
    properties = ("LoadState", "ActiveState", "SubState", "MainPID", "ControlPID", "NeedDaemonReload", "ExecStart")
    try:
        result = subprocess.run(
            ["systemctl", "show", unit, "--no-pager"]
            + ["--property=" + value for value in properties],
            check=True, capture_output=True, text=True, timeout=30)
    except (OSError, subprocess.SubprocessError) as error:
        raise DeployError("could not verify effective systemd state for " + unit) from error
    values = {}
    for line in result.stdout.splitlines():
        if "=" not in line:
            continue
        key, value = line.split("=", 1)
        if key in properties:
            if key in values:
                raise DeployError("systemd returned duplicate state for " + unit)
            values[key] = value
    if set(values) != set(properties):
        raise DeployError("systemd returned incomplete effective state for " + unit)
    expected = "{ path=" + expected_argv[0] + " ; argv[]=" + " ".join(expected_argv) + " ; ignore_errors=no ; "
    command = values["ExecStart"]
    if (values["LoadState"] != "loaded" or values["NeedDaemonReload"] != "no"
            or not command.startswith(expected) or command.count("{ path=") != 1 or not command.endswith(" }")):
        raise DeployError("effective systemd launch does not match the verified wrapper for " + unit)
    if require_inactive and (values["ActiveState"] != "inactive" or values["SubState"] != "dead"
                             or values["MainPID"] != "0" or values["ControlPID"] != "0"):
        raise DeployError("first helper conversion requires " + unit + " to be inactive; retry after it exits")


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


def _atomic_replace(path, content, mode, uid, gid):
    """Write one same-directory replacement while preserving the file identity policy."""
    path = Path(path)
    fd, name = tempfile.mkstemp(prefix="." + path.name + ".", suffix=".deploy.tmp", dir=str(path.parent))
    temporary = Path(name)
    try:
        os.fchown(fd, uid, gid)
        os.fchmod(fd, mode)
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            fd = -1
            stream.write(content)
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


def _record_or_none(selector, releases):
    if selector.exists() or selector.is_symlink():
        try:
            return helper_generation.verify_record(helper_generation.read_record(selector), releases)
        except helper_generation.GenerationError as error:
            raise DeployError(str(error)) from error
    return None


def _ensure_helper_generation_lease(selector, phase_lock, releases, dropins, launcher):
    """Create the lease only for a validated legacy first-conversion pair."""
    selector = Path(selector)
    releases = Path(releases)
    lease = helper_generation.lease_path(selector)
    if lease.exists() or lease.is_symlink():
        try:
            return helper_generation.ensure_generation_lease(selector, phase_lock)
        except (OSError, helper_generation.GenerationError) as error:
            raise DeployError("existing helper generation lease is invalid: " + str(error)) from error
    fence = helper_generation.manager_fence_path(selector)
    if selector.exists() or selector.is_symlink() or fence.exists() or fence.is_symlink():
        raise DeployError("installed helpers are missing their stable lease; refusing to recreate it")
    templates = []
    try:
        for name, role in (("backup-health", "health"), ("offsite-batch", "batch")):
            config = helper_generation.classify_dropin(
                Path(dropins[name]).read_text(), role, launcher, selector, releases)
            if config["state"] != "legacy":
                raise DeployError("converted helper wrappers require their existing lease and selector")
            templates.append(helper_generation.coherent_template(role, config["argv"], releases))
    except (KeyError, OSError, helper_generation.GenerationError) as error:
        raise DeployError("cannot provision first-conversion helper lease: " + str(error)) from error
    if templates[0] != templates[1]:
        raise DeployError("cannot provision first-conversion lease for mismatched helper role templates")
    try:
        return helper_generation.ensure_generation_lease(selector, phase_lock)
    except (OSError, helper_generation.GenerationError) as error:
        raise DeployError("could not establish helper generation lease: " + str(error)) from error


def _install_helpers_locked(fcd, source, fcd_sha, releases=HELPER_RELEASES, dropins=DROPINS, reload=True,
                            selector=HELPER_SELECTOR, launcher=HELPER_LAUNCHER, phase_lock=PHASE_LOCK):
    """Stage static wrappers and reconcile manager without activating a candidate.

    Both legacy launch shapes are parsed and verified before any file is changed.
    During conversion the selector stays on the coherent old fcd until both role
    wrappers have been written and systemd has reconciled them. The candidate is
    selected only after the controller rollout is independently verified.
    """
    releases = Path(releases)
    selector = Path(selector)
    launcher = Path(launcher)
    if not helper_generation.SOURCE.fullmatch(source) or not helper_generation.SHA256.fullmatch(fcd_sha):
        raise DeployError("helper source or sha256 is malformed")
    if (not Path(fcd).is_file() or Path(fcd).is_symlink()
            or not Path(fcd).stat().st_mode & 0o111 or sha256(fcd) != fcd_sha):
        raise DeployError("candidate fcd does not match its source hash")

    roles = {"backup-health": "health", "offsite-batch": "batch"}
    parsed = {}
    templates = {}
    try:
        for name, role in roles.items():
            path = Path(dropins[name])
            text = path.read_text()
            config = helper_generation.classify_dropin(text, role, launcher, selector, releases)
            parsed[name] = {"role": role, "path": path, "text": text, "mode": path.stat().st_mode & 0o777,
                            "uid": path.stat().st_uid, "gid": path.stat().st_gid, **config}
            templates[name] = helper_generation.coherent_template(role, config["argv"], releases)
    except (KeyError, OSError, helper_generation.GenerationError) as error:
        raise DeployError("helper drop-in preflight failed: " + str(error)) from error

    old_generations = list(templates.values())
    if any(value != old_generations[0] for value in old_generations[1:]):
        raise DeployError("backup-health and offsite-batch do not reference one coherent old fcd generation")
    old_generation = old_generations[0]
    selector_record = _record_or_none(selector, releases)
    try:
        phase_info = Path(phase_lock).lstat()
        helper_generation.validate_generation_lease(selector, phase_info.st_gid)
        if (selector_record is not None
                and helper_generation._selector_group(selector) != phase_info.st_gid):
            raise helper_generation.GenerationError("generation selector group differs from the backup phase lock")
        fenced = helper_generation.manager_fenced(selector, phase_lock)
    except (OSError, helper_generation.GenerationError) as error:
        raise DeployError("helper lease or first-conversion fence is unavailable: " + str(error)) from error
    try:
        selector_parent = selector.parent.lstat()
    except OSError as error:
        raise DeployError("helper selector directory is unavailable") from error
    if (not stat.S_ISDIR(selector_parent.st_mode) or stat.S_ISLNK(selector_parent.st_mode)
            or selector_parent.st_mode & 0o022):
        raise DeployError("helper selector directory must be trusted and not group/world writable")
    states = {item["state"] for item in parsed.values()}
    if fenced and states != {"wrapper"}:
        raise DeployError("legacy helper commands reappeared after the manager fence; refusing automatic conversion")
    if selector_record is None:
        if states != {"legacy"}:
            raise DeployError("converted helper wrappers require their existing generation selector")
        try:
            lock_info = Path(phase_lock).lstat()
        except OSError as error:
            raise DeployError("existing backup phase lock is unavailable") from error
        if not stat.S_ISREG(lock_info.st_mode) or Path(phase_lock).is_symlink():
            raise DeployError("backup phase lock must be an existing regular file")
        if not (lock_info.st_mode & 0o444):
            raise DeployError("backup phase lock grants no read access for the private selector")
    else:
        if states == {"legacy"} and selector_record["current"] != old_generation:
            raise DeployError("legacy helper drop-ins do not match the existing selected generation")
        if len(states) > 1 and selector_record["current"] != old_generation:
            raise DeployError("interrupted helper conversion no longer selects its verified old generation")

    target = releases / source / "fcd"
    try:
        if target.parent.exists() or target.parent.is_symlink():
            release_info = target.parent.lstat()
            if not stat.S_ISDIR(release_info.st_mode) or stat.S_ISLNK(release_info.st_mode):
                raise DeployError("helper source release must be a regular non-symlink directory")
        if target.exists() or target.is_symlink():
            target_mode = target.lstat().st_mode
            if (not stat.S_ISREG(target_mode) or stat.S_ISLNK(target_mode) or not target_mode & 0o111
                    or target_mode & 0o222 or sha256(target) != fcd_sha):
                raise DeployError("existing helper binary for this source has a different hash or file type")
        else:
            target.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
            fd, name = tempfile.mkstemp(prefix=".fcd.", suffix=".deploy.tmp", dir=str(target.parent))
            os.close(fd)
            tmp = Path(name)
            try:
                shutil.copy2(fcd, tmp)
                os.chmod(tmp, 0o555)
                if sha256(tmp) != fcd_sha:
                    raise DeployError("helper binary copy hash mismatch")
                os.replace(tmp, target)
            except BaseException:
                try:
                    tmp.unlink()
                except FileNotFoundError:
                    pass
                raise
    except OSError as error:
        raise DeployError("could not stage helper release: " + str(error)) from error

    try:
        launcher.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
        launcher_source = HERE / "helper_generation.py"
        launcher_text = launcher_source.read_text()
        launcher_mode = 0o555
        if launcher.exists() or launcher.is_symlink():
            info = launcher.lstat()
            if not stat.S_ISREG(info.st_mode) or launcher.is_symlink():
                raise DeployError("stable helper launcher must be a regular non-symlink file")
            if launcher.read_text() != launcher_text or stat.S_IMODE(info.st_mode) != launcher_mode:
                _atomic_replace(launcher, launcher_text, launcher_mode, info.st_uid, info.st_gid)
        else:
            _atomic_replace(launcher, launcher_text, launcher_mode, os.geteuid(), os.getegid())
    except OSError as error:
        raise DeployError("could not install stable helper launcher: " + str(error)) from error

    if selector_record is None:
        try:
            selector_record = helper_generation.write_record(selector, old_generation, access_from=phase_lock)
        except (OSError, helper_generation.GenerationError) as error:
            raise DeployError("could not initialize verified old helper selection: " + str(error)) from error

    updates = []
    for name, item in parsed.items():
        if item["state"] == "wrapper":
            continue
        try:
            after = helper_generation.render_dropin(item["text"], item["role"], item["argv"],
                                                    launcher, selector, releases)
            if after != item["text"]:
                _atomic_replace(item["path"], after, item["mode"], item["uid"], item["gid"])
                updates.append(name)
        except (OSError, helper_generation.GenerationError) as error:
            raise DeployError("could not install " + name + " helper wrapper: " + str(error)) from error

    if reload:
        subprocess.run(["systemctl", "daemon-reload"], check=True, timeout=60)
        for name, role in roles.items():
            argv = helper_generation.wrapper_argv(role, parsed[name]["argv"], launcher, selector, releases)
            _verify_helper_unit(HELPER_UNITS[name], argv, require_inactive=not fenced)
        # On first conversion, legacy processes launched before daemon-reload
        # never held the new generation lease. Do not activate any candidate
        # until both effective units are confirmed stopped. Their state output
        # is restricted to non-secret systemd properties.
        if not fenced:
            try:
                helper_generation.write_manager_fence(selector, phase_lock)
            except (OSError, helper_generation.GenerationError) as error:
                raise DeployError("could not record first-conversion manager fence: " + str(error)) from error
    return updates


def install_helpers(fcd, source, fcd_sha, releases=HELPER_RELEASES, dropins=DROPINS, reload=True,
                    selector=HELPER_SELECTOR, launcher=HELPER_LAUNCHER, phase_lock=PHASE_LOCK,
                    lock_wait=1200):
    """Run the staging/reload helper operation under generation-then-phase custody."""
    lease = _ensure_helper_generation_lease(selector, phase_lock, releases, dropins, launcher)
    with contextlib.ExitStack() as held:
        deadline = time.monotonic() + lock_wait
        held.enter_context(flock_wait(lease, lock_wait))
        held.enter_context(flock_wait(phase_lock, max(0, deadline - time.monotonic())))
        return _install_helpers_locked(fcd, source, fcd_sha, releases, dropins, reload,
                                       selector, launcher, phase_lock)


def _select_verified_helper_generation_locked(selector, releases, source, fcd_sha):
    """Select a verified candidate; caller must hold generation then phase locks."""
    candidate = {"source": source, "sha256": fcd_sha}
    try:
        helper_generation.verify_generation(candidate, releases)
        record = helper_generation.verify_record(helper_generation.read_record(selector), releases)
        if record["pending"] is not None and record["pending"]["candidate"] != candidate:
            raise DeployError("pending rollout selects a different verified helper candidate")
        if record["current"] != candidate:
            return helper_generation.write_record(selector, candidate, record["current"])
        if record["pending"] is not None:
            return helper_generation.write_record(selector, candidate, record["previous"])
        return record
    except (OSError, helper_generation.GenerationError) as error:
        raise DeployError("could not select the verified helper generation: " + str(error)) from error


def _begin_helper_rollout_locked(selector, releases, previous_source, previous_image, source, sha, image):
    record = _record_or_none(selector, releases)
    if record is None or record["current"]["source"] != previous_source or record["pending"] is not None:
        raise DeployError("active controller and helper generations must agree before rollout")
    candidate = {"source": source, "sha256": sha}
    try:
        helper_generation.verify_generation(candidate, releases)
        helper_generation.write_record(selector, record["current"], record["previous"], pending={
            "candidate": candidate, "image": image, "old_image": previous_image,
        })
    except (OSError, helper_generation.GenerationError) as error:
        raise DeployError("could not persist helper rollout hold: " + str(error)) from error


def _reconcile_pending_helpers_locked(selector, releases, observed_source, observed_image, args):
    record = _record_or_none(selector, releases)
    if record is None or record["pending"] is None:
        return False
    pending = record["pending"]
    candidate = pending["candidate"]
    if (observed_source, observed_image) == (record["current"]["source"], pending["old_image"]):
        if not args.confirm_prior_outcome:
            raise DeployError("the prior image is visible, but a pending replacement may still arrive; confirm its outcome before resuming")
        verify(observed_source, observed_image)
        helper_generation.write_record(selector, record["current"], record["previous"])
        log("helper_rollout_reconciled", source=observed_source, image=observed_image)
        return False
    if (observed_source, observed_image) != (candidate["source"], pending["image"]):
        raise DeployError("cannot reconcile helpers until one of the recorded controller images is verified")
    if (args.source, args.fcd_sha256, args.image) != (candidate["source"], candidate["sha256"], pending["image"]):
        raise DeployError("retry the exact pending controller image and helper before deploying a different release")
    verify(observed_source, observed_image)
    verify_assets(args.assets, observed_source)
    _select_verified_helper_generation_locked(selector, releases, candidate["source"], candidate["sha256"])
    log("helper_rollout_reconciled", source=observed_source, image=observed_image)
    return True


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


def verify_assets(binding_path, expected_source):
    if binding_path is None:
        return None
    sys.path.insert(0, str(HERE))
    from browser_asset_binding import Refuse, verify_live

    def fetch(path):
        with urllib.request.urlopen(URL + path, timeout=10) as r:
            return r.read()
    try:
        binding = json.loads(Path(binding_path).read_text())
        if not isinstance(binding, dict) or binding.get("source") != expected_source:
            raise Refuse("source binding differs")
        return verify_live(binding, expected_source, fetch("/"), fetch)
    except ValueError as error:
        raise DeployError("browser asset verification failed: " + str(error)) from error


def prune_after_deploy(keep_images, releases=HELPER_RELEASES, dropins=None, keep_recent=2,
                       selector=HELPER_SELECTOR, launcher=HELPER_LAUNCHER):
    """Remove FireController images and helper binaries no longer in use.

    Keeps the given image digests (current and previous), helper releases named
    by any FireController systemd unit or drop-in, and the newest `keep_recent`.
    """
    generation = _record_or_none(Path(selector), releases)
    if generation is not None and generation["pending"] is not None:
        raise DeployError("cannot safely prune while a helper rollout is pending")
    removed_images = 0
    listing = subprocess.run(["k3s", "ctr", "-n", "k8s.io", "images", "ls"], check=True, capture_output=True, text=True, timeout=60).stdout
    for line in listing.splitlines()[1:]:
        cols = line.split()
        if (len(cols) >= 3 and cols[0].startswith(("localhost/firecontroller:", "localhost/firecontroller@"))
                and cols[2] not in keep_images):
            subprocess.run(["k3s", "ctr", "-n", "k8s.io", "images", "rm", cols[0]], capture_output=True, timeout=60)
            removed_images += 1
    referenced = set()
    units = list(Path("/etc/systemd/system").glob("firecontroller-*.service")) + list(Path("/etc/systemd/system").glob("firecontroller-*.service.d/*.conf"))
    for unit in (dropins if dropins is not None else units):
        text = Path(unit).read_text()
        referenced |= set(re.findall(r"/releases/([0-9a-f]{40})/", text))
        try:
            referenced |= helper_generation.referenced_sources(text, launcher, selector, releases)
        except helper_generation.GenerationError as error:
            raise DeployError("cannot safely prune helper releases: " + str(error)) from error
    if generation is not None:
        referenced.add(generation["current"]["source"])
        if generation["previous"] is not None:
            referenced.add(generation["previous"]["source"])
    dirs = sorted((d for d in releases.iterdir() if d.is_dir() and not d.is_symlink()), key=lambda d: d.stat().st_mtime)
    recent = dirs[-keep_recent:] if keep_recent else []
    keep = referenced | {d.name for d in recent}
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
    p.add_argument("--confirm-prior-outcome", action="store_true",
                   help="confirm the pending replacement ended and the recorded prior image is authoritative")
    a = p.parse_args(argv)
    if not SHA.fullmatch(a.source) or not HEX64.fullmatch(a.fcd_sha256) or not re.fullmatch(r"localhost/[a-z0-9._/-]+@sha256:[0-9a-f]{64}", a.image):
        p.error("source, fcd hash or image reference is malformed")
    if sha256(a.fcd) != a.fcd_sha256:
        p.error("fcd binary does not match its hash")

    DEPLOY_LOCK.parent.mkdir(mode=0o700, exist_ok=True)
    DEPLOY_LOCK.touch(mode=0o600, exist_ok=True)
    with contextlib.ExitStack() as held:
        deploy_lock = held.enter_context(flock_wait(DEPLOY_LOCK, 0))
        try:
            generation_lease = _ensure_helper_generation_lease(
                HELPER_SELECTOR, PHASE_LOCK, HELPER_RELEASES, DROPINS, HELPER_LAUNCHER)
        except (OSError, helper_generation.GenerationError) as error:
            raise DeployError("could not establish helper generation lease: " + str(error)) from error
        lock_deadline = time.monotonic() + a.lock_wait
        helper_lock = held.enter_context(flock_wait(generation_lease, a.lock_wait))
        log("waiting_for_backup_phase_lock")
        backup_lock = held.enter_context(flock_wait(PHASE_LOCK, max(0, lock_deadline - time.monotonic())))
        before = kubectl_json("get", "deployment", "firecontroller")
        old_source = before["spec"]["template"]["metadata"].get("annotations", {}).get("firecontroller.dev/source-commit", "unknown")
        old_image = controller_image(before)
        log("locked", previous_source=old_source, previous_image=old_image)
        if _reconcile_pending_helpers_locked(HELPER_SELECTOR, HELPER_RELEASES, old_source, old_image, a):
            return 0

        label = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime()) + "-" + old_source[:8]
        snap = snapshot(DATA_DB, SNAPSHOTS, label)
        log("snapshot", path=str(snap), bytes=snap.stat().st_size)

        log("migration_rehearsed", **rehearse_migration(a.fcd, snap))

        log("helpers_staged", wrappers_updated=_install_helpers_locked(a.fcd, a.source, a.fcd_sha256))

        instance = INSTANCE_FILE.read_text().strip()
        rollout_cmd = [sys.executable, "-I", str(HERE / "kubernetes.py"), "--image", a.image, "--source-revision", a.source,
                       "--url", URL, "--instance-id", instance, "--credential-file", str(CREDENTIAL),
                       "--drain-seconds", str(a.drain_seconds), "--memory-limit", a.memory_limit]
        if a.retention_mode is not None:
            rollout_cmd.extend(["--retention-mode", a.retention_mode])
        if a.remote_mcp_mode is not None:
            rollout_cmd.extend(["--remote-mcp-mode", a.remote_mcp_mode])
        rollout_cmd.extend(["--kubectl", "k3s", "kubectl"])
        _begin_helper_rollout_locked(HELPER_SELECTOR, HELPER_RELEASES, old_source, old_image,
                                    a.source, a.fcd_sha256, a.image)
        try:
            lock_descriptors = tuple(lock.fileno() for lock in (deploy_lock, helper_lock, backup_lock))
            rollout_env = {**os.environ, "FC_ROLLOUT_CUSTODY_FDS": ",".join(map(str, lock_descriptors))}
            rollout = subprocess.run(rollout_cmd, capture_output=True, text=True,
                                     timeout=a.drain_seconds + 600, pass_fds=lock_descriptors, env=rollout_env)
        except (OSError, subprocess.SubprocessError) as error:
            log("rollout_failed_reconciliation_required", state="unknown", helper_state="held",
                error=str(error)[:300], rollback_image=old_image, rollback_snapshot=str(snap))
            return 1
        if rollout.returncode != 0:
            log("rollout_failed_reconciliation_required", state="unknown", helper_state="held",
                stderr=rollout.stderr[-600:], rollback_image=old_image, rollback_snapshot=str(snap))
            return 1
        log("rolled_out", result=rollout.stdout.strip()[-300:])
        try:
            checked = verify(a.source, a.image)
            assets = verify_assets(a.assets, a.source)
        except (DeployError, OSError, subprocess.SubprocessError) as error:
            log("rollout_unverified_reconciliation_required", state="unknown", helper_state="held",
                error=str(error)[:300], rollback_image=old_image, rollback_snapshot=str(snap))
            return 1
        # The image and its embedded source are now independently observed.
        # Keep the old helper generation until this point, and write the new
        # selection while both the generation and phase locks remain held.
        try:
            selected = _select_verified_helper_generation_locked(HELPER_SELECTOR, HELPER_RELEASES,
                                                                  a.source, a.fcd_sha256)
        except DeployError as error:
            log("helper_selection_failed_reconciliation_required", state="image_verified",
                helper_state="held", error=str(error)[:300], rollback_image=old_image,
                rollback_snapshot=str(snap))
            return 1
        log("verified", source=a.source, image=a.image, assets=assets,
            helper_generation=selected["current"]["source"], **checked)
        try:
            log("pruned", **prune_after_deploy({a.image.split("@", 1)[1], old_image.split("@", 1)[1]}))
        except (DeployError, OSError, subprocess.SubprocessError) as e:
            log("prune_skipped", error=str(e)[:200])  # the deploy itself already succeeded
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except DeployError as e:
        log("failed", error=str(e))
        sys.exit(1)
