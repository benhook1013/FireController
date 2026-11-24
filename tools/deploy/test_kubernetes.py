import importlib.util
import contextlib
import copy
import io
import json
import os
import subprocess
import tempfile
from unittest import mock
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("deployment", Path(__file__).with_name("kubernetes.py"))
deployment = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deployment)


class CrashLoopRecoveryTests(unittest.TestCase):
    def test_only_confirmed_crashloop_with_unavailable_api_can_replace(self):
        pod = {"metadata": {"ownerReferences": [{"kind": "ReplicaSet", "controller": True, "uid": "fixture-rs"}]},
               "status": {"containerStatuses": [{"name": "controller", "ready": False, "restartCount": 3,
                           "state": {"waiting": {"reason": "CrashLoopBackOff"}}, "lastState": {"terminated": {}}}]}}
        unavailable = mock.Mock(side_effect=deployment.OperatorUnavailable("fixture 503"))
        deployment.require_crashloop(unavailable, {}, [pod], "controller")
        with self.assertRaisesRegex(RuntimeError, "responds"):
            deployment.require_crashloop(mock.Mock(return_value={}), {}, [pod], "controller")
        for deployment_state, pods in (({"status": {"readyReplicas": 1}}, [pod]), ({}, [pod, pod]), ({}, [])):
            with self.assertRaisesRegex(RuntimeError, "exactly one unready"):
                deployment.require_crashloop(unavailable, deployment_state, pods, "controller")
        pod["status"]["containerStatuses"][0]["state"] = {"running": {}}
        with self.assertRaisesRegex(RuntimeError, "CrashLoopBackOff"):
            deployment.require_crashloop(unavailable, {}, [pod], "controller")


class RetentionArgumentTests(unittest.TestCase):
    def test_explicit_dry_run_and_enable_preserve_unrelated_args(self):
        now = ["--data-dir", "/data", "--backup-phase-lease=/backup/phase.lock",
               "--retention-dry-run=false", "-retention-dry-run=true", "--retention-interval=6h"]
        expected = now[:3] + now[-1:]
        self.assertEqual(deployment.retention_args(now, None), now)
        self.assertEqual(deployment.retention_args(now, "dry-run"), expected + ["--retention-dry-run"])
        self.assertEqual(deployment.retention_args(now, "enabled"), expected)
        with self.assertRaises(ValueError):
            deployment.retention_args(now, "unknown-mode")
        with self.assertRaises(ValueError):
            deployment.retention_args("--data-dir=/data", "dry-run")


