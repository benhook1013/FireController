"""Contract checks and a bounded model, not a claim about hosted runner timing."""
from collections import Counter, deque
from dataclasses import dataclass
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / '.github/workflows/ci.yml'
ACTIVATION_GATE = """concurrency:
  group: ${{ vars.FIRECONTROLLER_CI_INTERLEAVE == 'true' && format('firecontroller-ci-run-{0}', github.run_id) || 'firecontroller-ci' }}
  queue: max
  cancel-in-progress: false
"""


def inventory(text):
    """Read the deliberately explicit workflow shape; reject unmapped jobs."""
    header, body = text.split('\njobs:\n', 1)
    assert ACTIVATION_GATE in header
    assert text.count('vars.') == 1
    assert len(re.findall(r'^concurrency:', header, re.M)) == 1
    assert re.search(r'^      - develop$', header, re.M)
    assert re.search(r'^      - main$', header, re.M)
    assert '  pull_request:\n' in header
    blocks = re.split(r'^  ([\w-]+):\n', body, flags=re.M)
    jobs = dict(zip(blocks[1::2], blocks[2::2]))
    assert set(jobs) == {'changes', 'static', 'linux', 'qualification', 'container', 'native-smoke', 'validate', 'publish'}
    assert len(re.findall(r'^\s*queue:', text, re.M)) == 9
    assert len(re.findall(r'^\s*concurrency:', text, re.M)) == 9
    expected_names = {
        'changes': 'Select development checks', 'container': 'Container runtime',
        'publish': 'Publish tested container',
        'static': 'Source and browser checks', 'linux': 'Go race (${{ matrix.shard }})',
        'qualification': 'Packaged qualification',
        'native-smoke': 'Native smoke (${{ matrix.os }})', 'validate': 'validate',
    }
    result = []
    for job, block in jobs.items():
        assert f'    name: {expected_names[job]}\n' in block
        assert 'continue-on-error:' not in block
        assert not re.search(r'^\s+ref:', block, re.M), 'Checkout must retain the event revision'
        assert not re.search(r'^\s+max-parallel:', block, re.M)
        slot = re.search(
            r'^    concurrency:\n      group: firecontroller-ci-slot-(.+)\n'
            r'      queue: max\n      cancel-in-progress: false\n', block, re.M)
        assert slot is not None, f'job {job} lacks the expected slot concurrency block'
        if job in ('linux', 'native-smoke'):
            axis = 'shard' if job == 'linux' else 'os'
            assert slot[1] == '${{ matrix.slot }}'
            axis_values = re.search(r'^        '+axis+r': \[([^]]+)\]$', block, re.M)
            assert axis_values is not None
            values = axis_values[1].split(', ')
            entries = re.findall(r'^          - '+axis+r': ([\w-]+)\n            slot: (\d+)$', block, re.M)
            assert len(entries) == len(values) == len(set(values))
            assert Counter(value for value, _ in entries) == Counter(values)
            # Extra matrix dimensions/rows would invalidate the expanded inventory.
            matrix = block.split('      matrix:\n', 1)[1].split('    steps:', 1)[0]
            if job == 'native-smoke':
                matrix = matrix.split('    env:', 1)[0]
            assert set(re.findall(r'^        ([\w-]+):', matrix, re.M)) == {axis, 'include'}
            assert len(re.findall(r'^          - ', matrix, re.M)) == len(entries)
            for value, lane in entries:
                name = expected_names[job].replace('${{ matrix.'+axis+' }}', value)
                result.append((name, int(lane), job))
        else:
            assert slot[1].isdigit()
            result.append((expected_names[job], int(slot[1]), job))
    assert len(result) == 31
    assert len({name for name, _, _ in result}) == 31
    assert {lane for _, lane, _ in result} == set(range(20))
    assert all(0 <= lane < 20 for _, lane, _ in result)
    assert Counter(family for _, _, family in result) == {
        'linux': 23, 'changes': 1, 'container': 1, 'publish': 1, 'native-smoke': 2, 'static': 1, 'qualification': 1, 'validate': 1,
    }
    expected_lanes = {
        'Select development checks': 6, 'Source and browser checks': 2,
        'Packaged qualification': 3, 'Container runtime': 8,
        'Native smoke (windows-latest)': 4, 'Native smoke (macos-latest)': 5,
        'validate': 2, 'Publish tested container': 7,
    }
    race_order = [
        'backup-0', 'reviewexec-0', 'reviews-0', 'reviewruntime-0', 'rest-0',
        'backup-1', 'reviewexec-1', 'reviews-1', 'reviewruntime-1', 'rest-1',
        'backup-2', 'reviewexec-2', 'reviews-2', 'reviewruntime-2',
        'backup-3', 'reviewexec-3', 'reviews-3', 'backup-4', 'reviewexec-4',
        'backup-5', 'reviewexec-5', 'backup-6', 'backup-7',
    ]
    expected_lanes.update({f'Go race ({name})': index % 20
                           for index, name in enumerate(race_order)})
    assert {name: lane for name, lane, _ in result} == expected_lanes, 'documented slot affinity changed'
    return result


