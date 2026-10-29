import importlib.util
import contextlib
import copy
import io
import json
import subprocess
import tempfile
from unittest import mock
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("deployment", Path(__file__).with_name("kubernetes.py"))
deployment = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deployment)


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
    def exercise(self, forced=False, preflight_failure=False, replace_failure=False):
        image = "ghcr.io/example/controller@sha256:" + "a" * 64
        revision = "b" * 40
        original = {"metadata": {"resourceVersion": "7"}, "spec": {
            "replicas": 1, "strategy": {"type": "Recreate"}, "selector": {"matchLabels": {"app": "fixture"}},
            "template": {"metadata": {"annotations": {"firecontroller.dev/binary-sha256": "old"}}, "spec": {
                "containers": [{"name": "controller", "image": "old", "volumeMounts": [{"name": "private", "mountPath": "/data"}]}, {"name": "proxy", "image": "proxy"}],
                "volumes": [{"name": "private", "hostPath": {"path": "/private"}}]}}}}
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
                current = changed
                state["draining"] = False
                return subprocess.CompletedProcess(command, int(replace_failure), "")
            return subprocess.CompletedProcess(command, 0, "")

        with tempfile.TemporaryDirectory() as directory:
            credential = Path(directory) / "admin.token"
            credential.write_text("fixture-administrator")
            argv = ["kubernetes.py", "--image", image, "--source-revision", revision, "--url", "https://controller.example", "--instance-id", "instance-fixture", "--credential-file", str(credential), "--drain-seconds", "0"]
            output = io.StringIO()
            with mock.patch("sys.argv", argv), mock.patch.object(deployment.subprocess, "run", side_effect=run), mock.patch.object(deployment.urllib.request, "build_opener", return_value=Opener()), contextlib.redirect_stdout(output):
                if preflight_failure or replace_failure:
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

    def test_failed_image_check_never_drains_and_removes_its_owned_pod(self):
        actions, http = self.exercise(preflight_failure=True)
        self.assertNotIn(("POST", "deployment-drain"), http)
        self.assertTrue(any(action[0] == "delete" for action in actions))
        self.assertFalse(any(action[0] == "replace" for action in actions))

    def test_uncertain_replacement_does_not_reopen_old_admission(self):
        _, http = self.exercise(replace_failure=True)
        self.assertNotIn(("DELETE", "deployment-drain"), http)


if __name__ == "__main__":
    unittest.main()
