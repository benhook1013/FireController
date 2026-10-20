# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Executable adapter proof with an explicitly selected synthetic trusted package."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import unittest

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).parent))
import prepare_source_reviews as adapter

AT = "2026-10-01T00:00:00Z"
LATER = "2026-10-01T01:00:00Z"
MOCK = '''import json
class SqliteReviewRecords:
 def __init__(self,path):self.path=path
 def history_batch(self,prs,*,include_legacy_routes,include_display):
  assert include_legacy_routes is False and include_display is False
  with self._connect(read_only=True) as c:
   assert c.execute('PRAGMA query_only').fetchone()[0]==1
   c.execute('BEGIN')
   frame=json.loads(c.execute('SELECT payload FROM frame').fetchone()[0])
   return {pr:frame[str(pr)] for pr in prs}
'''


def history():
    run = dict(run_id="real-source-run", source_pr=7, channel="cli", source_head="a"*40,
               reviewer="", scope="diff", coverage_limits=[], outcome="completed", attributable=True,
               started_at=AT, finished_at=LATER, counts=dict(found=1, accepted=1, routed=0), finalized=True)
    finding = dict(run_id=run["run_id"], finding_id="source-finding", source_finding_key="original-key",
                   source_pr=7, source_channel="cli", title="Original <title>", detail="Original\n<script>inert</script> & text",
                   disposition="accepted", route_id=None, display_severity="high")
    decision = dict(decision_id="original-decision", scope="source", run_id=run["run_id"],
                    finding_id="source-finding", decision_pr=7, target_pr=None,
                    decision="accepted", actor="original-actor", reason="Original reason", decided_at=LATER)
    return {"7":dict(pr=7, runs=[run], findings=[finding], decisions=[decision], corrections=[],
                     source_resolutions=[], source_resolution_corrections=[], attempts=[dict(attempt_id="archived")],
                     routes=[dict(route_id="archived-receiver")], record_corrections=[])}


class ReviewsAdapterTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(); self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name); self.root.chmod(0o700)
        self.source = self.root/"trusted"; self.package=self.source/"dev-tools"/"pr_review"
        self.package.mkdir(parents=True)
        for name in ("__init__", "state", "controller", "policy", "sqlite_store"):
            (self.package/(name+".py")).write_text("")
        self.module=self.package/"sqlite_review_records.py"; self.module.write_text(MOCK)
        self.snapshot=self.root/"sealed.sqlite"; self.frame=history()
        with sqlite3.connect(self.snapshot) as c:c.execute("CREATE TABLE frame(payload TEXT)")
        self.snapshot.chmod(0o600); self.store_frame()
        self.selection=dict(version=1,repository_id=17,expected_revision=0,publisher="migration",source="original-controller",
            pulls=[dict(number=7,title="Selected PR",state="merged",head_sha="a"*40,base_ref="main",base_sha="b"*40,
                        ci_state="unknown",mergeability="unknown",run_ids=["real-source-run"])])
        self.selected=self.root/"selection.json"; self.output=self.root/"transfer.json"; self.proof=self.root/"provenance.json"
        self.save_selection()

    def save_selection(self):
        self.selected.write_bytes(adapter.canonical(self.selection));self.selected.chmod(0o600)

    def store_frame(self):
        with sqlite3.connect(self.snapshot) as c:
            c.execute('DELETE FROM frame');c.execute('INSERT INTO frame VALUES(?)',(json.dumps(self.frame),))

    def command(self):
        args=[sys.executable,"-B",str(Path(adapter.__file__).resolve())]
        for key,value in (("source-root",self.source),("snapshot",self.snapshot),("selection",self.selected),("output",self.output),("provenance",self.proof)):
            args += ["--"+key,str(value)]
        return args

    def invoke(self, expected=0, **kwargs):
        env=dict(os.environ,PYTHONDONTWRITEBYTECODE="1")
        result=subprocess.run(self.command(),capture_output=not kwargs,env=env,timeout=10,**kwargs)
        self.assertEqual(result.returncode,expected,result.stderr if not kwargs else "safe subprocess refusal")
        if not kwargs:
            self.assertNotIn(b"Original",result.stdout+result.stderr)
            self.assertNotIn(str(self.root).encode(),result.stdout+result.stderr)
        return result

    def test_aggregate_cell_budget_admits_bounded_source_above_eight_mib(self):
        payload = 'UNSELECTED_PRIVATE_' + 'x' * (65536 - len('UNSELECTED_PRIVATE_'))
        with sqlite3.connect(self.snapshot) as connection:
            connection.execute('CREATE TABLE unrelated(payload TEXT)')
            connection.executemany('INSERT INTO unrelated VALUES(?)', [(payload,)] * 160)
        before = self.snapshot.read_bytes()
        with sqlite3.connect(self.snapshot) as connection:
            # SQL aggregates may read cells inside SQLite; no unselected large
            # body should be materialized as a Python result during admission.
            def bounded_rows(cursor, row):
                self.assertFalse(any(isinstance(value, str) and len(value) > 16384 for value in row))
                return row
            connection.row_factory = bounded_rows
            bounds = adapter.bounded_database(connection)
            self.assertGreater(bounds['cell_bytes'], 8 << 20)
            self.assertLess(bounds['cell_bytes'], 32 << 20)
        self.invoke()
        value = json.loads(self.output.read_bytes())
        self.assertEqual(len(value['groups'][0]['pulls'][0]['runs']), 1)
        self.assertEqual(value['groups'][0]['pulls'][0]['runs'][0]['found'], 1)
        self.assertNotIn(b'UNSELECTED_PRIVATE_', self.output.read_bytes() + self.proof.read_bytes())
        self.assertEqual(self.snapshot.read_bytes(), before)
        self.assertEqual(json.loads(self.proof.read_bytes())['source_bounds']['cell_bytes'], bounds['cell_bytes'])

    def test_aggregate_cell_budget_above_thirty_two_mib_refuses(self):
        with sqlite3.connect(':memory:') as connection:
            connection.execute('CREATE TABLE cells(payload TEXT)')
            connection.executemany('INSERT INTO cells VALUES(?)', [('x' * 65536,)] * 513)
            connection.commit()
            before = connection.execute('SELECT count(*),sum(length(payload)) FROM cells').fetchone()
            self.assertGreater(before[1], 32 << 20)
            with self.assertRaises(adapter.Refusal) as error:
                adapter.bounded_database(connection)
            self.assertEqual(error.exception.code, 'snapshot_bounds')
            self.assertEqual(connection.execute('SELECT count(*),sum(length(payload)) FROM cells').fetchone(), before)
        self.assertFalse(self.output.exists())
        self.assertFalse(self.proof.exists())

    def test_aggregate_change_keeps_per_table_and_total_row_bounds(self):
        for tables, rows in ((1, 5001), (5, 4001)):
            with self.subTest(tables=tables, rows=rows), sqlite3.connect(':memory:') as connection:
                for n in range(tables):
                    connection.execute('CREATE TABLE t%d(value INTEGER)' % n)
                    connection.executemany('INSERT INTO t%d VALUES(?)' % n, [(1,)] * rows)
                connection.commit()
                with self.assertRaises(adapter.Refusal) as error:
                    adapter.bounded_database(connection)
                self.assertEqual(error.exception.code, 'snapshot_bounds')

    def test_actual_cli_semantics_private_replay_and_conservation(self):
        before=self.snapshot.read_bytes(),self.selected.read_bytes(),self.module.read_bytes()
        self.invoke();result=json.loads(self.output.read_bytes());p=result["groups"][0]["pulls"][0];r=p["runs"][0]
        self.assertEqual(result["groups"][0]["source"],"original-controller");self.assertIs(p["complete"],False)
        self.assertEqual(p["attempts"],[]);self.assertEqual(r["source_run_id"],"real-source-run")
        self.assertEqual(r["started_at"],AT);self.assertEqual(r["reviewer"],"")
        self.assertEqual(r["findings"][0]["detail"],self.frame["7"]["findings"][0]["detail"])
        self.assertEqual(r["findings"][0]["actor"],"original-actor");self.assertTrue(r["complete"])
        self.assertEqual(r["findings"][0]["source_finding_id"],self.frame["7"]["findings"][0]["finding_id"])
        self.assertNotEqual(r["findings"][0]["source_finding_id"],self.frame["7"]["findings"][0]["source_finding_key"])
        self.assertIn(b"\\u003c",self.output.read_bytes());self.assertNotIn(b"<script>",self.output.read_bytes())
        proof=json.loads(self.proof.read_bytes());self.assertEqual(proof["source_history"]["7"]["attempts"],[dict(attempt_id="archived")])
        self.assertTrue(proof["repository_identity_asserted_by_selection"])
        self.assertIs(proof["selected_count_projections"][0]["original_finalization_counts_available"],False)
        old=self.output.read_bytes(),self.proof.read_bytes(),self.output.stat().st_mtime_ns
        self.assertEqual(json.loads(self.invoke().stdout)["outcome"],"unchanged")
        self.assertEqual(old,(self.output.read_bytes(),self.proof.read_bytes(),self.output.stat().st_mtime_ns))
        self.assertEqual(before,(self.snapshot.read_bytes(),self.selected.read_bytes(),self.module.read_bytes()))
        for p in (self.output,self.proof):self.assertEqual(p.stat().st_mode&0o777,0o600)
        self.assertFalse(list(self.root.rglob("__pycache__")))
        self.assertFalse(any(Path(str(self.snapshot)+s).exists() for s in ("-wal","-shm","-journal")))
        # Interrupted two-file publication recovers exactly, never rewrites proof.
        self.output.unlink();self.invoke();self.assertEqual(self.proof.read_bytes(),old[1])

    def test_corrected_decision_preserves_frozen_original_counts(self):
        h=self.frame["7"];h["runs"][0]["original_counts"]=dict(found=1,accepted=1,routed=0)
        h["runs"][0]["counts"]=dict(found=1,accepted=0,routed=0)
        h["corrections"]=[dict(correction_id="correction",supersedes_id="original-decision",run_id="real-source-run",
            finding_id="source-finding",decision="rejected",target_pr=None,actor="corrector",reason="Correction reason",decided_at=LATER)]
        self.store_frame();self.invoke();r=json.loads(self.output.read_bytes())["groups"][0]["pulls"][0]["runs"][0]
        self.assertIn(adapter.CORRECTION_NOTE,r["coverage_limits"]);self.assertEqual(r["accepted"],1);self.assertEqual(r["findings"][0]["disposition"],"rejected")
        self.assertEqual(r["findings"][0]["actor"],"corrector")
        projection=json.loads(self.proof.read_bytes())["selected_count_projections"][0]
        self.assertEqual(projection["source_counts"]["accepted"],0)
        self.assertEqual(projection["effective_selected_findings"]["rejected"],1)

    def test_effective_resolution_retains_correction_provenance(self):
        h=self.frame["7"];r=dict(resolution_id="resolution",run_id="real-source-run",finding_id="source-finding",
            source_pr=7,source_channel="cli",source_finding_key="original-key",outcome="accepted_fixed",fix_sha="c"*40,
            effective_fix_sha="d"*40,actor="fixer",proof_note="original proof",resolved_at=LATER,
            corrections=[dict(sequence=1,correction_id="fix-correction",expected_fix_sha="c"*40,fix_sha="d"*40,
                actor="corrector",reason="fix correction",proof_note="corrected proof",corrected_at=LATER)])
        h["source_resolutions"]=[r];self.store_frame();self.invoke()
        finding=json.loads(self.output.read_bytes())["groups"][0]["pulls"][0]["runs"][0]["findings"][0]
        self.assertEqual((finding["fix_sha"],finding["proof_note"]),("d"*40,"corrected proof"))
        self.assertEqual(finding["actor"],"original-actor")
        self.assertEqual(json.loads(self.proof.read_bytes())["source_history"]["7"]["source_resolutions"][0]["actor"],"fixer")

    def test_malformed_selected_history_refuses_without_publication(self):
        initial=copy.deepcopy(self.frame)
        cases=[lambda h:h["runs"][0].update(finalized=False),lambda h:h["runs"][0].update(finished_at=None),lambda h:h["runs"][0].update(outcome="active"),
            lambda h:h["runs"][0].update(counts=dict(found=0,accepted=1,routed=0)),
            lambda h:h["runs"][0].update(started_at="2026-10-02T00:00:00Z"),
            lambda h:h["findings"][0].update(detail="x"*16385),lambda h:h["findings"].append(copy.deepcopy(h["findings"][0])),
            lambda h:h["corrections"].append(dict(run_id="real-source-run",finding_id="source-finding",correction_id="x",supersedes_id="broken")),
            lambda h:h.update(decisions=[]),lambda h:h["runs"][0].update(channel="unknown"),
            lambda h:h["corrections"].append(dict(run_id="real-source-run",finding_id="absent",correction_id="dangling")),
            lambda h:h["decisions"].append(copy.deepcopy(h["decisions"][0])),
            lambda h:h["findings"][0].update(display_severity="Unexpected"),
            lambda h:h["corrections"].extend([
                dict(correction_id="cycle",supersedes_id="original-decision",run_id="real-source-run",finding_id="source-finding",decision="rejected",target_pr=None,actor="a",reason="r",decided_at=LATER),
                dict(correction_id="original-decision",supersedes_id="cycle",run_id="real-source-run",finding_id="source-finding",decision="accepted",target_pr=None,actor="a",reason="r",decided_at=LATER)])]
        for change in cases:
            self.frame=copy.deepcopy(initial);change(self.frame["7"]);self.store_frame()
            with self.subTest(change=change):self.invoke(3);self.assertFalse(self.output.exists());self.assertFalse(self.proof.exists())

    def test_correction_note_refuses_full_original_coverage_limits(self):
        h=self.frame["7"];h["runs"][0]["coverage_limits"]=["Original limit "+str(i) for i in range(16)]
        h["runs"][0]["original_counts"]=dict(found=1,accepted=0,routed=0)
        self.store_frame();self.invoke(3);self.assertFalse(self.output.exists())

    def test_private_inputs_sidecars_strict_selection_and_output_conflict(self):
        self.selected.chmod(0o644);self.invoke(3);self.selected.chmod(0o600)
        sidecar=Path(str(self.snapshot)+"-wal");sidecar.write_bytes(b"sealed refusal")
        self.invoke(3);sidecar.unlink()
        self.selection["pulls"][0]["run_ids"]*=2;self.save_selection();self.invoke(3)
        self.selection["pulls"][0]["run_ids"]=["real-source-run"];self.save_selection()
        self.invoke();old=self.output.read_bytes(),self.proof.read_bytes()
        self.selection["publisher"]="changed";self.save_selection();self.invoke(3)
        self.assertEqual((self.output.read_bytes(),self.proof.read_bytes()),old)
        self.selected.write_bytes(b'{"version":1,"version":1}');self.invoke(3)

    def test_broken_resolution_or_rejected_fix_refuses(self):
        h=self.frame["7"]
        h["source_resolutions"]=[dict(run_id="real-source-run",finding_id="source-finding",source_pr=7,
            source_channel="cli",source_finding_key="original-key",outcome="accepted_fixed",fix_sha="c"*40,
            effective_fix_sha="d"*40,actor="fixer",proof_note="proof",resolved_at=LATER,corrections=[])]
        self.store_frame();self.invoke(3)
        h["source_resolutions"][0]["effective_fix_sha"]="c"*40
        h["corrections"]=[dict(correction_id="rejected",supersedes_id="original-decision",run_id="real-source-run",
            finding_id="source-finding",decision="rejected",target_pr=None,actor="a",reason="r",decided_at=LATER)]
        self.store_frame();self.invoke(3);self.assertFalse(self.output.exists())

    def test_helper_mutation_and_write_connection_refuse(self):
        for injected in ("self._connect(read_only=False)","self.path.with_name(self.path.name+'-wal').write_text('sidecar')"):
            self.module.write_text(MOCK.replace("assert include_legacy_routes",injected+"\n  assert include_legacy_routes"))
            self.invoke(3);self.assertFalse(self.output.exists());self.assertFalse(self.proof.exists())
            sidecar=Path(str(self.snapshot)+"-wal")
            if sidecar.exists():sidecar.unlink()

    def test_hidden_history_cell_and_row_budgets_refuse(self):
        with sqlite3.connect(self.snapshot) as c:c.execute('CREATE TABLE unrelated(payload TEXT)');c.execute('INSERT INTO unrelated VALUES(?)',('x'*((256<<10)+1),))
        self.invoke(3)
        with sqlite3.connect(self.snapshot) as c:c.execute('DELETE FROM unrelated');c.executemany('INSERT INTO unrelated VALUES(?)',[("x",)]*5001)
        self.invoke(3)
        self.assertFalse(self.output.exists())

    def test_helper_errors_and_broken_stdout_are_private(self):
        self.module.write_text(MOCK.replace("assert include_legacy_routes", "raise RuntimeError('PRIVATE_SOURCE_SENTINEL')\n  assert include_legacy_routes"))
        result=self.invoke(3);self.assertNotIn(b"PRIVATE_SOURCE_SENTINEL",result.stderr)
        self.module.write_text(MOCK)
        with open('/dev/full','wb') as full:
            result=subprocess.run(self.command(),stdout=full,stderr=subprocess.PIPE,timeout=10)
        self.assertEqual(result.returncode,3);self.assertIn(b'output_report_failed',result.stderr)
        self.assertTrue(self.output.exists());self.invoke()

    def test_known_severity_mapping_unknown_and_partial_coverage(self):
        for label,want in (("Critical","critical"),("Major","high"),("Minor","low"),("Trivial","info"),(None,"unknown")):
            f=copy.deepcopy(self.frame["7"]["findings"][0]);f["display_severity"]=label
            self.assertEqual(adapter.finding_input(f,self.frame["7"]["runs"][0],self.frame["7"],7)["severity"],want)
        self.frame["7"]["runs"][0]["counts"]["found"]=2
        self.store_frame();self.invoke()
        run=json.loads(self.output.read_bytes())["groups"][0]["pulls"][0]["runs"][0]
        self.assertFalse(run["complete"]);self.assertEqual(run["coverage_limits"],[])
        self.assertEqual(len(run["findings"]),1)

    @unittest.skipUnless(os.environ.get("FIRECONTROLLER_SOURCE_ROOT"), "explicit trusted source root not selected")
    def test_actual_selected_trusted_source_writers_and_history_reader(self):
        self.source=Path(os.environ["FIRECONTROLLER_SOURCE_ROOT"])
        self.snapshot.unlink()
        setup = """import json,sqlite3,sys
from pathlib import Path
sys.dont_write_bytecode=True
sys.path.insert(0,str(Path(sys.argv[1])/'dev-tools'))
from pr_review.state import ReviewState
from pr_review.sqlite_review_records import SqliteReviewRecords,FindingObservation
state=ReviewState(ordered_prs=(7,))
with sqlite3.connect(sys.argv[2]) as c:
 c.execute('PRAGMA user_version=1')
 c.execute('CREATE TABLE controller_metadata(singleton INTEGER PRIMARY KEY,data_model_version INTEGER,min_writer_build INTEGER)')
 c.execute('CREATE TABLE review_state(singleton INTEGER PRIMARY KEY,state_json TEXT)')
 c.execute('INSERT INTO controller_metadata VALUES(1,?,10)',(state.schema_version,))
 c.execute('INSERT INTO review_state VALUES(1,?)',(json.dumps(state.to_dict()),))
r=SqliteReviewRecords(sys.argv[2]);r.bootstrap()
r.record_run(run_id='real-source-run',source_pr=7,channel='cli',source_head='a'*40,reviewer='original-reviewer',scope='narrow',started_at='2026-10-01T00:00:00Z',finished_at='2026-10-01T01:00:00Z',findings=[FindingObservation('original-key','Original title',detail='Original detail',display_severity='Major')])
r.record_source_decision('real-source-run','original-key',decision_id='original-decision',decision='accepted',actor='source-author',reason='Source reason',decided_at='2026-10-01T01:00:00Z')
r.finalize_run('real-source-run',finalized_at='2026-10-01T01:00:00Z')
"""
        p=subprocess.run([sys.executable,"-B","-c",setup,str(self.source),str(self.snapshot)],capture_output=True,timeout=15)
        self.assertEqual(p.returncode,0,"trusted synthetic fixture setup failed")
        self.snapshot.chmod(0o600);before=self.snapshot.read_bytes();self.invoke()
        run=json.loads(self.output.read_bytes())["groups"][0]["pulls"][0]["runs"][0]
        self.assertEqual(run["reviewer"],"original-reviewer");self.assertEqual(run["findings"][0]["severity"],"high")
        self.assertEqual(run["findings"][0]["actor"],"source-author")
        self.assertEqual(before,self.snapshot.read_bytes())

    def test_wire_budget_and_unknown_time_are_never_truncated(self):
        s=adapter.wire_json(dict(text="<>&\u2028\u2029"))
        self.assertNotIn(b'<',s);self.assertIn(b'\\u003c',s);self.assertIn(b'\\u2028',s)
        self.assertEqual(adapter.clock("2026-10-01T00:00:00.000000001Z")[1][1],"000000001")
        with self.assertRaises(adapter.Refusal):adapter.clock(None)
        # Correct semantic size but Go HTML expansion exceeds native wrapper budget.
        h=self.frame["7"]
        for i in range(5):
            run=copy.deepcopy(h["runs"][0]);run["run_id"]="run-"+str(i);run["counts"]=dict(found=1,accepted=0,routed=0)
            f=copy.deepcopy(h["findings"][0]);f.update(run_id=run["run_id"],finding_id="finding-"+str(i),disposition="unadjudicated",detail="<"*12000)
            h["runs"].append(run);h["findings"].append(f);self.selection["pulls"][0]["run_ids"].append(run["run_id"])
        self.store_frame();self.save_selection();self.invoke(3);self.assertFalse(self.output.exists())


if __name__ == "__main__":unittest.main()