@dataclass(frozen=True)
class Job:
    run: str
    name: str
    lane: int
    duration: int = 1


class Scheduler:
    """One active and at most 100 pending per lane, FIFO at admission.

    Unlimited available OS runners are assumed. Real account/OS availability
    can reduce throughput, never increase the twenty-lane admission bound.
    """
    def __init__(self):
        self.now = 0
        self.active = {}
        self.pending = {i: deque() for i in range(20)}
        self.rejected = []
        self.intervals = []
        self.snapshots = []

    def snapshot(self):
        self.snapshots.append(tuple(job for job, _ in self.active.values()))

    def start(self, job):
        assert job.lane not in self.active
        self.active[job.lane] = (job, self.now + job.duration)
        self.intervals.append((self.now, self.now + job.duration, job))
        self.snapshot()

    def offer(self, job):
        if job.lane not in self.active:
            self.start(job)
        elif len(self.pending[job.lane]) < 100:
            self.pending[job.lane].append(job)
        else:
            self.rejected.append(job)

    def step(self):
        self.now = min(end for _, end in self.active.values())
        done = [job for job, end in self.active.values() if end == self.now]
        for job in done:
            del self.active[job.lane]
        for job in done:
            if self.pending[job.lane]:
                self.start(self.pending[job.lane].popleft())
        self.snapshot()
        return done


class DevelopmentRun:
    """Full/website DAG, with the real selector and gate prerequisites.

    WorkflowAdmission chooses which instances may enter the shared lanes.
    This is a success-only DAG, not hosted timing or operational readiness.
    Failure/cancellation gate behavior is exercised by the real shell tests;
    rejected-job completion and workflow-group release are not modeled here.
    """
    def __init__(self, scheduler, jobs, run, website=False, publish=True, slow=None):
        self.scheduler, self.run = scheduler, run
        self.jobs, self.website, self.publish = jobs, website, publish
        self.slow = slow
        self.done = set()
        self.offered = set()
        self.advance()

    def advance(self):
        for name, lane, family in self.jobs:
            if name in self.offered:
                continue
            if family in ('linux', 'native-smoke'):
                if self.website or 'Select development checks' not in self.done:
                    continue
            elif family == 'validate':
                required = {n for n, _, f in self.jobs if f not in ('validate', 'publish')
                            and not (self.website and f in ('linux', 'native-smoke'))}
                if not required <= self.done:
                    continue
            elif family == 'publish':
                if not self.publish or 'validate' not in self.done:
                    continue
            self.offered.add(name)
            duration = 100 if name == self.slow else 1
            self.scheduler.offer(Job(self.run, name, lane, duration))

    def complete(self, job):
        self.done.add(job.name)
        self.advance()


def workflow_group(setting, run_id):
    """Model the pinned GitHub expression, whose string equality ignores case.

    A missing repository variable evaluates to an empty string. Do not strip
    whitespace or use Python truthiness: both would broaden activation.
    """
    enabled = isinstance(setting, str) and setting.lower() == 'true'
    return f'firecontroller-ci-run-{run_id}' if enabled else 'firecontroller-ci'


