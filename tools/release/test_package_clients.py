import contextlib
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch
from urllib.parse import unquote, urlsplit

spec = importlib.util.spec_from_file_location("package_clients", Path(__file__).with_name("package-clients.py"))
clients = importlib.util.module_from_spec(spec)
spec.loader.exec_module(clients)


class InstalledGuidanceTests(unittest.TestCase):
    def test_routes_relative_and_moving_links_without_changing_prose_or_history(self):
        revision = "a" * 40
        historical = clients.REPOSITORY + "/blob/" + "b" * 40 + "/docs/history.md"
        usage_skill = (Path(__file__).resolve().parents[2] / "skills/firecontroller-use/SKILL.md").read_bytes()
        opening = b"<!-- BEGIN MCP SERVER INSTRUCTIONS -->"
        closing = b"<!-- END MCP SERVER INSTRUCTIONS -->"
        files = {
            "skills/firecontroller-use/SKILL.md": usage_skill,
            "skills/firecontroller-use/mcp_guidance.go": b"build input",
            "skills/firecontroller-use/references/jobs.md": b"",
            "skills/firecontroller-use/references/coordination.md": b"",
            "skills/firecontroller-use/references/editorial.md": b"",
            "skills/firecontroller-reviews/SKILL.md": b"[schedule](../../docs/review-scheduling.md#control) [ops](references/operations.md)\n",
            "skills/firecontroller-reviews/references/operations.md": ("[current](" + clients.REPOSITORY + "/blob/main/docs/review-scheduling.md#control) [old](" + historical + ")\n").encode(),
            "docs/review-scheduling.md": b"# Control\n[details](more.md) [worker](../skills/firecontroller-use/SKILL.md) [source](../internal/example.go)\n",
            "docs/more.md": b"# Details\n[json](source-inventory.json)\n",
            "docs/source-inventory.json": b"{}\n",
        }
        original = dict(files)
        generated = clients.installed_skill_guidance(files, revision)
        self.assertEqual(files, original)
        self.assertNotIn("skills/firecontroller-use/mcp_guidance.go", generated)
        installed = generated["skills/firecontroller-use/SKILL.md"]
        self.assertEqual(installed.split(opening)[1].split(closing)[0], usage_skill.split(opening)[1].split(closing)[0])
        self.assertIn(b"../firecontroller-reviews/SKILL.md", installed)
        self.assertIn(b"references/docs/review-scheduling.md#control", generated["skills/firecontroller-reviews/SKILL.md"])
        operations = generated["skills/firecontroller-reviews/references/operations.md"].decode()
        self.assertIn("docs/review-scheduling.md#control", operations)
        self.assertIn(historical, operations)
        document = generated["skills/firecontroller-reviews/references/docs/review-scheduling.md"].decode()
        self.assertIn("../../../firecontroller-use/SKILL.md", document)
        self.assertIn("/blob/" + revision + "/internal/example.go", document)
        self.assertEqual(generated["skills/firecontroller-reviews/references/docs/source-inventory.json"], b"{}\n")
        self.assertEqual({p for p in generated if p.endswith("/SKILL.md")}, {p for p in original if p.endswith("/SKILL.md")})

    def test_missing_docs_and_generated_namespace_collision_refuse_packaging(self):
        for files in [
            {"skills/test/SKILL.md": b"[missing](../../docs/missing.md)"},
            {"skills/test/SKILL.md": b"", "docs/guide.md": b"# Guide", "skills/test/references/docs/guide.md": b"maintained duplicate"},
        ]:
            with self.subTest(files=list(files)), self.assertRaises(ValueError):
                clients.installed_skill_guidance(files, "a" * 40)

    def test_real_release_archive_temp_install_link_closure_and_custom_metadata(self):
        root = Path(__file__).resolve().parents[2]
        revision = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=root, text=True).strip()
        check_output = subprocess.check_output

        def build_metadata(command, **kwargs):
            if command[:3] == ["go", "version", "-m"]:
                return "vcs.revision=" + revision + "\nvcs.modified=false\n"
            return check_output(command, **kwargs)

        with tempfile.TemporaryDirectory(prefix="fc guidance with spaces ") as temporary:
            temporary = Path(temporary)
            binaries = temporary / "bin"
            binaries.mkdir()
            for name in ("fc", "fcd", "fcmcp"):
                (binaries / name).write_bytes(b"synthetic tested binary")
            # Only binary build metadata is synthetic. Git archive, packaging,
            # extraction and the installer merge are real.
            with patch.object(clients.subprocess, "check_output", side_effect=build_metadata), contextlib.redirect_stdout(io.StringIO()):
                clients.package(binaries, "linux-amd64", temporary / "out", revision)
            package = temporary / "release"
            bundle = temporary / "out/firecontroller-linux-amd64.tar.gz"
            checksum = hashlib.sha256(bundle.read_bytes()).hexdigest()
            with tarfile.open(bundle) as archive:
                archive.extractall(package, filter="data")
            self.assertEqual(json.loads((package / "source.json").read_text())["source_revision"], revision)
            for client, layout in clients.SKILL_ROOTS.items():
                with self.subTest(client=client):
                    self.verify_installed_guidance(package, bundle, checksum, revision, client, temporary, layout)

    def verify_installed_guidance(self, package, bundle, checksum, revision, client, scope, layout):
        installed = scope / layout
        custom = installed / "firecontroller-reviews/references/host-local.md"
        custom.parent.mkdir(parents=True)
        custom.write_text("Custom host instructions.\n")
        metadata = installed / "firecontroller-reviews/agents/openai.yaml"
        metadata.parent.mkdir()
        metadata.write_text("custom: preserve\n")
        existing_skill = installed / "firecontroller-reviews/SKILL.md"
        existing_skill.write_text("---\nname: firecontroller-reviews\n---\nPrior release.\n")
        with contextlib.redirect_stdout(io.StringIO()) as plan:
            clients.install_skills(bundle, checksum, revision, client, scope)
        self.assertIn("planned", plan.getvalue())
        self.assertIn("Prior release.", existing_skill.read_text())
        self.assertFalse((installed / "firecontroller-use").exists())
        with contextlib.redirect_stdout(io.StringIO()):
            clients.install_skills(bundle, checksum, revision, client, scope, apply=True)
        with contextlib.redirect_stdout(io.StringIO()) as replay:
            clients.install_skills(bundle, checksum, revision, client, scope, apply=True)
        self.assertIn("installed 0 changes", replay.getvalue())
        self.assertFalse((installed / "firecontroller-onboard").exists())
        self.assertEqual(custom.read_text(), "Custom host instructions.\n")
        self.assertEqual(metadata.read_text(), "custom: preserve\n")
        self.assertFalse((installed.parent / "docs").exists())
        for name in ("firecontroller-use", "firecontroller-reviews"):
            manifest = Path(name) / "SKILL.md"
            self.assertEqual((installed / manifest).read_bytes(), (package / "skills" / manifest).read_bytes())
        linked = 0
        for source in installed.rglob("*.md"):
            for destination in re.findall(r"\]\(([^\s)]+)", source.read_text()):
                destination = destination.strip("<>")
                parsed = urlsplit(destination)
                if parsed.scheme or parsed.netloc:
                    self.assertFalse(destination.startswith(clients.REPOSITORY + "/blob/main/"))
                    continue
                target = (source.parent / unquote(parsed.path)).resolve() if parsed.path else source
                self.assertTrue(target.is_relative_to(installed.resolve()), (source, destination))
                self.assertTrue(target.exists(), (source, destination))
                linked += 1
        self.assertGreater(linked, 100)
        scheduling = installed / "firecontroller-reviews/references/docs/review-scheduling.md"
        self.assertTrue(scheduling.is_file())
        # Generated documentation differs only in link destinations.
        normalize = lambda body: re.sub(r"(?<=\]\()[^\s)]+", "LINK", body)
        self.assertEqual(normalize(scheduling.read_text()), normalize((package / "docs/review-scheduling.md").read_text()))


