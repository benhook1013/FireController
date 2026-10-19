#!/usr/bin/env python3
# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Offline, explicitly selected source-helper adapter; never acquires live history.

The history envelope is a SOURCE OWNER assertion of a canonical effective frame,
quiescence and authoritative obligation clearance. This utility checks their
shape/binding, not those live assertions.
An explicit source-root is trusted executable Python code, not an inert dataset.
"""
from __future__ import annotations

import argparse
import contextlib
import hashlib
import importlib
import json
import os
from pathlib import Path
import sqlite3
import stat
import sys
import tempfile
import unicodedata

MAX_SNAPSHOT = 32 << 20
MAX_HISTORY = 1 << 20
MAX_SELECTION = 16 << 10


class Refusal(Exception):
    def __init__(self, code, **details):
        super().__init__(code)
        self.code, self.details = code, details


def refuse(code):
    raise Refusal(code)


def canonical(value):
    return (json.dumps(value, sort_keys=True, ensure_ascii=True, separators=(",", ":"), allow_nan=False) + "\n").encode()


def digest(body):
    return hashlib.sha256(body).hexdigest()


def integer(value, lower, upper):
    return type(value) is int and lower <= value <= upper


def exact_keys(value, keys):
    return type(value) is dict and set(value) == set(keys)


def strict_json(body):
    def pairs(items):
        out = {}
        for key, value in items:
            if key in out:
                refuse("input_invalid")
            out[key] = value
        return out

    def bad_constant(_):
        refuse("input_invalid")

    try:
        return json.loads(body.decode("utf-8"), object_pairs_hook=pairs, parse_constant=bad_constant)
    except (UnicodeError, ValueError, RecursionError):
        refuse("input_invalid")


def normalized_path(raw):
    path = Path(raw)
    if not path.is_absolute() or str(path) != raw or os.path.normpath(raw) != raw:
        refuse("path_invalid")
    return path


def private_regular(info):
    return (stat.S_ISREG(info.st_mode) and info.st_mode & 0o077 == 0
            and (not hasattr(os, "getuid") or info.st_uid == os.getuid()))


def identity(info):
    return (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_mode, info.st_uid)


@contextlib.contextmanager
def private_input(raw, maximum, *, snapshot=False):
    path = normalized_path(raw)
    try:
        initial = path.lstat()
        if not private_regular(initial) or not 0 < initial.st_size <= maximum:
            refuse("input_file_invalid")
        if snapshot:
            no_sidecars(path)
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        try:
            handle = os.fdopen(fd, "rb")
        except BaseException:
            os.close(fd)
            raise
    except OSError:
        refuse("input_unavailable")
    with handle:
        try:
            if identity(os.fstat(handle.fileno())) != identity(initial):
                refuse("source_changed")
            body = handle.read(maximum + 1)
            if len(body) != initial.st_size:
                refuse("source_changed")
        except OSError:
            refuse("input_unavailable")
        try:
            yield path, body
        finally:
            try:
                current = path.lstat()
                handle.seek(0)
                if identity(current) != identity(initial) or digest(handle.read(maximum + 1)) != digest(body):
                    refuse("source_changed")
                if snapshot:
                    no_sidecars(path)
            except OSError:
                refuse("input_unavailable")


def no_sidecars(path):
    for suffix in ("-wal", "-shm", "-journal"):
        if os.path.lexists(str(path) + suffix):
            refuse("snapshot_unsealed")


def source_hash(root):
    package = normalized_path(root) / "dev-tools" / "pr_review"
    if not package.is_dir():
        refuse("source_unsupported")
    files, total = {}, 0
    # Hash the complete selected source package, not an ambient installed module.
    for path in sorted(package.rglob("*.py")):
        info = path.lstat()
        total += info.st_size
        if not stat.S_ISREG(info.st_mode) or total > 16 << 20 or len(files) >= 256:
            refuse("source_unsupported")
        files[path.relative_to(package).as_posix()] = digest(path.read_bytes())
    if not {"__init__.py", "state.py", "controller.py", "policy.py", "sqlite_store.py"} <= files.keys():
        refuse("source_unsupported")
    return package, files, digest(canonical(files))


def load_helpers(root):
    package, hashes, code_hash = source_hash(root)
    sys.dont_write_bytecode = True
    sys.path.insert(0, str(package.parent))
    try:
        modules = {name: importlib.import_module("pr_review." + name)
                   for name in ("state", "controller", "policy", "sqlite_store")}
        for module in tuple(modules.values()) + (importlib.import_module("pr_review"),):
            if not Path(module.__file__).resolve().is_relative_to(package.resolve()):
                refuse("source_unsupported")
        checks = (
            (modules["state"].ReviewState, "from_dict"),
            (modules["controller"].ReviewController, "_bounded_allocation_evidence"),
            (modules["controller"].ReviewController, "_bounded_allocation_taper_baseline"),
            (modules["controller"].ReviewController, "_bounded_allocation_taper_source_run_baseline"),
            (modules["controller"].ReviewController, "_default_period_progress"),
            (modules["controller"].ReviewController, "_durable_stop_allocations"),
            (modules["policy"], "fresh_taper_history"), (modules["policy"], "required_taper"),
            (modules["policy"], "taper_satisfied"), (modules["policy"], "taper_satisfied_for_state"),
            (modules["policy"], "_review_entries"),
            (modules["sqlite_store"], "validate_controller_compatibility"),
            (modules["sqlite_store"], "load_validated_controller_state"),
        )
        if any(not callable(getattr(owner, name, None)) for owner, name in checks):
            refuse("source_unsupported")
    except (ImportError, AttributeError, TypeError):
        refuse("source_unsupported")
    return modules, hashes, code_hash


def selection_input(value):
    keys = ("version", "repository_id", "expected_revision", "source", "source_key", "pull_number", "channel")
    if not exact_keys(value, keys) or value["version"] != 1 or type(value["version"]) is not int:
        refuse("selection_invalid")
    if (not integer(value["repository_id"], 1, (1 << 63) - 1)
            or not integer(value["expected_revision"], 0, 0)
            or not integer(value["pull_number"], 1, 2147483647)
            or value["channel"] not in ("cli", "hosted")):
        refuse("selection_invalid")
    for key in ("source", "source_key"):
        text = value[key]
        if (not isinstance(text, str) or not text or text != text.strip() or len(text.encode()) > 200
                or any(unicodedata.category(ch) == "Cc" for ch in text)):
            refuse("selection_invalid")
    return value


def selected_history(value, selected, snapshot_sha):
    if type(value) is not dict:
        refuse("history_invalid")
    # This independent owner assertion covers accepted-finding resolution and
    # incoming/outgoing routes. Unknown or unavailable clearance is not clear.
    # Quiescence alone only excludes active operations, not unfinished findings.
    if value.get("obligations_clear") is not True:
        refuse("controls_unsettled")
    if (not exact_keys(value, ("version", "repository_id", "snapshot_sha256", "projection_complete", "quiescent", "obligations_clear", "prs"))
            or type(value["version"]) is not int or value["version"] != 1
            or type(value["repository_id"]) is not int or value["repository_id"] != selected["repository_id"]
            or value["snapshot_sha256"] != snapshot_sha
            or value["projection_complete"] is not True or value["quiescent"] is not True
            or not isinstance(value["prs"], list) or not 1 <= len(value["prs"]) <= 100):
        refuse("history_invalid")
    found, seen = None, set()
    for pull in value["prs"]:
        if (not exact_keys(pull, ("pull_number", "channels"))
                or not integer(pull["pull_number"], 1, 2147483647)
                or pull["pull_number"] in seen or type(pull["channels"]) is not dict
                or not pull["channels"] or set(pull["channels"]) - {"cli", "hosted"}):
            refuse("history_invalid")
        seen.add(pull["pull_number"])
        for channel, history in pull["channels"].items():
            if not isinstance(history, list):
                refuse("history_invalid")
            for item in history:
                if type(item) is not dict or type(item.get("pr")) is not int or item["pr"] != pull["pull_number"]:
                    refuse("history_invalid")
                if item.get("channel", channel) != channel:
                    refuse("history_invalid")
                for key in ("completed", "attributable", "anchored", "provisional", "correction", "non_counting",
                            "active_review", "active_reservation", "ambiguous", "terminal_ambiguous", "held", "unstable"):
                    if key in item and item[key] is not None and type(item[key]) is not bool:
                        refuse("history_invalid")
                if "accepted" in item and not integer(item["accepted"], 0, (1 << 63) - 1):
                    refuse("history_invalid")
            if pull["pull_number"] == selected["pull_number"] and channel == selected["channel"]:
                found = history
    if found is None:
        refuse("history_invalid")
    if any(any(item.get(key) is True for key in ("active_review", "active_reservation", "ambiguous", "terminal_ambiguous", "held", "unstable", "unreconciled")) for item in found):
        refuse("controls_unsettled")
    for item in found:
        if "source_resolution_status" in item and item["source_resolution_status"] != "resolved":
            refuse("controls_unsettled")
        eligible_accepted = (item.get("completed") is True and item.get("attributable") is True
                             and not item.get("provisional") and not item.get("correction")
                             and not item.get("non_counting") and item.get("accepted", 0) > 0)
        if eligible_accepted and item.get("source_resolution_status") != "resolved":
            refuse("controls_unsettled")
    return found


def control_facts(helpers, state, selected, history):
    controller = helpers["controller"].ReviewController
    policy = helpers["policy"]
    pr, channel = selected["pull_number"], policy.Channel(selected["channel"])
    if pr not in state.ordered_prs:
        refuse("selection_invalid")
    allocation = state.allocations.get(f"{pr}:{channel.value}")
    if allocation is not None and allocation.stop_basis is not None:
        # The canonical stop helper needs no provider or instantiated controller.
        stopped = controller._durable_stop_allocations(object.__new__(controller), state, pr).get(channel.value)
        if not stopped or stopped.get("status") != "STOPPED":
            refuse("projection_invalid")
        raise Refusal("closure_only", pull_number=pr, channel=channel.value,
                      capability="fc reviews rounds close", numeric_counts="unknown")
    if allocation is not None:
        if allocation.max_additional_completed is None:
            refuse("controls_unsupported")
        snapshot = controller._bounded_allocation_evidence(allocation, history)
        if snapshot.get("error") is not None or snapshot.get("in_flight") != 0:
            refuse("projection_invalid")
        minimum = allocation.min_additional_completed or 0
        maximum, used = allocation.max_additional_completed, len(snapshot["results"])
        baselines = {
            "baseline_checkpoints": controller._bounded_allocation_taper_baseline(allocation, snapshot, history),
            "baseline_source_run_ids": controller._bounded_allocation_taper_source_run_baseline(allocation, snapshot, history),
        }
        frame = policy.fresh_taper_history(state, channel, history, **baselines)
        projected = policy.taper_satisfied_for_state(state, channel, history, **baselines)
    else:
        progress = controller._default_period_progress(state, pr, channel, history)
        if progress.get("error") is not None or progress.get("in_flight") != 0:
            refuse("projection_invalid")
        minimum, maximum, used = progress["minimum"], progress["maximum"], progress["used"]
        frame, projected = progress["taper_history"], progress["taper_complete"]
    # fresh_taper_history deliberately returns [] for an ambiguous reopen anchor;
    # an exporter must refuse that uncertainty instead of recording a fresh zero.
    reopen = next((item for item in reversed(state.judgments)
                   if item.pr == pr and item.channel == channel.value and item.decision == "reopen"), None)
    if reopen is not None:
        matches = [item for item in history if not item.get("correction") and not item.get("non_counting")
                   and item.get("head", item.get("reviewed_head")) == reopen.head
                   and item.get("checkpoint", item.get("checkpoint_id")) == reopen.checkpoint
                   and item.get("patch_id", item.get("patch_identity")) == reopen.patch_id]
        if len(matches) != 1:
            refuse("projection_invalid")
    threshold = policy.required_taper(state, channel, frame)
    tapered = policy.taper_satisfied(channel, frame, threshold)
    if type(projected) is not bool or type(tapered) is not bool or projected != tapered:
        refuse("projection_invalid")
    dry = 0
    # Reuse the canonical entry filter and single-entry eligibility evaluator;
    # do not duplicate its attribution/anchor/correction policy.
    entries = policy._review_entries([item for item in frame if not item.non_counting])
    for item in entries:
        if item.accepted > 0:
            dry = 0
        elif policy.taper_satisfied(channel, [item], 1):
            dry += 1
    if (not integer(minimum, 0, 1000) or not integer(maximum, 1, 1000)
            or minimum > maximum or not integer(used, 0, maximum)
            or not integer(dry, 0, 1000) or not integer(threshold, 1, 1000)):
        refuse("controls_unsupported")
    minimum, maximum = max(0, minimum - used), max(0, maximum - used)
    return dict(minimum_remaining=minimum, maximum_remaining=maximum, initial_dry=dry,
                taper=threshold, tapered=tapered, closed=False)


def check_output(raw, body):
    path = normalized_path(raw)
    parent = path.parent.lstat()
    if (not stat.S_ISDIR(parent.st_mode) or parent.st_mode & 0o077
            or (hasattr(os, "getuid") and parent.st_uid != os.getuid())):
        refuse("output_invalid")
    if os.path.lexists(path):
        with private_input(str(path), max(len(body), 1)) as (_, previous):
            if previous != body:
                refuse("output_conflict")
        return False
    return True


def publish(raw, body):
    if not check_output(raw, body):
        return "unchanged"
    path = Path(raw)
    fd, temporary = tempfile.mkstemp(prefix=".prepared-controls-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(body)
            handle.flush()
            os.fsync(handle.fileno())
        try:
            os.link(temporary, path)  # create-only: never replace another file
        except FileExistsError:
            if check_output(raw, body):
                refuse("output_conflict")
            return "unchanged"
        return "created"
    finally:
        os.unlink(temporary)


def prepare(args):
    helpers, hashes, code_hash = load_helpers(args.source_root)
    # All inputs remain open and unchanged through projection, then are verified
    # before any prepared output is published.
    with private_input(args.selection, MAX_SELECTION) as (_, selection_bytes), \
            private_input(args.history_file, MAX_HISTORY) as (_, history_bytes), \
            private_input(args.snapshot, MAX_SNAPSHOT, snapshot=True) as (path, snapshot_bytes):
        selected = selection_input(strict_json(selection_bytes))
        snapshot_sha, history_sha = digest(snapshot_bytes), digest(history_bytes)
        history = selected_history(strict_json(history_bytes), selected, snapshot_sha)
        uri = path.as_uri() + "?mode=ro&immutable=1"
        with contextlib.closing(sqlite3.connect(uri, uri=True)) as connection:
            connection.execute("PRAGMA query_only=ON")
            connection.execute("BEGIN")
            model = helpers["sqlite_store"].validate_controller_compatibility(connection)
            row = connection.execute("SELECT state_json FROM review_state WHERE singleton=1").fetchone()
            if row is None or not isinstance(row[0], str):
                refuse("snapshot_invalid")
            strict_json(row[0].encode())
            state = helpers["sqlite_store"].load_validated_controller_state(connection, data_model_version=model)
            facts = control_facts(helpers, state, selected, history)
        binding = dict(version=1, snapshot_sha256=snapshot_sha, history_sha256=history_sha,
                       source_code_sha256=code_hash)
        prepared = {key: selected[key] for key in ("version", "repository_id", "expected_revision", "pull_number", "channel", "source", "source_key")}
        prepared.update(source_sha256=digest(canonical(binding)), **facts)
        provenance = dict(version=1, kind="offline_source_control_preparation", binding=binding,
                          source_sha256=prepared["source_sha256"], source_helpers=hashes,
                          selection_sha256=digest(selection_bytes), prepared_sha256=digest(canonical(prepared)),
                          source_owner_assertions={"projection_complete": True, "quiescent": True, "obligations_clear": True},
                          independently_verified_live_assertions=False)
    if source_hash(args.source_root)[2] != code_hash:
        refuse("source_changed")
    if args.output == args.provenance or args.output in (args.snapshot, args.history_file, args.selection) or args.provenance in (args.snapshot, args.history_file, args.selection):
        refuse("output_invalid")
    output, proof = canonical(prepared), canonical(provenance)
    check_output(args.output, output)
    check_output(args.provenance, proof)
    publish(args.provenance, proof)
    outcome = publish(args.output, output)
    return dict(kind="prepared_source_controls", outcome=outcome, bytes=len(output),
                sha256=digest(output), source_sha256=prepared["source_sha256"])


class QuietParser(argparse.ArgumentParser):
    def error(self, message):
        self.exit(2, '{"error":"arguments_invalid"}\n')


class UniqueInput(argparse.Action):
    def __call__(self, parser, namespace, values, option_string=None):
        if getattr(namespace, self.dest, None) is not None:
            parser.error("each input option must occur exactly once")
        setattr(namespace, self.dest, values)


def parser():
    out = QuietParser(allow_abbrev=False, description=__doc__, epilog="History is owner-exported JSON v1: repository_id, snapshot_sha256, projection_complete:true, quiescent:true, obligations_clear:true, prs:[{pull_number,channels:{cli:[effective rows],hosted:[effective rows]}}]. Selection is v1: repository_id, expected_revision:0, source, source_key, pull_number, channel. Output is a native 14-field prepared control; separate provenance records input/helper hashes. obligations_clear asserts authoritative accepted-finding and incoming/outgoing route clearance; unknown/unavailable clearance is not clear. No live acquisition or independent assertion verification or migration-readiness claim.")
    for flag in ("source-root", "snapshot", "history-file", "selection", "output", "provenance"):
        out.add_argument("--" + flag, required=True, action=UniqueInput)
    return out


def report_result(result):
    try:
        print(json.dumps(result, sort_keys=True))
        sys.stdout.flush()
    except OSError:
        # A buffered failure must not be retried at interpreter shutdown.
        # Test/embedded streams may have no underlying file descriptor.
        try:
            target_fd = sys.stdout.fileno()
            sink_fd = os.open(os.devnull, os.O_WRONLY)
            if sink_fd != target_fd:
                try:
                    os.dup2(sink_fd, target_fd)
                finally:
                    os.close(sink_fd)
            # If opening devnull reused the closed stdout descriptor, ownership
            # passes directly to stdout for its remaining shutdown lifetime.
        except (AttributeError, OSError, ValueError):
            pass
        print('{"error":"output_report_failed"}', file=sys.stderr)
        return 3
    return 0


def main(argv=None):
    args = parser().parse_args(argv)
    try:
        result = prepare(args)
    except Refusal as exc:
        print(json.dumps(dict(error=exc.code, **exc.details), sort_keys=True), file=sys.stderr)
        return 3
    except Exception:
        # Explicit source helpers can raise their own exception classes. Their
        # errors may contain private record text; never print a traceback/body.
        print('{"error":"source_projection_refused"}', file=sys.stderr)
        return 3
    return report_result(result)


if __name__ == "__main__":
    raise SystemExit(main())