class RemoteMCPArgumentTests(unittest.TestCase):
    ORIGIN = "https://controller.example"
    UNRELATED = ["--data-dir", "/data", "--remote-mcp-allowed-origin=https://client.example",
                 "--retention-dry-run", "--retention-interval=6h", "--listen=127.0.0.1:8081"]

    def test_omitted_enabled_disabled_and_all_existing_spellings(self):
        for existing in ([], ["--remote-mcp-origin", "https://old.example"],
                         ["--remote-mcp-origin=https://old.example"],
                         ["-remote-mcp-origin", "https://old.example"],
                         ["-remote-mcp-origin=https://old.example"]):
            now = self.UNRELATED[:2] + existing + self.UNRELATED[2:]
            before = now[:]
            with self.subTest(existing=existing):
                self.assertIs(deployment.remote_mcp_args(now, None, self.ORIGIN), now)
                self.assertEqual(deployment.remote_mcp_args(now, "enable", self.ORIGIN + "/"),
                                 self.UNRELATED + ["--remote-mcp-origin", self.ORIGIN])
                self.assertEqual(deployment.remote_mcp_args(now, "disable", self.ORIGIN), self.UNRELATED)
                self.assertEqual(now, before)

    def test_malformed_and_duplicate_selectors_refuse_without_mutating_input(self):
        invalid = [["--remote-mcp-origin"], ["--remote-mcp-origin", "--listen=:8081"],
                   ["--remote-mcp-origin="], ["-remote-mcp-origin", ""],
                   ["--remote-mcp-origin=https://one.example", "-remote-mcp-origin", self.ORIGIN]]
        invalid += [["--remote-mcp-origin=" + value] for value in
                    ("http://localhost", "https://user@host", "https://host/path", "https://host?x=1",
                     "https://host#fragment", "https://host:", "https://host:bad", "https://host:99999",
                     "https://host/ ", "https://[broken", "https://")]
        for now in invalid:
            self.assertIs(deployment.remote_mcp_args(now, None, self.ORIGIN), now)
            for mode in ("enable", "disable"):
                with self.subTest(now=now, mode=mode), self.assertRaises(ValueError):
                    deployment.remote_mcp_args(now, mode, self.ORIGIN)
        for now in ("--remote-mcp-origin=bad", [None]):
            with self.assertRaises(ValueError):
                deployment.remote_mcp_args(now, "enable", self.ORIGIN)

    def test_enable_requires_https_but_disable_allows_loopback_http_service(self):
        with self.assertRaises(ValueError):
            deployment.remote_mcp_args([], "enable", "http://localhost")
        self.assertEqual(deployment.remote_mcp_args([], "disable", "http://localhost"), [])
        with self.assertRaises(ValueError):
            deployment.remote_mcp_args(["--"], "enable", self.ORIGIN)
        with self.assertRaises(ValueError):
            deployment.remote_mcp_args([], "enabled", self.ORIGIN)

    def test_http_enable_refuses_before_credentials_or_external_actions(self):
        argv = ["kubernetes.py", "--image", "example/controller@sha256:" + "a" * 64,
                "--source-revision", "b" * 40, "--url", "http://localhost", "--instance-id", "fixture",
                "--credential-file", "/not-read", "--remote-mcp-mode", "enable"]
        with mock.patch("sys.argv", argv), mock.patch.object(Path, "read_text") as read, mock.patch.object(deployment.subprocess, "run") as run:
            with self.assertRaises(ValueError):
                deployment.main()
            read.assert_not_called()
            run.assert_not_called()


