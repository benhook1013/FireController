# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Pure synthetic canonical-record fixtures; no source package or SQL is used."""
import copy
import json
import unittest
from unittest.mock import Mock, call

from source_obligation_routes import RouteRefusal, selected_routes


def route(identifier="a", source=7, target=8):
    return dict(route_id=identifier, origin="review_records", finding_id="f-" + identifier[:50],
                source_pr=source, target_pr=target, source_channel="cli",
                source_finding_key="key-" + identifier, status="open",
                title="Enriched display title must not replace the original")


def records_for(routes):
    ledgers = {}
    for item in routes:
        ledger = ledgers.setdefault(item["source_pr"], dict(pr=item["source_pr"],
                                                         runs=[], findings=[]))
        ledger["runs"].append(dict(run_id="run-" + item["route_id"][:90],
                                   source_pr=item["source_pr"], channel=item["source_channel"]))
        ledger["findings"].append(dict(
            run_id="run-" + item["route_id"][:90], finding_id=item["finding_id"],
            source_pr=item["source_pr"], source_channel=item["source_channel"],
            source_finding_key=item["source_finding_key"], route_id=item["route_id"],
            disposition="routed", title="Original title", detail="Exact detail"))
    return Mock(list_routes=Mock(return_value=routes),
                history=Mock(side_effect=lambda number, **_: ledgers[number])), ledgers


