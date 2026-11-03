# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Pure projection of selected canonical routes into native original holds."""
import json
import re
import unicodedata


class RouteRefusal(Exception):
    """A fixed local refusal code, without source exception text or records."""


_MAX_ROWS = 10000
_CHANNELS = {"hosted", "cli", "manual", "subagent"}
_COLLECTIONS = {
    "runs", "findings", "routes", "decisions", "source_resolutions",
    "source_resolution_corrections", "attempts", "corrections",
    "record_corrections", "provider_origins", "imported_artifacts",
    "historical_gaps", "historical_gap_artifacts",
}


def _pr(value):
    return type(value) is int and 0 < value < 2**31


def _text(value, maximum, *, empty=False):
    if type(value) is not str:
        return False
    try:
        size = len(value.encode("utf-8"))
    except UnicodeError:
        return False
    return (size <= maximum and (empty or bool(value.strip()))
            and not any(unicodedata.category(c) == "Cc" and c not in "\t\n\r"
                        for c in value))


def _identifier(value, maximum):
    # Evidence pointers contain opaque identifiers, never filesystem paths.
    return (type(value) is str and len(value) <= maximum
            and re.fullmatch(r"[A-Za-z0-9_.:-]+", value) is not None)


def _source_key(value):
    return (_text(value, 200) and value == value.strip()
            and not any(unicodedata.category(c) == "Cc" for c in value))


def _native_json(value):
    # Match Go's compact UTF-8 JSON/HTML escaping rather than charging every
    # non-ASCII character as an escape or overlooking HTML expansion.
    body = json.dumps(value, ensure_ascii=False, allow_nan=False,
                      separators=(",", ":"))
    for character, escaped in (("<", "\\u003c"), (">", "\\u003e"),
                               ("&", "\\u0026"), ("\u2028", "\\u2028"),
                               ("\u2029", "\\u2029")):
        body = body.replace(character, escaped)
    return body.encode("utf-8")


def _history(records, source_pr):
    try:
        history = records.history(source_pr, include_legacy_routes=True,
                                  include_display=False)
    except Exception:
        raise RouteRefusal("route_records_unavailable") from None
    if (type(history) is not dict or history.get("pr") != source_pr
            or type(history.get("pr")) is not int
            or set(history) - (_COLLECTIONS | {"pr"})
            or not {"runs", "findings"} <= set(history)):
        raise RouteRefusal("route_history_invalid")
    for name in set(history) - {"pr"}:
        rows = history[name]
        if (type(rows) is not list or len(rows) > _MAX_ROWS
                or any(type(row) is not dict for row in rows)):
            raise RouteRefusal("route_history_invalid")
    findings, runs = {}, {}
    for finding in history["findings"]:
        if not _identifier(finding.get("finding_id"), 64):
            raise RouteRefusal("route_history_invalid")
        findings.setdefault(finding["finding_id"], []).append(finding)
    for run in history["runs"]:
        if not _identifier(run.get("run_id"), 100):
            raise RouteRefusal("route_history_invalid")
        runs.setdefault(run["run_id"], []).append(run)
    return findings, runs


def selected_routes(records, selected_pull_numbers):
    """Return complete selected original route holds or one fixed refusal.

    Selection is an explicit set of positive 32-bit PR numbers. The canonical
    records object owns source reads; this module performs no other I/O.
    Selected legacy records without structured finding/run linkage refuse.
    """
    if (type(selected_pull_numbers) not in (set, frozenset)
            or not selected_pull_numbers
            or any(not _pr(number) for number in selected_pull_numbers)):
        raise RouteRefusal("route_selection_invalid")
    try:
        routes = records.list_routes(status="open", include_legacy_routes=True)
    except Exception:
        raise RouteRefusal("route_records_unavailable") from None
    if type(routes) is not list or len(routes) > _MAX_ROWS:
        raise RouteRefusal("route_list_invalid")
    seen, cache, obligations = set(), {}, []
    for route in routes:
        if (type(route) is not dict or not _identifier(route.get("route_id"), 194)
                or not _pr(route.get("source_pr")) or "target_pr" not in route
                or route["target_pr"] is not None and not _pr(route["target_pr"])
                or route.get("status") != "open"
                or route.get("origin") not in ("review_records", "legacy_controller")
                or type(route.get("source_channel")) is not str
                or route["source_channel"] not in _CHANNELS
                or not _source_key(route.get("source_finding_key"))):
            raise RouteRefusal("route_record_invalid")
        route_id = route["route_id"]
        if route_id in seen:
            raise RouteRefusal("route_identity_conflict")
        seen.add(route_id)
        if (route["origin"] == "review_records"
                and not _identifier(route.get("finding_id"), 64)):
            raise RouteRefusal("route_source_invalid")
        affected = sorted(selected_pull_numbers.intersection(
            {route["source_pr"], route["target_pr"]}))
        if not affected:
            continue
        if route["origin"] != "review_records":
            raise RouteRefusal("route_legacy_unsupported")
        source_pr = route["source_pr"]
        if source_pr not in cache:
            cache[source_pr] = _history(records, source_pr)
        findings, runs = cache[source_pr]
        matches = findings.get(route["finding_id"], [])
        if len(matches) != 1:
            raise RouteRefusal("route_source_invalid")
        finding = matches[0]
        run_id = finding.get("run_id")
        if (not _identifier(run_id, 100)
                or type(finding.get("source_pr")) is not int
                or finding["source_pr"] != source_pr
                or finding.get("source_channel") != route["source_channel"]
                or finding.get("source_finding_key") != route["source_finding_key"]
                or finding.get("route_id") != route_id
                or finding.get("disposition") != "routed"
                or not _text(finding.get("title"), 16 << 10)
                or not _text(finding.get("detail"), 16 << 10, empty=True)):
            raise RouteRefusal("route_source_invalid")
        linked = runs.get(run_id, [])
        if (len(linked) != 1 or type(linked[0].get("source_pr")) is not int
                or linked[0]["source_pr"] != source_pr
                or linked[0].get("channel") != route["source_channel"]):
            raise RouteRefusal("route_source_invalid")
        summary = finding["title"] + "\n" + finding["detail"]
        if not _text(summary, 16 << 10):
            raise RouteRefusal("route_original_too_large")
        evidence = ["route:" + route_id, "run:" + run_id,
                    "finding:" + finding["finding_id"],
                    "target-pr:" + (str(route["target_pr"])
                                    if route["target_pr"] is not None else "unassigned")]
        original = {"source_key": "route:" + route_id, "kind": "route",
                    "affected_pull_numbers": affected, "summary": summary,
                    "evidence": evidence, "original_status": "open"}
        if (not _source_key(original["source_key"]) or len(evidence) > 10
                or any(not _text(pointer, 1024) for pointer in evidence)
                or len(_native_json(original)) > 32 << 10):
            raise RouteRefusal("route_original_too_large")
        obligations.append(original)
    return sorted(obligations, key=lambda original: original["source_key"])