class RemoteMCPForwardingTests(unittest.TestCase):
    def assert_mode(self, command, mode):
        if mode is None:
            self.assertNotIn("--remote-mcp-mode", command)
        else:
            self.assertEqual(command[command.index("--remote-mcp-mode") + 1], mode)

    def test_local_wrapper_forwards_only_explicit_mode(self):
        import deploy
        import browser_asset_binding
        import shlex
        revision = "b" * 40
        digest = "sha256:" + "a" * 64
        package_sha = "c" * 64
        stage = deploy.REMOTE_ROOT + "/" + revision + ".Ab12Cd34"

        def run_command(args, **kwargs):
            if "archive" in args:
                self.assertIn(revision, args)
                Path(args[args.index("--output") + 1]).touch()
            if "--metadata-file" in args:
                Path(args[args.index("--metadata-file") + 1]).write_text(
                    json.dumps({"containerimage.digest": digest})
                )

        cases = [(None, digest, False), ("enable", digest, False),
                 ("disable", digest, False), (None, "sha256:" + "d" * 64, False),
                 (None, digest, True)]
        for mode, imported, dry_run in cases:
            with self.subTest(mode=mode, imported=imported, dry_run=dry_run), contextlib.ExitStack() as stack:
                listing = "REF TYPE DIGEST\nlocalhost/firecontroller:" + revision + " type " + imported + "\n"
                stack.enter_context(mock.patch.object(deploy, "require_clean_develop", return_value=revision))
                wrapper_run = stack.enter_context(mock.patch.object(deploy, "run", side_effect=run_command))
                outputs = ["fixture-container", "e" * 64, json.dumps({"package_sha256": package_sha})]
                if not dry_run:
                    outputs.extend([stage, listing])
                stack.enter_context(mock.patch.object(deploy, "out", side_effect=outputs))
                save = stack.enter_context(mock.patch.object(deploy.subprocess, "Popen"))
                save.return_value.wait.return_value = 0
                run = stack.enter_context(mock.patch.object(deploy.subprocess, "run", return_value=subprocess.CompletedProcess([], 0)))
                stack.enter_context(mock.patch.object(Path, "read_bytes", return_value=b"fixture-binary"))
                stack.enter_context(mock.patch.object(browser_asset_binding, "source_binding", return_value={}))
                stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
                argv = ["--dry-run"] if dry_run else []
                if mode is not None:
                    argv.extend(["--remote-mcp-mode", mode])
                if imported != digest:
                    with self.assertRaisesRegex(SystemExit, "host image digest differs"):
                        deploy.main(argv)
                else:
                    self.assertEqual(deploy.main(argv), 0)
                commands = [call.args[0] for call in wrapper_run.call_args_list]
                if dry_run:
                    self.assertTrue(all(command[0] != "ssh" for command in commands))
                    self.assertIn("materialize", commands[-1])
                    save.assert_not_called()
                else:
                    materialize = next(command[-1] for command in commands if " materialize " in command[-1])
                    self.assertIn("--package-sha256 " + package_sha, materialize)
                    self.assertIn("--output " + stage + "/payload", materialize)
                if dry_run or imported != digest:
                    run.assert_not_called()
                else:
                    host = shlex.split(run.call_args.args[0][-1])
                    self.assertEqual(host[2], stage + "/payload/deploy_host.py")
                    self.assertIn(stage + "/payload/fcd", host)
                    self.assert_mode(host, mode)

    def test_host_wrapper_forwards_only_explicit_mode(self):
        import deploy_host
        revision, digest = "b" * 40, "a" * 64
        before = {"spec": {"template": {"metadata": {}, "spec": {"containers": [
            {"name": "controller", "image": "localhost/firecontroller@sha256:" + "c" * 64}]}}}}
        for mode in (None, "enable", "disable"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory, contextlib.ExitStack() as stack:
                root = Path(directory)
                instance = root / "instance"
                instance.write_text("fixture-instance")
                snap = root / "snapshot"
                snap.write_bytes(b"fixture-snapshot")
                stack.enter_context(mock.patch.object(deploy_host, "DEPLOY_LOCK", root / "deploy.lock"))
                stack.enter_context(mock.patch.object(deploy_host, "_ensure_helper_generation_lease", return_value=root / "helper.lease"))
                stack.enter_context(mock.patch.object(deploy_host, "INSTANCE_FILE", instance))
                stack.enter_context(mock.patch.object(deploy_host, "sha256", return_value=digest))
                held = [contextlib.nullcontext(mock.Mock(fileno=lambda fd=fd: fd)) for fd in (10, 11, 12)]
                stack.enter_context(mock.patch.object(deploy_host, "flock_wait", side_effect=held))
                stack.enter_context(mock.patch.object(deploy_host, "kubectl_json", return_value=before))
                stack.enter_context(mock.patch.object(deploy_host, "snapshot", return_value=snap))
                stack.enter_context(mock.patch.object(deploy_host, "rehearse_migration", return_value={}))
                stack.enter_context(mock.patch.object(deploy_host, "_install_helpers_locked"))
                stack.enter_context(mock.patch.object(deploy_host, "_reconcile_pending_helpers_locked", return_value=False))
                stack.enter_context(mock.patch.object(deploy_host, "_begin_helper_rollout_locked"))
                stack.enter_context(mock.patch.object(deploy_host, "verify", return_value={}))
                stack.enter_context(mock.patch.object(deploy_host, "verify_assets"))
                stack.enter_context(mock.patch.object(deploy_host, "_select_verified_helper_generation_locked",
                                                      return_value={"current": {"source": revision}}))
                stack.enter_context(mock.patch.object(deploy_host, "prune_after_deploy", return_value={}))
                stack.enter_context(mock.patch.object(deploy_host, "log"))
                run = stack.enter_context(mock.patch.object(deploy_host.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, "{}")))
                argv = ["--source", revision, "--image", "localhost/firecontroller@sha256:" + digest,
                        "--fcd", str(root / "fcd"), "--fcd-sha256", digest]
                if mode is not None:
                    argv.extend(["--remote-mcp-mode", mode])
                self.assertEqual(deploy_host.main(argv), 0)
                command = run.call_args.args[0]
                self.assert_mode(command, mode)
                self.assertEqual(command[-3:], ["--kubectl", "k3s", "kubectl"])
                self.assertEqual(run.call_args.kwargs["pass_fds"], (10, 11, 12))
                self.assertEqual(run.call_args.kwargs["env"]["FC_ROLLOUT_CUSTODY_FDS"], "10,11,12")