class InstallSkillBoundaryTests(unittest.TestCase):
    revision = "a" * 40
    predecessor = "b" * 40

    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.bundle = self.root / "current.tar.gz"
        self.files = {
            "source.json": json.dumps({"source_revision": self.revision}).encode(),
            "skills/firecontroller-use/SKILL.md": b"---\nname: firecontroller-use\n---\nCurrent.\n",
            "skills/firecontroller-reviews/SKILL.md": b"---\nname: firecontroller-reviews\n---\nCurrent.\n",
        }
        self.write_bundle()
        self.installed = self.root / ".claude/skills"

    def write_bundle(self):
        with tarfile.open(self.bundle, "w:gz") as archive:
            for name, data in self.files.items():
                member = tarfile.TarInfo(name)
                member.size = len(data)
                archive.addfile(member, io.BytesIO(data))
        self.checksum = hashlib.sha256(self.bundle.read_bytes()).hexdigest()

    def install(self, **options):
        with contextlib.redirect_stdout(io.StringIO()):
            clients.install_skills(self.bundle, options.pop("checksum", self.checksum),
                                   options.pop("revision", self.revision), "claude-code",
                                   self.root, **options)

    def test_refuses_unverified_bundles_and_skill_path_conflicts_before_writing(self):
        for options, reason in (
            ({"checksum": "0" * 64}, "SHA-256 mismatch"),
            ({"revision": "0" * 40}, "source revision mismatch"),
        ):
            with self.subTest(options=options), self.assertRaisesRegex(ValueError, reason):
                self.install(apply=True, **options)
            self.assertFalse(self.installed.exists())

        self.files["skills/firecontroller-use/../foreign"] = b"invalid"
        self.write_bundle()
        with self.assertRaisesRegex(ValueError, "unexpected member"):
            self.install(apply=True)
        self.assertFalse(self.installed.exists())

    def test_preserves_unrelated_skill_collision_and_first_skill(self):
        review = self.installed / "firecontroller-reviews/SKILL.md"
        review.parent.mkdir(parents=True)
        review.write_text("---\nname: other-skill\n---\nLocal.\n")
        with self.assertRaisesRegex(ValueError, "belongs to a different skill"):
            self.install(apply=True)
        self.assertIn("Local.", review.read_text())
        self.assertFalse((self.installed / "firecontroller-use").exists())

    def test_preserves_unowned_file_at_a_package_path(self):
        relative = "firecontroller-reviews/references/operations.md"
        self.files["skills/" + relative] = b"Packaged instructions."
        self.write_bundle()
        local = self.installed / relative
        local.parent.mkdir(parents=True)
        local.write_bytes(b"Local instructions.")
        with self.assertRaisesRegex(ValueError, "no matching installed manifest"):
            self.install(apply=True)
        self.assertEqual(local.read_bytes(), b"Local instructions.")
        self.assertFalse((self.installed / "firecontroller-use").exists())

    @unittest.skipUnless(__import__("os").name == "posix", "POSIX symlink fixture")
    def test_only_managed_links_are_replaced_and_current_generation_is_verified(self):
        generation = self.root / "releases" / self.revision
        for name, body in self.files.items():
            path = generation / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(body)
        current = generation.parent / "current"
        current.symlink_to(generation)
        previous = generation.parent / self.predecessor
        old_usage = previous / "skills/firecontroller-use"
        old_usage.mkdir(parents=True)
        (previous / "source.json").write_text(json.dumps({"source_revision": self.predecessor}))
        (old_usage / "SKILL.md").write_text("Prior release.\n")

        self.installed.mkdir(parents=True)
        usage = self.installed / "firecontroller-use"
        review = self.installed / "firecontroller-reviews"
        usage.symlink_to(old_usage)
        foreign = self.root / "unrelated"
        foreign.mkdir()
        review.symlink_to(foreign)

        with self.assertRaisesRegex(ValueError, "occupied"):
            self.install(apply=True, managed_current=current)
        self.assertEqual(usage.readlink(), old_usage)
        self.assertEqual(review.readlink(), foreign)
        self.assertEqual(list(foreign.iterdir()), [])

        review.unlink()
        review.symlink_to(current / "skills/firecontroller-reviews")
        review_stat = review.lstat()
        self.install(managed_current=current)
        self.assertEqual(usage.readlink(), old_usage)
        self.install(apply=True, managed_current=current)
        self.assertEqual(usage.readlink(), current / "skills/firecontroller-use")
        self.assertEqual(usage.resolve(), generation / "skills/firecontroller-use")
        self.assertEqual(review.lstat(), review_stat)
        self.assertEqual((old_usage / "SKILL.md").read_text(), "Prior release.\n")
        self.assertEqual((usage / "SKILL.md").read_bytes(), self.files["skills/firecontroller-use/SKILL.md"])

        (generation / "skills/firecontroller-use/SKILL.md").write_text("Local modification.\n")
        with self.assertRaisesRegex(ValueError, "differs from the verified bundle"):
            self.install(apply=True, managed_current=current)
        self.assertEqual((usage / "SKILL.md").read_text(), "Local modification.\n")


