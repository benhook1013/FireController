import fcntl
import hashlib
import sqlite3
import tempfile
import unittest
from pathlib import Path

import deploy
import deploy_host as h

OLD = "b59eb13e2406075995b037ce594a03c9f760f4af"
NEW = "a15c23296c72ff2cb43dfa500515ca6a90627799"
HEALTH = ("[Service]\nExecStart=\nExecStart=/usr/bin/python3 /x/releases/9530c8a8660be56a5dbcae0952c4b053b430e352/publish-backup-health.py"
          " --fcd /x/releases/" + OLD + "/fcd --sha256 " + "1" * 64 + "\n")
BATCH = "[Service]\nExecStart=\nExecStart=/x/releases/" + OLD + "/fcd backup-push-batch --config /etc/firecontroller/offsite-batch.json\n"


class DropIns(unittest.TestCase):
    def test_repoints_binary_and_hash_only(self):
        out = h.repoint_dropin(HEALTH, NEW, "2" * 64)
        self.assertIn("/releases/" + NEW + "/fcd --sha256 " + "2" * 64, out)
        self.assertIn("9530c8a8660be56a5dbcae0952c4b053b430e352/publish-backup-health.py", out)
        self.assertNotIn(OLD, out)

    def test_batch_has_no_hash(self):
        self.assertEqual(h.repoint_dropin(BATCH, NEW, "2" * 64), BATCH.replace(OLD, NEW))

    def test_ambiguous_dropins_refused(self):
        for text in ["[Service]\n", BATCH + BATCH, HEALTH.replace("\n", " --sha256 " + "3" * 64 + "\n")]:
            with self.assertRaises(h.DeployError):
                h.repoint_dropin(text, NEW, "2" * 64)

    def test_install_helpers_copies_once_and_rewrites(self):
        with tempfile.TemporaryDirectory() as d:
            d = Path(d)
            fcd = d / "fcd"
            fcd.write_bytes(b"binary")
            digest = hashlib.sha256(b"binary").hexdigest()
            releases = d / "releases"
            releases.mkdir()
            dropins = {"health": d / "50.conf", "batch": d / "60.conf"}
            dropins["health"].write_text(HEALTH)
            dropins["batch"].write_text(BATCH)
            changed = h.install_helpers(fcd, NEW, digest, releases, dropins, reload=False)
            self.assertEqual(sorted(changed), ["batch", "health"])
            self.assertEqual((releases / NEW / "fcd").read_bytes(), b"binary")
            # A rerun is a no-op; a different binary for the same source is refused.
            self.assertEqual(h.install_helpers(fcd, NEW, digest, releases, dropins, reload=False), [])
            fcd.write_bytes(b"other")
            with self.assertRaises(h.DeployError):
                h.install_helpers(fcd, NEW, hashlib.sha256(b"other").hexdigest(), releases, dropins, reload=False)


class Snapshots(unittest.TestCase):
    def test_snapshot_copies_live_rows_and_keeps_two(self):
        with tempfile.TemporaryDirectory() as d:
            d = Path(d)
            live = d / "live.sqlite"
            c = sqlite3.connect(live)
            c.execute("PRAGMA journal_mode=WAL")
            c.execute("CREATE TABLE t(x)")
            c.execute("INSERT INTO t VALUES (1),(2)")
            c.commit()  # keep the writer open, as the daemon does
            root = d / "snapshots"
            for label in ["20261118T010000Z-a", "20261118T020000Z-b", "20261118T030000Z-c"]:
                path = h.snapshot(live, root, label)
            c.close()
            self.assertEqual(sorted(p.name for p in root.iterdir()), ["20261118T020000Z-b", "20261118T030000Z-c"])
            s = sqlite3.connect(path)
            self.assertEqual(s.execute("SELECT count(*) FROM t").fetchone()[0], 2)
            s.close()
            self.assertEqual(oct(path.stat().st_mode & 0o777), "0o600")


class Locks(unittest.TestCase):
    def test_wait_times_out_without_holding(self):
        with tempfile.NamedTemporaryFile() as f:
            holder = open(f.name, "r+")
            fcntl.flock(holder, fcntl.LOCK_EX)
            clock = iter(range(0, 100, 10))
            with self.assertRaises(h.DeployError):
                h.flock_wait(f.name, 15, sleep=lambda s: None, clock=lambda: next(clock))
            holder.close()
            got = h.flock_wait(f.name, 0)
            got.close()


class Digest(unittest.TestCase):
    def test_parse_digest(self):
        listing = ("REF TYPE DIGEST SIZE\n"
                   "localhost/firecontroller:" + NEW + " application/vnd.docker.distribution.manifest.v2+json sha256:" + "a" * 64 + " 100MiB\n")
        self.assertEqual(deploy.parse_digest(listing, "localhost/firecontroller:" + NEW), "sha256:" + "a" * 64)
        with self.assertRaises(SystemExit):
            deploy.parse_digest(listing, "localhost/firecontroller:" + OLD)


if __name__ == "__main__":
    unittest.main()


class Prune(unittest.TestCase):
    def test_keeps_referenced_and_recent_helper_releases(self):
        import os, time
        from unittest import mock
        with tempfile.TemporaryDirectory() as d:
            d = Path(d)
            releases = d / "releases"
            names = [c * 40 for c in "abcde"]
            for i, n in enumerate(names):
                (releases / n).mkdir(parents=True)
                os.utime(releases / n, (time.time() - 100 + i, time.time() - 100 + i))
            unit = d / "60.conf"
            unit.write_text("ExecStart=/x/releases/" + names[0] + "/fcd backup-push-batch\n")
            listing = mock.Mock(stdout="REF TYPE DIGEST\nlocalhost/firecontroller@sha256:1 t sha256:1\nlocalhost/firecontroller@sha256:2 t sha256:2\ndocker.io/x t sha256:3\n")
            with mock.patch.object(h.subprocess, "run", return_value=listing) as run:
                out = h.prune_after_deploy({"sha256:1"}, releases, [unit])
            self.assertEqual(out, {"images_removed": 1, "helper_releases_removed": 2})
            self.assertEqual(sorted(p.name for p in releases.iterdir()), sorted([names[0], names[3], names[4]]))
            self.assertIn(["k3s", "ctr", "-n", "k8s.io", "images", "rm", "localhost/firecontroller@sha256:2"], [c.args[0] for c in run.call_args_list])