class DrainDeadlineTests(unittest.TestCase):
    def test_waits_only_until_local_dispatch_finishes(self):
        states = iter([1, 1, 0])
        clock, sleeps = [0], []
        def sleep(seconds):
            sleeps.append(seconds)
            clock[0] += seconds
        forced = deployment.wait_for_drain(lambda: {"draining": True, "active_dispatches": next(states)}, 600, sleep, lambda: clock[0])
        self.assertFalse(forced)
        self.assertEqual(sleeps, [2, 2])

    def test_ten_minute_default_and_short_override_have_a_hard_deadline(self):
        self.assertEqual(deployment.DEFAULT_DRAIN_SECONDS, 600)
        clock = [0]
        def sleep(seconds):
            clock[0] += seconds
        self.assertTrue(deployment.wait_for_drain(lambda: {"draining": True, "active_dispatches": 1}, 3, sleep, lambda: clock[0]))
        self.assertEqual(clock[0], 3)

    def test_invalid_or_cleared_drain_never_looks_safe(self):
        for state in [{}, {"draining": False, "active_dispatches": 0}, {"draining": True, "active_dispatches": -1}, {"draining": True, "active_dispatches": False}]:
            with self.subTest(state=state), self.assertRaises(ValueError):
                deployment.wait_for_drain(lambda: state, 0)


class RecoveryWindowTests(unittest.TestCase):
    def test_transient_gateway_failure_is_rechecked_without_another_replace(self):
        clock = [0]
        calls = [0]
        def read():
            calls[0] += 1
            if calls[0] < 3:
                raise deployment.OperatorUnavailable("HTTP 503")
            return {"instance_id": "original"}, {"draining": False}
        def sleep(seconds):
            clock[0] += seconds
        deployment.wait_for_recovery(read, "original", 3, sleep, lambda: clock[0])
        self.assertEqual(calls[0], 3)
        self.assertEqual(clock[0], 2)

    def test_wrong_instance_fails_immediately_and_closed_admission_is_bounded(self):
        with self.assertRaises(ValueError):
            deployment.wait_for_recovery(lambda: ({"instance_id": "different"}, {"draining": False}), "original", 0)
        with self.assertRaises(RuntimeError):
            deployment.wait_for_recovery(lambda: ({"instance_id": "original"}, {"draining": True}), "original", 0)


