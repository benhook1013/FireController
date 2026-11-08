"""Contract checks and a bounded model, not a claim about hosted runner timing."""
from collections import Counter, deque
from dataclasses import dataclass
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / '.github/workflows/ci.yml'
COMPATIBILITY_LOCK = """concurrency:
  group: firecontroller-ci
  queue: max
  cancel-in-progress: false
"""


def inventory(text):
    """Read the deliberately explicit workflow shape; reject unmapped jobs."""
    header, body = text.split('\njobs:\n', 1)
    assert COMPATIBILITY_LOCK in header
    assert 'vars.' not in text
    assert len(re.findall(r'^concurrency:', header, re.M)) == 1
    assert re.search(r'^      - develop$', header, re.M)
    assert re.search(r'^      - main$', header, re.M)
    assert '  pull_request:\n' in header
    blocks = re.split(r'^  ([\w-]+):\n', body, flags=re.M)
    jobs = dict(zip(blocks[1::2], blocks[2::2]))
    assert set(jobs) == {'static', 'linux', 'qualification', 'native-smoke', 'validate'}
    assert len(re.findall(r'^\s*queue:', text, re.M)) == 6
    assert len(re.findall(r'^\s*concurrency:', text, re.M)) == 6
    expected_names = {
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
    assert len(result) == 27
    assert len({name for name, _, _ in result}) == 27
    assert {lane for _, lane, _ in result} == set(range(20))
    assert all(0 <= lane < 20 for _, lane, _ in result)
    assert Counter(family for _, _, family in result) == {
        'linux': 22, 'native-smoke': 2, 'static': 1, 'qualification': 1, 'validate': 1,
    }
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


class LegacyRun:
    """Pre-lane admission: 16 Linux shards plus independent non-matrix jobs.

    The unchanged job inventory comes from the current workflow. The historical
    linux max-parallel is 16; validate needs every other family to finish. Jobs
    consume runners directly, never a shared lane, with immediate availability.
    """
    def __init__(self, jobs, run, duration=lambda name: 1):
        self.now = 0
        self.active = {}
        self.pending_linux = deque()
        self.intervals = []
        self.snapshots = []
        self.families = {name: family for name, _, family in jobs}
        self.completed = set()
        self.required = {name for name, _, family in jobs if family != 'validate'}
        self.gate = Job(run, 'validate', -1, duration('validate'))
        for name, _, family in jobs:
            job = Job(run, name, -1, duration(name))
            if family == 'linux':
                self.pending_linux.append(job)
            elif family != 'validate':
                self.start(job)
        self.admit_linux()

    def start(self, job):
        assert job.name not in self.active and job.name not in self.completed
        self.active[job.name] = (job, self.now + job.duration)
        self.intervals.append((self.now, self.now + job.duration, job))
        self.snapshots.append(tuple(job for job, _ in self.active.values()))

    def admit_linux(self):
        running = sum(self.families[name] == 'linux' for name in self.active)
        while self.pending_linux and running < 16:
            self.start(self.pending_linux.popleft())
            running += 1

    def step(self):
        self.now = min(end for _, end in self.active.values())
        for name in [name for name, (_, end) in self.active.items() if end == self.now]:
            del self.active[name]
            self.completed.add(name)
        self.admit_linux()
        if self.completed == self.required:
            self.start(self.gate)
        self.snapshots.append(tuple(job for job, _ in self.active.values()))


class ProposedWorkflowAdmission:
    """Captured group selection is immutable for an already submitted run.

    This is a proposed future opt-in gate, NOT the current literal-lock workflow.
    Models admission, not provider variable propagation timing. The caller
    supplies a hypothetical value; the current workflow reads no variable.
    """
    def __init__(self):
        self.groups = {}
        self.active = {}
        self.pending = {}

    def submit(self, run, variable='', legacy=False):
        enabled = not legacy and variable.lower() == 'true'
        group = f'firecontroller-ci-run-{run}' if enabled else 'firecontroller-ci'
        self.groups[run] = group
        if group not in self.active:
            self.active[group] = run
        else:
            self.pending.setdefault(group, deque()).append(run)

    def finish(self, run):
        group = self.groups[run]
        assert self.active[group] == run
        del self.active[group]
        if self.pending.get(group):
            self.active[group] = self.pending[group].popleft()


class SchedulingTests(unittest.TestCase):
    def setUp(self):
        self.text = WORKFLOW.read_text()
        self.jobs = inventory(self.text)

    def offer_run(self, scheduler, run, duration=lambda name: 1):
        for name, lane, family in self.jobs:
            if family != 'validate':
                scheduler.offer(Job(run, name, lane, duration(name)))

    def finish_runs(self, scheduler, runs):
        completed = Counter()
        gate_lane = next(lane for _, lane, family in self.jobs if family == 'validate')
        while scheduler.active:
            for job in scheduler.step():
                if job.name != 'validate':
                    completed[job.run] += 1
                    if completed[job.run] == 26:
                        scheduler.offer(Job(job.run, 'validate', gate_lane))
        self.assertEqual(completed, {run: 26 for run in runs})
        self.assertEqual(len(scheduler.intervals), 27 * len(runs))
        self.assertFalse(scheduler.rejected)
        self.assertLessEqual(max(map(len, scheduler.snapshots)), 20)
        for run in runs:
            gate_start = next(start for start, _, j in scheduler.intervals
                              if j.run == run and j.name == 'validate')
            self.assertTrue(all(end <= gate_start for _, end, j in scheduler.intervals
                                if j.run == run and j.name != 'validate'))

    def test_real_inventory_includes_every_required_context(self):
        self.assertEqual(len(self.jobs), 27)

    def test_guards_refuse_missing_lane_extra_jobs_and_revision_changes(self):
        mutations = [
            self.text.replace('slot: 19', 'slot: 20'),
            self.text.replace('queue: max', 'queue: single', 1),
            self.text.replace('  group: firecontroller-ci\n', '  group: different-legacy-group\n', 1),
            self.text.replace('  group: firecontroller-ci\n', '  group: ${{ github.run_id }}\n', 1),
            self.text.replace('cancel-in-progress: false', 'cancel-in-progress: true', 1),
            self.text.replace('            slot: 19\n', ''),
            self.text.replace('    steps:\n', '    steps:\n      - with:\n          ref: main\n', 1),
            self.text.replace('    name: validate', '    name: renamed'),
            self.text.replace('      - develop\n', ''),
            self.text + '\n  uncounted:\n    runs-on: ubuntu-latest\n',
            self.text.replace('        shard:', '        other: [one, two]\n        shard:', 1),
        ]
        for changed in mutations:
            with self.subTest(changed=changed[-100:]):
                with self.assertRaises(AssertionError):
                    inventory(changed)

    def test_single_run_can_use_all_twenty(self):
        s = Scheduler()
        self.offer_run(s, 'PR-A')
        self.assertEqual(len(s.active), 20)
        self.finish_runs(s, ['PR-A'])

    def test_proposed_interleaving_independent_pr_and_develop_runs_overlap_including_gate(self):
        s = Scheduler()
        runs = ['PR-A', 'PR-B', 'develop']
        for run in runs:
            self.offer_run(s, run, lambda name: 50 if run == 'PR-B' and name == 'Go race (reviewexec-5)' else 3)
        self.finish_runs(s, runs)
        self.assertTrue(any(len({j.run for j in snapshot}) > 1 for snapshot in s.snapshots))
        self.assertTrue(any(any(j.name == 'validate' for j in snapshot)
                            and any(j.name != 'validate' for j in snapshot)
                            for snapshot in s.snapshots))
        # Both matrix and non-matrix jobs consume the same finite lane budget.
        self.assertTrue(any(any(j.name == 'Packaged qualification' for j in snapshot)
                            and any(j.name.startswith('Go race') for j in snapshot)
                            for snapshot in s.snapshots))

    def test_proposed_interleaving_slow_tail_does_not_hold_other_nineteen_lanes(self):
        s = Scheduler()
        slow = 'Go race (backup-0)'
        self.offer_run(s, 'PR-A', lambda name: 100 if name == slow else 1)
        self.offer_run(s, 'PR-B')
        self.finish_runs(s, ['PR-A', 'PR-B'])
        progressed = {j.lane for start, _, j in s.intervals if j.run == 'PR-B' and start < 100}
        self.assertEqual(progressed, set(range(1, 20)))

    def test_proposed_interleaving_many_runs_with_uneven_durations_stay_within_budget(self):
        s = Scheduler()
        runs = [f'PR-{index}' for index in range(20)]
        for index, run in enumerate(runs):
            self.offer_run(s, run, lambda name: 1 + (sum(map(ord, name)) + index) % 17)
        self.finish_runs(s, runs)
        self.assertEqual(max(map(len, s.snapshots)), 20)
        self.assertTrue(all(len({j.lane for j in snapshot}) == len(snapshot)
                            for snapshot in s.snapshots))

    def test_affinity_explicitly_strands_spare_capacity(self):
        s = Scheduler()
        s.offer(Job('PR-A', 'slow', 0, 100))
        s.offer(Job('PR-B', 'ready', 0))
        self.assertEqual(len(s.active), 1)
        self.assertEqual(len(s.pending[0]), 1)
        self.assertEqual(sum(not q for lane, q in s.pending.items() if lane != 0), 19)

    def test_queue_overflow_can_reject_latest_live_revision(self):
        s = Scheduler()
        s.offer(Job('old', 'running', 0, 100))
        for index in range(100):
            s.offer(Job(f'PR-{index}', 'waiting', 0))
        latest = Job('PR-newest-live', 'required check', 0)
        s.offer(latest)
        self.assertEqual(s.rejected, [latest])
        self.assertEqual(len(s.pending[0]), 100)
        # No queued work is displaced, but latest arrival is not guaranteed admission.
        self.assertEqual(s.pending[0][0].run, 'PR-0')

    def test_current_workflow_requires_literal_compatibility_lock(self):
        header = self.text.split('\njobs:\n', 1)[0]
        self.assertIn(COMPATIBILITY_LOCK, header)
        self.assertNotIn('vars.', self.text)
        self.assertEqual(re.findall(r'^  group: (.+)$', header, re.M), ['firecontroller-ci'])

    def test_literal_lock_admission_serializes_with_legacy_workflows(self):
        gate = ProposedWorkflowAdmission()
        # legacy=True selects the literal lock, matching the verified current header.
        gate.submit('old', legacy=True)
        gate.submit('new', legacy=True)
        gate.submit('another', legacy=True)
        self.assertEqual(list(gate.active.values()), ['old'])
        gate.finish('old')
        self.assertEqual(list(gate.active.values()), ['new'])
        gate.finish('new')
        self.assertEqual(list(gate.active.values()), ['another'])

    def test_proposed_interleaving_gate_matches_github_case_insensitive_comparison(self):
        for value in ('true', 'TRUE', 'True'):
            gate = ProposedWorkflowAdmission()
            gate.submit('run', variable=value)
            self.assertEqual(gate.groups['run'], 'firecontroller-ci-run-run')
        for value in ('', 'false', '1', 'true ', 'enabled'):
            gate = ProposedWorkflowAdmission()
            gate.submit('run', variable=value)
            self.assertEqual(gate.groups['run'], 'firecontroller-ci')

    def test_proposed_interleaving_activation_does_not_reassign_already_queued_groups(self):
        gate = ProposedWorkflowAdmission()
        gate.submit('default-running')
        gate.submit('default-queued')
        gate.submit('enabled', variable='true')
        self.assertEqual(gate.groups['default-queued'], 'firecontroller-ci')
        self.assertEqual(set(gate.active.values()), {'default-running', 'enabled'})
        # Previously gated runs still use lanes, even across activation.
        s = Scheduler()
        for run in gate.active.values():
            self.offer_run(s, run)
        self.finish_runs(s, ['default-running', 'enabled'])
        gate.finish('default-running')
        self.assertIn('default-queued', gate.active.values())

    def test_proposed_interleaving_rollback_affects_future_groups_not_existing_runs(self):
        gate = ProposedWorkflowAdmission()
        gate.submit('enabled-A', variable='true')
        gate.submit('enabled-B', variable='true')
        gate.submit('rollback-A', variable='false')
        gate.submit('rollback-B')
        runs = ['enabled-A', 'enabled-B', 'rollback-A']
        self.assertEqual(set(gate.active.values()), set(runs))
        self.assertEqual(gate.groups['enabled-A'], 'firecontroller-ci-run-enabled-A')
        s = Scheduler()
        for run in runs:
            self.offer_run(s, run)
        self.finish_runs(s, runs)
        gate.finish('rollback-A')
        self.assertIn('rollback-B', gate.active.values())

    def test_proposed_interleaving_legacy_workflow_is_outside_new_lane_bound(self):
        admission = ProposedWorkflowAdmission()
        admission.submit('legacy', legacy=True)
        admission.submit('new', variable='true')
        self.assertEqual(set(admission.active.values()), {'legacy', 'new'})
        s = Scheduler()
        self.offer_run(s, 'new')
        legacy = LegacyRun(self.jobs, 'legacy')
        families = Counter(legacy.families[name] for name in legacy.active)
        self.assertEqual(families, {'linux': 16, 'static': 1, 'qualification': 1, 'native-smoke': 2})
        self.assertEqual(len(legacy.pending_linux), 6)
        self.assertNotIn('validate', legacy.active)
        self.assertEqual(len(s.active), 20)
        # Both admitted runs start at model time zero. Legacy jobs occupy real
        # modeled runner intervals but bypass the new run's shared lane budget.
        self.assertGreater(len(s.active) + len(legacy.active), 20)
        self.finish_runs(s, ['new'])
        while legacy.active:
            legacy.step()
        self.assertEqual(legacy.completed, {name for name, _, _ in self.jobs})
        intervals = s.intervals + legacy.intervals
        times = {start for start, _, _ in intervals}
        peak = max(sum(start <= now < end for start, end, _ in intervals) for now in times)
        self.assertEqual(peak, 40)

    def test_legacy_matrix_refills_and_validation_waits_for_every_family(self):
        # Long non-matrix work must not consume Linux matrix permits or let the
        # gate start early after all race shards finish.
        legacy = LegacyRun(self.jobs, 'legacy', lambda name: 5 if name == 'Packaged qualification' else 1)
        legacy.step()
        self.assertEqual(sum(legacy.families[name] == 'linux' for name in legacy.active), 6)
        self.assertFalse(legacy.pending_linux)
        legacy.step()
        self.assertEqual(set(legacy.active), {'Packaged qualification'})
        while legacy.active:
            legacy.step()
        self.assertEqual(len(legacy.intervals), len(self.jobs))
        self.assertEqual(legacy.completed, {name for name, _, _ in self.jobs})
        self.assertEqual(max(map(len, legacy.snapshots)), 20)
        self.assertLessEqual(max(sum(legacy.families[j.name] == 'linux' for j in snapshot)
                                 for snapshot in legacy.snapshots), 16)
        gate_start = next(start for start, _, job in legacy.intervals if job.name == 'validate')
        self.assertTrue(all(end <= gate_start for _, end, job in legacy.intervals
                            if job.name != 'validate'))


if __name__ == '__main__':
    unittest.main()
