"""Contract checks and a bounded model, not a claim about hosted runner timing."""
from collections import Counter, deque
from dataclasses import dataclass
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / '.github/workflows/ci.yml'
WORKFLOW_SUPERSESSION = """concurrency:
  group: firecontroller-ci-${{ github.event_name }}-${{ github.ref }}
  cancel-in-progress: true
"""


def inventory(text):
    """Read the deliberately explicit workflow shape; reject unmapped jobs."""
    header, body = text.split('\njobs:\n', 1)
    assert WORKFLOW_SUPERSESSION in header
    assert len(re.findall(r'^  group:', header, re.M)) == 1
    assert not re.search(r'^  queue:', header, re.M)
    assert len(re.findall(r'^concurrency:', header, re.M)) == 1
    assert re.search(r'^      - develop$', header, re.M)
    assert re.search(r'^      - main$', header, re.M)
    assert '  pull_request:\n' in header
    blocks = re.split(r'^  ([\w-]+):\n', body, flags=re.M)
    jobs = dict(zip(blocks[1::2], blocks[2::2]))
    assert set(jobs) == {'changes', 'static', 'linux', 'qualification', 'container', 'native-smoke', 'validate', 'publish'}
    assert len(re.findall(r'^\s*queue:', text, re.M)) == 8
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

    This is a success-only DAG, not hosted timing or operational readiness.
    Failure/cancellation gate behavior is exercised by the real shell tests;
    lane rejection and asynchronous cancellation are outside this DAG model.
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


class WorkflowAdmission:
    """Model GitHub's captured workflow group and latest-arrival replacement."""
    def __init__(self):
        self.active = {}
        self.cancelled = []

    def submit(self, run, event, ref):
        group = f'firecontroller-ci-{event}-{ref}'.lower()
        if group in self.active:
            self.cancelled.append(self.active[group])
        self.active[group] = run


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
        self.assertIn(WORKFLOW_SUPERSESSION, self.text)
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
                        self.text.replace(WORKFLOW_SUPERSESSION, WORKFLOW_SUPERSESSION.replace('github.ref', 'github.run_id'), 1),
                        self.text.replace(WORKFLOW_SUPERSESSION, WORKFLOW_SUPERSESSION.replace('github.event_name', 'github.run_id'), 1),
                        self.text.replace(WORKFLOW_SUPERSESSION, WORKFLOW_SUPERSESSION.replace('  cancel-in-progress: true', '  queue: max\n  cancel-in-progress: false'), 1),
                        self.text.replace('      cancel-in-progress: false', '      cancel-in-progress: true', 1),
                        self.text + '\n  unnoticed:\n    runs-on: ubuntu-latest\n',
                        self.text.replace('    name: Publish tested container', '    name: changed'),
                        self.text.replace('        shard:', '        extra: [a, b]\n        shard:', 1)]:
            with self.assertRaises(AssertionError):
                inventory(changed)

    def test_latest_arrival_supersedes_only_the_same_pr_or_push_ref(self):
        admission = WorkflowAdmission()
        requests = [
            ('pr-45-old', 'pull_request', 'refs/pull/45/merge'),
            ('pr-89', 'pull_request', 'refs/pull/89/merge'),
            ('develop-old', 'push', 'refs/heads/develop'),
            ('main-old', 'push', 'refs/heads/main'),
            ('pr-45-new', 'pull_request', 'refs/pull/45/merge'),
            ('develop-new', 'push', 'refs/heads/develop'),
            ('main-new', 'push', 'refs/heads/main'),
        ]
        for request in requests:
            admission.submit(*request)
        self.assertEqual(admission.cancelled, ['pr-45-old', 'develop-old', 'main-old'])
        self.assertEqual(set(admission.active.values()), {'pr-45-new', 'pr-89', 'develop-new', 'main-new'})

    def test_many_revisions_keep_one_run_per_pr(self):
        admission = WorkflowAdmission()
        for revision in range(120):
            admission.submit(revision, 'pull_request', 'refs/pull/45/merge')
        self.assertEqual(list(admission.active.values()), [119])
        self.assertEqual(admission.cancelled, list(range(119)))

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

    def test_interleaving_slow_tail_gate_and_publish_all_count(self):
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
        self.test_interleaving_slow_tail_gate_and_publish_all_count()

    def test_overlap_refuses_missing_slow_job(self):
        self.jobs = [('renamed' if name == 'Go race (backup-0)' else name, lane, family)
                     for name, lane, family in self.jobs]
        with self.assertRaises(AssertionError):
            self.test_interleaving_slow_tail_gate_and_publish_all_count()

    def test_interleaving_all_publishers_share_a_counted_lane(self):
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
        self.assertIn(WORKFLOW_SUPERSESSION, self.text)


if __name__ == '__main__':
    unittest.main()
