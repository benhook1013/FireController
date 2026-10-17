# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Stdlib tests; optional real-helper proof uses FIRECONTROLLER_SOURCE_ROOT.

The optional root must be an explicitly selected trusted source checkout. Every
SQLite/history/state input below is synthetic and exclusively owned by the test.
No installed source path or live controller state is discovered.
"""
import contextlib
import dataclasses
import importlib.util
import io
import json
import os
from pathlib import Path
import sqlite3
import tempfile
import types
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("prepare_source_controls", Path(__file__).with_name("prepare_source_controls.py"))
adapter = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(adapter)


class LocalContractTests(unittest.TestCase):
    def setUp(self):
        self.owned = tempfile.TemporaryDirectory()
        self.addCleanup(self.owned.cleanup)
        self.root = Path(self.owned.name)
        self.root.chmod(0o700)
        self.selected = dict(version=1, repository_id=17, expected_revision=0, source="copied-source", source_key="pr-7-cli", pull_number=7, channel="cli")

    def private(self, name, body):
        path = self.root / name
        path.write_bytes(body)
        path.chmod(0o600)
        return path

    def test_cli_rejects_duplicate_and_abbreviated_bindings(self):
        args = []
        for name in ("source-root", "snapshot", "history-file", "selection", "output", "provenance"):
            args.extend(["--" + name, "/private/" + name])
        self.assertEqual(adapter.parser().parse_args(args).snapshot, "/private/snapshot")
        for changed in (args + ["--snapshot", "/private/WRONG_INPUT"], ["--source-roo", "/private/source-root"] + args[2:], args + ["--unknown", "/private/WRONG_INPUT"]):
            diagnostics = io.StringIO()
            with contextlib.redirect_stderr(diagnostics), self.assertRaises(SystemExit) as error:
                adapter.parser().parse_args(changed)
            self.assertEqual(error.exception.code, 2)
            self.assertNotIn("WRONG_INPUT", diagnostics.getvalue())

    def test_strict_selection_and_duplicate_json(self):
        self.assertEqual(adapter.selection_input(self.selected), self.selected)
        for key, value in (("expected_revision", True), ("expected_revision", 1), ("pull_number", 0), ("repository_id", True), ("channel", "all"), ("source", "bad\nsource"), ("source", " spaced "), ("source_key", "bad\u0080key")):
            with self.subTest(key=key, value=value), self.assertRaises(adapter.Refusal):
                adapter.selection_input(dict(self.selected, **{key: value}))
        for body in (b'{"x":1,"x":2}', b'{"x":NaN}', b'\xff'):
            with self.subTest(body=body), self.assertRaises(adapter.Refusal):
                adapter.strict_json(body)

    def test_history_assertions_scope_and_unsettled_refuse(self):
        history = dict(version=1, repository_id=17, snapshot_sha256="a" * 64, projection_complete=True, quiescent=True,
                       prs=[dict(pull_number=7, channels={"cli": []})])
        self.assertEqual(adapter.selected_history(history, self.selected, "a" * 64), [])
        for key, value in (("quiescent", False), ("projection_complete", False), ("repository_id", 18), ("snapshot_sha256", "b" * 64)):
            with self.subTest(key=key), self.assertRaises(adapter.Refusal):
                adapter.selected_history(dict(history, **{key: value}), self.selected, "a" * 64)
        for flag in ("active_review", "active_reservation", "terminal_ambiguous", "ambiguous", "held"):
            changed = dict(history, prs=[dict(pull_number=7, channels={"cli": [dict(pr=7, **{flag: True})]})])
            with self.subTest(flag=flag), self.assertRaises(adapter.Refusal):
                adapter.selected_history(changed, self.selected, "a" * 64)
        changed = dict(history, prs=[dict(pull_number=7, channels={"cli": [dict(pr=8)]})])
        with self.assertRaises(adapter.Refusal):
            adapter.selected_history(changed, self.selected, "a" * 64)

    def test_input_private_symlink_size_and_sidecars(self):
        path = self.private("input", b"sealed")
        with adapter.private_input(str(path), 6) as (_, body):
            self.assertEqual(body, b"sealed")
        path.chmod(0o644)
        with self.assertRaises(adapter.Refusal):
            with adapter.private_input(str(path), 6):
                self.fail("public input admitted")
        path.chmod(0o600)
        link = self.root / "link"
        link.symlink_to(path)
        with self.assertRaises(adapter.Refusal):
            with adapter.private_input(str(link), 6):
                self.fail("symlink admitted")
        with self.assertRaises(adapter.Refusal):
            with adapter.private_input(str(path), 5):
                self.fail("oversize admitted")
        self.private("input-wal", b"x")
        with self.assertRaises(adapter.Refusal):
            with adapter.private_input(str(path), 6, snapshot=True):
                self.fail("unsealed input admitted")

    def test_source_change_detected_after_callback(self):
        path = self.private("changing", b"before")
        with self.assertRaises(adapter.Refusal) as result:
            with adapter.private_input(str(path), 64):
                path.write_bytes(b"after")
        self.assertEqual(result.exception.code, "source_changed")

    def test_output_create_exact_replay_and_no_overwrite(self):
        path = self.root / "prepared.json"
        self.assertEqual(adapter.publish(str(path), b"value\n"), "created")
        self.assertEqual(adapter.publish(str(path), b"value\n"), "unchanged")
        before = path.read_bytes()
        with self.assertRaises(adapter.Refusal):
            adapter.publish(str(path), b"changed\n")
        self.assertEqual(path.read_bytes(), before)
        link = self.root / "output-link"
        link.symlink_to(path)
        with self.assertRaises(adapter.Refusal):
            adapter.publish(str(link), before)
        self.assertEqual(list(self.root.glob(".prepared-controls-*")), [])

    def test_fake_canonical_helpers_are_used_for_dry_and_latch(self):
        # A tiny opaque helper fixture verifies delegation and refusal plumbing;
        # actual copied policy semantics are covered by the opt-in class below.
        item = types.SimpleNamespace(accepted=0, non_counting=False)
        policy = types.SimpleNamespace(Channel=lambda value: types.SimpleNamespace(value=value),
                                       required_taper=mock.Mock(return_value=3),
                                       taper_satisfied=mock.Mock(side_effect=[False, True]),
                                       _review_entries=mock.Mock(return_value=[item]))
        controller = types.SimpleNamespace(_default_period_progress=mock.Mock(return_value=dict(error=None, in_flight=0, minimum=3, maximum=6, used=2, taper_history=[item], taper_complete=False)))
        state = types.SimpleNamespace(ordered_prs=(7,), allocations={}, judgments=())
        facts = adapter.control_facts(dict(policy=policy, controller=types.SimpleNamespace(ReviewController=controller)), state, self.selected, [])
        self.assertEqual(facts, dict(minimum_remaining=1, maximum_remaining=4, initial_dry=1, taper=3, tapered=False, closed=False))
        policy._review_entries.assert_called_once_with([item])
        self.assertEqual(policy.taper_satisfied.call_count, 2)
        policy.taper_satisfied.assert_called_with(policy.Channel("cli"), [item], 1)


@unittest.skipUnless(os.environ.get("FIRECONTROLLER_SOURCE_ROOT"), "explicit trusted source root not selected")
class ActualSourceHelperTests(LocalContractTests):
    @classmethod
    def setUpClass(cls):
        cls.source_root = os.environ["FIRECONTROLLER_SOURCE_ROOT"]
        cls.helpers, _, _ = adapter.load_helpers(cls.source_root)

    def setUp(self):
        super().setUp()
        self.state_module = self.helpers["state"]
        self.state = self.state_module.ReviewState(ordered_prs=(7,))
        self.snapshot = self.root / "source.sqlite3"
        self.history_path = self.root / "history.json"
        self.selection_path = self.private("selection.json", adapter.canonical(self.selected))
        self.args = types.SimpleNamespace(source_root=self.source_root, snapshot=str(self.snapshot), history_file=str(self.history_path), selection=str(self.selection_path), output=str(self.root / "control.json"), provenance=str(self.root / "provenance.json"))

    def review(self, index, accepted=0, **changes):
        value = dict(pr=7, channel="cli", head="a" * 40, child_head="a" * 40, parent_identity="main", parent_head="b" * 40,
                     merge_base="c" * 40, patch_id="synthetic-patch", checkpoint=f"checkpoint-{index}", source_run_id=f"run-{index}",
                     completed=True, attributable=True, anchored=True, accepted=accepted, raw=accepted, corrected_state=True)
        value.update(changes)
        return value

    def inputs(self, history):
        with sqlite3.connect(self.snapshot) as db:
            db.execute("PRAGMA user_version=1")
            db.execute("CREATE TABLE controller_metadata(singleton INTEGER PRIMARY KEY,data_model_version INTEGER,min_writer_build INTEGER)")
            db.execute("CREATE TABLE review_state(singleton INTEGER PRIMARY KEY,state_json TEXT)")
            db.execute("INSERT INTO controller_metadata VALUES(1,?,?)", (self.state.schema_version, 10))
            db.execute("INSERT INTO review_state VALUES(1,?)", (json.dumps(self.state.to_dict()),))
        self.snapshot.chmod(0o600)
        value = dict(version=1, repository_id=17, snapshot_sha256=adapter.digest(self.snapshot.read_bytes()), projection_complete=True, quiescent=True,
                     prs=[dict(pull_number=7, channels={"cli": history})])
        self.private("history.json", adapter.canonical(value))

    def run_preparation(self):
        # Any accidental provider/network/subprocess work is a hard failure.
        with mock.patch("subprocess.run", side_effect=AssertionError("no subprocess")), \
                mock.patch("subprocess.Popen", side_effect=AssertionError("no subprocess")), \
                mock.patch("socket.socket", side_effect=AssertionError("no network")):
            result = adapter.prepare(self.args)
        return result, json.loads(Path(self.args.output).read_bytes())

    def test_partial_cli_two_and_deterministic_sealed_output(self):
        self.inputs([self.review(1), self.review(2)])
        before = self.snapshot.read_bytes()
        result, control = self.run_preparation()
        self.assertEqual((control["initial_dry"], control["taper"], control["tapered"], control["minimum_remaining"], control["maximum_remaining"]), (2, 3, False, 1, 4))
        self.assertEqual(len(control), 14)
        self.assertEqual(self.run_preparation()[0]["outcome"], "unchanged")
        self.assertEqual(before, self.snapshot.read_bytes())
        proof = json.loads(Path(self.args.provenance).read_bytes())
        self.assertFalse(proof["independently_verified_live_assertions"])
        self.assertEqual(proof["binding"]["snapshot_sha256"], adapter.digest(before))
        self.assertEqual(proof["binding"]["history_sha256"], adapter.digest(self.history_path.read_bytes()))
        self.assertEqual(control["source_sha256"], adapter.digest(adapter.canonical(proof["binding"])))
        self.assertEqual(result["sha256"], adapter.digest(Path(self.args.output).read_bytes()))
        self.assertFalse(any(Path(str(self.snapshot) + suffix).exists() for suffix in ("-wal", "-shm", "-journal")))

    def test_actual_command_entrypoint_and_exact_retry(self):
        self.inputs([self.review(1), self.review(2)])
        argv = ["--source-root", self.source_root, "--snapshot", str(self.snapshot), "--history-file", str(self.history_path), "--selection", str(self.selection_path), "--output", self.args.output, "--provenance", self.args.provenance]
        for expected in ("created", "unchanged"):
            with contextlib.redirect_stdout(io.StringIO()) as output, contextlib.redirect_stderr(io.StringIO()) as errors, mock.patch("subprocess.run", side_effect=AssertionError("no subprocess")), mock.patch("subprocess.Popen", side_effect=AssertionError("no subprocess")), mock.patch("socket.socket", side_effect=AssertionError("no network")):
                status = adapter.main(argv)
            self.assertEqual(status, 0)
            self.assertEqual(errors.getvalue(), "")
            self.assertEqual(json.loads(output.getvalue())["outcome"], expected)
            self.assertLess(len(output.getvalue()), 512)
            self.assertNotIn("synthetic-patch", output.getvalue())
        self.assertEqual(json.loads(Path(self.args.output).read_bytes())["initial_dry"], 2)

    def test_failed_stdout_preserves_prepared_exact_retry(self):
        self.inputs([self.review(1), self.review(2)])
        argv = ["--source-root", self.source_root, "--snapshot", str(self.snapshot), "--history-file", str(self.history_path), "--selection", str(self.selection_path), "--output", self.args.output, "--provenance", self.args.provenance]
        class FailedOutput:
            def write(self, value):
                raise OSError("synthetic closed output")
        with contextlib.redirect_stdout(FailedOutput()), contextlib.redirect_stderr(io.StringIO()) as errors:
            self.assertEqual(adapter.main(argv), 3)
        self.assertEqual(json.loads(errors.getvalue()), {"error": "output_report_failed"})
        before = Path(self.args.output).read_bytes()
        self.assertEqual(self.run_preparation()[0]["outcome"], "unchanged")
        self.assertEqual(Path(self.args.output).read_bytes(), before)

    def test_reset_and_sticky_latch_are_separate(self):
        self.inputs([self.review(1), self.review(2), self.review(3), self.review(4, 1)])
        _, control = self.run_preparation()
        self.assertEqual((control["initial_dry"], control["tapered"]), (0, True))

    def test_ignored_rows_do_not_invent_or_reset_dry(self):
        self.inputs([self.review(1), self.review(2), self.review(3, 1, non_counting=True), self.review(4, 1, correction=True), self.review(5, 1, completed=False), self.review(6, 1, attributable=False), self.review(7, 1, provisional=True)])
        _, control = self.run_preparation()
        self.assertEqual((control["initial_dry"], control["minimum_remaining"], control["tapered"]), (2, 1, False))

    def allocation(self, **changes):
        values = dict(pr=7, channel="cli", head="a" * 40, parent_identity="main", parent_head="b" * 40,
                      merge_base="c" * 40, patch_id="synthetic-patch", baseline_checkpoints=(), reason="Synthetic explicit allowance",
                      min_additional_completed=0, max_additional_completed=8)
        values.update(changes)
        allocation = self.state_module.ReviewAllocation(**values)
        self.state = dataclasses.replace(self.state, allocations={"7:cli": allocation})

    def timeline(self):
        return dict(pr=7, channel="cli", head="a" * 40, checkpoint="scope-timeline:complete", scope_timeline=True, scope_timeline_complete=True, completed=False, attributable=False, anchored=False, accepted=0, raw=0, non_counting=True)

    def test_bounded_minimum_preserves_early_sticky_latch(self):
        self.allocation(min_additional_completed=4)
        self.inputs([self.timeline(), self.review(1), self.review(2), self.review(3)])
        _, control = self.run_preparation()
        self.assertEqual((control["minimum_remaining"], control["maximum_remaining"], control["initial_dry"], control["tapered"]), (1, 5, 3, True))

    def test_accepted_tranche_establishes_fresh_boundary(self):
        self.allocation()
        self.inputs([self.timeline(), self.review(1), self.review(2), self.review(3), self.review(4, 1), self.review(5)])
        _, control = self.run_preparation()
        self.assertEqual((control["initial_dry"], control["tapered"], control["maximum_remaining"]), (1, False, 3))

    def test_custom_effective_threshold_is_preserved(self):
        override = self.state_module.PolicyOverride(cli_zero_useful=2, head="a" * 40, checkpoint="checkpoint-2", reason="Synthetic custom threshold", patch_id="synthetic-patch")
        self.state = dataclasses.replace(self.state, policy_overrides={"7:cli": override})
        self.inputs([self.review(1), self.review(2)])
        _, control = self.run_preparation()
        self.assertEqual((control["taper"], control["initial_dry"], control["tapered"]), (2, 2, True))

    def test_stop_is_closure_only_without_output_or_counts(self):
        self.allocation(stop_basis="direct_human", stop_head="a" * 40, stop_parent_identity="main", stop_parent_head="b" * 40, stop_reason="Synthetic explicit stop")
        self.inputs([])
        with self.assertRaises(adapter.Refusal) as result:
            adapter.prepare(self.args)
        self.assertEqual(result.exception.code, "closure_only")
        self.assertEqual(result.exception.details["numeric_counts"], "unknown")
        self.assertFalse(Path(self.args.output).exists())
        self.assertFalse(Path(self.args.provenance).exists())

    def test_unknown_unbounded_controls_refuse(self):
        self.allocation(min_additional_completed=2, max_additional_completed=None)
        self.inputs([])
        with self.assertRaises(adapter.Refusal) as result:
            adapter.prepare(self.args)
        self.assertEqual(result.exception.code, "controls_unsupported")

    def test_ambiguous_timeline_refuses(self):
        self.allocation()
        self.inputs([self.review(1)])
        with self.assertRaises(adapter.Refusal) as result:
            adapter.prepare(self.args)
        self.assertEqual(result.exception.code, "projection_invalid")

    def test_helper_projection_disagreement_refuses(self):
        self.allocation()
        self.inputs([self.timeline(), self.review(1), self.review(2), self.review(3)])
        with mock.patch.object(self.helpers["policy"], "taper_satisfied_for_state", return_value=False), self.assertRaises(adapter.Refusal) as result:
            adapter.prepare(self.args)
        self.assertEqual(result.exception.code, "projection_invalid")

    def test_source_history_changed_during_projection_refuses(self):
        self.inputs([self.review(1), self.review(2)])
        original = adapter.control_facts
        def change(*args):
            result = original(*args)
            self.history_path.write_bytes(self.history_path.read_bytes() + b" ")
            return result
        with mock.patch.object(adapter, "control_facts", side_effect=change), self.assertRaises(adapter.Refusal) as result:
            adapter.prepare(self.args)
        self.assertEqual(result.exception.code, "source_changed")
        self.assertFalse(Path(self.args.output).exists())

    def test_snapshot_changed_during_projection_refuses(self):
        self.inputs([self.review(1)])
        original = adapter.control_facts
        def change(*args):
            result = original(*args)
            with sqlite3.connect(self.snapshot) as db:
                db.execute("CREATE TABLE synthetic_change(value INTEGER)")
            return result
        with mock.patch.object(adapter, "control_facts", side_effect=change), self.assertRaises(adapter.Refusal) as result:
            adapter.prepare(self.args)
        self.assertEqual(result.exception.code, "source_changed")
        self.assertFalse(Path(self.args.output).exists())

    def test_unsupported_threshold_and_bounds_are_not_rewritten(self):
        self.allocation(max_additional_completed=1001)
        self.inputs([self.timeline()])
        with self.assertRaises(adapter.Refusal) as result:
            adapter.prepare(self.args)
        self.assertEqual(result.exception.code, "controls_unsupported")
        self.assertFalse(Path(self.args.output).exists())
        # A distinct owned input set exercises an actually applicable override.
        self.snapshot.unlink()
        self.state = dataclasses.replace(self.state, allocations={}, policy_overrides={"7:cli": self.state_module.PolicyOverride(cli_zero_useful=1001, head="a" * 40, checkpoint="checkpoint-1", patch_id="synthetic-patch", reason="Synthetic unsupported threshold")})
        self.inputs([self.review(1)])
        with self.assertRaises(adapter.Refusal) as result:
            adapter.prepare(self.args)
        self.assertEqual(result.exception.code, "controls_unsupported")

    def test_safe_cli_errors_do_not_echo_source_helper_details(self):
        private_message = "private-source-detail-never-print"
        with mock.patch.object(adapter, "prepare", side_effect=RuntimeError(private_message)), contextlib.redirect_stderr(io.StringIO()) as errors:
            status = adapter.main(["--source-root", self.source_root, "--snapshot", str(self.snapshot), "--history-file", str(self.history_path), "--selection", str(self.selection_path), "--output", self.args.output, "--provenance", self.args.provenance])
        self.assertEqual(status, 3)
        self.assertEqual(json.loads(errors.getvalue()), {"error": "source_projection_refused"})
        self.assertNotIn(private_message, errors.getvalue())

    def test_source_helper_changed_after_projection_refuses(self):
        self.inputs([self.review(1)])
        original = adapter.source_hash
        calls = 0
        def changed(root):
            nonlocal calls
            calls += 1
            package, files, sha = original(root)
            return package, files, ("f" * 64 if calls == 2 else sha)
        with mock.patch.object(adapter, "source_hash", side_effect=changed), self.assertRaises(adapter.Refusal) as result:
            adapter.prepare(self.args)
        self.assertEqual(result.exception.code, "source_changed")
        self.assertFalse(Path(self.args.output).exists())


if __name__ == "__main__":
    unittest.main()