class RolloutSequenceTests(unittest.TestCase):
    def exercise(self, forced=False, preflight_failure=False, replace_failure=False, memory=None,
                 remote_mode=None, controller_args=None, observed_args=None, invalid_args=False,
                 retention_mode=None, lease_environment=None, artifact_fds=(), replacement_fds=()):
        image = "ghcr.io/example/controller@sha256:" + "a" * 64
        revision = "b" * 40
        original = {"metadata": {"resourceVersion": "7"}, "spec": {
            "replicas": 1, "strategy": {"type": "Recreate"}, "selector": {"matchLabels": {"app": "fixture"}},
            "template": {"metadata": {"annotations": {"firecontroller.dev/binary-sha256": "old"}}, "spec": {
                "containers": [{"name": "controller", "image": "old", "volumeMounts": [{"name": "private", "mountPath": "/data"}]}, {"name": "proxy", "image": "proxy"}],
                "volumes": [{"name": "private", "hostPath": {"path": "/private"}}]}}}}
        current = copy.deepcopy(original)
        if controller_args is not None:
            original["spec"]["template"]["spec"]["containers"][0]["args"] = controller_args[:]
            current = copy.deepcopy(original)
        actions, http = [], []
        state = {"draining": False, "active_dispatches": 1 if forced else 0}

        class Opener:
            def open(self, request, timeout):
                method, path = request.get_method(), request.full_url.rsplit("/", 1)[-1]
                http.append((method, path))
                if path == "instance":
                    return io.BytesIO(json.dumps({"instance_id": "instance-fixture"}).encode())
                if method == "POST": state["draining"] = True
                if method == "DELETE": state["draining"] = False
                return io.BytesIO(json.dumps(state).encode())

        def run(command, **kwargs):
            nonlocal current
            args = command[command.index("-n") + 2:]
            expected_fds = replacement_fds if args[0] == "replace" else artifact_fds
            self.assertEqual(kwargs.get("pass_fds", ()), expected_fds)
            actions.append(args)
            if args[:2] == ["get", "deployment"]:
                return subprocess.CompletedProcess(command, 0, json.dumps(current))
            if args[:2] == ["get", "pods"]:
                return subprocess.CompletedProcess(command, 0, json.dumps({"items": [{"spec": {"nodeName": "fixture-node"}, "status": {"phase": "Running"}}]}))
            if args[0] == "create":
                pod = json.loads(kwargs["input"])
                self.assertNotIn("volumes", pod["spec"])
                self.assertNotIn("env", pod["spec"]["containers"][0])
                self.assertFalse(pod["spec"]["automountServiceAccountToken"])
                self.assertEqual(pod["spec"]["containers"][0]["image"], image)
                return subprocess.CompletedProcess(command, int(preflight_failure), "")
            if args[0] == "replace":
                self.assertIn(("POST", "deployment-drain"), http)
                changed = json.loads(kwargs["input"])
                self.assertEqual(changed["metadata"]["resourceVersion"], "7")
                self.assertEqual(changed["spec"]["template"]["spec"]["volumes"], original["spec"]["template"]["spec"]["volumes"])
                self.assertEqual(changed["spec"]["template"]["spec"]["containers"][1], original["spec"]["template"]["spec"]["containers"][1])
                self.assertEqual(changed["spec"]["template"]["metadata"]["annotations"]["firecontroller.dev/source-commit"], revision)
                self.assertNotIn("firecontroller.dev/binary-sha256", changed["spec"]["template"]["metadata"]["annotations"])
                self.assertRegex(changed["spec"]["template"]["metadata"]["annotations"]["firecontroller.dev/rollout-id"], r"^[0-9a-f]{32}$")
                self.assertEqual(changed["spec"]["template"]["spec"]["containers"][0].get("resources", {}).get("limits", {}).get("memory"), memory)
                expected_args = controller_args or []
                if remote_mode is not None:
                    expected_args = deployment.remote_mcp_args(expected_args, remote_mode, "https://controller.example")
                if retention_mode is not None:
                    expected_args = deployment.retention_args(expected_args, retention_mode)
                self.assertEqual(changed["spec"]["template"]["spec"]["containers"][0].get("args", []), expected_args)
                current = changed
                if observed_args is not None:
                    current["spec"]["template"]["spec"]["containers"][0]["args"] = observed_args
                state["draining"] = False
                return subprocess.CompletedProcess(command, int(replace_failure), "")
            return subprocess.CompletedProcess(command, 0, "")

        with tempfile.TemporaryDirectory() as directory:
            credential = Path(directory) / "admin.token"
            credential.write_text("fixture-administrator")
            argv = ["kubernetes.py", "--image", image, "--source-revision", revision, "--url", "https://controller.example", "--instance-id", "instance-fixture", "--credential-file", str(credential), "--drain-seconds", "0"] + (["--memory-limit", memory] if memory else [])
            if remote_mode is not None:
                argv.extend(["--remote-mcp-mode", remote_mode])
            if retention_mode is not None:
                argv.extend(["--retention-mode", retention_mode])
            output = io.StringIO()
            with (mock.patch("sys.argv", argv),
                  mock.patch.dict(os.environ, lease_environment or {}, clear=True),
                  mock.patch.object(deployment.subprocess, "run", side_effect=run),
                  mock.patch.object(deployment.urllib.request, "build_opener", return_value=Opener()),
                  contextlib.redirect_stdout(output)):
                if invalid_args:
                    with self.assertRaises(ValueError): deployment.main()
                elif preflight_failure or replace_failure or observed_args is not None:
                    with self.assertRaises(RuntimeError): deployment.main()
                else:
                    deployment.main()
                    result = json.loads(output.getvalue())
                    self.assertEqual(result["state"], "deployed")
                    self.assertEqual(result["drain_deadline_reached"], forced)
        return actions, http

    def test_same_digest_rollout_preserves_private_bindings_and_reports_deadline(self):
        self.exercise()
        self.exercise(forced=True)

    def test_memory_limit_is_applied_with_the_image(self):
        self.exercise(memory="1Gi")

    def test_remote_mcp_uses_the_guarded_rollout_and_preserves_sibling_settings(self):
        now = RemoteMCPArgumentTests.UNRELATED + ["-remote-mcp-origin=https://old.example"]
        for mode in (None, "enable", "disable"):
            with self.subTest(mode=mode):
                actions, _ = self.exercise(remote_mode=mode, controller_args=now)
                self.assertEqual(sum(action[0] == "replace" for action in actions), 1)
        self.exercise(remote_mode="enable", controller_args=now, memory="1Gi", retention_mode="enabled")

    def test_remote_mcp_checks_actually_observed_args_and_never_replaces_twice(self):
        for mode, observed in (("enable", []), ("disable", ["--remote-mcp-origin=https://old.example"]),
                               ("enable", ["--remote-mcp-origin=https://wrong.example"])):
            with self.subTest(mode=mode, observed=observed):
                actions, http = self.exercise(remote_mode=mode, observed_args=observed)
                self.assertEqual(sum(action[0] == "replace" for action in actions), 1)
                self.assertNotIn(("DELETE", "deployment-drain"), http)

    def test_invalid_remote_mcp_selector_refuses_before_preflight_or_drain(self):
        for now in (["--remote-mcp-origin="], ["--remote-mcp-origin=https://one.example", "-remote-mcp-origin=https://two.example"]):
            actions, http = self.exercise(remote_mode="enable", controller_args=now, invalid_args=True)
            self.assertEqual([action[0] for action in actions], ["get"])
            self.assertEqual(http, [])

    def test_failed_image_check_never_drains_and_removes_its_owned_pod(self):
        actions, http = self.exercise(preflight_failure=True)
        self.assertNotIn(("POST", "deployment-drain"), http)
        self.assertTrue(any(action[0] == "delete" for action in actions))
        self.assertFalse(any(action[0] == "replace" for action in actions))

    def test_uncertain_replacement_does_not_reopen_old_admission(self):
        _, http = self.exercise(replace_failure=True)
        self.assertNotIn(("DELETE", "deployment-drain"), http)

    @unittest.skipUnless(os.name == "posix", "inherited POSIX leases")
    def test_only_replace_inherits_rollout_custody_and_merges_artifact_lease(self):
        with contextlib.ExitStack() as stack:
            fds = tuple(stack.enter_context(tempfile.TemporaryFile()).fileno() for _ in range(4))
            custody, separate = fds[:3], fds[3]
            for artifact, replacement in ((None, custody), (separate, custody + (separate,)),
                                          (custody[1], custody)):
                with self.subTest(artifact=artifact):
                    environment = {"FC_ROLLOUT_CUSTODY_FDS": ",".join(map(str, custody))}
                    if artifact is not None:
                        environment["FC_ARTIFACT_LEASE_FD"] = str(artifact)
                    self.exercise(lease_environment=environment, replacement_fds=replacement,
                                  artifact_fds=() if artifact is None else (artifact,))

    @unittest.skipUnless(os.name == "posix", "inherited POSIX leases")
    def test_invalid_rollout_custody_refuses_before_credentials_or_external_actions(self):
        with tempfile.TemporaryFile() as first, tempfile.TemporaryFile() as second:
            reader, writer = os.pipe()
            self.addCleanup(os.close, reader)
            self.addCleanup(os.close, writer)
            with tempfile.TemporaryFile() as file:
                closed = file.fileno()
            prefix = f"{first.fileno()},{second.fileno()},"
            for raw in ("", "3,4", "3,4,5,6", prefix + " 5", prefix + "2", prefix + "9999999999",
                        prefix + str(first.fileno()), prefix + str(reader), prefix + str(closed)):
                with (self.subTest(raw=raw),
                      mock.patch.dict(os.environ, {"FC_ROLLOUT_CUSTODY_FDS": raw}, clear=True),
                      mock.patch.object(Path, "read_text") as read,
                      mock.patch.object(deployment.subprocess, "run") as run,
                      mock.patch.object(deployment.urllib.request, "build_opener") as opener):
                    with self.assertRaisesRegex(ValueError, "rollout custody"):
                        deployment.main()
                    read.assert_not_called()
                    run.assert_not_called()
                    opener.assert_not_called()


