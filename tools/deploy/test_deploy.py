import fcntl
import hashlib
import contextlib
import os
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

import deploy
import deploy_host as h
import helper_generation as g

OLD = "b59eb13e2406075995b037ce594a03c9f760f4af"
NEW = "a15c23296c72ff2cb43dfa500515ca6a90627799"
HEALTH_SOURCE = "9530c8a8660be56a5dbcae0952c4b053b430e352"


class DropIns(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.releases = self.root / "releases"
        self.releases.mkdir()
        self.helper_root = self.root / "operator"
        self.launcher = self.helper_root / "helper_generation.py"
        self.selector = self.root / "coordination" / "helper-generation.json"
        self.selector.parent.mkdir()
        self.phase_lock = self.selector.parent / "phase.lock"
        self.phase_lock.write_text("")
        os.chmod(self.phase_lock, 0o600)
        self.old_bytes = b"old fcd"
        self.new_bytes = b"candidate fcd"
        self.old_sha = hashlib.sha256(self.old_bytes).hexdigest()
        self.new_sha = hashlib.sha256(self.new_bytes).hexdigest()
        self._release(OLD, self.old_bytes)
        (self.releases / HEALTH_SOURCE).mkdir()
        self.fcd = self.root / "candidate-fcd"
        self.fcd.write_bytes(self.new_bytes)
        os.chmod(self.fcd, 0o755)
        self.health = self.root / "health.conf"
        self.batch = self.root / "batch.conf"
        self.health.write_text(
            "[Service]\nExecStart=\nExecStart=/usr/bin/python3 "
            + str(self.releases / HEALTH_SOURCE / "publish-backup-health.py")
            + " --fcd " + str(self.releases / OLD / "fcd")
            + " --sha256 " + self.old_sha + " --report /private/health.json\n"
        )
        self.batch.write_text(
            "[Service]\nExecStart=\nExecStart=" + str(self.releases / OLD / "fcd")
            + " backup-push-batch --config /etc/firecontroller/offsite-batch.json --verbose\n"
        )
        self.dropins = {"backup-health": self.health, "offsite-batch": self.batch}

    def _release(self, source, content):
        directory = self.releases / source
        directory.mkdir(parents=True, exist_ok=True)
        binary = directory / "fcd"
        binary.write_bytes(content)
        os.chmod(binary, 0o555)
        return binary

    def _install(self, reload=True):
        with patch.object(h, "_verify_helper_unit"):
            return h.install_helpers(self.fcd, NEW, self.new_sha, self.releases, self.dropins, reload,
                                     self.selector, self.launcher, self.phase_lock)

    def _activate(self):
        lease_fd = g.acquire_generation_exclusive(self.selector)
        try:
            with self.phase_lock.open("r+") as lock:
                fcntl.flock(lock, fcntl.LOCK_EX)
                return h._select_verified_helper_generation_locked(self.selector, self.releases, NEW, self.new_sha)
        finally:
            os.close(lease_fd)

    def _record(self):
        return g.read_record(self.selector)

    def test_validates_known_legacy_roles_and_installs_one_shared_wrapper(self):
        health = g.classify_dropin(self.health.read_text(), "health", self.launcher,
                                   self.selector, self.releases)
        batch = g.classify_dropin(self.batch.read_text(), "batch", self.launcher,
                                  self.selector, self.releases)
        self.assertEqual(health["state"], "legacy")
        self.assertEqual(batch["state"], "legacy")
        self.assertEqual(g.coherent_template("health", health["argv"], self.releases),
                         {"source": OLD, "sha256": self.old_sha})
        self.assertEqual(g.coherent_template("batch", batch["argv"], self.releases),
                         {"source": OLD, "sha256": self.old_sha})

        self.assertEqual(sorted(self._install(reload=False)), ["backup-health", "offsite-batch"])
        record = self._record()
        self.assertEqual(record["current"], {"source": OLD, "sha256": self.old_sha})
        self.assertIsNone(record["previous"])
        selector_mode = self.selector.stat().st_mode & 0o777
        self.assertEqual(selector_mode & 0o007, 0)
        self.assertEqual(selector_mode & 0o222, 0)
        health_wrapper = g.classify_dropin(self.health.read_text(), "health", self.launcher,
                                           self.selector, self.releases)
        batch_wrapper = g.classify_dropin(self.batch.read_text(), "batch", self.launcher,
                                          self.selector, self.releases)
        self.assertEqual(health_wrapper["state"], "wrapper")
        self.assertEqual(batch_wrapper["state"], "wrapper")
        self.assertEqual(health_wrapper["argv"], health["argv"])
        self.assertEqual(batch_wrapper["argv"], batch["argv"])
        self.assertIn("/private/health.json", health_wrapper["argv"])
        self.assertIn("--verbose", batch_wrapper["argv"])

    def test_invalid_pair_fails_before_any_mutation(self):
        original_health = self.health.read_text()
        original_batch = self.batch.read_text()
        other_source = "c" * 40
        self._release(other_source, b"a different verified old release")
        self.batch.write_text(original_batch.replace("/releases/" + OLD + "/fcd",
                                                     "/releases/" + other_source + "/fcd"))
        invalid_batch = self.batch.read_text()
        with self.assertRaises(h.DeployError):
            self._install()
        self.assertEqual(self.health.read_text(), original_health)
        self.assertEqual(self.batch.read_text(), invalid_batch)
        self.assertEqual((self.releases / other_source / "fcd").read_bytes(),
                         b"a different verified old release")
        self.assertFalse(self.selector.exists())
        self.assertFalse((self.releases / NEW).exists())
        self.assertFalse(self.launcher.exists())

    def test_legacy_health_hash_is_checked_before_any_mutation(self):
        original = self.health.read_text().replace(self.old_sha, "0" * 64)
        self.health.write_text(original)
        with self.assertRaises(h.DeployError):
            self._install()
        self.assertEqual(self.health.read_text(), original)
        self.assertFalse(self.selector.exists())
        self.assertFalse((self.releases / NEW).exists())

    def test_unsupported_systemd_quoting_is_refused_without_guessing(self):
        original = self.batch.read_text().replace("/etc/firecontroller/offsite-batch.json",
                                                  '"/etc/firecontroller/offsite-batch.json"')
        self.batch.write_text(original)
        with self.assertRaises(h.DeployError):
            self._install()
        self.assertEqual(self.batch.read_text(), original)
        self.assertFalse(self.selector.exists())
        self.assertFalse((self.releases / NEW).exists())

    def test_extra_systemd_command_is_refused_before_any_write(self):
        original = self.batch.read_text().replace("[Service]\n", "[Service]\nExecStartPre=/usr/bin/true\n")
        self.batch.write_text(original)
        with self.assertRaises(h.DeployError):
            self._install()
        self.assertEqual(self.batch.read_text(), original)
        self.assertEqual(self.health.read_text().splitlines()[1], "ExecStart=")
        self.assertFalse(self.selector.exists())
        self.assertFalse((self.releases / NEW).exists())

    def test_interrupted_first_conversion_keeps_old_selection_until_reload(self):
        original_replace = h._atomic_replace
        failed = False

        def interrupt_batch(path, *args):
            nonlocal failed
            if Path(path) == self.batch and not failed:
                failed = True
                raise OSError("simulated interruption")
            return original_replace(path, *args)

        with patch.object(h, "_atomic_replace", side_effect=interrupt_batch):
            with self.assertRaises(h.DeployError):
                self._install()
        self.assertEqual(self._record()["current"], {"source": OLD, "sha256": self.old_sha})
        self.assertEqual(g.classify_dropin(self.health.read_text(), "health", self.launcher,
                                           self.selector, self.releases)["state"], "wrapper")
        self.assertEqual(g.classify_dropin(self.batch.read_text(), "batch", self.launcher,
                                           self.selector, self.releases)["state"], "legacy")

        with patch.object(h.subprocess, "run") as run:
            self.assertEqual(self._install(reload=True), ["offsite-batch"])
        run.assert_called_once_with(["systemctl", "daemon-reload"], check=True, timeout=60)
        self.assertEqual(self._record()["current"], {"source": OLD, "sha256": self.old_sha})
        self.assertIsNone(self._record()["previous"])
        self._activate()
        self.assertEqual(self._record()["current"], {"source": NEW, "sha256": self.new_sha})
        self.assertEqual(self._record()["previous"], {"source": OLD, "sha256": self.old_sha})

    def test_first_conversion_refuses_active_legacy_unit_before_candidate_activation(self):
        checked = []

        def active_on_batch(unit, expected_argv, require_inactive):
            self.assertTrue(require_inactive)
            self.assertEqual(expected_argv[0], "/usr/bin/python3")
            checked.append(unit)
            if unit == h.HELPER_UNITS["offsite-batch"]:
                raise h.DeployError("active legacy unit")
            return None

        with patch.object(h, "_verify_helper_unit", side_effect=active_on_batch):
            with patch.object(h.subprocess, "run"):
                with self.assertRaisesRegex(h.DeployError, "active legacy unit"):
                    h.install_helpers(self.fcd, NEW, self.new_sha, self.releases, self.dropins, True,
                                      self.selector, self.launcher, self.phase_lock)
        self.assertEqual(checked, [h.HELPER_UNITS["backup-health"], h.HELPER_UNITS["offsite-batch"]])
        self.assertEqual(self._record()["current"], {"source": OLD, "sha256": self.old_sha})
        self.assertFalse(g.manager_fenced(self.selector, self.phase_lock))
        self.assertEqual(g.classify_dropin(self.health.read_text(), "health", self.launcher,
                                           self.selector, self.releases)["state"], "wrapper")
        self.assertEqual(g.classify_dropin(self.batch.read_text(), "batch", self.launcher,
                                           self.selector, self.releases)["state"], "wrapper")

        # A retry after the legacy process exits performs the fence and still
        # leaves the original selection in place until post-rollout verification.
        with patch.object(h.subprocess, "run"):
            self._install(reload=True)
        self.assertTrue(g.manager_fenced(self.selector, self.phase_lock))
        self.assertEqual(self._record()["current"], {"source": OLD, "sha256": self.old_sha})

    def test_effective_systemd_unit_must_use_verified_wrapper(self):
        argv = ["/usr/bin/python3", "/tmp/launcher.py", "launch"]
        state = ("LoadState=loaded\nActiveState=inactive\nSubState=dead\nMainPID=0\nControlPID=0\n"
                 "NeedDaemonReload=no\nExecStart={ path=/usr/bin/python3 ; argv[]=/usr/bin/python3 "
                 "/tmp/launcher.py launch ; ignore_errors=no ; pid=0 }\n")
        with patch.object(h.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, state)) as run:
            h._verify_helper_unit("firecontroller-backup-health.service", argv, True)
        command = run.call_args.args[0]
        self.assertEqual(command[:3], ["systemctl", "show", "firecontroller-backup-health.service"])
        self.assertEqual({item.split("=", 1)[1] for item in command if item.startswith("--property=")},
                         {"LoadState", "ActiveState", "SubState", "MainPID", "ControlPID", "NeedDaemonReload", "ExecStart"})
        for previous, invalid in (("ActiveState=inactive", "ActiveState=active"),
                                  ("NeedDaemonReload=no", "NeedDaemonReload=yes"),
                                  ("/tmp/launcher.py launch", "/tmp/legacy.py run")):
            with patch.object(h.subprocess, "run", return_value=subprocess.CompletedProcess(
                    [], 0, state.replace(previous, invalid))):
                with self.assertRaises(h.DeployError):
                    h._verify_helper_unit("firecontroller-backup-health.service", argv, True)
        with patch.object(h.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, "ActiveState=inactive\n")):
            with self.assertRaises(h.DeployError):
                h._verify_helper_unit("firecontroller-backup-health.service", argv, True)

    def test_manager_fence_is_never_visible_before_its_contents_are_ready(self):
        with patch.object(g.os, "fsync", side_effect=OSError("simulated interrupted write")):
            with self.assertRaises(OSError):
                g.write_manager_fence(self.selector, self.phase_lock)
        self.assertFalse(g.manager_fence_path(self.selector).exists())
        g.write_manager_fence(self.selector, self.phase_lock)
        self.assertTrue(g.manager_fenced(self.selector, self.phase_lock))

    def test_generation_lease_is_never_visible_before_its_modes_are_ready(self):
        with patch.object(g.os, "fsync", side_effect=OSError("simulated interrupted write")):
            with self.assertRaises(g.GenerationError):
                g.ensure_generation_lease(self.selector, self.phase_lock)
        self.assertFalse(g.lease_path(self.selector).exists())
        self.assertEqual(g.ensure_generation_lease(self.selector, self.phase_lock).stat().st_mode & 0o777, 0o640)

    def test_fresh_role_launches_use_one_exact_record_and_preserve_other_arguments(self):
        with patch.object(h.subprocess, "run"):
            self._install(reload=True)
        self._activate()
        record = self._record()
        for role, path in (("health", self.health), ("batch", self.batch)):
            wrapper = g.classify_dropin(path.read_text(), role, self.launcher,
                                        self.selector, self.releases)
            encoded = g.wrapper_argv(role, wrapper["argv"], self.launcher,
                                     self.selector, self.releases)[-1]
            with patch.object(g.os, "execv", side_effect=RuntimeError("exec intercepted")) as execute:
                with self.assertRaisesRegex(RuntimeError, "exec intercepted"):
                    g.launch(role, self.selector, self.releases, encoded)
            launched = execute.call_args.args[1]
            if role == "health":
                self.assertEqual(launched[launched.index("--fcd") + 1],
                                 str(self.releases / record["current"]["source"] / "fcd"))
                self.assertEqual(launched[launched.index("--sha256") + 1], record["current"]["sha256"])
                self.assertIn("--report", launched)
                self.assertIn("/private/health.json", launched)
            else:
                self.assertEqual(launched[0], str(self.releases / record["current"]["source"] / "fcd"))
                self.assertIn("--verbose", launched)
            self.assertEqual(record["current"], {"source": NEW, "sha256": self.new_sha})

    def test_verified_old_selection_drives_both_converted_roles(self):
        self._install(reload=False)
        record = self._record()
        self.assertEqual(record["current"], {"source": OLD, "sha256": self.old_sha})
        for role, path in (("health", self.health), ("batch", self.batch)):
            wrapper = g.classify_dropin(path.read_text(), role, self.launcher,
                                        self.selector, self.releases)
            encoded = g.wrapper_argv(role, wrapper["argv"], self.launcher,
                                     self.selector, self.releases)[-1]
            with patch.object(g.os, "execv", side_effect=RuntimeError("exec intercepted")) as execute:
                with self.assertRaisesRegex(RuntimeError, "exec intercepted"):
                    g.launch(role, self.selector, self.releases, encoded)
            launched = execute.call_args.args[1]
            if role == "health":
                self.assertEqual(launched[launched.index("--fcd") + 1],
                                 str(self.releases / OLD / "fcd"))
                self.assertEqual(launched[launched.index("--sha256") + 1], self.old_sha)
            else:
                self.assertEqual(launched[0], str(self.releases / OLD / "fcd"))

    def test_existing_release_is_immutable_for_a_source(self):
        self._install(reload=False)
        self.assertEqual(self._install(reload=False), [])
        self.fcd.write_bytes(b"different bytes")
        other_sha = hashlib.sha256(b"different bytes").hexdigest()
        with self.assertRaises(h.DeployError):
            h.install_helpers(self.fcd, NEW, other_sha, self.releases, self.dropins, False,
                              self.selector, self.launcher, self.phase_lock)

    def test_previous_selection_is_exact_and_uses_phase_lock(self):
        with patch.object(h.subprocess, "run"):
            self._install(reload=True)
        self._activate()
        selected = g.rollback_previous(self.selector, self.releases, self.phase_lock)
        self.assertEqual(selected, {"source": OLD, "sha256": self.old_sha})
        record = self._record()
        self.assertEqual(record["current"], {"source": OLD, "sha256": self.old_sha})
        self.assertEqual(record["previous"], {"source": NEW, "sha256": self.new_sha})
        self.assertEqual(g.verify_record(record, self.releases)["current"], selected)

    def test_prune_keeps_selector_pair_and_release_references_hidden_in_wrapper(self):
        with patch.object(h.subprocess, "run"):
            self._install(reload=True)
        self._activate()
        extras = ["c" * 40, "d" * 40]
        for source in extras:
            (self.releases / source).mkdir()
        listing = Mock(stdout="REF TYPE DIGEST\n")
        with patch.object(h.subprocess, "run", return_value=listing):
            result = h.prune_after_deploy({"sha256:1"}, self.releases,
                                          [self.health, self.batch], keep_recent=0,
                                          selector=self.selector, launcher=self.launcher)
        self.assertEqual(result["helper_releases_removed"], len(extras))
        self.assertEqual({path.name for path in self.releases.iterdir()},
                         {OLD, NEW, HEALTH_SOURCE})

    def test_prune_only_removes_images_in_the_controller_repository(self):
        listing = Mock(stdout=("REF TYPE DIGEST\n"
                               "localhost/firecontroller:old application sha256:old\n"
                               "localhost/firecontroller:current application sha256:current\n"
                               "localhost/firecontroller-backup:other application sha256:other\n"))
        with patch.object(h.subprocess, "run", return_value=listing) as run:
            result = h.prune_after_deploy({"sha256:current"}, self.releases,
                                          [self.health, self.batch],
                                          selector=self.selector, launcher=self.launcher)
        self.assertEqual(result["images_removed"], 1)
        run.assert_called_with(["k3s", "ctr", "-n", "k8s.io", "images", "rm",
                                "localhost/firecontroller:old"], capture_output=True, timeout=60)

    def test_interrupted_selector_replacement_keeps_old_or_complete_new_record(self):
        with patch.object(h.subprocess, "run"):
            self._install(reload=True)
        original_replace = os.replace

        def fail_selector(source, destination):
            if Path(destination) == self.selector:
                raise OSError("simulated selector replacement interruption")
            return original_replace(source, destination)

        with patch.object(h.subprocess, "run"):
            with patch.object(os, "replace", side_effect=fail_selector):
                with self.assertRaises(h.DeployError):
                    self._activate()
        self.assertEqual(self._record()["current"], {"source": OLD, "sha256": self.old_sha})
        self._activate()
        self.assertEqual(self._record()["current"], {"source": NEW, "sha256": self.new_sha})
        self.assertEqual(self._record()["previous"], {"source": OLD, "sha256": self.old_sha})

    def test_reload_is_reconciled_on_noop_retry_before_selector_switch(self):
        self._install(reload=False)
        self.assertEqual(self._record()["current"], {"source": OLD, "sha256": self.old_sha})
        with patch.object(h.subprocess, "run") as run:
            self.assertEqual(self._install(reload=True), [])
        run.assert_called_once_with(["systemctl", "daemon-reload"], check=True, timeout=60)
        self.assertEqual(self._record()["current"], {"source": OLD, "sha256": self.old_sha})
        self._activate()
        self.assertEqual(self._record()["current"], {"source": NEW, "sha256": self.new_sha})

    def test_lease_is_private_stable_and_required_by_fresh_helper_launch(self):
        lease = g.ensure_generation_lease(self.selector, self.phase_lock)
        before = lease.stat()
        self.assertEqual(lease.stat().st_ino, before.st_ino)
        self.assertEqual(lease.stat().st_gid, self.phase_lock.stat().st_gid)
        self.assertEqual(lease.stat().st_mode & 0o777, 0o640)
        self.assertEqual(g.ensure_generation_lease(self.selector, self.phase_lock), lease)
        self.assertEqual(lease.stat().st_ino, before.st_ino)
        os.chmod(lease, 0o600)
        with self.assertRaises(g.GenerationError):
            g.acquire_generation_shared(self.selector)
        os.chmod(lease, 0o640)

        g.write_record(self.selector, {"source": OLD, "sha256": self.old_sha}, access_from=self.phase_lock)
        g.write_manager_fence(self.selector, self.phase_lock)
        lease.unlink()
        with self.assertRaises(h.DeployError):
            h._ensure_helper_generation_lease(self.selector, self.phase_lock, self.releases,
                                              self.dropins, self.launcher)
        self.assertFalse(lease.exists())

        missing_selector = self.root / "coordination" / "missing.json"
        with self.assertRaises(g.GenerationError):
            g.acquire_generation_shared(missing_selector)

    def test_existing_wrappers_without_a_lease_refuse_to_replace_the_inode(self):
        self._install(reload=False)
        lease = g.lease_path(self.selector)
        lease.unlink()
        with self.assertRaisesRegex(h.DeployError, "missing their stable lease"):
            self._install(reload=True)
        self.assertFalse(lease.exists())
        self.assertFalse(g.manager_fenced(self.selector, self.phase_lock))
        self.assertEqual(self._record()["current"], {"source": OLD, "sha256": self.old_sha})

    def _executable_generation(self, source):
        directory = self.releases / source
        directory.mkdir(parents=True, exist_ok=True)
        binary = directory / "fcd"
        if binary.exists():
            binary.chmod(0o755)
        script = ("#!" + sys.executable + "\n"
                  "import os, pathlib, sys, time\n"
                  "selected = os.environ.get('FC_HELPER_SELECTED')\n"
                  "if selected: pathlib.Path(selected).write_text(pathlib.Path(sys.argv[0]).parent.name)\n"
                  "started = os.environ.get('FC_HELPER_STARTED')\n"
                  "if started:\n"
                  " pathlib.Path(started).write_text('entered')\n"
                  " release = pathlib.Path(os.environ['FC_HELPER_RELEASE'])\n"
                  " while not release.exists(): time.sleep(0.01)\n")
        binary.write_text(script)
        binary.chmod(0o555)
        return {"source": source, "sha256": g.sha256(binary)}

    def _batch_template(self):
        return [str(self.releases / OLD / "fcd"), "backup-push-batch", "--config",
                str(self.root / "private-batch.json"), "--verbose"]

    def _launch_command(self, template=None):
        encoded = g.wrapper_argv("batch", self._batch_template() if template is None else template,
                                 self.launcher, self.selector, self.releases)[-1]
        return [sys.executable, str(Path(g.__file__).resolve()), "launch", "--role", "batch",
                "--selector", str(self.selector), "--releases", str(self.releases),
                "--argv-b64", encoded]

    def _wait_for_file(self, path, process):
        deadline = time.monotonic() + 5
        while not path.exists() and time.monotonic() < deadline:
            if process.poll() is not None:
                raise AssertionError("helper exited before handshake: " + str(process.returncode))
            time.sleep(0.01)
        self.assertTrue(path.exists(), "timed out waiting for helper handshake")

    def _prepare_process_generations(self, previous=None):
        old = self._executable_generation(OLD)
        new = self._executable_generation(NEW)
        g.ensure_generation_lease(self.selector, self.phase_lock)
        g.write_record(self.selector, old, previous, access_from=self.phase_lock)
        return old, new

    def test_helper_started_under_exclusive_custody_reads_only_after_switch(self):
        old, new = self._prepare_process_generations()
        attempt = self.root / "lock-attempted"
        selected = self.root / "selected-source"
        env = os.environ.copy()
        env["FC_HELPER_SELECTED"] = str(selected)
        lease_fd = g.acquire_generation_exclusive(self.selector)
        code = """import fcntl, pathlib, sys
sys.path.insert(0, sys.argv[5])
import helper_generation as g
marker = pathlib.Path(sys.argv[1])
real = g.fcntl.flock
def signal_lock(fd, op):
    if op == fcntl.LOCK_SH:
        marker.write_text("waiting")
    return real(fd, op)
g.fcntl.flock = signal_lock
g.launch("batch", pathlib.Path(sys.argv[2]), pathlib.Path(sys.argv[3]), sys.argv[4])
"""
        process = subprocess.Popen([sys.executable, "-c", code, str(attempt), str(self.selector),
                                    str(self.releases), self._launch_command()[-1],
                                    str(Path(g.__file__).resolve().parent)], env=env,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            self._wait_for_file(attempt, process)
            self.assertFalse(selected.exists())
            g.write_record(self.selector, new, old)
        finally:
            os.close(lease_fd)
        _, stderr = process.communicate(timeout=5)
        self.assertEqual(process.returncode, 0, stderr)
        self.assertEqual(selected.read_text(), NEW)

    def test_execed_helper_retains_shared_lease_until_process_exit(self):
        old, _ = self._prepare_process_generations()
        started, release = self.root / "started", self.root / "release"
        env = os.environ.copy()
        env["FC_HELPER_STARTED"] = str(started)
        env["FC_HELPER_RELEASE"] = str(release)
        process = subprocess.Popen(self._launch_command(), env=env, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, text=True)
        try:
            self._wait_for_file(started, process)
            with g.lease_path(self.selector).open("r+") as lease:
                with self.assertRaises(BlockingIOError):
                    fcntl.flock(lease, fcntl.LOCK_EX | fcntl.LOCK_NB)
        finally:
            release.write_text("finish")
        _, stderr = process.communicate(timeout=5)
        self.assertEqual(process.returncode, 0, stderr)
        lease_fd = g.acquire_generation_exclusive(self.selector)
        os.close(lease_fd)

    def test_pending_rollout_holds_new_launches_and_can_reconcile_after_process_loss(self):
        old, new = self._prepare_process_generations()
        old_image = "localhost/firecontroller@sha256:" + "b" * 64
        new_image = "localhost/firecontroller@sha256:" + "d" * 64
        args = SimpleNamespace(source=NEW, fcd_sha256=new["sha256"], image=new_image, assets=None)
        h._begin_helper_rollout_locked(self.selector, self.releases, OLD, old_image, NEW, new["sha256"], new_image)
        self.assertEqual(self._record()["pending"]["candidate"], new)
        result = subprocess.run(self._launch_command(), capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("held", result.stderr)
        with patch.object(h, "verify", side_effect=h.DeployError("not ready")):
            with self.assertRaisesRegex(h.DeployError, "not ready"):
                h._reconcile_pending_helpers_locked(self.selector, self.releases, NEW, new_image, args)
        self.assertEqual(self._record()["pending"]["candidate"], new)
        with patch.object(h.subprocess, "run") as run:
            with self.assertRaisesRegex(h.DeployError, "pending"):
                h.prune_after_deploy(set(), self.releases, [], selector=self.selector)
            run.assert_not_called()
        with patch.object(h, "verify") as verify, patch.object(h, "verify_assets") as verify_assets, patch.object(h, "log"):
            self.assertTrue(h._reconcile_pending_helpers_locked(self.selector, self.releases, NEW, new_image, args))
            verify.assert_called_once_with(NEW, new_image)
            verify_assets.assert_called_once_with(None, NEW)
        self.assertIsNone(self._record()["pending"])
        self.assertEqual(self._record()["current"], new)
        self.assertEqual(self._record()["previous"], old)

    def test_reconcile_verified_old_generation_before_retry(self):
        old, new = self._prepare_process_generations()
        old_image = "localhost/firecontroller@sha256:" + "b" * 64
        new_image = "localhost/firecontroller@sha256:" + "d" * 64
        h._begin_helper_rollout_locked(self.selector, self.releases, OLD, old_image, NEW, new["sha256"], new_image)
        with patch.object(h, "verify") as verify:
            with self.assertRaisesRegex(h.DeployError, "replacement may still arrive"):
                h._reconcile_pending_helpers_locked(self.selector, self.releases, OLD, old_image,
                                                   SimpleNamespace(confirm_prior_outcome=False))
            verify.assert_not_called()
        self.assertEqual(self._record()["pending"]["candidate"], new)
        with patch.object(h, "verify") as verify, patch.object(h, "log"):
            self.assertFalse(h._reconcile_pending_helpers_locked(self.selector, self.releases, OLD, old_image,
                                                                SimpleNamespace(confirm_prior_outcome=True)))
            verify.assert_called_once_with(OLD, old_image)
        self.assertIsNone(self._record()["pending"])
        self.assertEqual(self._record()["current"], old)

    def test_rollback_waits_for_execed_helper_then_selects_previous(self):
        old, new = self._prepare_process_generations()
        g.write_record(self.selector, new, old)
        started, release = self.root / "rollback-started", self.root / "rollback-release"
        env = os.environ.copy()
        env["FC_HELPER_STARTED"] = str(started)
        env["FC_HELPER_RELEASE"] = str(release)
        process = subprocess.Popen(self._launch_command(), env=env, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, text=True)
        self._wait_for_file(started, process)
        done = threading.Event()
        waiting_for_lease = threading.Event()
        outcomes, errors = [], []

        acquire = g.acquire_generation_exclusive

        def observe_exclusive_attempt(selector, *args):
            waiting_for_lease.set()
            return acquire(selector, *args)

        def rollback():
            try:
                outcomes.append(g.rollback_previous(self.selector, self.releases, self.phase_lock))
            except BaseException as error:
                errors.append(error)
            finally:
                done.set()

        worker = threading.Thread(target=rollback)
        with patch.object(g, "acquire_generation_exclusive", side_effect=observe_exclusive_attempt):
            worker.start()
            try:
                self.assertTrue(waiting_for_lease.wait(5))
                time.sleep(0.05)
                self.assertFalse(done.is_set())
                self.assertEqual(self._record()["current"], new)
            finally:
                release.write_text("finish")
        _, stderr = process.communicate(timeout=5)
        self.assertEqual(process.returncode, 0, stderr)
        worker.join(timeout=5)
        self.assertFalse(worker.is_alive())
        self.assertEqual(errors, [])
        self.assertEqual(outcomes, [old])
        self.assertEqual(self._record()["current"], old)


class Snapshots(unittest.TestCase):
    def test_snapshot_copies_live_rows_and_keeps_two(self):
        with tempfile.TemporaryDirectory() as d:
            d = Path(d)
            live = d / "live.sqlite"
            c = sqlite3.connect(live)
            c.execute("PRAGMA journal_mode=WAL")
            c.execute("CREATE TABLE t(x)")
            c.execute("INSERT INTO t VALUES (1),(2)")
            c.commit()  # keep the writer open, as the daemon does
            root = d / "snapshots"
            for label in ["20261118T010000Z-a", "20261118T020000Z-b", "20261118T030000Z-c"]:
                path = h.snapshot(live, root, label)
            c.close()
            self.assertEqual(sorted(p.name for p in root.iterdir()), ["20261118T020000Z-b", "20261118T030000Z-c"])
            s = sqlite3.connect(path)
            self.assertEqual(s.execute("SELECT count(*) FROM t").fetchone()[0], 2)
            s.close()
            self.assertEqual(oct(path.stat().st_mode & 0o777), "0o600")


class Locks(unittest.TestCase):
    def test_wait_times_out_without_holding(self):
        with tempfile.NamedTemporaryFile() as f:
            holder = open(f.name, "r+")
            fcntl.flock(holder, fcntl.LOCK_EX)
            clock = iter(range(0, 100, 10))
            with self.assertRaises(h.DeployError):
                h.flock_wait(f.name, 15, sleep=lambda s: None, clock=lambda: next(clock))
            holder.close()
            got = h.flock_wait(f.name, 0)
            got.close()


class Digest(unittest.TestCase):
    def test_parse_digest(self):
        listing = ("REF TYPE DIGEST SIZE\n"
                   "localhost/firecontroller:" + NEW + " application/vnd.docker.distribution.manifest.v2+json sha256:" + "a" * 64 + " 100MiB\n")
        self.assertEqual(deploy.parse_digest(listing, "localhost/firecontroller:" + NEW), "sha256:" + "a" * 64)
        with self.assertRaises(SystemExit):
            deploy.parse_digest(listing, "localhost/firecontroller:" + OLD)


class HelperActivation(unittest.TestCase):
    def test_failed_rollout_keeps_old_helper_generation_and_never_activates_candidate(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            phase = root / "phase.lock"
            phase.write_text("")
            os.chmod(phase, 0o600)
            selector = root / "operator" / "helper-generation.json"
            selector.parent.mkdir()
            releases = root / "releases"
            releases.mkdir()
            old = releases / OLD
            old.mkdir()
            old_binary = old / "fcd"
            old_binary.write_bytes(b"verified old helper")
            old_binary.chmod(0o555)
            old_generation = {"source": OLD, "sha256": g.sha256(old_binary)}
            g.write_record(selector, old_generation, access_from=phase)
            candidate = root / "candidate-fcd"
            candidate.write_bytes(b"candidate helper")
            candidate.chmod(0o755)
            candidate_sha = h.sha256(candidate)
            (releases / NEW).mkdir()
            staged = releases / NEW / "fcd"
            staged.write_bytes(candidate.read_bytes())
            staged.chmod(0o555)
            image = "localhost/firecontroller@sha256:" + "d" * 64
            before = {"spec": {"template": {"metadata": {"annotations": {
                "firecontroller.dev/source-commit": OLD}}, "spec": {"containers": [
                    {"name": "controller", "image": image}]}}}}
            instance = root / "instance"
            instance.write_text("fixture-instance")
            snapshot = root / "snapshot"
            snapshot.write_bytes(b"snapshot")
            rollout = subprocess.CompletedProcess([], 1, "", "replacement outcome unknown")

            with patch.object(h, "DEPLOY_LOCK", root / "deploy" / "deploy.lock"), \
                    patch.object(h, "PHASE_LOCK", phase), \
                    patch.object(h, "HELPER_SELECTOR", selector), \
                    patch.object(h, "HELPER_RELEASES", releases), \
                    patch.object(h, "HELPER_LAUNCHER", root / "operator" / "helper_generation.py"), \
                    patch.object(h, "INSTANCE_FILE", instance), \
                    patch.object(h, "_ensure_helper_generation_lease", return_value=root / "lease"), \
                    patch.object(h, "flock_wait", side_effect=lambda *args: contextlib.nullcontext(Mock(fileno=lambda: 10))), \
                    patch.object(h, "kubectl_json", return_value=before), \
                    patch.object(h, "snapshot", return_value=snapshot), \
                    patch.object(h, "rehearse_migration", return_value={}), \
                    patch.object(h, "_install_helpers_locked", return_value=[]), \
                    patch.object(h, "verify") as verify, \
                    patch.object(h, "verify_assets") as verify_assets, \
                    patch.object(h, "_select_verified_helper_generation_locked") as activate, \
                    patch.object(h, "log"), \
                    patch.object(h.subprocess, "run", return_value=rollout), \
                    patch.object(h, "CREDENTIAL", root / "private-admin.token"):
                result = h.main(["--source", NEW, "--image", image, "--fcd", str(candidate),
                                 "--fcd-sha256", candidate_sha])
            self.assertEqual(result, 1)
            self.assertEqual(g.read_record(selector)["current"], old_generation)
            self.assertEqual(g.read_record(selector)["pending"]["candidate"], {"source": NEW, "sha256": candidate_sha})
            activate.assert_not_called()
            verify.assert_not_called()
            verify_assets.assert_not_called()


class Prune(unittest.TestCase):
    def test_keeps_referenced_and_recent_helper_releases(self):
        import os, time
        from unittest import mock
        with tempfile.TemporaryDirectory() as d:
            d = Path(d)
            releases = d / "releases"
            names = [c * 40 for c in "abcde"]
            for i, n in enumerate(names):
                (releases / n).mkdir(parents=True)
                os.utime(releases / n, (time.time() - 100 + i, time.time() - 100 + i))
            unit = d / "60.conf"
            unit.write_text("ExecStart=/x/releases/" + names[0] + "/fcd backup-push-batch\n")
            listing = mock.Mock(stdout="REF TYPE DIGEST\nlocalhost/firecontroller@sha256:1 t sha256:1\nlocalhost/firecontroller@sha256:2 t sha256:2\ndocker.io/x t sha256:3\n")
            with mock.patch.object(h.subprocess, "run", return_value=listing) as run:
                out = h.prune_after_deploy({"sha256:1"}, releases, [unit])
            self.assertEqual(out, {"images_removed": 1, "helper_releases_removed": 2})
            self.assertEqual(sorted(p.name for p in releases.iterdir()), sorted([names[0], names[3], names[4]]))
            self.assertIn(["k3s", "ctr", "-n", "k8s.io", "images", "rm", "localhost/firecontroller@sha256:2"], [c.args[0] for c in run.call_args_list])


if __name__ == "__main__":
    unittest.main()
