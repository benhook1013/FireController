# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Populated acquisition proof with owned source, provider, and Git fixtures."""
import importlib
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import unittest
from unittest.mock import patch

import acquire_source_history as acquire
import test_source_acquisition as assembled_fixture
import test_source_acquisition_bindings as binding_fixture
import test_source_acquisition_frame as frame_fixture
from source_acquisition_bindings import bind_source


HEAD = "a" * 40
PARENT = "b" * 40
PATCH_ID = "c" * 64
NOW = "2026-01-02T03:04:05Z"


class SyntheticGit:
    """Owned proof facts; the scratch repository preflight remains real."""

    def remote_heads(self):
        return {"main": PARENT, "feature": HEAD}

    def branch_head(self, ref):
        return self.remote_heads()[ref]

    def branch_exists(self, ref):
        return ref in self.remote_heads()

    def is_ancestor(self, ancestor, descendant):
        return ancestor in (PARENT, HEAD) and descendant in (PARENT, HEAD)

    def merge_base(self, left, right):
        return PARENT

    def patch_identity(self, merge_base, head):
        return PATCH_ID

    def test_merge_tree(self, base, head):
        return "d" * 40


def checkpoint(comment_id, body, created_at, author="owned-fixture"):
    return {
        "databaseId": comment_id,
        "body": body,
        "createdAt": created_at,
        "updatedAt": created_at,
        "url": f"https://example.invalid/owned/comment/{comment_id}",
        "author": {"login": author},
    }


def pull_payload(comments, reviews=(), changed_files=0):
    return {"data": {"repository": {
        "databaseId": 17,
        "nameWithOwner": "owner/project",
        "defaultBranchRef": {"name": "main"},
        "pullRequest": {
            "number": 7,
            "state": "OPEN",
            "isDraft": False,
            "baseRefName": "main",
            "baseRefOid": PARENT,
            "headRefName": "feature",
            "headRefOid": HEAD,
            "headRepository": {"nameWithOwner": "owner/project"},
            "changedFiles": changed_files,
            "mergeable": "MERGEABLE",
            "mergedAt": None,
            "reviewThreads": {"nodes": [], "pageInfo": {"hasNextPage": False, "endCursor": None}},
            "comments": {"nodes": list(comments), "pageInfo": {"hasNextPage": False, "endCursor": None}},
            "reviews": {"nodes": list(reviews), "pageInfo": {"hasNextPage": False, "endCursor": None}},
        },
    }}}