if __name__ == "__main__":
    unittest.main()


class ArtifactLeaseTests(unittest.TestCase):
    def test_absent_environment_preserves_legacy_options(self):
        import os
        from unittest.mock import patch
        with patch.dict(os.environ, {}, clear=True):
            self.assertEqual(clients.artifact_lease_options(), {})

    def test_invalid_or_closed_descriptor_refuses(self):
        import os
        from unittest.mock import patch
        for value in ("", "0", "2", "-1", "abc", "3.0", "999999999999999"):
            with self.subTest(value=value), patch.dict(os.environ, {"FC_ARTIFACT_LEASE_FD":value}), self.assertRaises(ValueError):
                clients.artifact_lease_options()
        with tempfile.TemporaryFile() as file:
            closed = file.fileno()
        with patch.dict(os.environ, {"FC_ARTIFACT_LEASE_FD":str(closed)}), self.assertRaises(ValueError):
            clients.artifact_lease_options()

    @unittest.skipUnless(__import__("os").name == "posix", "inherited POSIX lease")
    def test_real_child_inherits_descriptor(self):
        import os
        import sys
        from unittest.mock import patch
        with tempfile.TemporaryFile() as file, patch.dict(os.environ, {"FC_ARTIFACT_LEASE_FD":str(file.fileno())}):
            options = clients.artifact_lease_options()
            self.assertEqual(options, {"pass_fds":(file.fileno(),)})
            child = subprocess.run([sys.executable, "-c", "import os; os.fstat(int(os.environ['FC_ARTIFACT_LEASE_FD']))"], **options)
            self.assertEqual(child.returncode,0)

    def test_all_direct_subprocess_sites_preserve_lease(self):
        import ast
        tree=ast.parse(Path(clients.__file__).read_text())
        calls=[node for node in ast.walk(tree) if isinstance(node,ast.Call) and isinstance(node.func,ast.Attribute) and isinstance(node.func.value,ast.Name) and node.func.value.id=="subprocess"]
        self.assertEqual(len(calls),3)
        for call in calls:
            self.assertTrue(any(keyword.arg is None and isinstance(keyword.value,ast.Call) and isinstance(keyword.value.func,ast.Name) and keyword.value.func.id=="artifact_lease_options" for keyword in call.keywords))

    def test_invalid_lease_refuses_before_any_external_action(self):
        import os
        from unittest.mock import patch
        with patch.dict(os.environ, {"FC_ARTIFACT_LEASE_FD":"invalid"}), patch.object(clients.subprocess,"run") as run, patch.object(clients.subprocess,"check_output") as output:
            with self.assertRaises(ValueError):
                clients.package("unused", "linux-amd64", "unused", "a" * 40)
            run.assert_not_called()
            output.assert_not_called()