if __name__ == "__main__":
    unittest.main()


class ArtifactLeaseTests(unittest.TestCase):
    def test_absent_environment_preserves_legacy_options(self):
        import os
        from unittest.mock import patch
        with patch.dict(os.environ, {}, clear=True):
            self.assertEqual(deployment.artifact_lease_options(), {})

    def test_invalid_or_closed_descriptor_refuses(self):
        import os
        from unittest.mock import patch
        for value in ("", "0", "2", "-1", "abc", "3.0", "999999999999999"):
            with self.subTest(value=value), patch.dict(os.environ, {"FC_ARTIFACT_LEASE_FD":value}), self.assertRaises(ValueError):
                deployment.artifact_lease_options()
        with tempfile.TemporaryFile() as file:
            closed = file.fileno()
        with patch.dict(os.environ, {"FC_ARTIFACT_LEASE_FD":str(closed)}), self.assertRaises(ValueError):
            deployment.artifact_lease_options()

    @unittest.skipUnless(__import__("os").name == "posix", "inherited POSIX lease")
    def test_real_child_inherits_descriptor(self):
        import os
        import sys
        from unittest.mock import patch
        with tempfile.TemporaryFile() as file, patch.dict(os.environ, {"FC_ARTIFACT_LEASE_FD":str(file.fileno())}):
            options = deployment.artifact_lease_options()
            self.assertEqual(options, {"pass_fds":(file.fileno(),)})
            child = subprocess.run([sys.executable, "-c", "import os; os.fstat(int(os.environ['FC_ARTIFACT_LEASE_FD']))"], **options)
            self.assertEqual(child.returncode,0)

    def test_all_direct_subprocess_sites_preserve_lease(self):
        import ast
        tree=ast.parse(Path(deployment.__file__).read_text())
        calls=[node for node in ast.walk(tree) if isinstance(node,ast.Call) and isinstance(node.func,ast.Attribute) and isinstance(node.func.value,ast.Name) and node.func.value.id=="subprocess"]
        self.assertEqual(len(calls),3)
        for call in calls:
            self.assertTrue(any(keyword.arg is None and isinstance(keyword.value,ast.Call) and isinstance(keyword.value.func,ast.Name) and keyword.value.func.id=="artifact_lease_options" for keyword in call.keywords))

    def test_invalid_lease_refuses_before_any_external_action(self):
        import os
        from unittest.mock import patch
        with patch.dict(os.environ, {"FC_ARTIFACT_LEASE_FD":"invalid"}), patch.object(deployment.subprocess,"run") as run, patch.object(deployment.subprocess,"check_output") as output:
            with self.assertRaises(ValueError):
                deployment.main()
            run.assert_not_called()
            output.assert_not_called()
