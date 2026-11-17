import fcntl
import json
import unittest
from unittest.mock import patch
import os
import prepare_rehearsal
import prepare_successor
import hashlib
from test_rehearsal_operation import RehearsalOperationTests
import bounded_rehearsal as bounded
from artifact_retention import Refuse


class BoundedTests(RehearsalOperationTests):
    def setUp(self):
        super().setUp()
        self.budget = self.base / 'budget'
        self.source_lock = self.base / 'source-lock'
        for path in (self.budget, self.source_lock):
            path.touch(mode=0o600)

    def bounded(self, **changes):
        return bounded.execute(budget_lease=self.budget, source_lease=self.source_lock,
                               **dict(self.args, **changes))

    def test_unique_output_bounds_next_run(self):
        result = self.bounded()
        self.assertEqual(result['retention'], 'explicit_disposition_required')
        before = (self.root/'first/data/firecontroller.sqlite').read_bytes()
        with self.assertRaisesRegex(Refuse, 'explicit disposition'):
            self.bounded(operation='second')
        self.assertFalse((self.root/'second').exists())
        self.assertEqual((self.root/'first/data/firecontroller.sqlite').read_bytes(), before)

    def test_equivalent_output_only_removed(self):
        self.set_runner(self.code.replace(" db.execute('PRAGMA user_version=7')", ' pass'))
        for name in ('first', 'second'):
            result = self.bounded(operation=name)
            self.assertEqual(result['retention'], 'verified_equivalent_output_removed')
            self.assertFalse((self.root/name/'data/firecontroller.sqlite').exists())
            self.assertFalse((self.root/name/'migration-runner').exists())
            self.assertTrue((self.root/name/'terminal.json').exists())
        self.assertEqual(self.source.read_bytes(), self.original)

    def test_busy_locks_do_not_allocate(self):
        for path in (self.budget, self.source_lock, self.lease):
            with path.open() as fd:
                fcntl.flock(fd, fcntl.LOCK_EX|fcntl.LOCK_NB)
                with self.assertRaises(BlockingIOError):
                    self.bounded()
            self.assertFalse((self.root/'first').exists())

    def test_failed_output_blocks_next(self):
        self.set_runner(self.code+'\nraise SystemExit(1)\n')
        with self.assertRaises(Refuse):
            self.bounded()
        with self.assertRaisesRegex(Refuse, 'explicit disposition'):
            self.bounded(operation='second')
        self.assertFalse((self.root/'second').exists())

    def test_unknown_entries_and_unsafe_mode_refused(self):
        with self.assertRaisesRegex(Refuse, 'forbids'):
            self.bounded(discard_passed_output=True)
        (self.root/'unknown').touch()
        with self.assertRaisesRegex(Refuse, 'unknown'):
            self.bounded()

    def test_unexpected_file_blocks_reuse(self):
        self.set_runner(self.code.replace(" db.execute('PRAGMA user_version=7')", ' pass'))
        self.bounded()
        (self.root/'first/unknown').touch()
        with self.assertRaisesRegex(Refuse, 'unexpected'):
            self.bounded(operation='second')

    def test_source_phase_held_for_whole_runner(self):
        actual = bounded.operation.execute
        def run(**kwargs):
            for path in (self.source_lock, self.budget):
                with path.open() as fd:
                    with self.assertRaises(BlockingIOError):
                        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return actual(**kwargs)
        with patch.object(bounded.operation, 'execute', side_effect=run):
            self.bounded()

    def test_foreign_source_owner_must_be_explicit(self):
        with self.assertRaisesRegex(Refuse, 'ownership'):
            bounded.execute(budget_lease=self.budget, source_lease=self.source_lock,
                            source_lease_uid=os.geteuid()+1, **self.args)
        self.assertFalse((self.root/'first').exists())

    def test_preparer_requires_explicit_mode(self):
        policy = dict(root=str(self.root), budget_lease=str(self.budget),
                      source_lease=str(self.source_lock), source_lease_uid=os.geteuid(),
                      source_owner_uid=os.geteuid(), lease=str(self.lease))
        with self.assertRaises(Refuse):
            prepare_rehearsal.render({}, policy, self.base)
        config = {k:str(self.args[k]) for k in prepare_rehearsal.INPUTS}
        script = prepare_rehearsal.render(dict(config, mode='bounded'), policy, self.base)
        compile(script, 'rehearse.py', 'exec')
        self.assertIn("'supervised': True", script)
        self.assertIn(str(self.root), script)
        proof = dict(state='passed', storage_schema_unchanged=True, new_populated_rehearsal=False)
        skip = prepare_rehearsal.render(dict(mode='schema-unchanged', proof=proof), policy, self.base)
        self.assertNotIn('bounded_rehearsal', skip)
        compile(skip, 'rehearse.py', 'exec')

    def test_successor_mandatory_rehearsal_and_hashes(self):
        package = self.base/'package'
        package.mkdir()
        templates = {'image-next-template.py':"p='/old'\n", 'rollout-next-template.py':"p='/old'\n", 'image_layers.py':'# helper\n'}
        for name, body in templates.items():
            (package/name).write_text(body)
        policy = dict(root=str(self.root), budget_lease=str(self.budget),
                      source_lease=str(self.source_lock), source_lease_uid=os.geteuid(),
                      source_owner_uid=os.geteuid(), lease=str(self.lease))
        manifest = dict(source='a'*40, original_operation='/old', rehearsal_policy=policy,
                        sha256={n:hashlib.sha256(v.encode()).hexdigest() for n,v in templates.items()})
        (package/'manifest.json').write_text(json.dumps(manifest))
        target = self.base/'new'
        target.mkdir()
        replacements = {n:{'/old':str(target)} for n in ('image.py','rollout.py')}
        with self.assertRaises(Refuse):
            prepare_successor.prepare(package,target,replacements,{})
        self.assertEqual(list(target.iterdir()), [])
        proof=dict(state='passed',storage_schema_unchanged=True,new_populated_rehearsal=False)
        result=prepare_successor.prepare(package,target,replacements,dict(mode='schema-unchanged',proof=proof))
        self.assertEqual(result['rehearsal_mode'], 'schema-unchanged')
        self.assertEqual(set(result['outputs']), {'image.py','rollout.py','image_layers.py','rehearse.py'})
        with self.assertRaisesRegex(ValueError, 'exists'):
            prepare_successor.prepare(package,target,replacements,dict(mode='schema-unchanged',proof=proof))
        (package/'image_layers.py').write_text('# changed')
        with self.assertRaisesRegex(ValueError, 'template changed'):
            prepare_successor.prepare(package,target,replacements,dict(mode='schema-unchanged',proof=proof))

if __name__ == '__main__':
    unittest.main()