class PopulatedAcquisitionTests(unittest.TestCase):
    def setUp(self):
        assembled_fixture.AcquisitionTests.setUp(self)
        plan_path = Path(self.args.plan)
        plan = json.loads(plan_path.read_bytes())
        plan["prs"][0]["channels"] = ["cli"]
        plan_path.write_text(json.dumps(plan))
        plan_path.chmod(0o600)

    def _select_channels(self, channels):
        plan_path = Path(self.args.plan)
        plan = json.loads(plan_path.read_bytes())
        plan["prs"][0]["channels"] = list(channels)
        plan_path.write_text(json.dumps(plan))
        plan_path.chmod(0o600)

    def _populate_cli_capture(self, *, accepted=False):
        helpers = binding_fixture.HELPERS
        records = helpers["sqlite_review_records"].SqliteReviewRecords(self.database)
        anchor = {
            "parent_ref": "main", "child_head_sha": HEAD, "parent_sha": PARENT,
            "merge_base": PARENT, "patch_identity": PATCH_ID,
        }
        attempt_metadata = {
            "run_id": "run.Synth1", "kind": "cli", "repository": "owner/project",
            "pull_request": 7, "candidate_sha": HEAD, **anchor,
        }
        if accepted:
            instructions = "**Synthetic accepted issue**\nEvidence: owned fixture finding."
            text_helper = importlib.import_module("pr_review.sqlite_provider_imports")
            finding_text = importlib.import_module("pr_review.sqlite_finding_text")
            finding_event = {"type": "finding", "codegenInstructions": instructions}
            findings = [helpers["sqlite_review_records"].FindingObservation(
                source_finding_key="cli-run:run.Synth1:finding:1",
                title=text_helper._cli_finding_title(instructions, ""),
                detail=finding_text._safe_finding_detail(text_helper._cli_detail(instructions)),
            )]
            completion = {"type": "complete", "status": "review_completed",
                          "findings": 1, "reviewedFiles": ["owned.py"]}
            accepted_count, candidate_files = 1, 1
        else:
            finding_event, findings = None, []
            completion = {"type": "complete", "status": "review_completed",
                          "findings": 0, "reviewedFiles": []}
            accepted_count, candidate_files = 0, 0
        capture_metadata = {
            **attempt_metadata, "capture_completion_marker": "capture-complete",
            "exit_status": 0, "duration_seconds": 1, "candidate_files": candidate_files,
        }
        events = "" if finding_event is None else json.dumps(finding_event, separators=(",", ":")) + "\n"
        events += json.dumps(completion, separators=(",", ":")) + "\n"
        records.start_attempt(attempt_id="run.Synth1", source_pr=7, channel="cli",
                              candidate_sha=HEAD, started_at=NOW, metadata=attempt_metadata)
        records.complete_attempt_run(
            "run.Synth1",
            finish={"state": "completed", "finished_at": NOW, "duration_seconds": 1,
                    "exit_status": 0,
                    "artifacts": {"metadata": json.dumps(capture_metadata, sort_keys=True),
                                  "cli_events": events}},
            run={"run_id": "run.Synth1", "source_pr": 7, "channel": "cli",
                 "findings": findings, "outcome": "completed", "attributable": True,
                 "source_head": HEAD, "reviewer": "owned-fixture", "scope": "narrow",
                 "started_at": NOW, "finished_at": NOW},
            finalize_empty=not accepted,
        )
        if accepted:
            finding_key = "cli-run:run.Synth1:finding:1"
            records.record_source_decision(
                "run.Synth1", finding_key, decision_id="decision.Synth1", decision="accepted",
                actor="owned-fixture", reason="synthetic accepted finding", decided_at=NOW,
            )
            records.finalize_run("run.Synth1", finalized_at=NOW)
            records.record_source_resolution(
                "run.Synth1", finding_key, source_pr=7, resolution_id="resolution.Synth1",
                fix_sha="e" * 40, actor="owned-fixture", proof_note="synthetic verified fix",
                resolved_at=NOW,
            )
            return ("CLI: 1 found / 1 accepted · " + HEAD[:12] +
                    " · 1 files\n<!-- firemud-cli-run: run.Synth1 -->")
        return "CLI: 0 found / 0 accepted · " + HEAD[:12] + " · 0 files\n<!-- firemud-cli-run: run.Synth1 -->"

    def _provider_facts(self, payload):
        github = importlib.import_module("pr_review.github")
        identity = {
            "number": 7, "state": "OPEN", "isDraft": False, "baseRefName": "main",
            "baseRefOid": PARENT, "headRefName": "feature", "headRefOid": HEAD,
            "headRepository": {"nameWithOwner": "owner/project"}, "changedFiles": 0,
            "mergeable": "MERGEABLE", "mergedAt": None,
        }
        return [
            patch.object(github, "run_gh_query", return_value={"data": {"repository": {
                "databaseId": 17, "nameWithOwner": "owner/project",
                "defaultBranchRef": {"name": "main"}}}}),
            patch.object(github, "fetch_pr_identity", return_value=identity),
            patch.object(github, "fetch_pr_identity_batch", return_value={7: identity}),
            patch.object(github, "fetch_pull_request", return_value=payload),
            patch.object(github, "branch_head", return_value=PARENT),
            patch("pr_review.controller.DefaultGitProvider", return_value=SyntheticGit()),
            patch.object(socket, "socket", side_effect=AssertionError("no network")),
        ]

    def _run_wrapper_function(self, *, checkpoint_body=None, payload=None, common=None):
        body = self._populate_cli_capture() if checkpoint_body is None else checkpoint_body
        if payload is None:
            payload = pull_payload([checkpoint(101, body, NOW)])
        args = self.args
        if common is not None:
            args = type("Args", (), vars(self.args).copy())()
            args.common = str(common)
        snapshot_before = self.database.read_bytes()
        patches = self._provider_facts(payload)
        with patches[0], patches[1], patches[2], patches[3], patches[4], patches[5], patches[6]:
            result = acquire.acquire(args)
        self.assertEqual(self.database.read_bytes(), snapshot_before)
        return result

    def test_populated_cli_sql_capture_resolves_exact_checkpoint(self):
        result = self._run_wrapper_function()
        self.assertEqual((result["outcome"], result["prs"], result["channels"]), ("created", 1, 1))
        frame = json.loads(Path(self.args.output).read_bytes())
        row = frame["prs"][0]["channels"]["cli"][0]
        self.assertEqual(row["source_run_id"], "run.Synth1")
        self.assertTrue(row["completed"] and row["attributable"] and row["anchored"])
        self.assertEqual((row["raw"], row["accepted"], row["head"]), (0, 0, HEAD))

    def test_populated_accepted_cli_finding_has_resolved_status(self):
        body = self._populate_cli_capture(accepted=True)
        result = self._run_wrapper_function(checkpoint_body=body)
        self.assertEqual((result["outcome"], result["channels"]), ("created", 1))
        row = json.loads(Path(self.args.output).read_bytes())["prs"][0]["channels"]["cli"][0]
        self.assertEqual((row["raw"], row["accepted"], row["source_resolution_status"]), (1, 1, "resolved"))
        self.assertEqual(row["source_run_id"], "run.Synth1")

    def test_populated_hosted_publication_projects_final_summary_and_anchor(self):
        cli_body = self._populate_cli_capture()
        self._select_channels(("cli", "hosted"))
        hosted = binding_fixture.HELPERS["hosted"]
        trigger_id, review_id = 201, 301
        trigger_time, review_time, checkpoint_time = NOW, "2026-01-02T03:04:06Z", "2026-01-02T03:04:07Z"
        command = hosted.FULL_COMMAND
        hosted_dir = self.common / "firemud/hosted/owner_project/pr-7"
        hosted_dir.mkdir(parents=True, mode=0o700)
        trigger_url = "https://example.invalid/owned/comment/201"
        trigger = {
            "schema_version": 2, "status": "posted", "repository": "owner/project", "pr_number": 7,
            "head_sha": HEAD,
            "anchor": {"child_head": HEAD, "parent_identity": "main", "parent_head": PARENT,
                       "merge_base": PARENT, "patch_id": PATCH_ID},
            "trigger": {"id": trigger_id, "created_at": trigger_time, "url": trigger_url,
                        "author_login": "owned-fixture", "type": "full", "command": command},
        }
        (hosted_dir / "trigger.json").write_text(json.dumps(trigger))
        comments = [
            checkpoint(101, cli_body, trigger_time),
            checkpoint(trigger_id, command, trigger_time),
            checkpoint(202, "Hosted: 0 found / 0 accepted · " + HEAD[:12] +
                       " · 0 files\n<!-- firemud-hosted-review: 301 -->", checkpoint_time),
        ]
        publication = "12345678-1234-1234-1234-123456789abc"
        attempt = "abcdefab-cdef-abcd-efab-cdefabcdefab"
        reviews = [{"databaseId": review_id, "author": {"login": "coderabbitai[bot]"},
                    "state": "COMMENTED", "submittedAt": review_time, "commit": {"oid": HEAD},
                    "body": "**Actionable comments posted: 0**\n"
                            f"<!-- coderabbit-review-publication v1 publication={publication} attempt={attempt} batch=1/1 -->"}]
        payload = pull_payload(comments, reviews)
        result = self._run_wrapper_function(checkpoint_body=cli_body, payload=payload)
        self.assertEqual((result["outcome"], result["channels"]), ("created", 2))
        row = json.loads(Path(self.args.output).read_bytes())["prs"][0]["channels"]["hosted"][0]
        self.assertEqual(row["source_run_id"], "hosted-review:301")
        self.assertTrue(row["completed"] and row["attributable"] and row["anchored"])
        self.assertEqual((row["raw"], row["accepted"], row["head"]), (0, 0, HEAD))

    def test_hosted_common_callback_uses_selected_handoff_without_git(self):
        hosted = binding_fixture.HELPERS["hosted"]
        evidence = binding_fixture.HELPERS["evidence"]
        original = evidence.git_common_dir
        with patch.object(subprocess, "run", side_effect=AssertionError("ambient Git must not run")), \
             patch.object(subprocess, "Popen", side_effect=AssertionError("ambient Git must not run")):
            with bind_source(binding_fixture.HELPERS, common=self.common, snapshot=self.database):
                self.assertEqual(hosted._git_common_dir(), self.common)
        self.assertIs(evidence.git_common_dir, original)

    def test_missing_checkpoint_sql_association_refuses(self):
        body = ("CLI: 0 found / 0 accepted · " + HEAD[:12] +
                " · 0 files\n<!-- firemud-cli-run: run.Missing -->")
        with self.assertRaises(Exception):
            self._run_wrapper_function(checkpoint_body=body)
        self.assertFalse(Path(self.args.output).exists())
        self.assertFalse(Path(self.args.provenance).exists())

    def test_corrupt_linked_sql_events_refuse_publication(self):
        self._populate_cli_capture()
        import sqlite3
        with sqlite3.connect(self.database) as connection:
            connection.execute("UPDATE review_artifacts SET content = ? WHERE attempt_id = ? AND kind = ?",
                               ("{}", "run.Synth1", "cli_events"))
        body = ("CLI: 0 found / 0 accepted · " + HEAD[:12] +
                " · 0 files\n<!-- firemud-cli-run: run.Synth1 -->")
        with self.assertRaises(Exception):
            self._run_wrapper_function(checkpoint_body=body)
        self.assertFalse(Path(self.args.output).exists())
        self.assertFalse(Path(self.args.provenance).exists())

    def test_wrong_handoff_layout_refuses_publication(self):
        body = self._populate_cli_capture()
        wrong = self.root / "wrong-handoff"
        wrong.mkdir(mode=0o700)
        with self.assertRaises(Exception):
            self._run_wrapper_function(checkpoint_body=body, common=wrong)
        self.assertFalse(Path(self.args.output).exists())
        self.assertFalse(Path(self.args.provenance).exists())

    def _subprocess_bootstrap(self, payload, *, fail_provider=False, stdout_failure=False):
        """Write an owned child bootstrap that patches only external boundaries."""
        fixture_path = self.root / "provider-fixture.json"
        fixture_path.write_text(json.dumps(payload))
        script = Path(acquire.__file__).resolve()
        bootstrap = self.root / "owned-bootstrap.py"
        bootstrap.write_text(
            "import json, socket, sys\n"
            "from pathlib import Path\n"
            "from unittest.mock import patch\n"
            "import os\n"
            f"sys.path.insert(0, {str(script.parent)!r})\n"
            "sys.path.insert(0, os.environ['FIRECONTROLLER_SOURCE_ROOT'] + '/dev-tools')\n"
            "import acquire_source_history as module\n"
            "import pr_review.github as github\n"
            "import pr_review.controller as controller\n"
            f"facts=json.loads(Path({str(fixture_path)!r}).read_text())\n"
            "identity={'number':7,'state':'OPEN','isDraft':False,'baseRefName':'main',"
            f"'baseRefOid':{PARENT!r},'headRefName':'feature','headRefOid':{HEAD!r},"
            "'headRepository':{'nameWithOwner':'owner/project'},'changedFiles':0,"
            "'mergeable':'MERGEABLE','mergedAt':None}\n"
            "class GitFacts:\n"
            f"  def remote_heads(self): return {{'main':{PARENT!r},'feature':{HEAD!r}}}\n"
            "  def branch_head(self, ref): return self.remote_heads()[ref]\n"
            "  def branch_exists(self, ref): return ref in self.remote_heads()\n"
            f"  def is_ancestor(self, a, d): return a in ({PARENT!r},{HEAD!r}) and d in ({PARENT!r},{HEAD!r})\n"
            f"  def merge_base(self, a, d): return {PARENT!r}\n"
            f"  def patch_identity(self, a, d): return {PATCH_ID!r}\n"
            f"  def test_merge_tree(self, a, d): return {'d' * 40!r}\n"
            "def blocked(*a, **kw): raise AssertionError('provider boundary must not be called on replay')\n"
            "class BrokenStdout:\n"
            "  def write(self, value): raise OSError('owned stdout failure')\n"
            "  def flush(self): pass\n"
            f"stdout_failure={bool(stdout_failure)!r}\n"
            f"bad={bool(fail_provider)!r}\n"
            "patches=[patch.object(github,'run_gh_query',side_effect=blocked if bad else None,"
            "return_value={'data':{'repository':{'databaseId':17,'nameWithOwner':'owner/project',"
            "'defaultBranchRef':{'name':'main'}}}}),"
            "patch.object(github,'fetch_pr_identity',side_effect=blocked if bad else None,return_value=identity),"
            "patch.object(github,'fetch_pr_identity_batch',side_effect=blocked if bad else None,return_value={7:identity}),"
            "patch.object(github,'fetch_pull_request',side_effect=blocked if bad else None,return_value=facts),"
            f"patch.object(github,'branch_head',side_effect=blocked if bad else None,return_value={PARENT!r}),"
            "patch.object(controller,'DefaultGitProvider',return_value=GitFacts()),"
            "patch.object(socket,'socket',side_effect=AssertionError('network forbidden'))]\n"
            "from contextlib import ExitStack\n"
            "with ExitStack() as stack:\n"
            "  [stack.enter_context(p) for p in patches]\n"
            "  if stdout_failure: sys.stdout=BrokenStdout()\n"
            "  status=module.main()\n"
            "  raise SystemExit(status)\n"
        )
        # The main call reads real CLI arguments; provider facts remain owned.
        return bootstrap

    def test_public_script_subprocess_main_creates_and_replays(self):
        body = self._populate_cli_capture()
        payload = pull_payload([checkpoint(101, body, NOW)])
        argv = [
            "--source-root", self.args.source_root, "--common", self.args.common,
            "--snapshot", self.args.snapshot, "--git-root", self.args.git_root,
            "--plan", self.args.plan, "--output", self.args.output,
            "--provenance", self.args.provenance,
        ]
        before = self.database.read_bytes()
        for replay in (False, True):
            bootstrap = self._subprocess_bootstrap(payload, fail_provider=replay)
            env = dict(os.environ)
            env["PYTHONDONTWRITEBYTECODE"] = "1"
            env["FIRECONTROLLER_SOURCE_ROOT"] = self.args.source_root
            command = [sys.executable, str(bootstrap), *argv]
            completed = subprocess.run(command, cwd=Path(__file__).resolve().parents[2],
                                       env=env, check=False, capture_output=True, text=True)
            self.assertEqual(completed.returncode, 0, completed.stderr)
            lines = completed.stdout.splitlines()
            self.assertEqual(len(lines), 1)
            result = json.loads(lines[0])
            self.assertEqual(result["outcome"], "unchanged" if replay else "created")
            self.assertEqual(result["replayed"], replay)
            self.assertEqual(self.database.read_bytes(), before)
        frame = json.loads(Path(self.args.output).read_bytes())
        self.assertEqual(frame["prs"][0]["channels"]["cli"][0]["source_run_id"], "run.Synth1")

    def test_public_script_stdout_failure_returns_compact_error(self):
        body = self._populate_cli_capture()
        payload = pull_payload([checkpoint(101, body, NOW)])
        bootstrap = self._subprocess_bootstrap(payload, stdout_failure=True)
        env = dict(os.environ)
        env["PYTHONDONTWRITEBYTECODE"] = "1"
        env["FIRECONTROLLER_SOURCE_ROOT"] = self.args.source_root
        argv = [
            "--source-root", self.args.source_root, "--common", self.args.common,
            "--snapshot", self.args.snapshot, "--git-root", self.args.git_root,
            "--plan", self.args.plan, "--output", self.args.output,
            "--provenance", self.args.provenance,
        ]
        completed = subprocess.run([sys.executable, str(bootstrap), *argv],
                                   cwd=Path(__file__).resolve().parents[2], env=env,
                                   check=False, capture_output=True, text=True)
        self.assertEqual(completed.returncode, 3, completed.stderr)
        self.assertEqual(completed.stdout, "")
        self.assertEqual(completed.stderr, '{"error":"output_report_failed"}\n')
        self.assertTrue(Path(self.args.provenance).exists())
        self.assertTrue(Path(self.args.output).exists())


if __name__ == "__main__":
    unittest.main()
