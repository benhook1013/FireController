import contextlib
import importlib.util
import io
import json
from pathlib import Path
import re
import shutil
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
        files = {
            "skills/firecontroller-use/SKILL.md": b"[other](../firecontroller-reviews/SKILL.md)\n",
            "skills/firecontroller-reviews/SKILL.md": b"[schedule](../../docs/review-scheduling.md#control) [ops](references/operations.md)\n",
            "skills/firecontroller-reviews/references/operations.md": ("[current](" + clients.REPOSITORY + "/blob/main/docs/review-scheduling.md#control) [old](" + historical + ")\n").encode(),
            "docs/review-scheduling.md": b"# Control\n[details](more.md) [worker](../skills/firecontroller-use/SKILL.md) [source](../internal/example.go)\n",
            "docs/more.md": b"# Details\n[json](source-inventory.json)\n",
            "docs/source-inventory.json": b"{}\n",
        }
        original = dict(files)
        generated = clients.installed_skill_guidance(files, revision)
        self.assertEqual(files, original)
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
            # extraction and the operator's whole-directory merge are real.
            with patch.object(clients.subprocess, "check_output", side_effect=build_metadata), contextlib.redirect_stdout(io.StringIO()):
                clients.package(binaries, "linux-amd64", temporary / "out", revision)
            package = temporary / "release"
            with tarfile.open(temporary / "out/firecontroller-linux-amd64.tar.gz") as archive:
                archive.extractall(package, filter="data")
            self.assertEqual(json.loads((package / "source.json").read_text())["source_revision"], revision)
            installed = temporary / "host skills"
            custom = installed / "firecontroller-reviews/references/host-local.md"
            custom.parent.mkdir(parents=True)
            custom.write_text("Custom host instructions.\n")
            metadata = installed / "firecontroller-reviews/agents/openai.yaml"
            metadata.parent.mkdir()
            metadata.write_text("custom: preserve\n")
            for skill in (package / "skills").iterdir():
                shutil.copytree(skill, installed / skill.name, dirs_exist_ok=True)
            self.assertEqual(custom.read_text(), "Custom host instructions.\n")
            self.assertEqual(metadata.read_text(), "custom: preserve\n")
            self.assertFalse((temporary / "docs").exists())
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


if __name__ == "__main__":
    unittest.main()