class WorkflowAdmission:
    """Workflow groups are selected once; a setting change does not migrate them.

    Compatibility workflows always use the legacy group. Activated and still
    serialized lane-capable workflows share Scheduler's twenty job groups.
    The workflow queue and each job queue independently permit 100 waiters.
    """
    def __init__(self, scheduler, jobs):
        self.scheduler, self.jobs = scheduler, jobs
        self.groups, self.pending, self.runs, self.selected = {}, {}, {}, {}
        self.rejected = []

    def offer(self, run_id, setting='', compatibility=False, **options):
        assert run_id not in self.selected
        group = 'firecontroller-ci' if compatibility else workflow_group(setting, run_id)
        self.selected[run_id] = group
        request = (run_id, options)
        if group not in self.groups:
            self.start(group, request)
        elif len(self.pending.setdefault(group, deque())) < 100:
            self.pending[group].append(request)
        else:
            self.rejected.append(run_id)

    def start(self, group, request):
        run_id, options = request
        self.groups[group] = run_id
        self.runs[run_id] = DevelopmentRun(self.scheduler, self.jobs, run_id, **options)

    def step(self):
        for job in self.scheduler.step():
            run = self.runs[job.run]
            run.complete(job)
            expected = (6 if run.website else 31) - (not run.publish)
            if len(run.done) == expected:
                group = self.selected[run.run]
                del self.groups[group]
                if self.pending.get(group):
                    self.start(group, self.pending[group].popleft())

    def finish(self):
        while self.scheduler.active:
            self.step()
        assert not self.groups
        assert not any(self.pending.values())


