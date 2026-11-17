"""Opt-in disposable local systemd observation; no output disposal."""
import fcntl
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import unittest
import uuid


def show(unit):
    fields = ('LoadState', 'ActiveState', 'SubState', 'InvocationID', 'ControlGroup', 'MainPID', 'ControlPID', 'ExecMainStatus', 'Result')
    result = subprocess.run(['/usr/bin/systemctl', 'show', unit, '--no-pager'] + ['--property=' + f for f in fields], capture_output=True, text=True, check=True, timeout=5)
    return dict(line.split('=', 1) for line in result.stdout.splitlines() if '=' in line)


def terminal(unit, expected_invocation, observed_group, lease):
    # Test-only cooperating-systemd model: a known earlier invocation/cgroup
    # is mandatory. Missing units and a new invocation are never success.
    with lease.open('r+') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        first = show(unit)
        if (not expected_invocation or not observed_group.startswith('/system.slice/')
                or first.get('LoadState') != 'loaded'
                or first.get('InvocationID') != expected_invocation
                or first.get('ActiveState') != 'failed'
                or first.get('SubState') != 'failed'
                or first.get('MainPID') != '0' or first.get('ControlPID') != '0'
                or first.get('Result') != 'exit-code' or first.get('ExecMainStatus') != '7'):
            raise ValueError('unqualified terminal invocation')
        cgroup = Path('/sys/fs/cgroup') / observed_group.lstrip('/')
        if cgroup.exists():
            # A retained cgroup with descendants is not terminal evidence.
            events = dict(line.split() for line in (cgroup/'cgroup.events').read_text().splitlines())
            if events.get('populated') != '0':
                raise ValueError('populated cgroup')
        if show(unit) != first:
            raise ValueError('unit changed across observation')
        return first


@unittest.skipUnless(os.environ.get('FC_REHEARSAL_SERVICE_PROOF') == '1', 'explicit disposable local systemd proof only')
class SupervisedFailureProof(unittest.TestCase):
    def test_real_nonzero_lease_and_invocation_fencing(self):
        self.assertEqual(os.geteuid(), 0)
        unit = 'fc-rehearsal-proof-' + uuid.uuid4().hex + '.service'
        self.assertEqual(show(unit)['LoadState'], 'not-found')
        with tempfile.TemporaryDirectory(prefix='fc-rehearsal-proof-', dir='/run') as tmp:
            root = Path(tmp); lease = root/'lease'; lease.touch(mode=0o600)
            script = root/'runner.py'
            script.write_text("import fcntl,time,sys\nf=open(sys.argv[1],'r+')\nfcntl.flock(f,fcntl.LOCK_EX)\ntime.sleep(.8)\nraise SystemExit(7)\n")
            def launch():
                subprocess.run(['/usr/bin/systemd-run', '--quiet', '--unit='+unit,
                    '--property=Type=exec', '--property=KillMode=control-group',
                    '--property=RuntimeMaxSec=10', '/usr/bin/python3', str(script), str(lease)], check=True, timeout=5)
            try:
                launch()
                deadline = time.monotonic()+5
                active = show(unit)
                while (not active.get('InvocationID') or not active.get('ControlGroup')) and time.monotonic()<deadline:
                    time.sleep(.01); active=show(unit)
                self.assertEqual(active['ActiveState'], 'active')
                invocation=active['InvocationID']; group=active['ControlGroup']
                # Active service must refuse even if child has not acquired lease yet.
                with self.assertRaises((ValueError, BlockingIOError)):
                    terminal(unit, invocation, group, lease)
                while show(unit)['ActiveState'] != 'failed' and time.monotonic()<deadline:
                    time.sleep(.02)
                receipt=terminal(unit, invocation, group, lease)
                self.assertEqual(receipt['ExecMainStatus'], '7')
                subprocess.run(['/usr/bin/systemctl','reset-failed',unit], check=True, timeout=5)
                with self.assertRaises(ValueError):
                    terminal(unit, invocation, group, lease)
                launch()
                second=show(unit)
                self.assertNotEqual(second['InvocationID'], invocation)
                with self.assertRaises((ValueError, BlockingIOError)):
                    terminal(unit, invocation, group, lease)
            finally:
                subprocess.run(['/usr/bin/systemctl','stop',unit], capture_output=True, timeout=15)
                subprocess.run(['/usr/bin/systemctl','reset-failed',unit], capture_output=True, timeout=5)
                self.assertEqual(show(unit)['LoadState'], 'not-found')

if __name__ == '__main__':
    unittest.main()