class SelectedRoutesTests(unittest.TestCase):
    def refuse(self, records, selected=frozenset({7}), code=None):
        with self.assertRaises(RouteRefusal) as failure:
            selected_routes(records, selected)
        if code is not None:
            self.assertEqual(failure.exception.args, (code,))
        return failure.exception

    def test_selected_incoming_outgoing_unassigned_and_original_bytes(self):
        routes = [route("incoming", 99, 7), route("outgoing", 8, 77),
                  route("unassigned", 7, None), route("both", 7, 8),
                  route("unselected", 55, None)]
        records, ledgers = records_for(routes)
        finding = ledgers[7]["findings"][1]
        finding["title"] = "  Original <title> é  "
        finding["detail"] = "\tInert `command` & exact \"bytes\"\r\nlast\n"
        original = copy.deepcopy((routes, ledgers))
        result = selected_routes(records, {8, 7})
        self.assertEqual([item["source_key"] for item in result],
                         ["route:both", "route:incoming", "route:outgoing", "route:unassigned"])
        self.assertEqual(result[0], dict(
            source_key="route:both", kind="route", affected_pull_numbers=[7, 8],
            summary=finding["title"] + "\n" + finding["detail"],
            evidence=["route:both", "run:run-both", "finding:f-both", "target-pr:8"],
            original_status="open"))
        self.assertEqual([item["affected_pull_numbers"] for item in result],
                         [[7, 8], [7], [8], [7]])
        self.assertEqual(result[-1]["evidence"][-1], "target-pr:unassigned")
        self.assertEqual((routes, ledgers), original)
        records.list_routes.assert_called_once_with(status="open", include_legacy_routes=True)
        self.assertEqual(records.history.call_args_list, [
            call(99, include_legacy_routes=True, include_display=False),
            call(8, include_legacy_routes=True, include_display=False),
            call(7, include_legacy_routes=True, include_display=False)])

    def test_selection_and_route_types_refuse_without_coercion(self):
        for selected in (None, [], (), {}, set(), {True}, {0}, {-1}, {2**31}, {"7"}, {7.0}):
            with self.subTest(selected=selected):
                records, _ = records_for([])
                self.refuse(records, selected, "route_selection_invalid")
                records.list_routes.assert_not_called()
        for field, value in (
                ("route_id", None), ("route_id", "/private/record"),
                ("source_pr", True), ("source_pr", "7"), ("target_pr", False),
                ("target_pr", 2**31), ("source_channel", []), ("source_channel", "other"),
                ("origin", "other"), ("status", "resolved"),
                ("source_finding_key", " spaced "), ("source_finding_key", "bad\nkey")):
            with self.subTest(field=field, value=value):
                item = route()
                item[field] = value
                self.refuse(Mock(list_routes=Mock(return_value=[item])), code="route_record_invalid")
        item = route()
        del item["target_pr"]
        self.refuse(Mock(list_routes=Mock(return_value=[item])), code="route_record_invalid")
        item = route(source=99, target=None)
        del item["finding_id"]
        self.refuse(Mock(list_routes=Mock(return_value=[item])), code="route_source_invalid")

    def test_duplicate_route_ids_refuse_instead_of_collapsing(self):
        for target in (8, 9):
            with self.subTest(target=target):
                first = route()
                second = dict(first, target_pr=target)
                records, _ = records_for([first])
                records.list_routes.return_value = [first, second]
                self.refuse(records, code="route_identity_conflict")

    def test_selected_legacy_record_is_explicitly_unsupported(self):
        item = route()
        item["origin"] = "legacy_controller"
        del item["finding_id"]
        records = Mock(list_routes=Mock(return_value=[item]))
        self.refuse(records, code="route_legacy_unsupported")
        records.history.assert_not_called()

    def test_missing_duplicate_and_mismatched_observation_or_run_refuse(self):
        mutations = {
            "missing-finding": lambda h: h["findings"].clear(),
            "duplicate-finding": lambda h: h["findings"].append(copy.deepcopy(h["findings"][0])),
            "missing-run": lambda h: h["runs"].clear(),
            "duplicate-run": lambda h: h["runs"].append(copy.deepcopy(h["runs"][0])),
        }
        for field, value in (("source_pr", 8), ("source_pr", True),
                             ("source_channel", "hosted"), ("source_finding_key", "other"),
                             ("finding_id", "other"), ("run_id", "other"),
                             ("route_id", "other"), ("disposition", "accepted"),
                             ("title", None), ("detail", None), ("detail", "bad\x00text")):
            mutations["finding-" + field + repr(value)] = (
                lambda h, field=field, value=value: h["findings"][0].update({field: value}))
        for field, value in (("run_id", "other"), ("source_pr", 8),
                             ("source_pr", True), ("channel", "hosted")):
            mutations["run-" + field + repr(value)] = (
                lambda h, field=field, value=value: h["runs"][0].update({field: value}))
        for name, mutate in mutations.items():
            with self.subTest(name=name):
                records, ledgers = records_for([route()])
                mutate(ledgers[7])
                self.refuse(records, code="route_source_invalid")

    def test_all_collections_and_routes_have_complete_bounds(self):
        records, _ = records_for([])
        for value in (None, (), iter(()), [None]):
            with self.subTest(routes=value):
                records.list_routes.return_value = value
                self.refuse(records, code="route_list_invalid" if value != [None]
                            else "route_record_invalid")
        unselected = [route("r" + str(n), 99, None) for n in range(10000)]
        records.list_routes.return_value = unselected
        self.assertEqual(selected_routes(records, {7}), [])
        records.history.assert_not_called()
        records.list_routes.return_value = unselected + [route("extra", 99, None)]
        self.refuse(records, code="route_list_invalid")
        for collection in ("runs", "findings", "decisions", "source_resolutions", "attempts"):
            for value in (None, (), [None], [{}] * 10001):
                with self.subTest(collection=collection, value_type=type(value)):
                    records, ledgers = records_for([route()])
                    ledgers[7][collection] = value
                    self.refuse(records, code="route_history_invalid")
        records, ledgers = records_for([route()])
        ledgers[7]["decisions"] = [{}] * 10000
        self.assertEqual(len(selected_routes(records, {7})), 1)

    def test_native_summary_json_key_and_evidence_budgets(self):
        records, ledgers = records_for([route("r" * 194)])
        finding = ledgers[7]["findings"][0]
        finding["title"] = "t"
        finding["detail"] = "é" * 8191
        result = selected_routes(records, {7})[0]
        self.assertEqual(len(result["summary"].encode()), 16 << 10)
        self.assertEqual(len(result["source_key"].encode()), 200)
        self.assertLessEqual(len(result["evidence"]), 10)
        self.assertTrue(all(len(p.encode()) <= 1024 for p in result["evidence"]))
        finding["detail"] += "x"
        self.refuse(records, code="route_original_too_large")
        finding["detail"] = "<" * 6000
        self.refuse(records, code="route_original_too_large")
        finding["detail"] = '"' * 16382
        self.refuse(records, code="route_original_too_large")

        finding["detail"] = "<" * 5000
        original = selected_routes(records, {7})[0]

        def encode(value):
            body = json.dumps(value, ensure_ascii=False, separators=(",", ":"))
            return body.replace("<", "\\u003c").encode()

        finding["detail"] += "x" * ((32 << 10) - len(encode(original)))
        self.assertEqual(len(encode(selected_routes(records, {7})[0])), 32 << 10)
        finding["detail"] += "x"
        self.refuse(records, code="route_original_too_large")
        records, _ = records_for([route("r" * 195)])
        self.refuse(records, code="route_record_invalid")

    def test_history_binding_and_helper_errors_use_fixed_local_codes(self):
        for history in (None, [], {}, {"pr": True, "runs": [], "findings": []},
                        {"pr": 8, "runs": [], "findings": []},
                        {"pr": 7, "runs": [], "findings": [], "unknown": []}):
            with self.subTest(history=history):
                records, _ = records_for([route()])
                records.history.side_effect = None
                records.history.return_value = history
                self.refuse(records, code="route_history_invalid")
        for method in ("list_routes", "history"):
            records, _ = records_for([route()])
            getattr(records, method).side_effect = ValueError("private source path and record")
            failure = self.refuse(records, code="route_records_unavailable")
            self.assertNotIn("private", str(failure))


if __name__ == "__main__":
    unittest.main()