class DevelopmentSchedulingTests(unittest.TestCase):
    def setUp(self):
        self.text = WORKFLOW.read_text()
        self.jobs = inventory(self.text)

    def finish(self, scheduler, runs):
        by_id = {run.run: run for run in runs}
        while scheduler.active:
            for job in scheduler.step():
                by_id[job.run].complete(job)
        self.assertFalse(scheduler.rejected)
        self.assertLessEqual(max(map(len, scheduler.snapshots)), 20)
        for run in runs:
            expected = 6 if run.website else 31
            if not run.publish:
                expected -= 1
            self.assertEqual(len(run.done), expected)
            intervals = {job.name: (start, end) for start, end, job in scheduler.intervals if job.run == run.run}
            gate_start = intervals['validate'][0]
            for name, (_, end) in intervals.items():
                if name not in ('validate', 'Publish tested container'):
                    self.assertLessEqual(end, gate_start)
                if name.startswith(('Go race', 'Native smoke')):
                    self.assertGreaterEqual(intervals[name][0], intervals['Select development checks'][1])
            if run.publish:
                self.assertGreaterEqual(intervals['Publish tested container'][0], intervals['validate'][1])

    def test_exact_development_inventory_and_preserved_conditions(self):
        self.assertEqual(len(self.jobs), 31)
        self.assertIn(ACTIVATION_GATE, self.text)
        self.assertEqual(self.text.count('vars.'), 1)
        self.assertEqual(self.text.count("if: needs.changes.outputs.website_only != 'true'"), 2)
        self.assertEqual(self.text.count('    needs: [changes]\n'), 2)
        self.assertIn('needs: [changes, static, linux, qualification, container, native-smoke]', self.text)
        self.assertIn("if: github.event_name == 'push' && (github.ref == 'refs/heads/develop' || github.ref == 'refs/heads/main')", self.text)
        self.assertIn('      packages: write', self.text)
        self.assertIn('      actions: read', self.text)
        self.assertIn("python3 tools/ci/development-checks.py --base", self.text)
        self.assertIn('          sha256sum --check firecontroller-image.tar.sha256', self.text)

    def test_inventory_refuses_uncounted_jobs_and_altered_lock(self):
        for changed in [self.text.replace('slot: 19', 'slot: 20'),
                        self.text.replace('group: firecontroller-ci-slot-2', 'group: firecontroller-ci-slot-9', 1),
                        self.text.replace('shard: backup-0\n            slot: 0', 'shard: backup-0\n            slot: 19', 1),
                        self.text.replace('queue: max', 'queue: single', 1),
                        self.text.replace(ACTIVATION_GATE, 'concurrency:\n  group: ${{ github.run_id }}\n  queue: max\n  cancel-in-progress: false\n'),
                        self.text.replace("== 'true'", "!= 'false'", 1),
                        self.text.replace('cancel-in-progress: false', 'cancel-in-progress: true', 1),
                        self.text + '\n  unnoticed:\n    runs-on: ubuntu-latest\n',
                        self.text.replace('    name: Publish tested container', '    name: changed'),
                        self.text.replace('        shard:', '        extra: [a, b]\n        shard:', 1)]:
            with self.assertRaises(AssertionError):
                inventory(changed)

    def test_gate_defaults_closed_and_uses_distinct_run_ids_only_when_enabled(self):
        for setting in ('', 'false', 'FALSE', '0', '1', 'yes', ' true', 'true ', 'null', None):
            with self.subTest(setting=setting):
                self.assertEqual(workflow_group(setting, 42), 'firecontroller-ci')
        for setting in ('true', 'True', 'TRUE'):
            self.assertEqual(workflow_group(setting, 42), 'firecontroller-ci-run-42')
            self.assertNotEqual(workflow_group(setting, 42), workflow_group(setting, 43))

    def test_disabled_gate_serializes_whole_runs_including_the_slow_tail(self):
        s = Scheduler()
        admission = WorkflowAdmission(s, self.jobs)
        admission.offer('old', slow='Go race (backup-0)')
        admission.offer('new', setting='false')
        self.assertNotIn('new', admission.runs)
        admission.finish()
        old_end = max(end for _, end, job in s.intervals if job.run == 'old')
        new_start = min(start for start, _, job in s.intervals if job.run == 'new')
        self.assertGreaterEqual(new_start, old_end)
        self.assertTrue(all(len({job.run for job in snapshot}) <= 1 for snapshot in s.snapshots))

    def test_queued_lane_compatibility_and_enabled_workflows_share_twenty_slots(self):
        s = Scheduler()
        admission = WorkflowAdmission(s, self.jobs)
        admission.offer('compatibility-active', compatibility=True, slow='Go race (backup-0)')
        admission.offer('compatibility-pending', compatibility=True)
        admission.offer('enabled', setting='true', publish=False)
        self.assertNotIn('compatibility-pending', admission.runs)
        admission.finish()
        self.assertFalse(s.rejected)
        self.assertFalse(admission.rejected)
        self.assertEqual(max(map(len, s.snapshots)), 20)
        self.assertTrue(any(len({job.run for job in snapshot}) > 1 for snapshot in s.snapshots))
        # A queued workflow retains its original group despite later activation.
        self.assertEqual(admission.selected['compatibility-pending'], 'firecontroller-ci')
        first_end = max(end for _, end, job in s.intervals if job.run == 'compatibility-active')
        pending_start = min(start for start, _, job in s.intervals if job.run == 'compatibility-pending')
        self.assertGreaterEqual(pending_start, first_end)

    def test_rollback_changes_future_groups_but_does_not_serialize_admitted_runs(self):
        s = Scheduler()
        admission = WorkflowAdmission(s, self.jobs)
        admission.offer('enabled', setting='true', slow='Go race (backup-0)')
        admission.step()
        admission.offer('after-disable', setting='false')
        admission.offer('waiting-after-disable', setting='false')
        self.assertNotIn('waiting-after-disable', admission.runs)
        admission.finish()
        self.assertEqual(admission.selected['enabled'], 'firecontroller-ci-run-enabled')
        self.assertEqual(admission.selected['after-disable'], 'firecontroller-ci')
        self.assertTrue(any({'enabled', 'after-disable'} <= {j.run for j in snapshot}
                            for snapshot in s.snapshots))
        self.assertLessEqual(max(map(len, s.snapshots)), 20)
        self.assertFalse(s.rejected)

    def test_workflow_queue_overflow_does_not_replace_pending_or_running_revisions(self):
        admission = WorkflowAdmission(Scheduler(), self.jobs)
        admission.offer('running')
        for index in range(100):
            admission.offer(f'pending-{index}')
        admission.offer('latest')
        self.assertEqual(admission.rejected, ['latest'])
        self.assertEqual(admission.groups['firecontroller-ci'], 'running')
        self.assertEqual([r for r, _ in admission.pending['firecontroller-ci']],
                         [f'pending-{index}' for index in range(100)])

    def test_single_full_run_uses_twenty_after_selector(self):
        s = Scheduler()
        run = DevelopmentRun(s, self.jobs, 'develop')
        self.finish(s, [run])
        self.assertEqual(max(map(len, s.snapshots)), 20)

    def test_website_only_dag_preserves_six_jobs_and_skips_only_race_native(self):
        s = Scheduler()
        run = DevelopmentRun(s, self.jobs, 'develop-website', website=True)
        self.finish(s, [run])
        self.assertEqual(run.done, {'Select development checks', 'Source and browser checks',
                                   'Packaged qualification', 'Container runtime', 'validate',
                                   'Publish tested container'})

    def test_pr_does_not_publish(self):
        s = Scheduler()
        run = DevelopmentRun(s, self.jobs, 'PR', publish=False)
        self.finish(s, [run])
        self.assertNotIn('Publish tested container', run.done)

    def test_enabled_interleaving_slow_tail_gate_and_publish_all_count(self):
        s = Scheduler()
        slow = 'Go race (backup-0)'
        names = {name for name, _, _ in self.jobs}
        self.assertIn(slow, names)
        self.assertIn('Go race (backup-5)', names)
        slow_lane, = [lane for name, lane, _ in self.jobs if name == slow]
        runs = [DevelopmentRun(s, self.jobs, 'PR-A', publish=False, slow=slow),
                DevelopmentRun(s, self.jobs, 'develop-B', slow='Go race (backup-5)'),
                DevelopmentRun(s, self.jobs, 'develop-website', website=True)]
        self.finish(s, runs)
        self.assertTrue(any(len({job.run for job in snapshot}) > 1 for snapshot in s.snapshots))
        for name in ('Select development checks', 'validate', 'Publish tested container'):
            self.assertTrue(any(any(job.name == name for job in snapshot)
                                and any(job.name.startswith('Go race') for job in snapshot)
                                for snapshot in s.snapshots), name)
        progressed = {job.lane for start, _, job in s.intervals
                      if job.run == 'develop-B' and start < 100}
        self.assertEqual(progressed, {lane for _, lane, _ in self.jobs} - {slow_lane})

    def test_slow_tail_follows_reassigned_inventory_lanes(self):
        self.jobs = [(name, (lane + 7) % 20, family) for name, lane, family in self.jobs]
        self.test_enabled_interleaving_slow_tail_gate_and_publish_all_count()

    def test_overlap_refuses_missing_slow_job(self):
        self.jobs = [('renamed' if name == 'Go race (backup-0)' else name, lane, family)
                     for name, lane, family in self.jobs]
        with self.assertRaises(AssertionError):
            self.test_enabled_interleaving_slow_tail_gate_and_publish_all_count()

    def test_enabled_interleaving_all_publishers_share_a_counted_lane(self):
        publish_lane = next(lane for _, lane, family in self.jobs if family == 'publish')
        s = Scheduler()
        s.offer(Job('main', 'Publish tested container', publish_lane, 5))
        s.offer(Job('develop', 'Publish tested container', publish_lane, 5))
        self.assertEqual(len(s.active), 1)
        self.assertEqual(len(s.pending[publish_lane]), 1)
        s.step()
        self.assertEqual(s.active[publish_lane][0].run, 'develop')

    def test_affinity_and_latest_overflow_remain_explicit_limits(self):
        s = Scheduler()
        s.offer(Job('old', 'slow', 0, 100))
        for n in range(100):
            s.offer(Job(str(n), 'ready', 0))
        latest = Job('latest', 'required', 0)
        s.offer(latest)
        self.assertEqual(len(s.active), 1)
        self.assertEqual(s.rejected, [latest])
        self.assertEqual(len(s.pending[0]), 100)

    def test_legacy_overlap_counterexample_requires_drain_before_activation(self):
        # An enabled independent run group cannot coordinate with a legacy
        # workflow lacking job lanes. This is a rollout prohibition, not a cap.
        s = Scheduler()
        run = DevelopmentRun(s, self.jobs, 'future')
        self.finish(s, [run])
        self.assertEqual(max(map(len, s.snapshots)) + 20, 40)
        self.assertIn(ACTIVATION_GATE, self.text)


if __name__ == '__main__':
    unittest.main()
