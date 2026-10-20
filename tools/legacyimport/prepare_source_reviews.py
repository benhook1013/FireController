#!/usr/bin/env python3
# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Prepare one additive review transfer from explicitly trusted source code/data.

No acquisition, source bootstrap, attempts, captures or receiving-route import.
Native transfer preview remains authoritative for destination admission.
"""
from __future__ import annotations

import contextlib
import importlib
import io
import os
from pathlib import Path
import re
import sqlite3
import sys
import time
from datetime import datetime, timezone

sys.dont_write_bytecode = True

from prepare_source_controls import (QuietParser, UniqueInput, Refusal, canonical,
    check_output, digest, exact_keys, integer, normalized_path, private_input,
    publish, refuse, source_hash, strict_json)

MAX_SELECTION = 256 << 10
MAX_PROJECTION = 1 << 20
CHANNELS = ("cli", "hosted", "manual", "subagent")
CORRECTION_NOTE = "Historical counts retain original finalization; findings include later source corrections."
SEVERITIES = {"Critical": "critical", "Major": "high", "Minor": "low", "Trivial": "info",
              "High": "high", "Medium": "medium", "Low": "low",
              **{v: v for v in ("critical", "high", "medium", "low", "info", "unknown")}}
PULL_KEYS = ("number", "title", "state", "head_sha", "base_ref", "base_sha", "ci_state", "mergeability", "run_ids")


def text(value, maximum, required=False):
    if (not isinstance(value, str) or len(value.encode("utf-8")) > maximum
            or required and not value.strip()
            or any(ord(c) == 127 or ord(c) < 32 and c not in "\t\r\n" for c in value)):
        refuse("source_field_invalid")
    return value


def sha(value, required=False):
    text(value, 40)
    if (required or value) and not re.fullmatch("[0-9a-f]{40}", value):
        refuse("source_sha_unsupported")
    return value


def clock(value, required=True):
    if value in (None, "") and not required:
        return "", None
    text(value, 64, True)
    if not re.fullmatch(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?(?:Z|[+-]\d\d:\d\d)", value):
        refuse("source_time_invalid")
    try:
        parsed = datetime.fromisoformat(re.sub(r"\.\d+", "", value).replace("Z", "+00:00"))
        if parsed.year < 1:
            refuse("source_time_invalid")
    except ValueError:
        refuse("source_time_invalid")
    # Preserve spelling/precision in output; this key retains nanoseconds for ordering.
    fraction = re.search(r"\.(\d+)", value)
    return value, (parsed.astimezone(timezone.utc).replace(microsecond=0),
                   (fraction[1] if fraction else "").ljust(9, "0"))


def wire_json(value):
    # Match Go encoding/json HTML safety; ensure_ascii also covers U+2028/2029.
    return canonical(value).replace(b"<", b"\\u003c").replace(b">", b"\\u003e").replace(b"&", b"\\u0026")


def counts(value):
    if (not exact_keys(value, ("found", "accepted", "routed"))
            or any(not integer(n, 0, (1 << 63)-1) for n in value.values())
            or value["accepted"] + value["routed"] > value["found"]):
        refuse("source_counts_invalid")
    return value.copy()


def selection_input(value):
    if (not exact_keys(value, ("version", "repository_id", "expected_revision", "publisher", "source", "pulls"))
            or type(value["version"]) is not int or value["version"] != 1
            or not integer(value["repository_id"], 1, (1 << 63)-1)
            or not integer(value["expected_revision"], 0, (1 << 63)-1)
            or type(value["pulls"]) is not list or not 1 <= len(value["pulls"]) <= 100):
        refuse("selection_invalid")
    for key in ("publisher", "source"):
        text(value[key], 128, True)
    seen, runs = set(), set()
    for p in value["pulls"]:
        if (not isinstance(p, dict) or not set(PULL_KEYS).issubset(p) or not set(p).issubset(set(PULL_KEYS) | {"diff", "is_draft", "merged_at"}) or not integer(p["number"], 1, 2147483647)
                or p["number"] in seen or type(p["run_ids"]) is not list or not 1 <= len(p["run_ids"]) <= 100):
            refuse("selection_invalid")
        seen.add(p["number"])
        text(p["title"], 500, True); text(p["base_ref"], 256)
        sha(p["head_sha"]); sha(p["base_sha"])
        if (p["state"] not in ("open", "merged", "closed", "unknown")
                or p["ci_state"] not in ("unknown", "pending", "success", "failure")
                or p["mergeability"] not in ("unknown", "mergeable", "conflicting")):
            refuse("selection_invalid")
        if "diff" in p:
            diff = p["diff"]
            if (not exact_keys(diff, ("files", "additions", "deletions"))
                    or any(not integer(n, 0, 2147483647) for n in diff.values())):
                refuse("selection_invalid")
        if "is_draft" in p and type(p["is_draft"]) is not bool:
            refuse("selection_invalid")
        if "merged_at" in p:
            if p["state"] != "merged" or not p["merged_at"]:
                refuse("selection_invalid")
            clock(p["merged_at"])
        for run in p["run_ids"]:
            text(run, 256, True)
            if run in runs:
                refuse("selection_invalid")
            runs.add(run)
    if len(runs) > 200:
        refuse("selection_invalid")
    return value


def bounded_database(connection):
    """Conservative whole-source admission before helper fetchall/JSON expansion."""
    connection.execute("PRAGMA query_only=ON")
    connection.execute("BEGIN")
    total_rows = total_bytes = 0
    tables = list(connection.execute("SELECT name,sql FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%'"))
    if not 1 <= len(tables) <= 100:
        refuse("snapshot_bounds")
    for name, sql in tables:
        if not re.fullmatch(r"[A-Za-z_][A-Za-z_0-9]*", name) or "VIRTUAL TABLE" in sql.upper():
            refuse("snapshot_bounds")
        columns = list(connection.execute('PRAGMA table_info("'+name+'")'))
        if not 1 <= len(columns) <= 100:
            refuse("snapshot_bounds")
        count = connection.execute('SELECT count(*) FROM "'+name+'"').fetchone()[0]
        total_rows += count
        if count > 5000 or total_rows > 20000:
            refuse("snapshot_bounds")
        # SQL lengths first: no oversized cell is fetched into Python/helper JSON.
        for column in columns:
            col = column[1].replace('"', '""')
            maximum, size = connection.execute('SELECT coalesce(max(length(CAST("'+col+'" AS BLOB))),0),coalesce(sum(length(CAST("'+col+'" AS BLOB))),0) FROM "'+name+'"').fetchone()
            total_bytes += size
            if maximum > 256 << 10 or total_bytes > 32 << 20:
                refuse("snapshot_bounds")
    return dict(tables=len(tables), rows=total_rows, cell_bytes=total_bytes)


def load_records(root, path):
    package, hashes, code_hash = source_hash(root)
    sys.dont_write_bytecode = True
    sys.path.insert(0, str(package.parent))
    module = importlib.import_module("pr_review.sqlite_review_records")
    for item in (module, importlib.import_module("pr_review")):
        if not Path(item.__file__).resolve().is_relative_to(package.resolve()):
            refuse("source_unsupported")
    if "sqlite_review_records.py" not in hashes or not callable(getattr(module.SqliteReviewRecords, "history_batch", None)):
        refuse("source_unsupported")

    class ImmutableRecords(module.SqliteReviewRecords):
        def _connect(self, *, read_only, deadline=None):
            if read_only is not True:
                refuse("source_write_refused")
            connection = sqlite3.connect(path.as_uri()+"?mode=ro&immutable=1", uri=True,
                                         isolation_level=None, timeout=1)
            connection.execute("PRAGMA query_only=ON")
            connection.execute("PRAGMA foreign_keys=ON")
            # Bound helper SQL work as well as source cells: malformed joins may
            # otherwise amplify a small source before the projection-size check.
            steps = 0
            deadline = time.monotonic() + 15
            def progress():
                nonlocal steps
                steps += 1000
                return int(steps > 2000000 or time.monotonic() > deadline)
            connection.set_progress_handler(progress, 1000)
            return connection

    return ImmutableRecords(path), hashes, code_hash


def relevant(history, name, run_id):
    rows = history.get(name)
    if type(rows) is not list or any(type(row) is not dict for row in rows):
        refuse("source_history_invalid")
    return [row for row in rows if row.get("run_id") == run_id]


def decision(history, run_id, finding_id, pr):
    roots = [r for r in relevant(history, "decisions", run_id)
             if r.get("finding_id") == finding_id and r.get("scope") == "source"]
    corrections = [r for r in relevant(history, "corrections", run_id) if r.get("finding_id") == finding_id]
    if len(roots) > 1 or corrections and not roots:
        refuse("source_decision_chain_invalid")
    if not roots:
        return None
    current = roots[0]
    if current.get("decision_pr") != pr:
        refuse("source_decision_chain_invalid")
    previous_time = None
    seen = set()
    for index, row in enumerate(roots + corrections):
        identifier = row.get("decision_id") if index == 0 else row.get("correction_id")
        text(identifier, 256, True)
        _, when = clock(row.get("decided_at"))
        if (identifier in seen or index and row.get("supersedes_id") != current_id
                or previous_time is not None and when < previous_time
                or row.get("decision") not in ("accepted", "routed", "rejected")
                or row.get("decision") != "routed" and row.get("target_pr") is not None
                or row.get("decision") == "routed" and row.get("target_pr") is not None and not integer(row["target_pr"], 1, 2147483647)):
            refuse("source_decision_chain_invalid")
        text(row.get("actor"), 128); text(row.get("reason"), 2 << 10)
        seen.add(identifier); current_id = identifier; previous_time = when; current = row
    return current


def finding_input(item, run, history, pr):
    if item.get("source_pr") != pr or item.get("source_channel") != run["channel"]:
        refuse("source_finding_invalid")
    identifier = text(item.get("finding_id"), 256, True)
    key = text(item.get("source_finding_key"), 256, True)
    disposition = item.get("disposition")
    if disposition not in ("unadjudicated", "accepted", "routed", "rejected"):
        refuse("source_finding_invalid")
    original_severity = item.get("display_severity")
    if original_severity is None or original_severity == "":
        severity = "unknown"
    elif not isinstance(original_severity, str) or original_severity not in SEVERITIES:
        refuse("source_severity_unsupported")
    else:
        severity = SEVERITIES[original_severity]
    effective = decision(history, run["run_id"], identifier, pr)
    if effective is None and disposition != "unadjudicated":
        refuse("source_decision_missing")
    result = dict(source_finding_id=identifier, title=text(item.get("title"), 500, True),
                  detail=text(item.get("detail"), 16 << 10), severity=severity,
                  disposition=effective["decision"] if effective else disposition,
                  decision_reason=effective["reason"] if effective else "",
                  actor=effective["actor"] if effective else "",
                  decided_at=effective["decided_at"] if effective else "",
                  fix_sha="", proof_note="", resolved_at="")
    resolutions = [r for r in relevant(history, "source_resolutions", run["run_id"]) if r.get("finding_id") == identifier]
    if len(resolutions) > 1:
        refuse("source_resolution_invalid")
    if resolutions:
        r = resolutions[0]
        if (result["disposition"] != "accepted" or r.get("outcome") != "accepted_fixed"
                or r.get("source_pr") != pr or r.get("source_channel") != run["channel"]
                or r.get("source_finding_key") != key):
            refuse("source_resolution_invalid")
        effective_sha = sha(r.get("fix_sha"), True)
        _, prior_time = clock(r.get("resolved_at"))
        text(r.get("actor"), 128); proof = text(r.get("proof_note"), 2 << 10)
        resolved = r["resolved_at"]
        chain = r.get("corrections")
        if type(chain) is not list:
            refuse("source_resolution_invalid")
        seen = set(); sequence = 0
        for correction in chain:
            identity = text(correction.get("correction_id"), 256, True)
            _, at = clock(correction.get("corrected_at"))
            if (identity in seen or not integer(correction.get("sequence"), sequence+1, (1 << 63)-1)
                    or sha(correction.get("expected_fix_sha"), True) != effective_sha or at < prior_time):
                refuse("source_resolution_invalid")
            text(correction.get("actor"), 128); text(correction.get("reason"), 2 << 10)
            proof = text(correction.get("proof_note"), 2 << 10)
            effective_sha = sha(correction.get("fix_sha"), True)
            resolved = correction["corrected_at"]; prior_time = at; sequence = correction["sequence"]; seen.add(identity)
        if sha(r.get("effective_fix_sha"), True) != effective_sha:
            refuse("source_resolution_invalid")
        result.update(fix_sha=effective_sha, proof_note=proof, resolved_at=resolved)
    return result


def run_input(run, history, pr):
    if (run.get("source_pr") != pr or run.get("channel") not in CHANNELS or run.get("outcome") != "completed"
            or run.get("finalized") is not True or type(run.get("attributable")) is not bool):
        refuse("source_run_unsupported")
    started, start = clock(run.get("started_at")); finished, finish = clock(run.get("finished_at"))
    if finish < start:
        refuse("source_time_invalid")
    current = counts(run.get("counts"))
    original = counts(run["original_counts"]) if "original_counts" in run else current
    selected_counts = original if run["finalized"] else current
    limits = run.get("coverage_limits")
    if type(limits) is not list or len(limits) > 16:
        refuse("source_field_invalid")
    limits = [text(v, 1024, True) for v in limits]
    observed = relevant(history, "findings", run["run_id"])
    identifiers = [f.get("finding_id") for f in observed]
    if any(not isinstance(v, str) for v in identifiers) or len(identifiers) != len(set(identifiers)):
        refuse("source_finding_invalid")
    for name in ("decisions", "corrections", "source_resolutions"):
        for row in relevant(history, name, run["run_id"]):
            if row.get("finding_id") not in identifiers or name == "decisions" and row.get("scope") != "source":
                refuse("source_decision_chain_invalid")
    findings = [finding_input(f, run, history, pr) for f in observed]
    keys = [f["source_finding_id"] for f in findings]
    if len(keys) != len(set(keys)) or len(keys) > 100 or len(keys) > selected_counts["found"]:
        refuse("source_counts_invalid")
    effective = {key: sum(f["disposition"] == key for f in findings) for key in ("accepted", "routed")}
    if current != original or len(keys) == selected_counts["found"] and any(effective[k] != selected_counts[k] for k in effective):
        if len(limits) >= 16:
            refuse("source_coverage_bounds")
        limits = limits + [CORRECTION_NOTE]
    return dict(source_run_id=text(run.get("run_id"), 256, True), channel=run["channel"],
                head_sha=sha("" if run.get("source_head") is None else run["source_head"]), reviewer=text(run.get("reviewer"), 128),
                scope=text(run.get("scope"), 128, True), outcome="completed", attributable=run["attributable"],
                complete=len(keys) == selected_counts["found"], started_at=started, finished_at=finished,
                **selected_counts, finalized=run["finalized"], coverage_limits=limits, findings=findings)


def prepare(args):
    # Source package is explicitly trusted executable input. Suppress accidental
    # source diagnostics, whose text could contain private records.
    with private_input(args.selection, MAX_SELECTION) as (_, selection_bytes), \
            private_input(args.snapshot, 32 << 20, snapshot=True) as (path, snapshot_bytes):
        selected = selection_input(strict_json(selection_bytes))
        with contextlib.closing(sqlite3.connect(path.as_uri()+"?mode=ro&immutable=1", uri=True)) as connection:
            bounds = bounded_database(connection)
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            records, hashes, code_hash = load_records(args.source_root, path)
            history = records.history_batch([p["number"] for p in selected["pulls"]],
                                            include_legacy_routes=False, include_display=False)
        if type(history) is not dict or set(history) != {p["number"] for p in selected["pulls"]}:
            refuse("source_history_invalid")
        projection = canonical(history)
        if len(projection) > MAX_PROJECTION:
            refuse("source_projection_bounds")
        pulls = []; run_total = findings_total = 0; count_projections = []
        for p in selected["pulls"]:
            h = history[p["number"]]
            if type(h) is not dict or h.get("pr") != p["number"]:
                refuse("source_history_invalid")
            raw_runs = h.get("runs")
            if type(raw_runs) is not list or any(type(r) is not dict for r in raw_runs):
                refuse("source_history_invalid")
            by_id = {r.get("run_id"): r for r in raw_runs}
            if len(by_id) != len(raw_runs) or not set(p["run_ids"]) <= by_id.keys():
                refuse("source_run_missing")
            runs = [run_input(by_id[r], h, p["number"]) for r in p["run_ids"]]
            for mapped in runs:
                original = by_id[mapped["source_run_id"]]
                count_projections.append(dict(run_id=mapped["source_run_id"], source_counts=original["counts"],
                    source_original_counts=original.get("original_counts"),
                    original_finalization_counts_available="original_counts" in original,
                    transfer_counts={key: mapped[key] for key in ("found", "accepted", "routed")},
                    effective_selected_findings={key: sum(f["disposition"] == key for f in mapped["findings"])
                        for key in ("accepted", "routed", "rejected", "unadjudicated")},
                    selected_findings_complete=mapped["complete"],
                    converter_added_coverage_limits=mapped["coverage_limits"][len(original["coverage_limits"]):],
                    source_severity=[dict(source_finding_key=f.get("source_finding_key"),
                        original_label=f.get("display_severity"), original_absent=f.get("display_severity") in (None, ""))
                        for f in relevant(h, "findings", original["run_id"])]))
            run_total += len(runs); findings_total += sum(len(r["findings"]) for r in runs)
            pulls.append(dict({k: v for k, v in p.items() if k != "run_ids"}, complete=False, runs=runs, attempts=[]))
        if findings_total > 1000:
            refuse("source_counts_invalid")
        prepared = dict(version=1, repository_id=selected["repository_id"], expected_revision=selected["expected_revision"],
                        publisher=selected["publisher"], groups=[dict(source=selected["source"], pulls=pulls)])
        output = wire_json(prepared)
        wrapper = wire_json(dict(preview_sha256="0"*64, batch=prepared))
        if len(output) > 256 << 10 or len(wrapper) > 256 << 10:
            refuse("transfer_bounds")
        provenance = dict(version=1, kind="offline_source_review_preparation", snapshot_sha256=digest(snapshot_bytes),
                          selection_sha256=digest(selection_bytes), source_code_sha256=code_hash, source_helpers=hashes,
                          prepared_sha256=digest(output), source_bounds=bounds,
                          source_history=history, selected_count_projections=count_projections, omissions=["attempts", "captures", "receiving_route_ledger", "native_execution_credit", "review_controls"],
                          effective_counts_and_correction_provenance="canonical counts and correction chains retained; original finalization counts unavailable unless original_counts supplied",
                          native_preview_required=True, repository_identity_asserted_by_selection=True)
    if source_hash(args.source_root)[2] != code_hash:
        refuse("source_changed")
    if (args.output == args.provenance or args.output in (args.snapshot, args.selection)
            or args.provenance in (args.snapshot, args.selection)
            or any(normalized_path(p).resolve().is_relative_to(normalized_path(args.source_root).resolve()) for p in (args.output, args.provenance))):
        refuse("output_invalid")
    proof = canonical(provenance)
    check_output(args.output, output); check_output(args.provenance, proof)
    publish(args.provenance, proof); outcome = publish(args.output, output)
    return dict(kind="prepared_source_reviews", outcome=outcome, pulls=len(pulls), runs=run_total,
                findings=findings_total, attempts=0, native_coverage=False, bytes=len(output), sha256=digest(output),
                archived_attempts=sum(len(h.get("attempts", [])) for h in history.values()),
                archived_routes=sum(len(h.get("routes", [])) for h in history.values()))


def parser():
    out = QuietParser(allow_abbrev=False, description=__doc__)
    for flag in ("source-root", "snapshot", "selection", "output", "provenance"):
        out.add_argument("--"+flag, required=True, action=UniqueInput)
    return out


def main(argv=None):
    import json
    try:
        result = prepare(parser().parse_args(argv))
    except Refusal as error:
        print(json.dumps({"error": error.code}), file=sys.stderr)
        return 3
    except Exception:
        print('{"error":"source_projection_refused"}', file=sys.stderr)
        return 3
    try:
        print(json.dumps(result, sort_keys=True), flush=True)
    except OSError:
        # Avoid a second buffered flush failure during interpreter shutdown.
        try:
            with open(os.devnull, "wb") as sink:
                os.dup2(sink.fileno(), sys.stdout.fileno())
        except (OSError, ValueError):
            pass
        print('{"error":"output_report_failed"}', file=sys.stderr)
        return 3
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
